// Portions Copyright (C) 2020 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0
package commands

import (
	"context"
	"fmt"

	"github.com/moby/buildkit/util/appcontext"
	"github.com/spf13/cobra"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/pkg/driver"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/version"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

type versionOptions struct {
	builder string
	commonKubeOptions
}

func getBuilderVersion(ctx context.Context, in versionOptions) string {
	driverName := in.builder
	if driverName == "" {
		driverName = "buildkit"
	}
	d, err := driver.GetDriver(ctx, driverName, nil, in.KubeClientConfig, []string{}, "" /* unused config file */, map[string]string{} /* DriverOpts unused */, "")
	if err != nil {
		return err.Error()
	}
	version, err := d.GetVersion(ctx)
	if err != nil {
		return err.Error()
	}
	return version
}

func runVersion(streams genericclioptions.IOStreams, in versionOptions) error {
	ctx := appcontext.Context()
	builderVersion := getBuilderVersion(ctx, in)

	fmt.Fprintf(streams.Out, "Client:  %s\n", version.Version)
	fmt.Fprintf(streams.Out, "Builder: %s\n", builderVersion)
	return nil
}

func versionCmd(streams genericclioptions.IOStreams, rootOpts *rootOptions) *cobra.Command {
	options := versionOptions{
		commonKubeOptions: commonKubeOptions{
			configFlags: genericclioptions.NewConfigFlags(true),
			IOStreams:   streams,
		},
	}

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Show client and builder version information ",
		RunE: func(cmd *cobra.Command, args []string) error {
			options.builder = rootOpts.builder

			if err := options.Complete(cmd, args); err != nil {
				return err
			}
			if err := options.Validate(); err != nil {
				return err
			}

			return runVersion(streams, options)
		},
	}
	return cmd
}
