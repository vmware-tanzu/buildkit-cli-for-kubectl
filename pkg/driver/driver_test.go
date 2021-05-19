// Copyright (C) 2021 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0
package driver

import (
	"testing"

	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/pkg/store"
)

func Test_GetPlatforms(t *testing.T) {
	t.Parallel()
	info := &Info{
		DynamicNodes: []store.Node{
			{},
		},
	}
	platforms, mixed := info.GetPlatforms()
	assert.False(t, mixed)
	assert.Len(t, platforms, 0)

	armv6 := store.Node{
		Platforms: []specs.Platform{
			{
				Architecture: "arm/v6",
				OS:           "linux",
			},
		},
	}
	armv7 := store.Node{
		Platforms: []specs.Platform{
			{
				Architecture: "arm/v7",
				OS:           "linux",
			},
			{
				Architecture: "arm/v6",
				OS:           "linux",
			},
		},
	}
	arm64 := store.Node{
		Platforms: []specs.Platform{
			{
				Architecture: "arm/v7",
				OS:           "linux",
			},
			{
				Architecture: "arm/v6",
				OS:           "linux",
			},
			{
				Architecture: "arm64",
				OS:           "linux",
			},
		},
	}
	amd64 := store.Node{
		Platforms: []specs.Platform{
			{
				Architecture: "amd64",
				OS:           "linux",
			},
			{
				Architecture: "i386",
				OS:           "linux",
			},
		},
	}

	info = &Info{
		DynamicNodes: []store.Node{
			armv6,
		},
	}
	platforms, mixed = info.GetPlatforms()
	assert.False(t, mixed)
	assert.Len(t, platforms, 1)

	info = &Info{
		DynamicNodes: []store.Node{
			armv6,
			armv7,
		},
	}
	platforms, mixed = info.GetPlatforms()
	assert.False(t, mixed)
	assert.Len(t, platforms, 2)

	info = &Info{
		DynamicNodes: []store.Node{
			armv7,
			armv6,
		},
	}
	platforms, mixed = info.GetPlatforms()
	assert.False(t, mixed)
	assert.Len(t, platforms, 2)

	info = &Info{
		DynamicNodes: []store.Node{
			armv7,
			amd64,
			armv6,
		},
	}
	platforms, mixed = info.GetPlatforms()
	assert.True(t, mixed)
	assert.Len(t, platforms, 4)

	info = &Info{
		DynamicNodes: []store.Node{
			amd64,
			arm64,
		},
	}
	platforms, mixed = info.GetPlatforms()
	assert.True(t, mixed)
	assert.Len(t, platforms, 5)
}
