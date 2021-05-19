// Portions Copyright (C) 2020 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0
package commands

import (
	"os"

	//imagetoolscmd "github.com/docker/buildx/commands/imagetools"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

func NewRootCmd(streams genericclioptions.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "buildkit",
		Short: "Interact with buildkit on a kubernetes cluster",
		Long: `BuildKit is a toolkit for converting source code to build artifacts in an efficient, expressive and repeatable manner.
This CLI allows you to leverage buildkit on your kubernetes cluster.  You can deploy one or more builders on
your cluster, then run builds against them to produce OCI images.  Those images can be loaded on the nodes in your cluster, or
can be pushed to a registry.`,
	}

	addCommands(cmd, streams)
	return cmd

}

type rootOptions struct {
	commonKubeOptions

	builder string
}

func addCommands(cmd *cobra.Command, streams genericclioptions.IOStreams) {
	opts := &rootOptions{
		commonKubeOptions: commonKubeOptions{
			configFlags: genericclioptions.NewConfigFlags(true),
			IOStreams:   streams,
		},
	}
	rootFlags(opts, cmd.PersistentFlags())

	cmd.AddCommand(
		buildCmd(streams, opts),
		//bakeCmd(streams, opts),
		createCmd(streams, opts),
		rmCmd(streams),
		lsCmd(streams),
		//useCmd(streams, opts),
		//inspectCmd(streams, opts),
		//stopCmd(streams, opts),
		//installCmd(streams),
		//uninstallCmd(streams),
		versionCmd(streams, opts),
		//pruneCmd(streams, opts),
		//duCmd(streams, opts),
		//imagetoolscmd.RootCmd(streams),
	)
}

func rootFlags(options *rootOptions, flags *pflag.FlagSet) {
	flags.StringVar(&options.builder, "builder", os.Getenv("BUILDX_BUILDER"), "Override the configured builder instance")
	options.configFlags.AddFlags(flags)
}

func NewRootBuildCmd(streams genericclioptions.IOStreams) *cobra.Command {
	opts := &rootOptions{
		commonKubeOptions: commonKubeOptions{
			configFlags: genericclioptions.NewConfigFlags(true),
			IOStreams:   streams,
		},
	}
	cmd := buildCmd(streams, opts)
	rootFlags(opts, cmd.PersistentFlags())
	return cmd
}
