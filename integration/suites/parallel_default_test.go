// Copyright (C) 2020 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0
package suites

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/integration/common"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

const ParallelDefaultBuildCount = 3

type parallelDefaultSuite struct {
	suite.Suite
	Name        string
	CreateFlags []string

	ClientSet *kubernetes.Clientset
	Namespace string

	configMapClient v1.ConfigMapInterface
}

func (s *parallelDefaultSuite) SetupSuite() {
	var err error
	s.ClientSet, s.Namespace, err = common.GetKubeClientset()
	require.NoError(s.T(), err, "%s: kube client failed", s.Name)
	s.configMapClient = s.ClientSet.CoreV1().ConfigMaps(s.Namespace)
}

func (s *parallelDefaultSuite) TearDownSuite() {
	common.LogBuilderLogs(context.Background(), s.Name, s.Namespace, s.ClientSet)
	logrus.Infof("%s: Removing builder", s.Name)
	err := common.RunBuildkit("rm", []string{
		s.Name,
	})
	require.NoError(s.T(), err, "%s: builder rm failed", s.Name)
	configMapClient := s.ClientSet.CoreV1().ConfigMaps(s.Namespace)
	_, err = configMapClient.Get(context.Background(), s.Name, metav1.GetOptions{})
	require.Error(s.T(), err, "config map wasn't cleaned up")
	require.Contains(s.T(), err.Error(), "not found")
}

func (s *parallelDefaultSuite) TestParallelDefaultBuilds() {
	logrus.Infof("%s: Parallel %d Build", s.Name, ParallelDefaultBuildCount)

	dirs := make([]string, ParallelDefaultBuildCount)
	errors := make([]error, ParallelDefaultBuildCount)

	// Create the contexts before threading
	for i := 0; i < ParallelDefaultBuildCount; i++ {
		dir, cleanup, err := common.NewSimpleBuildContext()
		dirs[i] = dir
		require.NoError(s.T(), err, "Failed to set up temporary build context")
		defer cleanup()
	}
	wg := &sync.WaitGroup{}
	wg.Add(ParallelDefaultBuildCount)

	for i := 0; i < ParallelDefaultBuildCount; i++ {
		go func(i int) {
			defer wg.Done()
			imageName := fmt.Sprintf("dummy.acme.com/pbuild:%d", i)
			args := []string{
				"--progress=plain",
				"--tag", imageName,
				dirs[i],
			}
			err := common.RunBuild(args)
			if err != nil {
				errors[i] = err
				return
			}
			errors[i] = common.RunSimpleBuildImageAsPod(
				context.Background(),
				fmt.Sprintf("%s-testbuiltimage-%d", s.Name, i),
				imageName,
				s.Namespace,
				s.ClientSet,
			)

		}(i)
	}
	wg.Wait()
	for i := 0; i < ParallelDefaultBuildCount; i++ {
		assert.NoError(s.T(), errors[i], "build/run %d failed", i)
	}
}

func TestParallelDefaultBuildSuite(t *testing.T) {
	common.Skipper(t)
	// We don't parallelize with other tests, since we use the default builder name
	suite.Run(t, &parallelDefaultSuite{
		Name:        "buildkit",
		CreateFlags: []string{"--buildkitd-flags=--debug"},
	})
}
