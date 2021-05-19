// Portions Copyright (C) 2020 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0
package build

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/platforms"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/pkg/driver"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/pkg/imagetools"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/pkg/progress"
	"google.golang.org/grpc"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/docker/distribution/reference"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/urlutil"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/upload/uploadprovider"
	"github.com/moby/buildkit/util/entitlements"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

var (
	errStdinConflict      = errors.New("invalid argument: can't use stdin for both build context and dockerfile")
	errDockerfileConflict = errors.New("ambiguous Dockerfile source: both stdin and flag correspond to Dockerfiles")
)

type Options struct {
	Inputs      Inputs
	Tags        []string
	Labels      map[string]string
	BuildArgs   map[string]string
	Pull        bool
	ImageIDFile string
	ExtraHosts  []string
	NetworkMode string

	NoCache   bool
	Target    string
	Platforms []specs.Platform
	Exports   []client.ExportEntry
	Session   []session.Attachable

	CacheFrom []client.CacheOptionsEntry
	CacheTo   []client.CacheOptionsEntry

	Allow []entitlements.Entitlement
	// DockerTarget
	FrontendImage string
}

type Inputs struct {
	ContextPath    string
	DockerfilePath string
	InStream       io.Reader
}

func toRepoOnly(in string) (string, error) {
	m := map[string]struct{}{}
	p := strings.Split(in, ",")
	for _, pp := range p {
		n, err := reference.ParseNormalizedNamed(pp)
		if err != nil {
			return "", err
		}
		m[n.Name()] = struct{}{}
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return strings.Join(out, ","), nil
}

func toSolveOpt(ctx context.Context, d driver.Driver, multiNodeRequested bool, opt Options, dl dockerLoadCallback) (*client.SolveOpt, func(), error) {
	defers := make([]func(), 0, 2)
	var err error
	releaseF := func() {
		for _, f := range defers {
			f()
		}
	}

	defer func() {
		if err != nil {
			releaseF()
		}
	}()

	if opt.ImageIDFile != "" {
		if multiNodeRequested || len(opt.Platforms) != 0 { // TODO the secondary test here will become redundant once multiPlatformRequested is complete
			return nil, nil, errors.Errorf("image ID file cannot be specified when building for multiple platforms")
		}
		// Avoid leaving a stale file if we eventually fail
		if err := os.Remove(opt.ImageIDFile); err != nil && !os.IsNotExist(err) {
			return nil, nil, errors.Wrap(err, "removing image ID file")
		}
	}

	for _, e := range opt.CacheTo {
		if e.Type != "inline" && !d.Features()[driver.CacheExport] {
			return nil, nil, notSupported(driver.CacheExport)
		}
	}

	solveOpt := client.SolveOpt{
		Frontend:            "dockerfile.v0",
		FrontendAttrs:       map[string]string{},
		LocalDirs:           map[string]string{},
		CacheExports:        opt.CacheTo,
		CacheImports:        opt.CacheFrom,
		AllowedEntitlements: opt.Allow,
	}

	if opt.FrontendImage != "" {
		solveOpt.Frontend = "gateway.v0"
		solveOpt.FrontendAttrs["source"] = opt.FrontendImage
	}

	if multiNodeRequested {
		// force creation of manifest list
		solveOpt.FrontendAttrs["multi-platform"] = "true"
	}

	switch len(opt.Exports) {
	case 1:
		// valid
	case 0:
		// Shouldn't happen - higher level constructs should create 1+ exports
		return nil, nil, errors.Errorf("zero outputs currently unsupported")
		// TODO - alternative would be
		// opt.Exports = []client.ExportEntry{{Type: "image", Attrs: map[string]string{}}}
	default:
		return nil, nil, errors.Errorf("multiple outputs currently unsupported")
	}

	// Update any generic "runtime" exports to be runtime specific now:
	driverFeatures := d.Features()
	for i, e := range opt.Exports {
		switch e.Type {
		case "runtime":
			// TODO - this is a bit messy - can we just leverage "image" Type and "do the right thing" instead of
			// having this alternate "runtime" type that's ~inconsistent?
			if driverFeatures[driver.ContainerdExporter] {
				opt.Exports[i].Type = "image"
			} else if driverFeatures[driver.DockerExporter] {
				opt.Exports[i].Type = "docker"
			} else {
				// TODO should we allow building without load or push, perhaps a new "nil" or equivalent output type?
				return nil, nil, errors.Errorf("loading image into cluster runtime not supported by this builder, please specify --push or a client local output: --output=type=local,dest=. --output=type=tar,dest=out.tar ")
			}
		}
	}

	// fill in image exporter names from tags
	if len(opt.Tags) > 0 {
		tags := make([]string, len(opt.Tags))
		for i, tag := range opt.Tags {
			ref, err := reference.Parse(tag)
			if err != nil {
				return nil, nil, errors.Wrapf(err, "invalid tag %q", tag)
			}
			tags[i] = ref.String()
		}
		for i, e := range opt.Exports {
			switch e.Type {
			case "image", "oci", "docker":
				opt.Exports[i].Attrs["name"] = strings.Join(tags, ",")
			}
		}
	} else {
		for _, e := range opt.Exports {
			if e.Type == "image" && e.Attrs["name"] == "" && e.Attrs["push"] != "" {
				if ok, _ := strconv.ParseBool(e.Attrs["push"]); ok {
					return nil, nil, errors.Errorf("tag is needed when pushing to registry")
				}
			}
		}
	}

	// set up exporters
	for i, e := range opt.Exports {
		pushing := false
		if p, ok := e.Attrs["push"]; ok {
			pushing, _ = strconv.ParseBool(p)
		}
		if (e.Type == "local" || e.Type == "tar") && opt.ImageIDFile != "" {
			return nil, nil, errors.Errorf("local and tar exporters are incompatible with image ID file")
		}

		// TODO - do more research on what this really means and if it's wired up correctly
		if e.Type == "oci" && !driverFeatures[driver.OCIExporter] {
			return nil, nil, notSupported(driver.OCIExporter)
		}
		if e.Type == "docker" {
			if !driverFeatures[driver.DockerExporter] {
				return nil, nil, notSupported(driver.DockerExporter)
			}
			// If the runtime is docker and we're not in
			// rootless mode, then we will have mounted
			// the docker.sock inside the buildkit pods, and
			// can use the exec trick with
			//   buildctl --addr unix://run/docker.sock
			// to proxy to the API endpoint and perform an image load
			// This isn't as efficient as tagging/exposing the images
			// directly within containerd, but will suffice during
			// the transition period while many people still run Docker
			// as their container runtime for kubernetes

			// If an Output (file) is already specified, let it pass through
			if e.Output == nil {
				w, cancel, err := dl("")
				if err != nil {
					return nil, nil, err
				}
				defers = append(defers, cancel)
				opt.Exports[i].Output = wrapWriteCloser(w)
			}
		}
		if e.Type == "containerd" { // TODO - should this just be dropped in favor of "image"?

			// TODO should this just be wired up as "image" instead?
			return nil, nil, notSupported(driver.ContainerdExporter)
			// TODO implement this scenario
		}
		if e.Type == "image" && !pushing {
			if !driverFeatures[driver.ContainerdExporter] {
				// TODO - this could use a little refinement - if the user specifies `--output=image` it would be nice
				// to auto-wire this to handle both runtimes (docker and containerd)
				return nil, nil, notSupported(driver.ContainerdExporter)
			}
			// TODO - is there a better layer to optimize this part of the flow at?
			multiNode := false
			builders, err := d.List(ctx) // TODO - we're doing this multiple times - clean this up by passing in some more context to toSolveOpt
			if err != nil {
				return nil, nil, err
			}
			if len(builders) > 1 { // TODO - this is messy and ~wrong - should just be getting nodes for a given builder here
				multiNode = true
			} else if len(builders) == 1 {
				if len(builders[0].Nodes) > 1 {
					multiNode = true
				}
			}

			// TODO - figure out how to wire this up so the exporter output type is oci instead of image

			// If an Output (file) is already specified, let it pass through
			if e.Output == nil && multiNode {
				// TODO - Explore if there's a model to avoid having to transfer to the builder node
				opt.Exports[i].Type = "oci" // TODO - this most likely means the image isn't saved locally too
				w, cancel, err := dl("")
				if err != nil {
					return nil, nil, err
				}
				defers = append(defers, cancel)
				opt.Exports[i].Output = wrapWriteCloser(w)
			}
		}

		// TODO - image + runtime==docker, use the same proxy as above
		/* Nuke this once everything's refactored
		if e.Type == "image" && isDefaultMobyDriver { // TODO kube driver use-case coverage here...
			opt.Exports[i].Type = "moby"
			if e.Attrs["push"] != "" {
				if ok, _ := strconv.ParseBool(e.Attrs["push"]); ok {
					return nil, nil, errors.Errorf("auto-push is currently not implemented for docker driver")
				}
			}
		}
		*/

		// TODO If we're multi-node and "image" is specified, should we replicate it on all nodes?
	}

	solveOpt.Exports = opt.Exports
	solveOpt.Session = opt.Session

	releaseLoad, err := LoadInputs(opt.Inputs, &solveOpt)
	if err != nil {
		return nil, nil, err
	}
	defers = append(defers, releaseLoad)

	if opt.Pull {
		solveOpt.FrontendAttrs["image-resolve-mode"] = "pull"
	}
	if opt.Target != "" {
		solveOpt.FrontendAttrs["target"] = opt.Target
	}
	if opt.NoCache {
		solveOpt.FrontendAttrs["no-cache"] = ""
	}
	for k, v := range opt.BuildArgs {
		solveOpt.FrontendAttrs["build-arg:"+k] = v
	}
	for k, v := range opt.Labels {
		solveOpt.FrontendAttrs["label:"+k] = v
	}

	// set platforms
	// TODO - likely needs some refactoring with native multi-arch support...
	if len(opt.Platforms) != 0 {
		pp := make([]string, len(opt.Platforms))
		for i, p := range opt.Platforms {
			pp[i] = platforms.Format(p)
		}
		if len(pp) > 1 && !d.Features()[driver.MultiPlatform] {
			return nil, nil, notSupported(driver.MultiPlatform)
		}
		solveOpt.FrontendAttrs["platform"] = strings.Join(pp, ",")
	}

	// setup networkmode
	switch opt.NetworkMode {
	case "host", "none":
		solveOpt.FrontendAttrs["force-network-mode"] = opt.NetworkMode
		solveOpt.AllowedEntitlements = append(solveOpt.AllowedEntitlements, entitlements.EntitlementNetworkHost)
	case "", "default":
	default:
		return nil, nil, errors.Errorf("network mode %q not supported by buildkit", opt.NetworkMode)
	}

	// setup extrahosts
	extraHosts, err := toBuildkitExtraHosts(opt.ExtraHosts)
	if err != nil {
		return nil, nil, err
	}
	solveOpt.FrontendAttrs["add-hosts"] = extraHosts
	return &solveOpt, releaseF, nil
}

func Build(ctx context.Context, drv driver.Driver, opt Options, kubeClientConfig clientcmd.ClientConfig, registrySecretName string, pw progress.Writer) (resp *client.SolveResponse, err error) {
	drvClient, _, err := driver.Boot(ctx, drv, pw)
	if err != nil {
		close(pw.Status())
		<-pw.Done()
		return nil, err
	}

	drvInfo, err := drv.Info(ctx)
	if err != nil {
		close(pw.Status())
		<-pw.Done()
		return nil, err
	}
	_, mixed := drvInfo.GetPlatforms()

	// Check for "auto" special case
	// TODO - while this would be cool, in reality it doesn't work in practice
	//        as most library images on hub don't have 386 binaries, so builds fail on x86
	//        and they lack armv6, so ARM builds fail.
	//        It might be possible to attempt and fall-back on well known errors for missing layers
	//        or build a filter of some sorts for uncommon old architectures
	//        or maybe there's a way to only build the latest platform
	/*
		for _, platform := range opt.Platforms {
			if strings.EqualFold(platform.Architecture, "auto") {
				opt.Platforms = drvPlatforms
				break
			}
		}
	*/

	// Determine if we want to build a manifestlist based image, or a simple/plain image
	// There are a few scenarios that can lead to this.
	// * If the user specifies multiple explicit platforms
	// * If the user specifies "auto" as the platform, and the driver nodes support multiples
	multiPlatformRequested := len(opt.Platforms) > 1

	// Determine if we want to split the build into multiple builder requests, or send to a single builder
	multiNodeRequested := multiPlatformRequested && mixed // TODO - consider refining this algorithm...

	// Determine the number of Solve calls we will perform
	solveCount := 1
	if multiNodeRequested {
		solveCount = len(opt.Platforms)
	}

	requestedPlatforms := make([]specs.Platform, len(opt.Platforms))
	for i, platform := range opt.Platforms {
		requestedPlatforms[i] = platform
	}

	// TODO - come back to this and make sure it's legit...
	defers := make([]func(), 0, 2)
	defer func() {
		if err != nil {
			for _, f := range defers {
				f()
			}
		}
	}()

	var auth imagetools.Auth
	driverName := drv.GetName()

	multiWriter := progress.NewMultiWriter(pw)
	errGroup, ctx := errgroup.WithContext(ctx)

	solveOpts := make([]*client.SolveOpt, solveCount)
	res := make([]*client.SolveResponse, solveCount)

	if auth == nil {
		auth = drv.GetAuthWrapper(registrySecretName)
		opt.Session = append(opt.Session, drv.GetAuthProvider(registrySecretName, os.Stderr))
	}

	for i := 0; i < len(solveOpts); i++ {
		// If we're multi-node, swap out the platform list for one platform at a time
		if multiNodeRequested {
			opt.Platforms = []specs.Platform{requestedPlatforms[i]}
		}
		solveOpt, release, err := toSolveOpt(ctx, drv, multiNodeRequested, opt, func(arg string) (io.WriteCloser, func(), error) {
			// Set up loader based on first found type (only 1 supported)
			for _, entry := range opt.Exports {
				if entry.Type == "docker" {
					return newDockerLoader(ctx, drv, kubeClientConfig, driverName, multiWriter)
				} else if entry.Type == "oci" {
					return newContainerdLoader(ctx, drv, kubeClientConfig, driverName, multiWriter)
				}
			}
			// TODO - Push scenario?  (or is this a "not reached" scenario now?)
			return nil, nil, fmt.Errorf("raw push scenario not yet supported")
		})
		if err != nil {
			return nil, err
		}
		defers = append(defers, release)
		solveOpts[i] = solveOpt
	}

	var respMu sync.Mutex

	// TODO this is WRONG
	multiTarget := false // TODO experiment with true/false on this to see differing behavior of the progress writer...

	wg := &sync.WaitGroup{}
	wg.Add(solveCount)

	var pushNames string

	errGroup.Go(func() error {
		pw := multiWriter.WithPrefix("default", false)
		defer close(pw.Status())
		wg.Wait()
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		respMu.Lock()
		resp = res[0]
		respMu.Unlock()
		if len(res) == 1 {
			// TODO - this is the single solve scenario
			if opt.ImageIDFile != "" {
				return ioutil.WriteFile(opt.ImageIDFile, []byte(res[0].ExporterResponse["containerimage.digest"]), 0644)
			}
			return nil
		}
		// TODO - this is the multi-solve scenario (aka, multiple nodes building, where the client needs to stitch things together)

		if pushNames != "" {
			progress.Write(pw, fmt.Sprintf("merging manifest list %s", pushNames), func() error {
				descs := make([]specs.Descriptor, 0, len(res))
				for _, r := range res {
					s, ok := r.ExporterResponse["containerimage.digest"]
					if ok {
						descs = append(descs, specs.Descriptor{
							Digest:    digest.Digest(s),
							MediaType: images.MediaTypeDockerSchema2ManifestList,
							Size:      -1,
						})
					}
				}
				if len(descs) > 0 {
					itpull := imagetools.New(imagetools.Opt{
						Auth: auth,
					})

					names := strings.Split(pushNames, ",")
					dt, desc, err := itpull.Combine(ctx, names[0], descs)
					if err != nil {
						return err
					}
					if opt.ImageIDFile != "" {
						return ioutil.WriteFile(opt.ImageIDFile, []byte(desc.Digest), 0644)
					}
					itpush := imagetools.New(imagetools.Opt{
						Auth: auth,
					})

					for _, n := range names {
						nn, err := reference.ParseNormalizedNamed(n)
						if err != nil {
							return err
						}
						if err := itpush.Push(ctx, nn, desc, dt); err != nil {
							return err
						}
					}

					respMu.Lock()
					resp = &client.SolveResponse{
						ExporterResponse: map[string]string{
							"containerimage.digest": desc.Digest.String(),
						},
					}
					respMu.Unlock()
				}
				return nil
			})
		}
		return nil
	})

	for i, so := range solveOpts {
		// TODO - this probably needs some refinement for multi-arch multi-node vs. single node multi-arch via cross compilation
		solveOpt := so
		i := i
		if multiPlatformRequested {
			for _, exportEntry := range solveOpt.Exports {
				switch exportEntry.Type {
				case "oci", "tar":
					return nil, errors.Errorf("%s for multi-node builds currently not supported", exportEntry.Type)
				case "image":
					if pushNames == "" && exportEntry.Attrs["push"] != "" {
						if ok, _ := strconv.ParseBool(exportEntry.Attrs["push"]); ok {
							pushNames = exportEntry.Attrs["name"]
							if pushNames == "" {
								return nil, errors.Errorf("tag is needed when pushing to registry")
							}
							names, err := toRepoOnly(exportEntry.Attrs["name"])
							if err != nil {
								return nil, err
							}
							exportEntry.Attrs["name"] = names
							exportEntry.Attrs["push-by-digest"] = "true"
							solveOpt.Exports[i].Attrs = exportEntry.Attrs
						}
					}
				}
			}
		}

		// TODO this needs further work around picking the right nodes...
		var c *client.Client
		var name string
		if multiNodeRequested {
			// TODO refine algorithm to select node (spread work around, etc.)
			c, name, err = drv.Client(ctx, requestedPlatforms[i])
			if err != nil {
				// TODO consider hardening for flaky builders
				return nil, err
			}
		} else {
			c = drvClient
			name = "default"
		}
		pw = multiWriter.WithPrefix(name, multiTarget)

		var statusCh chan *client.SolveStatus
		if pw != nil {
			pw = progress.ResetTime(pw)
			statusCh = pw.Status()
			errGroup.Go(func() error {
				<-pw.Done()
				return pw.Err()
			})
		}

		errGroup.Go(func() error {
			defer wg.Done()
			// TODO - make sure we don't have a stack goof here and pass in the wrong solveOpt...
			rr, err := c.Solve(ctx, nil, *solveOpt, statusCh)
			if err != nil {
				// Try to give a slightly more helpful error message if the use
				// hasn't wired up a kubernetes secret for push/pull properly
				if strings.Contains(strings.ToLower(err.Error()), "401 unauthorized") {
					msg := drv.GetAuthHintMessage()
					return errors.Wrap(err, msg)
				}
				return err
			}
			// TODO - resp and res are likely still not quite right...
			res[i] = rr
			return nil
		})
	}
	if err := errGroup.Wait(); err != nil {
		return nil, err
	}

	return resp, nil
}

func createTempDockerfile(r io.Reader) (string, error) {
	dir, err := ioutil.TempDir("", "dockerfile")
	if err != nil {
		return "", err
	}
	f, err := os.Create(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return "", err
	}
	return dir, err
}

func LoadInputs(inp Inputs, target *client.SolveOpt) (func(), error) {
	if inp.ContextPath == "" {
		return nil, errors.New("please specify build context (e.g. \".\" for the current directory)")
	}

	// TODO: handle stdin, symlinks, remote contexts, check files exist

	var (
		err              error
		dockerfileReader io.Reader
		dockerfileDir    string
		dockerfileName   = inp.DockerfilePath
		toRemove         []string
	)

	switch {
	case inp.ContextPath == "-":
		if inp.DockerfilePath == "-" {
			return nil, errStdinConflict
		}

		buf := bufio.NewReader(inp.InStream)
		magic, err := buf.Peek(archiveHeaderSize * 2)
		if err != nil && err != io.EOF {
			return nil, errors.Wrap(err, "failed to peek context header from STDIN")
		}

		if isArchive(magic) {
			// stdin is context
			up := uploadprovider.New()
			target.FrontendAttrs["context"] = up.Add(buf)
			target.Session = append(target.Session, up)
		} else {
			if inp.DockerfilePath != "" {
				return nil, errDockerfileConflict
			}
			// stdin is dockerfile
			dockerfileReader = buf
			inp.ContextPath, _ = ioutil.TempDir("", "empty-dir")
			toRemove = append(toRemove, inp.ContextPath)
			target.LocalDirs["context"] = inp.ContextPath
		}

	case isLocalDir(inp.ContextPath):
		target.LocalDirs["context"] = inp.ContextPath
		switch inp.DockerfilePath {
		case "-":
			dockerfileReader = inp.InStream
		case "":
			dockerfileDir = inp.ContextPath
		default:
			dockerfileDir = filepath.Dir(inp.DockerfilePath)
			dockerfileName = filepath.Base(inp.DockerfilePath)
		}

	case urlutil.IsGitURL(inp.ContextPath), urlutil.IsURL(inp.ContextPath):
		if inp.DockerfilePath == "-" {
			return nil, errors.Errorf("Dockerfile from stdin is not supported with remote contexts")
		}
		target.FrontendAttrs["context"] = inp.ContextPath
	default:
		return nil, errors.Errorf("unable to prepare context: path %q not found", inp.ContextPath)
	}

	if dockerfileReader != nil {
		dockerfileDir, err = createTempDockerfile(dockerfileReader)
		if err != nil {
			return nil, err
		}
		toRemove = append(toRemove, dockerfileDir)
		dockerfileName = "Dockerfile"
	}

	if dockerfileName == "" {
		dockerfileName = "Dockerfile"
	}
	target.FrontendAttrs["filename"] = dockerfileName

	if dockerfileDir != "" {
		target.LocalDirs["dockerfile"] = dockerfileDir
	}

	release := func() {
		for _, dir := range toRemove {
			os.RemoveAll(dir)
		}
	}
	return release, nil
}

func notSupported(f driver.Feature) error {
	return errors.Errorf("%s feature is currently not supported. See \"kubectl buildkit create --help\"", f)
}

type dockerLoadCallback func(name string) (io.WriteCloser, func(), error)

func newDockerLoader(ctx context.Context, d driver.Driver, kubeClientConfig clientcmd.ClientConfig, builderName string, mw *progress.MultiWriter) (io.WriteCloser, func(), error) {
	nodeNames := []string{}
	// TODO this isn't quite right - we need a better "list only pods from one instance" func
	builders, err := d.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, builder := range builders {
		if builder.Name != builderName {
			continue
		}
		for _, node := range builder.Nodes {
			nodeNames = append(nodeNames, node.Name)
		}
	}
	if len(nodeNames) == 0 {
		return nil, nil, fmt.Errorf("no builders found for %s", builderName)
	}
	// TODO revamp this flow to return a list of pods

	readers := make([]*io.PipeReader, len(nodeNames))
	writers := make([]io.Writer, len(nodeNames))
	pws := make([]*io.PipeWriter, len(nodeNames))
	names := make([]string, len(nodeNames))
	clients := make([]*dockerclient.Client, len(nodeNames))
	started := make([]chan struct{}, len(nodeNames))
	for i := range nodeNames {
		nodeName := nodeNames[i]
		tr := &http.Transport{
			// This is necessary as the Exec "upgrade" of the underlying connection
			// seems to confuse the Golang Transport idle connection caching mechanism
			// and multiple pods wind up sharing a connection that "appears" idle.
			//DisableKeepAlives: true,

			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				conn, err := d.RuntimeSockProxy(ctx, nodeName)
				if err != nil {
					return nil, fmt.Errorf("failed to set up docker runtime proxies through pod %s: %w", nodeName, err)
				}
				return conn, nil
			},
		}

		c, err := dockerclient.NewClientWithOpts(
			dockerclient.WithAPIVersionNegotiation(),
			dockerclient.WithHTTPClient(&http.Client{Transport: tr}),
			dockerclient.WithHost("http://"+nodeName), // TODO nuke
		)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to set up docker client via proxy: %w", err)
		}

		pr, pw := io.Pipe()
		readers[i] = pr
		writers[i] = pw
		pws[i] = pw
		clients[i] = c
		names[i] = nodeName
		started[i] = make(chan struct{})
	}

	w := &waitingWriter{
		Writer: io.MultiWriter(writers...),
		pws:    pws,
		names:  names,
		f: func(i int) {
			c := clients[i]
			resp, err := c.ImageLoad(ctx, readers[i], false)
			if err != nil {
				_ = readers[i].CloseWithError(err)
				close(started[i])
				return
			}

			prog := mw.WithPrefix("", false)

			close(started[i])

			progress.FromReader(prog, fmt.Sprintf("loading image to docker runtime via pod %s", names[i]), resp.Body)
			resp.Body.Close()
		},
		started: started,
	}

	return w, func() {
		for _, pr := range readers {
			pr.Close()
		}
	}, nil
}

type waitingWriter struct {
	io.Writer
	pws   []*io.PipeWriter
	names []string
	f     func(i int)
	once  sync.Once
	// mu      sync.Mutex
	// err     error
	started []chan struct{}
}

func (w *waitingWriter) Write(dt []byte) (int, error) {
	w.once.Do(func() {
		for i := range w.pws {
			go w.f(i)
		}
	})

	return w.Writer.Write(dt)
}

func (w *waitingWriter) Close() error {
	var err error
	for _, pw := range w.pws {
		err2 := pw.Close()
		if err2 != nil {
			err = err2
		}
	}
	for i := range w.pws {
		<-w.started[i]
	}
	return err
}

func newContainerdLoader(ctx context.Context, d driver.Driver, kubeClientConfig clientcmd.ClientConfig, builderName string, mw *progress.MultiWriter) (io.WriteCloser, func(), error) {
	nodeNames := []string{}
	// TODO this isn't quite right - we need a better "list only pods from one instance" func
	// TODO revamp this flow to return a list of pods

	builders, err := d.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, builder := range builders {
		if builder.Name != builderName {
			continue
		}
		for _, node := range builder.Nodes {
			// For containerd, we never have to load on the node where it was built
			// TODO - we may want to filter the source node, but when we switch the output.Type to "oci" we need to load it everywhere anyway
			nodeNames = append(nodeNames, node.Name)
		}
	}

	readers := make([]*io.PipeReader, len(nodeNames))
	writers := make([]io.Writer, len(nodeNames))
	pws := make([]*io.PipeWriter, len(nodeNames))
	names := make([]string, len(nodeNames))
	clients := make([]*containerd.Client, len(nodeNames))
	started := make([]chan struct{}, len(nodeNames))
	for i := range nodeNames {
		nodeName := nodeNames[i]
		c, err := containerd.New(nodeName,

			// TODO - plumb the containerd namespace through so this isn't hard-coded
			containerd.WithDefaultNamespace("k8s.io"),
			containerd.WithDialOpts([]grpc.DialOption{
				grpc.WithContextDialer(
					func(ctx context.Context, _ string) (net.Conn, error) {
						conn, err := d.RuntimeSockProxy(ctx, nodeName)
						if err != nil {
							return nil, fmt.Errorf("failed to set up containerd runtime proxies through pod %s: %w", nodeName, err)
						}
						return conn, nil
					},
				),
				grpc.WithInsecure(), // Nested connection on an existing secure transport
			}),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to set up docker client via proxy: %w", err)
		}

		pr, pw := io.Pipe()
		readers[i] = pr
		writers[i] = pw
		pws[i] = pw
		clients[i] = c
		names[i] = nodeName
		started[i] = make(chan struct{})
	}

	w := &waitingWriter{
		Writer: io.MultiWriter(writers...),
		pws:    pws,
		names:  names,
		f: func(i int) {
			//prog := mw.WithPrefix(names[i], false)

			c := clients[i]

			// TODO - is there some way to wire up fine-grain progress reporting stream here?
			// Adding calls to progress.Write(mw, "importing image", func() error { return nil }) seem to cause hangs...
			imgs, err := c.Import(ctx, readers[i],

				// TODO - might need to pass through the tagged name with the tag part stripped off
				//containerd.WithImageRefTranslator(archive.FilterRefPrefix(prefix),

				// TODO - might need this too..
				//containerd.WithIndexName(tag),

				// TODO - is this necessary or implicit?  (try without after it's working...)
				containerd.WithAllPlatforms(false),
			)
			if err != nil {
				_ = readers[i].CloseWithError(err)
				close(started[i])
				return
			}

			for _, img := range imgs {
				image := containerd.NewImage(c, img)

				// TODO: Show unpack status
				//progress.Write(prog, fmt.Sprintf("unpacking %s (%s)", img.Name, img.Target.Digest), func() error { return nil })
				err = image.Unpack(ctx, "") // Empty snapshotter is default
				if err != nil {
					_ = readers[i].CloseWithError(err)
					close(started[i])
					return
				}
			}
			close(started[i])
		},
		started: started,
	}

	return w, func() {
		for _, pr := range readers {
			pr.Close()
		}
	}, nil
}
