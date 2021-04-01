// Portions Copyright (C) 2020 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0
package kubernetes

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/moby/buildkit/client"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/pkg/driver"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/pkg/driver/kubernetes/execconn"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/pkg/driver/kubernetes/manifest"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/pkg/driver/kubernetes/podchooser"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/pkg/imagetools"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/pkg/progress"
	"github.com/vmware-tanzu/buildkit-cli-for-kubectl/pkg/store"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	clientappsv1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	clientcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	DriverName = "kubernetes"
)

const (
	// valid values for driver-opt loadbalance
	LoadbalanceRandom = "random"
	LoadbalanceSticky = "sticky"

	// valid values for driver-opt worker
	WorkerContainerd           = "containerd"
	WorkerRunc                 = "runc"
	DefaultContainerdNamespace = "k8s.io"
	DefaultContainerdSockPath  = "/run/containerd/containerd.sock"
	DefaultDockerSockPath      = "/var/run/docker.sock"

	//DefaultContainerRuntime    = "containerd" // TODO figure out a way to autodiscover this if not specified for better UX
	DefaultContainerRuntime = "docker" // Temporary since most kubernetes clusters are still Docker based today...

	// TODO - consider adding other default values here to aid users in fine-tuning by editing the configmap post deployment
	DefaultConfigFileTemplate = `# Default buildkitd configuration.  Use --config <path/to/file> to override during create
debug = false
[worker.containerd]
  namespace = "{{ .ContainerdNamespace }}"
`
)

type Driver struct {
	driver.InitConfig
	factory              driver.Factory
	minReplicas          int
	deployment           *appsv1.Deployment
	configMap            *corev1.ConfigMap
	clientset            *kubernetes.Clientset
	deploymentClient     clientappsv1.DeploymentInterface
	replicaSetClient     clientappsv1.ReplicaSetInterface
	podClient            clientcorev1.PodInterface
	configMapClient      clientcorev1.ConfigMapInterface
	secretClient         clientcorev1.SecretInterface
	podChooser           podchooser.PodChooser
	eventClient          clientcorev1.EventInterface
	userSpecifiedRuntime bool
	userSpecifiedConfig  bool
	namespace            string
	loadbalance          string
	authHintMessage      string
}

func (d *Driver) Bootstrap(ctx context.Context, l progress.Logger) error {
	return progress.Wrap("[internal] booting buildkit", l, func(sub progress.SubLogger) error {
		_, err := d.configMapClient.Get(ctx, d.configMap.Name, metav1.GetOptions{})

		if err != nil && kubeerrors.IsNotFound(err) {
			// Doesn't exist, create it
			_, err = d.configMapClient.Create(ctx, d.configMap, metav1.CreateOptions{})
		} else if err != nil {
			return errors.Wrapf(err, "configmap get error for %q", d.configMap.Name)
		} else if d.userSpecifiedConfig {
			// err was nil, thus it already exists, and user passed a new config, so update it
			_, err = d.configMapClient.Update(ctx, d.configMap, metav1.UpdateOptions{})
		}
		if err != nil {
			return errors.Wrapf(err, "configmap error for buildkitd.toml for %q", d.configMap.Name)
		}

		_, err = d.deploymentClient.Get(ctx, d.deployment.Name, metav1.GetOptions{})
		if err != nil {
			// TODO: return err if err != ErrNotFound
			_, err = d.deploymentClient.Create(ctx, d.deployment, metav1.CreateOptions{})
			if err != nil {
				return errors.Wrapf(err, "error while calling deploymentClient.Create for %q", d.deployment.Name)
			}
		}
		return sub.Wrap(
			fmt.Sprintf("waiting for %d pods to be ready for %s", d.minReplicas, d.deployment.Name),
			func() error {
				if err := d.wait(ctx, sub); err != nil {
					return err
				}
				return nil
			})
	})
}

func isChildOf(objectMeta metav1.ObjectMeta, parentUID string) bool {
	for _, ownerRef := range objectMeta.OwnerReferences {
		if string(ownerRef.UID) == parentUID {
			return true
		}
	}
	return false
}

func (d *Driver) wait(ctx context.Context, sub progress.SubLogger) error {
	var (
		err           error
		depl          *appsv1.Deployment
		deploymentUID string
		replicaUID    string
		refUID        *string
		refKind       *string
	)
	reportedEvents := map[string]interface{}{}

	depl, err = d.deploymentClient.Get(ctx, d.deployment.Name, metav1.GetOptions{})
	for try := 0; try < 100; try++ {
		if err == nil {
			if depl.Status.ReadyReplicas >= int32(d.minReplicas) {
				sub.Log(1, []byte(fmt.Sprintf("All %d replicas for %s online\n", d.minReplicas, d.deployment.Name)))
				return nil
			}
			deploymentUID = string(depl.ObjectMeta.GetUID())

			// Check to see if we have any underlying errors
			// TODO - consider moving this to a helper routine
			labelSelector := &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": d.deployment.ObjectMeta.Name,
				},
			}
			selector, err2 := metav1.LabelSelectorAsSelector(labelSelector)
			if err2 != nil {
				return err2
			}
			listOpts := metav1.ListOptions{
				LabelSelector: selector.String(),
			}

			// Check to see if we have any replicaset errors
			replicas, err2 := d.replicaSetClient.List(ctx, listOpts) // TODO - consider a get?
			if err2 != nil {
				return err2
			}

			// TODO DRY this out with the pod event handling below
			for i := range replicas.Items {
				replica := &replicas.Items[i]
				if !isChildOf(replica.ObjectMeta, deploymentUID) {
					continue
				}
				replicaUID = string(replica.ObjectMeta.GetUID())
				stringRefUID := string(replica.GetUID())
				if len(stringRefUID) > 0 {
					refUID = &stringRefUID
				}
				stringRefKind := replica.Kind
				if len(stringRefKind) > 0 {
					refKind = &stringRefKind
				}

				selector := d.eventClient.GetFieldSelector(&replica.Name, &replica.Namespace, refKind, refUID)
				options := metav1.ListOptions{FieldSelector: selector.String()}
				events, err2 := d.eventClient.List(ctx, options)
				if err2 != nil {
					return err2
				}

				for _, event := range events.Items {
					if event.InvolvedObject.UID != replica.ObjectMeta.UID {
						logrus.Infof("XXX SHOULD NOT BE HERE!") // TODO if this doesn't show up, this check can be removed as the selector above is sufficient
						continue
					}
					// TODO - better formatting alignment...
					msg := fmt.Sprintf("%s \t%s \t%s \t%s\n",
						event.Type,
						replica.Name,
						event.Reason,
						event.Message,
					)
					if _, alreadyProcessed := reportedEvents[msg]; alreadyProcessed {
						continue
					}
					reportedEvents[msg] = struct{}{}
					sub.Log(1, []byte(msg))
					// TODO handle known failure modes here...
				}
			}

			podList, err2 := d.podClient.List(ctx, listOpts)
			if err2 != nil {
				return err2
			}
		pods:
			for i := range podList.Items {
				pod := &podList.Items[i]
				if !isChildOf(pod.ObjectMeta, replicaUID) {
					continue
				}

				stringRefUID := string(pod.GetUID())
				if len(stringRefUID) > 0 {
					refUID = &stringRefUID
				}
				stringRefKind := pod.Kind
				if len(stringRefKind) > 0 {
					refKind = &stringRefKind
				}
				selector := d.eventClient.GetFieldSelector(&pod.Name, &pod.Namespace, refKind, refUID)
				options := metav1.ListOptions{FieldSelector: selector.String()}
				events, err2 := d.eventClient.List(ctx, options)
				if err2 != nil {
					return err2
				}

				for _, event := range events.Items {
					if event.InvolvedObject.UID != pod.ObjectMeta.UID {
						continue
					}
					// TODO - better formatting alignment...
					msg := fmt.Sprintf("%s \t%s \t%s \t%s\n",
						event.Type,
						pod.Name,
						event.Reason,
						event.Message,
					)
					if _, alreadyProcessed := reportedEvents[msg]; alreadyProcessed {
						continue
					}
					reportedEvents[msg] = struct{}{}
					sub.Log(1, []byte(msg))

					if event.Type == "Normal" {
						continue
					}
					// Handle known events that represent failed default deployments.
					if event.Reason == "FailedMount" && strings.Contains(event.Message, "is not a socket file") {
						if d.userSpecifiedRuntime {
							return fmt.Errorf("pod failed to initialize - did you pick the correct runtime? - %s", event.Message)
						}
						// Flip the logic for what the default runtime is, and re-init, then re-bootstrap and try once more.
						attemptedRuntime := DefaultContainerRuntime
						runtime := ""
						switch attemptedRuntime {
						case "containerd":
							runtime = "docker"
						case "docker":
							runtime = "containerd"
						default:
							return fmt.Errorf("unexpected runtime: %s", attemptedRuntime) // not reached
						}

						sub.Log(1, []byte(fmt.Sprintf("WARN: initial attempt to deploy configured for the %s runtime failed, retrying with %s\n", attemptedRuntime, runtime)))
						d.InitConfig.DriverOpts["runtime"] = runtime
						d.InitConfig.DriverOpts["worker"] = "auto"
						err = d.initDriverFromConfig() // This will toggle userSpecifiedRuntime to true to prevent cycles
						if err != nil {
							return err
						}
						// Instead of updating, we'll just delete the deployment and re-create
						if err := d.deploymentClient.Delete(ctx, d.deployment.Name, metav1.DeleteOptions{}); err != nil {
							return errors.Wrapf(err, "error while calling deploymentClient.Delete for %q", d.deployment.Name)
						}

						// Wait for the pods to wind down before re-deploying...
						for ; try < 100; try++ {
							remainingPods := 0
							podList, err2 := d.podClient.List(ctx, listOpts)
							if err2 != nil {
								return err2
							}
							for _, pod := range podList.Items {
								if !isChildOf(pod.ObjectMeta, replicaUID) {
									continue
								}
								remainingPods++
							}
							if remainingPods == 0 {
								break
							}

							<-time.After(time.Duration(100+try*20) * time.Millisecond)
						}
						// TODO instead of a dumb sleep, consider waiting for the pods to unwind first to avoid potential
						// races
						time.Sleep(2 * time.Second)
						// Note, we're not re-trying the config creation, just the deployment
						depl, err = d.deploymentClient.Create(ctx, d.deployment, metav1.CreateOptions{})
						if err != nil {
							// If we fail to re-create, bail out entirely
							return fmt.Errorf("failed to redeploy with updated settings: %w", err)
						}
						// Keep on waiting and hope this variation works.
						break pods
					}
					// TODO - add other common failure modes here...
				}
			}

			err = errors.Errorf("expected %d replicas to be ready, got %d",
				d.minReplicas, depl.Status.ReadyReplicas)
		} else {
			depl, err = d.deploymentClient.Get(ctx, d.deployment.Name, metav1.GetOptions{})
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(100+try*20) * time.Millisecond):
		}
	}
	return err
}

func (d *Driver) Info(ctx context.Context) (*driver.Info, error) {
	depl, err := d.deploymentClient.Get(ctx, d.deployment.Name, metav1.GetOptions{})
	if err != nil {
		// TODO: return err if err != ErrNotFound
		return &driver.Info{
			Status: driver.Inactive,
		}, nil
	}
	if depl.Status.ReadyReplicas <= 0 {
		return &driver.Info{
			Status: driver.Stopped,
		}, nil
	}
	pods, err := podchooser.ListRunningPods(ctx, d.podClient, depl)
	if err != nil {
		return nil, err
	}
	var dynNodes []store.Node
	for _, p := range pods {
		node := store.Node{
			Name: p.Name,
			// Other fields are unset (TODO: detect real platforms)
		}
		dynNodes = append(dynNodes, node)
	}
	return &driver.Info{
		Status:       driver.Running,
		DynamicNodes: dynNodes,
	}, nil
}

func (d *Driver) Stop(ctx context.Context, force bool) error {
	// future version may scale the replicas to zero here
	return nil
}

func (d *Driver) Rm(ctx context.Context, force bool) error {
	if err := d.deploymentClient.Delete(ctx, d.deployment.Name, metav1.DeleteOptions{}); err != nil {
		return errors.Wrapf(err, "error while calling deploymentClient.Delete for %q", d.deployment.Name)
	}
	// TODO - consider checking for our expected labels and preserve pre-existing ConfigMaps
	if err := d.configMapClient.Delete(ctx, d.configMap.Name, metav1.DeleteOptions{}); err != nil {
		return errors.Wrapf(err, "error while calling configMapClient.Delete for %q", d.configMap.Name)
	}
	return nil
}

func (d *Driver) Client(ctx context.Context) (*client.Client, string, error) {
	restClient := d.clientset.CoreV1().RESTClient()
	restClientConfig, err := d.KubeClientConfig.ClientConfig()
	if err != nil {
		return nil, "", err
	}
	pod, err := d.podChooser.ChoosePod(ctx)
	if err != nil {
		return nil, "", err
	}
	if len(pod.Spec.Containers) == 0 {
		return nil, "", errors.Errorf("pod %s does not have any container", pod.Name)
	}
	containerName := pod.Spec.Containers[0].Name
	cmd := []string{"buildctl", "dial-stdio"}
	conn, err := execconn.ExecConn(restClient, restClientConfig,
		pod.Namespace, pod.Name, containerName, cmd)
	if err != nil {
		return nil, "", err
	}
	client, err := client.New(ctx, "", client.WithDialer(func(string, time.Duration) (net.Conn, error) {
		return conn, nil
	}))
	return client, pod.Name, err
}

// TODO debugging concurrent access problems
// type wrappedConn struct {
// 	net.Conn
// 	name string
// }
//
// func (wc wrappedConn) Close() error {
// 	return wc.Conn.Close()
// }

func (d *Driver) RuntimeSockProxy(ctx context.Context, name string) (net.Conn, error) {
	restClient := d.clientset.CoreV1().RESTClient()
	restClientConfig, err := d.InitConfig.KubeClientConfig.ClientConfig()
	if err != nil {
		return nil, err
	}
	pods, err := podchooser.ListRunningPods(ctx, d.podClient, d.deployment)
	if err != nil {
		return nil, err
	}

	for _, pod := range pods {
		// TODO - this should really be node name based not pod name based
		if pod.Name != name {
			continue
		}

		if len(pod.Spec.Containers) == 0 {
			return nil, errors.Errorf("pod %s does not have any container", pod.Name)
		}
		containerName := pod.Spec.Containers[0].Name

		runtime := pod.ObjectMeta.Labels["runtime"]
		var sockPath string
		switch runtime {
		case "containerd":
			sockPath = "unix://" + DefaultContainerdSockPath
		case "docker":
			sockPath = "unix://" + DefaultDockerSockPath
		default:
			return nil, fmt.Errorf("unexpected runtime label (%v) on pod (%s)", runtime, pod.Name)
		}
		cmd := []string{"buildctl", "--addr", sockPath, "dial-stdio"}
		return execconn.ExecConn(restClient, restClientConfig,
			pod.Namespace, pod.Name, containerName, cmd)
	}

	return nil, fmt.Errorf("no available builder pods for %s", name)

}

func (d *Driver) GetVersion(ctx context.Context) (string, error) {
	restClient := d.clientset.CoreV1().RESTClient()
	restClientConfig, err := d.KubeClientConfig.ClientConfig()
	if err != nil {
		return "", err
	}
	pod, err := d.podChooser.ChoosePod(ctx)
	if err != nil {
		return "", err
	}
	if len(pod.Spec.Containers) == 0 {
		return "", errors.Errorf("pod %s does not have any container", pod.Name)
	}
	containerName := pod.Spec.Containers[0].Name
	cmd := []string{"buildkitd", "--version"}
	buf := &bytes.Buffer{}

	req := restClient.
		Post().
		Namespace(pod.Namespace).
		Resource("pods").
		Name(pod.Name).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   cmd,
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)
	u := req.URL()
	exec, err := remotecommand.NewSPDYExecutor(restClientConfig, "POST", u)
	if err != nil {
		return "", err
	}
	// TODO how to timeout if something goes bad...?
	serr := exec.Stream(remotecommand.StreamOptions{
		Stdout: buf,
		Stderr: os.Stderr,
		Tty:    false,
	})
	if serr != nil {
		return "", err
	}

	return strings.TrimSpace(buf.String()), err
}

func (d *Driver) List(ctx context.Context) ([]driver.Builder, error) {
	var builders []driver.Builder
	depls, err := d.deploymentClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to lookup builder deployments")
	}
	for _, depl := range depls.Items {
		// Check for the builkit annotation, else skip
		if _, found := depl.ObjectMeta.Annotations[manifest.AnnotationKey]; !found {
			continue
		}
		builder := driver.Builder{
			Name:   depl.ObjectMeta.Name,
			Driver: DriverName,
		}
		pods, err := podchooser.ListRunningPods(ctx, d.podClient, &depl)
		if err != nil {
			return nil, err
		}
		for _, p := range pods {
			node := driver.Node{
				// TODO this isn't ideal - Need a good way to translate between pod name and node hostname
				//Name:   p.Spec.NodeName,
				//Name:   p.Status.HostIP,
				Name:   p.Name,
				Status: p.Status.Message, // TODO - this seems to be blank, need to look at kubectl CLI magic...
				// Other fields are unset (TODO: detect real platforms)
			}
			builder.Nodes = append(builder.Nodes, node)
		}
		builders = append(builders, builder)
	}
	return builders, nil
}

func (d *Driver) Factory() driver.Factory {
	return d.factory
}

func isRootless(rootless string) bool {
	b, err := strconv.ParseBool(rootless)
	if err == nil {
		return b
	}
	// TODO consider logging failed parse error message
	return false
}

func (d *Driver) Features() map[driver.Feature]bool {
	res := map[driver.Feature]bool{
		driver.OCIExporter:        true,
		driver.DockerExporter:     false,
		driver.ContainerdExporter: false,
		driver.CacheExport:        true,
		driver.MultiPlatform:      true, // Untested (needs multiple Driver instances)
	}
	// Query the pod to figure out the runtime

	// TODO how will this work for multi-pod deployments?
	pod, err := d.podChooser.ChoosePod(context.TODO())
	if err == nil && len(pod.Spec.Containers) > 0 && !isRootless(pod.ObjectMeta.Labels["rootless"]) {
		switch pod.ObjectMeta.Labels["runtime"] {
		case "containerd":
			res[driver.ContainerdExporter] = true
		case "docker":
			res[driver.DockerExporter] = true
		default:
			// TODO consider logging warning for unrecognized runtime...
		}
	}
	return res
}

func (d *Driver) GetAuthWrapper(secretName string) imagetools.Auth {
	if secretName == "" {
		secretName = buildxNameToDeploymentName(d.InitConfig.Name)
	}

	return &authProvider{
		driver: d,
		name:   secretName,
	}
}
