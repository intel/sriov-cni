package main

import (
	"errors"
	"fmt"
	"runtime"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ipam"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/k8snetworkplumbingwg/sriov-cni/pkg/config"
	"github.com/k8snetworkplumbingwg/sriov-cni/pkg/sriov"
	"github.com/k8snetworkplumbingwg/sriov-cni/pkg/utils"
	"github.com/vishvananda/netlink"
)

type envArgs struct {
	types.CommonArgs
	MAC types.UnmarshallableString `json:"mac,omitempty"`
}

func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

func getEnvArgs(envArgsString string) (*envArgs, error) {
	if envArgsString != "" {
		e := envArgs{}
		err := types.LoadArgs(envArgsString, &e)
		if err != nil {
			return nil, err
		}
		return &e, nil
	}
	return nil, nil
}

func cmdAdd(args *skel.CmdArgs) error {
	var macAddr string
	netConf, err := config.LoadConf(args.StdinData)
	if err != nil {
		return fmt.Errorf("SRIOV-CNI failed to load netconf: %v", err)
	}

	envArgs, err := getEnvArgs(args.Args)
	if err != nil {
		return fmt.Errorf("SRIOV-CNI failed to parse args: %v", err)
	}

	if envArgs != nil {
		MAC := string(envArgs.MAC)
		if MAC != "" {
			netConf.MAC = MAC
		}
	}

	// RuntimeConfig takes preference than envArgs.
	// This maintains compatibility of using envArgs
	// for MAC config.
	if netConf.RuntimeConfig.Mac != "" {
		netConf.MAC = netConf.RuntimeConfig.Mac
	}

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", netns, err)
	}
	defer netns.Close()

	sm := sriov.NewSriovManager()
	if err := sm.ApplyVFConfig(netConf); err != nil {
		return fmt.Errorf("SRIOV-CNI failed to configure VF %q", err)
	}

	result := &current.Result{}
	result.Interfaces = []*current.Interface{{
		Name:    args.IfName,
		Sandbox: netns.Path(),
	}}

	if !netConf.DPDKMode {
		macAddr, err = sm.SetupVF(netConf, args.IfName, args.ContainerID, netns)
		defer func() {
			if err != nil {
				err := netns.Do(func(_ ns.NetNS) error {
					_, err := netlink.LinkByName(args.IfName)
					return err
				})
				if err == nil {
					_ = sm.ReleaseVF(netConf, args.IfName, args.ContainerID, netns)
				}
			}
		}()
		if err != nil {
			return fmt.Errorf("failed to set up pod interface %q from the device %q: %v", args.IfName, netConf.Master, err)
		}
		result.Interfaces[0].Mac = macAddr
	}

	// run the IPAM plugin
	if netConf.IPAM.Type != "" {
		r, err := ipam.ExecAdd(netConf.IPAM.Type, args.StdinData)
		if err != nil {
			return fmt.Errorf("failed to set up IPAM plugin type %q from the device %q: %v", netConf.IPAM.Type, netConf.Master, err)
		}

		defer func() {
			if err != nil {
				_ = ipam.ExecDel(netConf.IPAM.Type, args.StdinData)
			}
		}()

		// Convert the IPAM result into the current Result type
		newResult, err := current.NewResultFromResult(r)
		if err != nil {
			return err
		}

		if len(newResult.IPs) == 0 {
			return errors.New("IPAM plugin returned missing IP config")
		}

		newResult.Interfaces = result.Interfaces

		for _, ipc := range newResult.IPs {
			// All addresses apply to the container interface (move from host)
			ipc.Interface = current.Int(0)
		}

		if !netConf.DPDKMode {
			err = netns.Do(func(_ ns.NetNS) error {
				return ipam.ConfigureIface(args.IfName, newResult)
			})
			if err != nil {
				return err
			}
		}
		result = newResult
	}

	// Cache NetConf for CmdDel
	if err = utils.SaveNetConf(args.ContainerID, config.DefaultCNIDir, args.IfName, netConf); err != nil {
		return fmt.Errorf("error saving NetConf %q", err)
	}

	return types.PrintResult(result, current.ImplementedSpecVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	netConf, cRefPath, err := config.LoadConfFromCache(args)
	if err != nil {
		// If cmdDel() fails, cached netconf is cleaned up by
		// the followed defer call. However, subsequence calls
		// of cmdDel() from kubelet fail in a dead loop due to
		// cached netconf doesn't exist.
		// Return nil when LoadConfFromCache fails since the rest
		// of cmdDel() code relies on netconf as input argument
		// and there is no meaning to continue.
		return nil
	}

	defer func() {
		if err == nil && cRefPath != "" {
			_ = utils.CleanCachedNetConf(cRefPath)
		}
	}()

	if netConf.IPAM.Type != "" {
		err = ipam.ExecDel(netConf.IPAM.Type, args.StdinData)
		if err != nil {
			return err
		}
	}

	// https://github.com/kubernetes/kubernetes/pull/35240
	if args.Netns == "" {
		return nil
	}

	sm := sriov.NewSriovManager()

	defer func() {
		sm.ResetVFMacAddress(netConf)
	}()

	if !netConf.DPDKMode {
		netns, err := ns.GetNS(args.Netns)
		if err != nil {
			// according to:
			// https://github.com/kubernetes/kubernetes/issues/43014#issuecomment-287164444
			// if provided path does not exist (e.x. when node was restarted)
			// plugin should silently return with success after releasing
			// IPAM resources
			_, ok := err.(ns.NSPathNotExistErr)
			if ok {
				return nil
			}

			return fmt.Errorf("failed to open netns %s: %q", netns, err)
		}
		defer netns.Close()

		if err = sm.ReleaseVF(netConf, args.IfName, args.ContainerID, netns); err != nil {
			return err
		}
	}

	if err := sm.ResetVFConfig(netConf); err != nil {
		return fmt.Errorf("cmdDel() error reseting VF: %q", err)
	}

	return nil
}

func cmdCheck(args *skel.CmdArgs) error {
	return nil
}

func main() {
	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, "")
}
