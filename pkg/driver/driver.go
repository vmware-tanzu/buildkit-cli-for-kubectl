// Portions Copyright (C) 2020 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0
package driver

import (
	"context"
	"io"
	"math/rand"
	"net"
	"strings"
	"time"

	"github.com/containerd/containerd/platforms"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/session"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/pkg/imagetools"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/pkg/platformutil"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/pkg/progress"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/pkg/store"
)

// TODO - Will we want any other drivers, or is this driver abstraction overkill?
//        Perhaps a low-level containerd driver might make sense, but wiring that up
//        and accessing via kubectl CLI plugin seems strange.

var ErrNotRunning = errors.Errorf("driver not running")
var ErrNotConnecting = errors.Errorf("driver not connecting")

type Status int

const (
	Inactive Status = iota
	Starting
	Running
	Stopping
	Stopped
)

const maxBootRetries = 3

func (s Status) String() string {
	switch s {
	case Inactive:
		return "inactive"
	case Starting:
		return "starting"
	case Running:
		return "running"
	case Stopping:
		return "stopping"
	case Stopped:
		return "stopped"
	}
	return "unknown"
}

type Info struct {
	Status Status
	// DynamicNodes must be empty if the actual nodes are statically listed in the store
	DynamicNodes []store.Node
}

type Driver interface {
	Factory() Factory
	Bootstrap(context.Context, progress.Logger) error
	Info(context.Context) (*Info, error)
	Stop(ctx context.Context, force bool) error
	Rm(ctx context.Context, force bool) error
	Client(ctx context.Context, platforms ...specs.Platform) (*client.Client, string, error)
	Features() map[Feature]bool
	List(ctx context.Context) ([]Builder, error)
	RuntimeSockProxy(ctx context.Context, name string) (net.Conn, error)

	// TODO - do we really need both?  Seems like some cleanup needed here...
	GetAuthWrapper(string) imagetools.Auth
	GetAuthProvider(secretName string, stderr io.Writer) session.Attachable
	GetAuthHintMessage() string
	GetName() string
}

type Builder struct {
	Name   string
	Driver string
	Nodes  []Node

	// TODO consider adding these for a verbose listing
	//Flags      []string
	//ConfigFile string
	//DriverOpts map[string]string
}

type Node struct {
	Name      string
	NodeName  string
	Status    string
	Platforms []specs.Platform
}

func Boot(ctx context.Context, d Driver, pw progress.Writer) (*client.Client, string, error) {
	try := 0
	rand.Seed(time.Now().UnixNano())
	for {
		info, err := d.Info(ctx)
		if err != nil {
			return nil, "", err
		}
		try++
		if info.Status != Running {
			if try > maxBootRetries {
				return nil, "", errors.Errorf("failed to bootstrap builder in %d attempts (%s)", try, err)
			}
			if err = d.Bootstrap(ctx, func(s *client.SolveStatus) {
				if pw != nil {
					pw.Status() <- s
				}
			}); err != nil && (strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "does not match")) {
				// This most likely means another build is running in parallel
				// Give it just enough time to finish creating resources then retry
				time.Sleep(25 * time.Millisecond * time.Duration(1+rand.Int63n(39))) // 25 - 1000 ms
				continue
			} else if err != nil {
				return nil, "", err
			}
		}

		c, chosenNodeName, err := d.Client(ctx)
		if err != nil {
			if errors.Cause(err) == ErrNotRunning && try <= maxBootRetries {
				continue
			}
			return nil, "", err
		}
		return c, chosenNodeName, nil
	}
}

// GetPlatforms returns a de-duped set of available platforms across all nodes
// and a bool to indicate a "mixed" cluster with nodes of different types. False
// indicates a non-mixed cluster.  If the some nodes in the cluster contain a
// subset of supported platforms of others in the cluster (e.g. newer
// generations of the same family) False will be returned.
// TODO - bool to skip offline nodes?
func (info *Info) GetPlatforms() ([]specs.Platform, bool) {
	isMixed := false
	latestPlatforms := []specs.Platform{}
	allPlatforms := []specs.Platform{}
	for _, node := range info.DynamicNodes {
		if len(node.Platforms) == 0 {
			continue
		}
		allPlatforms = append(allPlatforms, node.Platforms...)
		if isMixed {
			// Short circuit if we've already determined it's a mixed cluster
			continue
		}
		if len(latestPlatforms) == 0 {
			latestPlatforms = make([]specs.Platform, len(node.Platforms))
			copy(latestPlatforms, node.Platforms)
		} else if len(latestPlatforms) >= len(node.Platforms) {
			found := false
			for _, nodeP := range node.Platforms {
				for i := 0; i < len(latestPlatforms); i++ {
					if platforms.Format(nodeP) == platforms.Format(latestPlatforms[i]) {
						found = true
						break
					}
				}
				if found {
					break
				}
			}
			if !found {
				isMixed = true
			}
		} else {
			found := false
			for _, latestP := range latestPlatforms {
				for i := 0; i < len(node.Platforms); i++ {
					if platforms.Format(latestP) == platforms.Format(node.Platforms[i]) {
						found = true
						break
					}
				}
				if found {
					break
				}
			}
			if found {
				latestPlatforms = make([]specs.Platform, len(node.Platforms))
				copy(latestPlatforms, node.Platforms)
			} else {
				isMixed = true
			}
		}
	}
	return platformutil.Dedupe(allPlatforms), isMixed
}
