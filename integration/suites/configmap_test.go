// Copyright (C) 2020 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0
package suites

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/integration/common"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

type configMapSuite struct {
	suite.Suite
	Name        string
	CreateFlags []string

	ClientSet *kubernetes.Clientset
	Namespace string

	configMapClient v1.ConfigMapInterface
}

func (s *configMapSuite) SetupSuite() {
	var err error
	s.ClientSet, s.Namespace, err = common.GetKubeClientset()
	require.NoError(s.T(), err, "%s: kube client failed", s.Name)
	s.configMapClient = s.ClientSet.CoreV1().ConfigMaps(s.Namespace)
}

func (s *configMapSuite) getConfigMap() *corev1.ConfigMap {
	payload := `# pre-existing configuration
# nothing to see here...
`
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: appsv1.SchemeGroupVersion.String(),
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: s.Namespace,
			Name:      s.Name,
		},
		BinaryData: map[string][]byte{
			"buildkitd.toml": []byte(payload),
		},
	}
}

func (s *configMapSuite) TestDefaultCreate() {
	logrus.Infof("%s: Creating builder with default config", s.Name)
	args := append(
		[]string{
			s.Name,
		},
		s.CreateFlags...,
	)
	err := common.RunBuildkit("create", args, common.RunBuildStreams{})
	require.NoError(s.T(), err, "%s: builder create failed", s.Name)
	cfg, err := s.configMapClient.Get(context.Background(), s.Name, metav1.GetOptions{})
	require.NoError(s.T(), err, "%s: fetch configmap failed", s.Name)
	data, ok := cfg.BinaryData["buildkitd.toml"]
	require.True(s.T(), ok, "missing buildkitd.toml: %#v", cfg.BinaryData)
	// Spot check an expected string
	require.Contains(s.T(), string(data), "Default buildkitd configuration.")

	// Tear down the builder
	common.LogBuilderLogs(context.Background(), s.Name, s.Namespace, s.ClientSet)
	logrus.Infof("%s: Removing builder", s.Name)
	err = common.RunBuildkit("rm", []string{
		s.Name,
	}, common.RunBuildStreams{})
	require.NoError(s.T(), err, "%s: builder rm failed", s.Name)
	_, err = s.configMapClient.Get(context.Background(), s.Name, metav1.GetOptions{})
	require.Error(s.T(), err, "config map wasn't cleaned up")
	require.Contains(s.T(), err.Error(), "not found")
}

// Pre-create a config and make sure it does not get overridden by the default creation flow
func (s *configMapSuite) TestPreExistingConfigDefaultCreate() {
	logrus.Infof("%s: Creating pre-existing config", s.Name)
	_, err := s.configMapClient.Create(context.Background(), s.getConfigMap(), metav1.CreateOptions{})
	require.NoError(s.T(), err, "%s: pre-existing configmap create failed", s.Name)

	logrus.Infof("%s: Creating builder with default config", s.Name)
	args := append(
		[]string{
			s.Name,
		},
		s.CreateFlags...,
	)
	err = common.RunBuildkit("create", args, common.RunBuildStreams{})
	require.NoError(s.T(), err, "%s: builder create failed", s.Name)
	cfg, err := s.configMapClient.Get(context.Background(), s.Name, metav1.GetOptions{})
	require.NoError(s.T(), err, "%s: fetch configmap failed", s.Name)
	data, ok := cfg.BinaryData["buildkitd.toml"]
	require.True(s.T(), ok, "missing buildkitd.toml: %#v", cfg.BinaryData)
	// Spot check an expected string doesn't exist
	require.NotContains(s.T(), string(data), "Default buildkitd configuration.")

	// Tear down the builder
	common.LogBuilderLogs(context.Background(), s.Name, s.Namespace, s.ClientSet)
	logrus.Infof("%s: Removing builder", s.Name)
	err = common.RunBuildkit("rm", []string{
		s.Name,
	}, common.RunBuildStreams{})
	require.NoError(s.T(), err, "%s: builder rm failed", s.Name)
	_, err = s.configMapClient.Get(context.Background(), s.Name, metav1.GetOptions{})
	// TODO if we preserve pre-existing configmaps this will need to be refined.
	require.Error(s.T(), err, "config map wasn't cleaned up")
	require.Contains(s.T(), err.Error(), "not found")
}

func (s *configMapSuite) TestCustomCreate() {
	logrus.Infof("%s: Creating builder with custom config", s.Name)
	dir, cleanup, err := common.NewBuildContext(map[string]string{
		"buildkitd.toml": `# Custom config file
# nothing to see here 2
`})
	require.NoError(s.T(), err, "%s: config file creation", s.Name)

	defer cleanup()

	args := append(
		[]string{
			"--config", filepath.Join(dir, "buildkitd.toml"),
			s.Name,
		},
		s.CreateFlags...,
	)
	err = common.RunBuildkit("create", args, common.RunBuildStreams{})
	require.NoError(s.T(), err, "%s: builder create failed", s.Name)
	cfg, err := s.configMapClient.Get(context.Background(), s.Name, metav1.GetOptions{})
	require.NoError(s.T(), err, "%s: fetch configmap failed", s.Name)
	data, ok := cfg.BinaryData["buildkitd.toml"]
	require.True(s.T(), ok, "missing buildkitd.toml: %#v", cfg.BinaryData)
	// Spot check an expected string
	require.NotContains(s.T(), string(data), "Default buildkitd configuration.", string(data))
	require.Contains(s.T(), string(data), "Custom config file", string(data))

	// Tear down the builder
	common.LogBuilderLogs(context.Background(), s.Name, s.Namespace, s.ClientSet)
	logrus.Infof("%s: Removing builder", s.Name)
	err = common.RunBuildkit("rm", []string{
		s.Name,
	}, common.RunBuildStreams{})
	require.NoError(s.T(), err, "%s: builder rm failed", s.Name)
	_, err = s.configMapClient.Get(context.Background(), s.Name, metav1.GetOptions{})
	require.Error(s.T(), err, "config map wasn't cleaned up")
	require.Contains(s.T(), err.Error(), "not found")
}
func (s *configMapSuite) TestPreExistingWithCustomCreate() {
	logrus.Infof("%s: Creating pre-existing config", s.Name)
	_, err := s.configMapClient.Create(context.Background(), s.getConfigMap(), metav1.CreateOptions{})
	require.NoError(s.T(), err, "%s: pre-existing configmap create failed", s.Name)

	logrus.Infof("%s: Creating builder with custom config", s.Name)
	dir, cleanup, err := common.NewBuildContext(map[string]string{
		"buildkitd.toml": `# Custom config file
# nothing to see here 2
`})
	require.NoError(s.T(), err, "%s: config file create failed", s.Name)

	defer cleanup()

	args := append(
		[]string{
			"--config", filepath.Join(dir, "buildkitd.toml"),
			s.Name,
		},
		s.CreateFlags...,
	)
	err = common.RunBuildkit("create", args, common.RunBuildStreams{})
	require.NoError(s.T(), err, "%s: builder create failed", s.Name)
	cfg, err := s.configMapClient.Get(context.Background(), s.Name, metav1.GetOptions{})
	require.NoError(s.T(), err, "%s: fetch configmap failed", s.Name)
	data, ok := cfg.BinaryData["buildkitd.toml"]
	require.True(s.T(), ok, "missing buildkitd.toml: %#v", cfg.BinaryData)
	// Spot check expected strings
	require.NotContains(s.T(), string(data), "Default buildkitd configuration.", string(data))
	require.NotContains(s.T(), string(data), "pre-existing configuration", string(data))
	require.Contains(s.T(), string(data), "Custom config file", string(data))

	// Tear down the builder
	common.LogBuilderLogs(context.Background(), s.Name, s.Namespace, s.ClientSet)
	logrus.Infof("%s: Removing builder", s.Name)
	err = common.RunBuildkit("rm", []string{
		s.Name,
	}, common.RunBuildStreams{})
	require.NoError(s.T(), err, "%s: builder rm failed", s.Name)
	_, err = s.configMapClient.Get(context.Background(), s.Name, metav1.GetOptions{})
	require.Error(s.T(), err, "config map wasn't cleaned up")
	require.Contains(s.T(), err.Error(), "not found")
}

func TestConfigMapSuite(t *testing.T) {
	common.Skipper(t)
	t.Parallel()
	suite.Run(t, &configMapSuite{
		Name:        "configmaptest",
		CreateFlags: []string{"--buildkitd-flags=--debug"},
	})
}
