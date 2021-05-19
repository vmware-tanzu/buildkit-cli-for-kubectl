// Portions Copyright (C) 2020 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0
package podchooser

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"time"

	"github.com/containerd/containerd/platforms"
	"github.com/moby/buildkit/client"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/serialx/hashring"
	"github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

type PodChooser interface {
	ChoosePod(ctx context.Context, drv ClientAccess, platformList ...specs.Platform) (*corev1.Pod, error)
}

type ClientAccess interface {
	GetWorkersForPod(ctx context.Context, pod *corev1.Pod, timeout time.Duration) ([]*client.WorkerInfo, error)
}

type RandomPodChooser struct {
	RandSource rand.Source
	PodClient  clientcorev1.PodInterface
	Deployment *appsv1.Deployment
}

func (pc *RandomPodChooser) ChoosePod(ctx context.Context, drv ClientAccess, platformList ...specs.Platform) (*corev1.Pod, error) {
	pods, err := ListRunningPods(ctx, pc.PodClient, pc.Deployment)
	if err != nil {
		return nil, err
	}
	if len(pods) == 0 {
		return nil, fmt.Errorf("no builder pods are running")
	}
	pods, err = filterPods(ctx, drv, pods, platformList)
	if err != nil {
		return nil, err
	}
	randSource := pc.RandSource
	if randSource == nil {
		randSource = rand.NewSource(time.Now().Unix())
	}
	rnd := rand.New(randSource)
	n := rnd.Int() % len(pods)
	logrus.Debugf("RandomPodChooser.ChoosePod(): len(pods)=%d, n=%d", len(pods), n)
	return pods[n], nil
}

type StickyPodChooser struct {
	Key        string
	PodClient  clientcorev1.PodInterface
	Deployment *appsv1.Deployment
}

func (pc *StickyPodChooser) ChoosePod(ctx context.Context, drv ClientAccess, platformList ...specs.Platform) (*corev1.Pod, error) {
	pods, err := ListRunningPods(ctx, pc.PodClient, pc.Deployment)
	if err != nil {
		return nil, err
	}
	pods, err = filterPods(ctx, drv, pods, platformList)
	if err != nil {
		return nil, err
	}
	var podNames []string
	podMap := make(map[string]*corev1.Pod, len(pods))
	for _, pod := range pods {
		podNames = append(podNames, pod.Name)
		podMap[pod.Name] = pod
	}
	ring := hashring.New(podNames)
	chosen, ok := ring.GetNode(pc.Key)
	if !ok {
		// NOTREACHED
		logrus.Errorf("no pod found for key %q", pc.Key)
		rpc := &RandomPodChooser{
			PodClient:  pc.PodClient,
			Deployment: pc.Deployment,
		}
		return rpc.ChoosePod(ctx, drv, platformList...)
	}
	return podMap[chosen], nil
}

func ListRunningPods(ctx context.Context, client clientcorev1.PodInterface, depl *appsv1.Deployment) ([]*corev1.Pod, error) {
	labelSelector := &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"app": depl.ObjectMeta.Name,
		},
	}
	selector, err := metav1.LabelSelectorAsSelector(labelSelector)
	if err != nil {
		return nil, err
	}
	listOpts := metav1.ListOptions{
		LabelSelector: selector.String(),
	}
	podList, err := client.List(ctx, listOpts)
	if err != nil {
		return nil, err
	}
	// TODO further filter pods based on Annotations
	var runningPods []*corev1.Pod
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Status.Phase == corev1.PodRunning {
			logrus.Debugf("pod runnning: %q", pod.Name)
			runningPods = append(runningPods, pod)
		}
	}
	sort.Slice(runningPods, func(i, j int) bool {
		return runningPods[i].Name < runningPods[j].Name
	})
	return runningPods, nil
}

// TODO - move this elsewhere...

func filterPods(ctx context.Context, drv ClientAccess, pods []*corev1.Pod, platformList []specs.Platform) ([]*corev1.Pod, error) {
	// TODO - cache clients so we're not doing it multiple times...
	// The driver should establish clients to the running builders, and hold them open, then recycle them
	// and do so with a fairly short timeout so if we have a bad builder but others are OK, we can proceed with the build
	// If the user is just pushing without multi-arch, one bad builder shouldn't break the system
	// but we should warn if they're not pushing since we can't replicate properly

	if len(platformList) == 0 {
		return pods, nil
	}
	res := []*corev1.Pod{}
	for _, pod := range pods {
		workers, err := drv.GetWorkersForPod(ctx, pod, 3*time.Second)
		if err != nil {
			return nil, err
		}
		if len(workers) == 1 {
			matched := false
			for _, platform := range platformList {
				for _, cmp := range workers[0].Platforms {
					if platforms.Format(platform) == platforms.Format(cmp) {
						matched = true
						break
					}
				}
				if !matched {
					break
				}
			}
			if matched {
				res = append(res, pod)
			}
		} else {
			return nil, fmt.Errorf("Multi-worker scenario not yet implemented")
		}
	}
	return res, nil
}
