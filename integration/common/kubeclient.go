// Copyright (C) 2020 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0
package common

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GetKubeClientset retrieves the clientset and namespace
func GetKubeClientset() (*kubernetes.Clientset, string, error) {
	configFlags := genericclioptions.NewConfigFlags(true)
	clientConfig := configFlags.ToRawKubeConfigLoader()
	ns, _, err := clientConfig.Namespace()
	if err != nil {
		return nil, "", err
	}
	restClientConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, "", err
	}
	clientset, err := kubernetes.NewForConfig(restClientConfig)
	return clientset, ns, err
}

func RunSimpleBuildImageAsPod(ctx context.Context, name, imageName, namespace string, clientset *kubernetes.Clientset) error {
	podClient := clientset.CoreV1().Pods(namespace)
	eventClient := clientset.CoreV1().Events(namespace)
	logrus.Infof("starting pod %s for image: %s", name, imageName)
	gracePeriod := int64(0) // Speed up the shutdown at the end
	// Start the pod
	pod, err := podClient.Create(ctx,
		&v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},

			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:            name,
						Image:           imageName,
						Command:         []string{"sleep", "60"},
						ImagePullPolicy: v1.PullNever,
					},
				},
				TerminationGracePeriodSeconds: &gracePeriod,
			},
		},
		metav1.CreateOptions{},
	)
	if err != nil {
		return err
	}

	defer func() {
		err := podClient.Delete(ctx, pod.Name, metav1.DeleteOptions{})
		if err != nil {
			logrus.Warnf("failed to clean up pod %s: %s", pod.Name, err)
		}
	}()

	logrus.Infof("waiting for pod to start...")
	// Wait for it to get started, and make sure it isn't complaining about image not being found
	// TODO - multi-node test clusters will need some refinement here if we wind up not scaling the builder up in some scenarios
	var refUID *string
	var refKind *string
	reportedEvents := map[string]interface{}{}

	// TODO - DRY this out with pkg/driver/kubernetes/driver.go:wait(...)
	for try := 0; try < 100; try++ {

		stringRefUID := string(pod.GetUID())
		if len(stringRefUID) > 0 {
			refUID = &stringRefUID
		}
		stringRefKind := pod.Kind
		if len(stringRefKind) > 0 {
			refKind = &stringRefKind
		}
		selector := eventClient.GetFieldSelector(&pod.Name, &pod.Namespace, refKind, refUID)
		options := metav1.ListOptions{FieldSelector: selector.String()}
		events, err2 := eventClient.List(ctx, options)
		if err2 != nil {
			return err2
		}

		for _, event := range events.Items {
			if event.InvolvedObject.UID != pod.ObjectMeta.UID {
				continue
			}
			msg := fmt.Sprintf("%s:%s:%s:%s\n",
				event.Type,
				pod.Name,
				event.Reason,
				event.Message,
			)
			if _, alreadyProcessed := reportedEvents[msg]; alreadyProcessed {
				continue
			}
			reportedEvents[msg] = struct{}{}
			logrus.Info(msg)

			if event.Reason == "ErrImageNeverPull" {
				// Fail fast, it will never converge
				return fmt.Errorf(msg)
			}
		}

		<-time.After(time.Duration(100+try*20) * time.Millisecond)
		pod, err = podClient.Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		logrus.Infof("Pod Phase: %s", pod.Status.Phase)
		if pod.Status.Phase == v1.PodRunning || pod.Status.Phase == v1.PodSucceeded {
			return nil
		}
	}
	return fmt.Errorf("pod never started")
}

// GetRuntime will return the runtime detected in the cluster
// Assumes a common runtime (first node found is returned)
func GetRuntime(ctx context.Context, clientset *kubernetes.Clientset) (string, error) {
	nodeClient := clientset.CoreV1().Nodes()
	nodes, err := nodeClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	if len(nodes.Items) > 0 {
		return nodes.Items[0].Status.NodeInfo.ContainerRuntimeVersion, nil
	}
	return "", fmt.Errorf("unable to retrieve node runtimes")

}

// GetNodeCount will return the number of nodes detected in the cluster that are ready
func GetNodeCount(ctx context.Context, clientset *kubernetes.Clientset) int32 {
	nodeClient := clientset.CoreV1().Nodes()
	nodes, err := nodeClient.List(ctx, metav1.ListOptions{})
	ready := int32(0)
	if err != nil {
		logrus.Errorf("failed to list nodes: %s", err)
		return 0
	}
	for _, node := range nodes.Items {
		nodeReady := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == v1.NodeReady && condition.Status == v1.ConditionTrue {
				nodeReady = true
				break
			}
		}
		if nodeReady {
			ready++
		} else {
			logrus.Warningf("node %s not ready", node.Name)
		}
	}
	return ready
}

// MaybeScaleUp will scale up the designated builder if the cluster has multiple ready nodes
func MaybeScaleUpBuilder(ctx context.Context, clientset *kubernetes.Clientset, namespace, builderName string) error {
	if count := GetNodeCount(ctx, clientset); count > 1 {
		logrus.Infof("scaling up %s to %d", builderName, count)
		deploymentClient := clientset.AppsV1().Deployments(namespace)
		scale, err := deploymentClient.GetScale(ctx, builderName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		scale.Spec.Replicas = count
		_, err = deploymentClient.UpdateScale(
			ctx,
			builderName,
			scale,
			metav1.UpdateOptions{})
		if err != nil {
			return err
		}

		// Wait for things to scale up...
		logrus.Infof("waiting for the builder %s to scale up to %d", builderName, count)
		ready := false
		for try := 0; try < 100; try++ {
			time.Sleep(250 * time.Millisecond)
			depl, err := deploymentClient.Get(ctx, builderName, metav1.GetOptions{})
			if err != nil {
				logrus.Errorf("Failed to get builder deployment after scaling up")
				return err
			}
			if depl.Status.ReadyReplicas >= count {
				logrus.Infof("builder %s has scaled up to %d", builderName, depl.Status.ReadyReplicas)
				ready = true
				break
			} else if try > 50 { // Keep it quiet for normal circumstances
				logrus.Infof("builder hasn't scaled up yet: %d of %d", depl.Status.ReadyReplicas, count)
				// TODO - if this becomes a failure mode of our tests consider adding more detailed
				//        reporting of why it hasn't hit scale yet...
			}
		}
		if !ready {
			return fmt.Errorf("builder failed to scale up")
		}
	}
	return nil
}

// LogBuilderLogs attempts to replay the log messages from the builder(s)
func LogBuilderLogs(ctx context.Context, name, namespace string, clientset *kubernetes.Clientset) {
	podClient := clientset.CoreV1().Pods(namespace)
	labelSelector := &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"app": name,
		},
	}
	selector, err := metav1.LabelSelectorAsSelector(labelSelector)
	if err != nil {
		logrus.Errorf("should not happen: %s", err)
		return
	}
	listOpts := metav1.ListOptions{
		LabelSelector: selector.String(),
	}
	podList, err := podClient.List(ctx, listOpts)
	if err != nil {
		logrus.Warnf("failed to get builder pods for logging: %s", err)
		return
	}
	logrus.Infof("Detected %d pods for builder %s - gathering logs", len(podList.Items), name)
	logrus.Infof("--- BEGIN BUILDER LOGS ---")

	for _, pod := range podList.Items {
		logrus.Infof("%s labels %#v", pod.Name, pod.Labels)
		req := podClient.GetLogs(pod.Name, &v1.PodLogOptions{})
		buf, err := req.DoRaw(ctx)
		if err != nil {
			logrus.Errorf("failed to get logs for %s: %s", pod.Name, err)
		}
		for _, line := range strings.Split(string(buf), "\n") {
			// Don't use logrus since that results in double levels and conflicting timestamps
			fmt.Printf("pod=\"%s\" %s\n", pod.Name, line)
		}
	}
	logrus.Infof("--- END BUILDER LOGS ---")

}
