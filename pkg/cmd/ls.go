// Portions Copyright (C) 2020 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0
package commands

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/moby/buildkit/util/appcontext"
	"github.com/spf13/cobra"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/pkg/driver"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/pkg/platformutil"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

type lsOptions struct {
	commonKubeOptions
}

func runLs(streams genericclioptions.IOStreams, in lsOptions) error {
	ctx := appcontext.Context()

	var builders []driver.Builder
	for name, factory := range driver.GetFactories() {
		d, err := driver.GetDriver(ctx, name, factory, in.KubeClientConfig, nil, "", nil, "")
		if err != nil {
			return err
		}
		b, err := d.List(ctx)
		if err != nil {
			return err
		}
		builders = append(builders, b...)
	}

	w := tabwriter.NewWriter(streams.Out, 0, 0, 1, ' ', 0)
	fmt.Fprintf(w, "NAME\tNODE\tDRIVER\tSTATUS\tPLATFORMS\n")

	for _, b := range builders {
		for _, n := range b.Nodes {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", b.Name, n.Name, b.Driver, n.Status, strings.Join(platformutil.FormatInGroups(n.Platforms), ", "))
		}
	}

	w.Flush()

	return nil
}

func lsCmd(streams genericclioptions.IOStreams) *cobra.Command {
	options := lsOptions{
		commonKubeOptions: commonKubeOptions{
			configFlags: genericclioptions.NewConfigFlags(true),
			IOStreams:   streams,
		},
	}

	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List builder instances",
		Args:  ExactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := options.Complete(cmd, args); err != nil {
				return err
			}
			if err := options.Validate(); err != nil {
				return err
			}

			return runLs(streams, options)
		},
		SilenceUsage: true,
	}
	options.configFlags.AddFlags(cmd.Flags())

	return cmd
}
