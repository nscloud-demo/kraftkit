// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2022, Unikraft GmbH and The KraftKit Authors.
// Licensed under the BSD-3-Clause License (the "License").
// You may not use this file except in compliance with the License.
package remove

import (
	"context"
	"fmt"

	"github.com/MakeNowJust/heredoc"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	machineapi "kraftkit.sh/api/machine/v1alpha1"
	networkapi "kraftkit.sh/api/network/v1alpha1"
	"kraftkit.sh/cmdfactory"
	"kraftkit.sh/iostreams"
	"kraftkit.sh/log"
	"kraftkit.sh/machine/network"
	mplatform "kraftkit.sh/machine/platform"
)

type RemoveOptions struct {
	All      bool `long:"all" usage:"Remove all machines"`
	platform string
}

// Remove stops and deletes a local Unikraft virtual machine.
func Remove(ctx context.Context, opts *RemoveOptions, args ...string) error {
	if opts == nil {
		opts = &RemoveOptions{}
	}

	return opts.Run(ctx, args)
}

func NewCmd() *cobra.Command {
	cmd, err := cmdfactory.New(&RemoveOptions{}, cobra.Command{
		Short:   "Remove one or more running unikernels",
		Use:     "rm [FLAGS] MACHINE [MACHINE [...]]",
		Args:    cobra.MinimumNArgs(0),
		Aliases: []string{"remove"},
		Long: heredoc.Doc(`
			Remove one or more running unikernels`),
		Annotations: map[string]string{
			cmdfactory.AnnotationHelpGroup: "run",
		},
	})
	if err != nil {
		panic(err)
	}

	cmd.Flags().VarP(
		cmdfactory.NewEnumFlag[mplatform.Platform](
			mplatform.Platforms(),
			mplatform.Platform("auto"),
		),
		"plat",
		"p",
		"Set the platform virtual machine monitor driver.  Set to 'auto' to detect the guest's platform and 'host' to use the host platform.",
	)

	return cmd
}

func (opts *RemoveOptions) Pre(cmd *cobra.Command, _ []string) error {
	opts.platform = cmd.Flag("plat").Value.String()
	return nil
}

func (opts *RemoveOptions) Run(ctx context.Context, args []string) error {
	var err error

	if len(args) == 0 && !opts.All {
		return fmt.Errorf("no machine(s) specified")
	}

	platform := mplatform.PlatformUnknown
	var controller machineapi.MachineService

	if opts.All || opts.platform == "auto" {
		controller, err = mplatform.NewMachineV1alpha1ServiceIterator(ctx)
	} else {
		if opts.platform == "host" {
			platform, _, err = mplatform.Detect(ctx)
			if err != nil {
				return err
			}
		} else {
			var ok bool
			platform, ok = mplatform.PlatformsByName()[opts.platform]
			if !ok {
				return fmt.Errorf("unknown platform driver: %s", opts.platform)
			}
		}

		strategy, ok := mplatform.Strategies()[platform]
		if !ok {
			return fmt.Errorf("unsupported platform driver: %s (contributions welcome!)", platform.String())
		}

		controller, err = strategy.NewMachineV1alpha1(ctx)
	}
	if err != nil {
		return err
	}

	machines, err := controller.List(ctx, &machineapi.MachineList{})
	if err != nil {
		return err
	}

	var remove []machineapi.Machine

	for _, machine := range machines.Items {
		if len(args) == 0 && opts.All {
			remove = append(remove, machine)
			continue
		}

		if args[0] == machine.Name || args[0] == string(machine.UID) {
			remove = append(remove, machine)
		}
	}

	if len(remove) == 0 {
		return fmt.Errorf("machine(s) not found")
	}

	netcontrollers := make(map[string]networkapi.NetworkService, 0)

	for _, machine := range remove {
		// First remove all the associated network interfaces.
		for _, net := range machine.Spec.Networks {
			netcontroller, ok := netcontrollers[net.Driver]

			// Store the instantiation of the network controller strategy.
			if !ok {
				strategy, ok := network.Strategies()[net.Driver]
				if !ok {
					return fmt.Errorf("unknown machine network driver: %s", net.Driver)
				}

				netcontroller, err = strategy.NewNetworkV1alpha1(ctx)
				if err != nil {
					return err
				}

				netcontrollers[net.Driver] = netcontroller
			}

			// Get the latest version of the network.
			found, err := netcontroller.Get(ctx, &networkapi.Network{
				ObjectMeta: metav1.ObjectMeta{
					Name: net.IfName,
				},
			})
			if err != nil {
				log.G(ctx).Warnf("could not get network information for %s: %v", net.IfName, err)
				continue
			}

			for _, machineIface := range net.Interfaces {
				// Remove the associated network interfaces
				for i, netIface := range found.Spec.Interfaces {
					if machineIface.UID == netIface.UID {
						ret := make([]networkapi.NetworkInterfaceTemplateSpec, 0)
						ret = append(ret, found.Spec.Interfaces[:i]...)
						found.Spec.Interfaces = append(ret, found.Spec.Interfaces[i+1:]...)
						break
					}
				}

				if _, err = netcontroller.Update(ctx, found); err != nil {
					log.G(ctx).Warnf("could not update network %s: %v", net.IfName, err)
					continue
				}
			}
		}

		// Stop the machine before deleting it.
		if machine.Status.State == machineapi.MachineStateRunning {
			if _, err := controller.Stop(ctx, &machine); err != nil {
				log.G(ctx).Errorf("could not stop machine %s: %v", machine.Name, err)
			}
		}

		// Now delete the machine.
		if _, err := controller.Delete(ctx, &machine); err != nil {
			log.G(ctx).Errorf("could not delete machine %s: %v", machine.Name, err)
		} else {
			fmt.Fprintln(iostreams.G(ctx).Out, machine.Name)
		}
	}

	return nil
}
