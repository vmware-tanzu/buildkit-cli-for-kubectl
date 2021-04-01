// Copyright (C) 2020 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0
package suites

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/integration/common"
)

type DefaultSuite struct{ common.BaseSuite }

func (s *DefaultSuite) TestVersion() {
	buf := &bytes.Buffer{}
	err := common.RunBuildkit("version", []string{}, common.RunBuildStreams{Out: buf})
	require.NoError(s.T(), err, "%s: builder version failed", s.Name)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(s.T(), lines, 2)
	require.Contains(s.T(), lines[0], "Client:")
	require.Contains(s.T(), lines[1], "buildkitd")
}

func TestDefaultSuite(t *testing.T) {
	common.Skipper(t)
	t.Parallel()
	suite.Run(t, &DefaultSuite{
		BaseSuite: common.BaseSuite{
			Name: "buildkit", // TODO pull this from the actual default name
			// For the "default" scenario, we rely on the initial test case to establish the builder with defaults
			SkipSetupCreate: true,
			CreateFlags:     []string{"--buildkitd-flags=--debug"},
		},
	})
}
