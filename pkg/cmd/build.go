// Portions Copyright (C) 2020 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0
package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/util/appcontext"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/pkg/build"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/pkg/driver"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/pkg/platformutil"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/pkg/progress"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"

	"k8s.io/cli-runtime/pkg/genericclioptions"
)

// BuildOptions provides information required to update
// the current context on a user's KUBECONFIG
type buildOptions struct {
	commonKubeOptions
	commonOptions

	//listBuilds bool
	//args       []string

	// Replicated from buildx
	contextPath    string
	dockerfileName string
	tags           []string
	labels         []string
	buildArgs      []string

	cacheFrom   []string
	cacheTo     []string
	target      string
	platforms   []string
	secrets     []string
	ssh         []string
	outputs     []string
	imageIDFile string
	extraHosts  []string
	networkMode string

	// unimplemented
	squash bool
	quiet  bool

	allow []string

	frontend string

	// hidden
	// untrusted   bool
	// ulimits        *opts.UlimitOpt
	// memory         opts.MemBytes
	// memorySwap     opts.MemSwapBytes
	// shmSize        opts.MemBytes
	// cpuShares      int64
	// cpuPeriod      int64
	// cpuQuota       int64
	// cpuSetCpus     string
	// cpuSetMems     string
	// cgroupParent   string
	// isolation      string
	// compress    bool
	// securityOpt []string
}

type commonOptions struct {
	builder            string
	noCache            *bool
	progress           string
	pull               *bool
	exportPush         bool
	exportLoad         bool
	registrySecretName string
}

type commonKubeOptions struct {
	genericclioptions.IOStreams

	configFlags      *genericclioptions.ConfigFlags
	KubeClientConfig clientcmd.ClientConfig

	// TODO do we need these or are they just cruft left over from the CLI plugin example
	resultingContext       *api.Context
	userSpecifiedCluster   string
	userSpecifiedContext   string
	userSpecifiedAuthInfo  string
	userSpecifiedNamespace string
	rawConfig              api.Config
}

func runBuild(streams genericclioptions.IOStreams, in buildOptions) error {
	if in.squash {
		return errors.Errorf("squash currently not implemented")
	}
	if in.quiet {
		return errors.Errorf("quiet currently not implemented")
	}

	ctx := appcontext.Context()

	noCache := false
	if in.noCache != nil {
		noCache = *in.noCache
	}
	pull := false
	if in.pull != nil {
		pull = *in.pull
	}

	opts := build.Options{
		Inputs: build.Inputs{
			ContextPath:    in.contextPath,
			DockerfilePath: in.dockerfileName,
			InStream:       streams.In,
		},
		Tags:          in.tags,
		Labels:        listToMap(in.labels, false),
		BuildArgs:     listToMap(in.buildArgs, true),
		Pull:          pull,
		NoCache:       noCache,
		Target:        in.target,
		ImageIDFile:   in.imageIDFile,
		ExtraHosts:    in.extraHosts,
		NetworkMode:   in.networkMode,
		FrontendImage: in.frontend,
	}

	platforms, err := platformutil.Parse(in.platforms)
	if err != nil {
		return err
	}
	opts.Platforms = platforms

	secrets, err := build.ParseSecretSpecs(in.secrets)
	if err != nil {
		return err
	}
	opts.Session = append(opts.Session, secrets)

	ssh, err := build.ParseSSHSpecs(in.ssh)
	if err != nil {
		return err
	}
	opts.Session = append(opts.Session, ssh)

	outputs, err := build.ParseOutputs(in.outputs)
	if err != nil {
		return err
	}
	if in.exportPush {
		if in.exportLoad {
			// not reached
			return errors.Errorf("push and load may not be set together at the moment")
		}
		if len(outputs) == 0 {
			outputs = []client.ExportEntry{{
				Type: "image",
				Attrs: map[string]string{
					"push": "true",
				},
			}}
		} else {
			switch outputs[0].Type {
			case "image":
				outputs[0].Attrs["push"] = "true"
			default:
				return errors.Errorf("push and %q output can't be used together", outputs[0].Type)
			}
		}
	}
	if in.exportLoad {
		if len(outputs) == 0 {
			outputs = []client.ExportEntry{{
				Type:  "runtime", // Will be translated to the applicable runtime later before solving
				Attrs: map[string]string{},
			}}
		} else {
			switch outputs[0].Type {
			case "containerd":
			case "docker":
			default:
				return errors.Errorf("load and %q output can't be used together", outputs[0].Type)
			}
		}
	}

	opts.Exports = outputs

	// TODO - figure out if we're multi-node, and should wire up replication of the
	//        image across all the builders

	cacheImports, err := build.ParseCacheEntry(in.cacheFrom)
	if err != nil {
		return err
	}
	opts.CacheFrom = cacheImports

	cacheExports, err := build.ParseCacheEntry(in.cacheTo)
	if err != nil {
		return err
	}
	opts.CacheTo = cacheExports

	allow, err := build.ParseEntitlements(in.allow)
	if err != nil {
		return err
	}
	opts.Allow = allow

	// key string used for kubernetes "sticky" mode
	contextPathHash, err := filepath.Abs(in.contextPath)
	if err != nil {
		contextPathHash = in.contextPath
	}

	return buildTargets(ctx, in.KubeClientConfig, streams, opts, in.progress, contextPathHash, in.registrySecretName, in.builder)
}

func buildTargets(ctx context.Context, kubeClientConfig clientcmd.ClientConfig, streams genericclioptions.IOStreams, opts build.Options, progressMode, contextPathHash, registrySecretName, instance string) error {
	driverName := instance
	if driverName == "" {
		driverName = "buildkit"
	}
	drv, err := driver.GetDriver(ctx, driverName, nil, kubeClientConfig, []string{} /* TODO what BuildkitFlags are these? */, "" /* unused config file */, map[string]string{} /* DriverOpts unused */, contextPathHash)
	if err != nil {
		return err
	}

	ctx2, cancel := context.WithCancel(context.TODO())
	defer cancel()

	pw := progress.NewPrinter(ctx2, os.Stderr, progressMode)

	_, err = build.Build(ctx, drv, opts, kubeClientConfig, registrySecretName, pw)
	return err
}

func buildCmd(streams genericclioptions.IOStreams, rootOpts *rootOptions) *cobra.Command {
	options := buildOptions{
		commonKubeOptions: commonKubeOptions{
			configFlags: genericclioptions.NewConfigFlags(true),
			IOStreams:   streams,
		},
	}

	cmd := &cobra.Command{
		Use:   "build [OPTIONS] PATH | URL | -",
		Short: "Start a build",
		Long: `Start a build

Aliases:
  'kubectl build ...'
  'kubectl buildkit build ...'

Hints:
  If --push or --output are not specified, and the builder(s) are not running
  in "rootless" mode, built images will be saved in the builder(s) runtime(s)

  To push or pull private images, create a k8s image pull secret with
  the same name as your builder (default "buildkit") or specify alternate
  name with '--registry-secret NAME'.

  If no builder already exists on your kubernetes cluster, a builder will be
  created automatically with sensible defaults based on your environment.

  For more control on builder settings see 'kubectl buildkit create --help'

`,
		Args: ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			options.contextPath = args[0]
			options.builder = rootOpts.builder
			if len(options.outputs) == 0 && !options.exportPush {
				options.exportLoad = true
			}
			if err := options.Complete(cmd, args); err != nil {
				return err
			}
			if err := options.Validate(); err != nil {
				return err
			}
			return runBuild(streams, options)
		},
		SilenceUsage: true,
	}

	flags := cmd.Flags()

	flags.BoolVar(&options.exportPush, "push", false, "Shorthand for --output=type=registry")

	flags.StringArrayVarP(&options.tags, "tag", "t", []string{}, "Name and optionally a tag in the 'name:tag' format")
	flags.StringArrayVar(&options.buildArgs, "build-arg", []string{}, "Set build-time variables")
	flags.StringVarP(&options.dockerfileName, "file", "f", "", "Name of the Dockerfile (Default is 'PATH/Dockerfile')")

	flags.StringArrayVar(&options.labels, "label", []string{}, "Set metadata for an image")

	flags.StringArrayVar(&options.cacheFrom, "cache-from", []string{}, "External cache sources (eg. user/app:cache, type=local,src=path/to/dir)")
	flags.StringArrayVar(&options.cacheTo, "cache-to", []string{}, "Cache export destinations (eg. user/app:cache, type=local,dest=path/to/dir)")

	flags.StringVar(&options.target, "target", "", "Set the target build stage to build.")

	flags.StringSliceVar(&options.allow, "allow", []string{}, "Allow extra privileged entitlement, e.g. network.host, security.insecure")

	// TODO this should have a build-time default injected
	flags.StringVar(&options.frontend, "frontend", "", "Specify an image to parse the Dockerfile and generate the build graph")

	// not implemented
	flags.BoolVarP(&options.quiet, "quiet", "q", false, "Suppress the build output and print image ID on success")
	flags.StringVar(&options.networkMode, "network", "default", "Set the networking mode for the RUN instructions during build")
	flags.StringSliceVar(&options.extraHosts, "add-host", []string{}, "Add a custom host-to-IP mapping (host:ip)")
	flags.StringVar(&options.imageIDFile, "iidfile", "", "Write the image ID to the file")
	flags.BoolVar(&options.squash, "squash", false, "Squash newly built layers into a single new layer")
	flags.MarkHidden("quiet")
	flags.MarkHidden("squash")

	// hidden flags
	var ignore string
	var ignoreSlice []string
	var ignoreBool bool
	var ignoreInt int64
	flags.StringVar(&ignore, "ulimit", "", "Ulimit options")
	flags.MarkHidden("ulimit")
	flags.StringSliceVar(&ignoreSlice, "security-opt", []string{}, "Security options")
	flags.MarkHidden("security-opt")
	flags.BoolVar(&ignoreBool, "compress", false, "Compress the build context using gzip")
	flags.MarkHidden("compress")
	flags.StringVarP(&ignore, "memory", "m", "", "Memory limit")
	flags.MarkHidden("memory")
	flags.StringVar(&ignore, "memory-swap", "", "Swap limit equal to memory plus swap: '-1' to enable unlimited swap")
	flags.MarkHidden("memory-swap")
	flags.StringVar(&ignore, "shm-size", "", "Size of /dev/shm")
	flags.MarkHidden("shm-size")
	flags.Int64VarP(&ignoreInt, "cpu-shares", "c", 0, "CPU shares (relative weight)")
	flags.MarkHidden("cpu-shares")
	flags.Int64Var(&ignoreInt, "cpu-period", 0, "Limit the CPU CFS (Completely Fair Scheduler) period")
	flags.MarkHidden("cpu-period")
	flags.Int64Var(&ignoreInt, "cpu-quota", 0, "Limit the CPU CFS (Completely Fair Scheduler) quota")
	flags.MarkHidden("cpu-quota")
	flags.StringVar(&ignore, "cpuset-cpus", "", "CPUs in which to allow execution (0-3, 0,1)")
	flags.MarkHidden("cpuset-cpus")
	flags.StringVar(&ignore, "cpuset-mems", "", "MEMs in which to allow execution (0-3, 0,1)")
	flags.MarkHidden("cpuset-mems")
	flags.StringVar(&ignore, "cgroup-parent", "", "Optional parent cgroup for the container")
	flags.MarkHidden("cgroup-parent")
	flags.StringVar(&ignore, "isolation", "", "Container isolation technology")
	flags.MarkHidden("isolation")
	flags.BoolVar(&ignoreBool, "rm", true, "Remove intermediate containers after a successful build")
	flags.MarkHidden("rm")
	flags.BoolVar(&ignoreBool, "force-rm", false, "Always remove intermediate containers")
	flags.MarkHidden("force-rm")

	platformsDefault := []string{}
	if v := os.Getenv("DOCKER_DEFAULT_PLATFORM"); v != "" {
		platformsDefault = []string{v}
	}
	flags.StringArrayVar(&options.platforms, "platform", platformsDefault, "Set target platform for build")

	flags.StringArrayVar(&options.secrets, "secret", []string{}, "Secret file to expose to the build: id=mysecret,src=/local/secret")

	flags.StringArrayVar(&options.ssh, "ssh", []string{}, "SSH agent socket or keys to expose to the build (format: default|<id>[=<socket>|<key>[,<key>]])")

	flags.StringArrayVarP(&options.outputs, "output", "o", []string{}, "Output destination (format: type=local,dest=path)")

	commonBuildFlags(&options.commonOptions, flags)

	options.configFlags.AddFlags(cmd.Flags())

	return cmd
}

func commonBuildFlags(options *commonOptions, flags *pflag.FlagSet) {
	options.noCache = flags.Bool("no-cache", false, "Do not use cache when building the image")
	flags.StringVar(&options.progress, "progress", "auto", "Set type of progress output (auto, plain, tty). Use plain to show container output")
	options.pull = flags.Bool("pull", false, "Always attempt to pull a newer version of the image")
	flags.StringVar(&options.registrySecretName, "registry-secret", "", "specify registry pull secret for pull/push operations (defaults to builder name)")

}

func listToMap(values []string, defaultEnv bool) map[string]string {
	result := make(map[string]string, len(values))
	for _, value := range values {
		kv := strings.SplitN(value, "=", 2)
		if len(kv) == 1 {
			if defaultEnv {
				v, ok := os.LookupEnv(kv[0])
				if ok {
					result[kv[0]] = v
				}
			} else {
				result[kv[0]] = ""
			}
		} else {
			result[kv[0]] = kv[1]
		}
	}
	return result
}

// Complete sets all information required for updating the current context
func (o *commonKubeOptions) Complete(cmd *cobra.Command, args []string) error {
	var err error
	o.KubeClientConfig = o.configFlags.ToRawKubeConfigLoader()
	o.rawConfig, err = o.KubeClientConfig.RawConfig()
	if err != nil {
		return err
	}

	o.userSpecifiedNamespace, err = cmd.Flags().GetString("namespace")
	if err != nil {
		return err
	}

	// if no namespace flag value was specified, then there
	// is no need to generate a resulting context
	if len(o.userSpecifiedNamespace) == 0 {
		return nil
	}

	o.userSpecifiedContext, err = cmd.Flags().GetString("context")
	if err != nil {
		return err
	}

	o.userSpecifiedCluster, err = cmd.Flags().GetString("cluster")
	if err != nil {
		return err
	}

	o.userSpecifiedAuthInfo, err = cmd.Flags().GetString("user")
	if err != nil {
		return err
	}

	currentContext, exists := o.rawConfig.Contexts[o.rawConfig.CurrentContext]
	if !exists {
		return errNoContext
	}

	o.resultingContext = api.NewContext()
	o.resultingContext.Cluster = currentContext.Cluster
	o.resultingContext.AuthInfo = currentContext.AuthInfo

	// if a target context is explicitly provided by the user,
	// use that as our reference for the final, resulting context
	if len(o.userSpecifiedContext) > 0 {
		if userCtx, exists := o.rawConfig.Contexts[o.userSpecifiedContext]; exists {
			o.resultingContext = userCtx.DeepCopy()
		}
	}

	// override context info with user provided values
	o.resultingContext.Namespace = o.userSpecifiedNamespace

	if len(o.userSpecifiedCluster) > 0 {
		o.resultingContext.Cluster = o.userSpecifiedCluster
	}
	if len(o.userSpecifiedAuthInfo) > 0 {
		o.resultingContext.AuthInfo = o.userSpecifiedAuthInfo
	}

	return nil
}

// Validate ensures that all required arguments and flag values are provided
func (o *commonKubeOptions) Validate() error {
	if len(o.rawConfig.CurrentContext) == 0 {
		return errNoContext
	}

	// TODO - other validations for the build parameters (catch pebkacs here)
	return nil
}
