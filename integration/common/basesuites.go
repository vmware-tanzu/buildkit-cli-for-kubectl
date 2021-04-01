// Copyright (C) 2020 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0
package common

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type BaseSuite struct {
	suite.Suite
	Name            string
	CreateFlags     []string
	SkipSetupCreate bool

	ClientSet *kubernetes.Clientset
	Namespace string
}

func (s *BaseSuite) SetupSuite() {
	var err error
	if !s.SkipSetupCreate {
		logrus.Infof("%s: Setting up builder", s.Name)
		args := append(
			[]string{
				s.Name,
			},
			s.CreateFlags...,
		)
		err := RunBuildkit("create", args, RunBuildStreams{})
		require.NoError(s.T(), err, "%s: builder create failed", s.Name)
	}

	s.ClientSet, s.Namespace, err = GetKubeClientset()
	require.NoError(s.T(), err, "%s: kube client failed", s.Name)
}

func (s *BaseSuite) TearDownSuite() {
	LogBuilderLogs(context.Background(), s.Name, s.Namespace, s.ClientSet)
	logrus.Infof("%s: Removing builder", s.Name)
	err := RunBuildkit("rm", []string{
		s.Name,
	}, RunBuildStreams{})
	require.NoError(s.T(), err, "%s: builder rm failed", s.Name)
	configMapClient := s.ClientSet.CoreV1().ConfigMaps(s.Namespace)
	_, err = configMapClient.Get(context.Background(), s.Name, metav1.GetOptions{})
	require.Error(s.T(), err, "config map wasn't cleaned up")
	require.Contains(s.T(), err.Error(), "not found")
}

func (s *BaseSuite) TestSimpleBuild() {
	logrus.Infof("%s: Simple Build", s.Name)

	dir, cleanup, err := NewSimpleBuildContext()
	defer cleanup()
	require.NoError(s.T(), err, "Failed to set up temporary build context")
	args := []string{"--progress=plain"}
	if s.Name != "buildkit" { // TODO wire up the default name variable
		args = append(
			args,
			"--builder", s.Name,
		)
	}
	imageName := "dummy.acme.com/" + s.Name + "replaceme:latest"
	args = append(
		args,
		"--tag", imageName,
		dir,
	)
	err = RunBuild(args, RunBuildStreams{})
	if isRootlessCreate(s.CreateFlags) {
		require.Error(s.T(), err)
		require.Contains(s.T(), err.Error(), "please specify")
	} else {
		require.NoError(s.T(), err, "build failed")
	}

	err = RunSimpleBuildImageAsPod(context.Background(), s.Name+"-testbuiltimage", imageName, s.Namespace, s.ClientSet)
	require.NoError(s.T(), err, "failed to start pod with image")
}

func isRootlessCreate(flags []string) bool {
	for _, flag := range flags {
		if strings.Contains(flag, "rootless") {
			return true
		}
	}
	return false
}

func (s *BaseSuite) TestLocalOutputTarBuild() {
	logrus.Infof("%s: Local Output Tar Build", s.Name)

	dir, cleanup, err := NewSimpleBuildContext()
	defer cleanup()
	require.NoError(s.T(), err, "Failed to set up temporary build context")
	args := []string{"--progress=plain"}
	if s.Name != "buildkit" { // TODO wire up the default name variable
		args = append(
			args,
			"--builder", s.Name,
		)
	}
	args = append(
		args,
		"--tag", s.Name+"replaceme:latest",
		fmt.Sprintf("--output=type=tar,dest=%s", path.Join(dir, "out.tar")),
		dir,
	)
	err = RunBuild(args, RunBuildStreams{})
	require.NoError(s.T(), err, "build failed")
	// TODO - consider inspecting the out.tar for validity...
}
