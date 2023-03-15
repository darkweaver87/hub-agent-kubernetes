/*
Copyright (C) 2022-2023 Traefik Labs

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published
by the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.
*/

package integration

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/banzaicloud/kurun/tunnel"
	tunnelws "github.com/banzaicloud/kurun/tunnel/websocket"
	"github.com/gorilla/websocket"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
)

const (
	kurunServerImage = "ghcr.io/banzaicloud/kurun-server:v0.2.1"
	kurunNamespace   = "default"
	kurunServicePort = 8080
	kurunServiceName = "hub-forward"
)

func (s *AgentSuite) setupForward(ctx context.Context) error {
	controlPort := corev1.ContainerPort{
		Name:          "control",
		ContainerPort: 8333,
	}
	requestPort := corev1.ContainerPort{
		Name:          "request",
		ContainerPort: 8444,
	}

	downstreamURL, err := url.Parse(s.hub.URL)
	if err != nil {
		return fmt.Errorf("failed to parse downstream URL: %w", err)
	}

	deploymentName := kurunServiceName
	labelsMap := map[string]string{
		"app.kubernetes.io/name": deploymentName,
	}

	kubeConfig, err := config.GetConfig()
	if err != nil {
		return err
	}

	kubeCluster, err := cluster.New(kubeConfig, func(o *cluster.Options) {
		o.Namespace = kurunNamespace
	})
	if err != nil {
		return err
	}

	go func() { _ = kubeCluster.Start(ctx) }()

	if !kubeCluster.GetCache().WaitForCacheSync(ctx) {
		return errors.New("cache did not sync")
	}

	kubeClient := kubeCluster.GetClient()

	kurunService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: kurunNamespace,
			Name:      kurunServiceName,
			Labels:    labelsMap,
		},
		Spec: corev1.ServiceSpec{
			Selector: labelsMap,
			Ports: []corev1.ServicePort{
				{
					Name:       "request",
					Port:       int32(kurunServicePort),
					TargetPort: intstr.FromString(requestPort.Name),
				},
				{
					Name:       "control",
					Port:       controlPort.ContainerPort,
					TargetPort: intstr.FromString(controlPort.Name),
				},
			},
		},
	}

	if err = kubeClient.Create(ctx, kurunService); err != nil {
		return err
	}

	tunnelServerContainer := corev1.Container{
		Name:            "tunnel-server",
		Image:           kurunServerImage,
		ImagePullPolicy: corev1.PullIfNotPresent, // HACK
		Args: []string{
			"--ctrl-srv-addr",
			fmt.Sprintf("0.0.0.0:%d", controlPort.ContainerPort),
			"--ctrl-srv-self-signed",
			"--req-srv-addr",
			fmt.Sprintf("0.0.0.0:%d", requestPort.ContainerPort),
		},
		Ports: []corev1.ContainerPort{
			requestPort,
			controlPort,
		},
	}

	requestScheme := "http"

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName,
			Namespace: kurunNamespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: labelsMap,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labelsMap,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						tunnelServerContainer,
					},
					Volumes: []corev1.Volume{},
				},
			},
		},
	}

	if err = kubeClient.Create(ctx, deployment); err != nil {
		return err
	}

	if err = waitForResource(ctx, kubeCluster.GetCache(), kubeCluster.GetScheme(), deployment, func(obj interface{}) bool {
		deploy, ok := obj.(*appsv1.Deployment)
		return ok && deploy.Namespace == deployment.Namespace && deploy.Name == deployment.Name && hasAvailable(deploy)
	}, 60*time.Second); err != nil {
		return err
	}

	proxyURL, err := url.Parse(kubeConfig.Host)
	if err != nil {
		return err
	}
	if proxyURL.Scheme != "https" {
		panic("API server URL not HTTPS")
	}
	proxyURL.Scheme = "wss"
	proxyURL.Path = fmt.Sprintf("/api/v1/namespaces/%s/services/https:%s:%d/proxy/", kurunNamespace, kurunService.Name, selectServicePort(kurunService, "control").Port)

	proxyTLSCfg, err := rest.TLSConfigFor(kubeConfig)
	if err != nil {
		return err
	}
	proxyTLSCfg.InsecureSkipVerify = true

	baseTransport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}
	transport := tunnel.RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		r.URL.Scheme = downstreamURL.Scheme
		r.URL.Host = downstreamURL.Host
		if downstreamURL.Path != "" {
			r.URL.Path = path.Join(downstreamURL.Path, r.URL.Path)
		}
		return baseTransport.RoundTrip(r)
	})

	tunnelClientCfg := tunnelws.NewClientConfig(
		proxyURL.String(),
		transport,
		tunnelws.WithDialerCtor(func() *websocket.Dialer {
			return &websocket.Dialer{
				TLSClientConfig: proxyTLSCfg.Clone(),
			}
		}),
	)
	go func() {
		if err := tunnelws.RunClient(ctx, *tunnelClientCfg); err != nil {
			klog.Error(err, "tunnel client exited with error")
		}
	}()

	fmt.Fprintf(os.Stdout, "Forwarding %s://%s.%s.svc:%d -> %s\n",
		requestScheme, kurunService.Name, kurunService.Namespace, selectServicePort(kurunService, "request").Port,
		downstreamURL.String())

	return nil
}

func waitForResource(ctx context.Context, kubeCache cache.Cache, scheme *runtime.Scheme, obj client.Object, filter func(interface{}) bool, timeout time.Duration) error {
	done := make(chan struct{}, 1)
	informer, err := kubeCache.GetInformer(ctx, obj)
	if err != nil {
		return err
	}
	_, _ = informer.AddEventHandler(toolscache.FilteringResourceEventHandler{
		FilterFunc: filter,
		Handler: toolscache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				select {
				case done <- struct{}{}:
				default:
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				select {
				case done <- struct{}{}:
				default:
				}
			},
		},
	})

	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		resourceType := "resource"
		if gvk, err := apiutil.GVKForObject(obj, scheme); err == nil {
			resourceType = strings.ToLower(gvk.Kind)
		}
		return fmt.Errorf("timeout waiting for %s", resourceType)
	}
}

func hasAvailable(deployment *appsv1.Deployment) bool {
	if deployment == nil {
		return false
	}
	for _, cond := range deployment.Status.Conditions {
		if cond.Type == appsv1.DeploymentAvailable && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func selectServicePort(svc *corev1.Service, name string) *corev1.ServicePort {
	if svc != nil {
		for i := range svc.Spec.Ports {
			port := &svc.Spec.Ports[i]
			if port.Name == name {
				return port
			}
		}
	}
	return nil
}
