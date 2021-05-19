// Copyright (C) 2021 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0
package build

import (
	"bytes"
	"testing"

	"github.com/moby/buildkit/client"
	"github.com/stretchr/testify/require"
)

func Test_toRepoOnly(t *testing.T) {
	t.Parallel()
	_, err := toRepoOnly("")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid reference format")

	ret, err := toRepoOnly("short")
	require.NoError(t, err)
	require.Equal(t, ret, "docker.io/library/short")

	ret, err = toRepoOnly("short:withtag")
	require.NoError(t, err)
	require.Equal(t, ret, "docker.io/library/short")

	ret, err = toRepoOnly("prefix/img:withtag")
	require.NoError(t, err)
	require.Equal(t, ret, "docker.io/prefix/img")

	ret, err = toRepoOnly("prefix.with.dots/img:withtag")
	require.NoError(t, err)
	require.Equal(t, ret, "prefix.with.dots/img")

}

func Test_LoadInputs(t *testing.T) {
	solveOpts := &client.SolveOpt{
		// Minimal initialization set for the routine under test
		LocalDirs:     map[string]string{},
		FrontendAttrs: map[string]string{},
	}
	f, err := LoadInputs(Inputs{}, solveOpts)
	require.Error(t, err)
	require.Contains(t, err.Error(), "please specify build context")
	require.Nil(t, f)

	f, err = LoadInputs(Inputs{
		ContextPath:    "-",
		DockerfilePath: "-",
	}, solveOpts)
	require.Error(t, err)
	require.Contains(t, err.Error(), "use stdin for both build context and dockerfile")
	require.Nil(t, f)

	dockerfile := bytes.NewBufferString(`FROM scratch`)
	f, err = LoadInputs(Inputs{
		ContextPath:    "-",
		DockerfilePath: "dummy",
		InStream:       dockerfile,
	}, solveOpts)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ambiguous Dockerfile source")
	require.Nil(t, f)

	f, err = LoadInputs(Inputs{
		DockerfilePath: "-",
		InStream:       dockerfile,
	}, solveOpts)
	require.Error(t, err)
	require.Contains(t, err.Error(), "please specify build context")
	require.Nil(t, f)

	f, err = LoadInputs(Inputs{
		ContextPath:    "https://acme.com/foo",
		DockerfilePath: "-",
		InStream:       dockerfile,
	}, solveOpts)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Dockerfile from stdin is not supported with remote contexts")
	require.Nil(t, f)

	f, err = LoadInputs(Inputs{
		ContextPath: "-",
		InStream:    dockerfile,
	}, solveOpts)
	require.NoError(t, err)
	require.NotNil(t, f)
	f()

	f, err = LoadInputs(Inputs{
		DockerfilePath: "-",
		ContextPath:    ".",
		InStream:       dockerfile,
	}, solveOpts)
	require.NoError(t, err)
	require.NotNil(t, f)
	f()

	f, err = LoadInputs(Inputs{
		ContextPath: "https://acme.com/foo",
	}, solveOpts)
	require.NoError(t, err)
	require.NotNil(t, f)
	f()

}
