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
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/suite"
	hubv1alpha1 "github.com/traefik/hub-agent-kubernetes/pkg/crd/api/hub/v1alpha1"
	hubclientset "github.com/traefik/hub-agent-kubernetes/pkg/crd/generated/client/hub/clientset/versioned"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	testutils "k8s.io/kubernetes/test/utils"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/third_party/helm"
)

const (
	hubImageEnvVar = "HUB_IMAGE"
	pollInterval   = 5 * time.Second
	pollTimeout    = 60 * time.Second
)

type AgentSuite struct {
	K8SSuite
	hub              *httptest.Server
	hubMock          *hubHandler
	agentNamespace   string
	traefikNamespace string
}

func int32Ptr(i int32) *int32 {
	newI := i
	return &newI
}

func bigintPtr(i int64) *big.Int {
	newI := big.NewInt(i)
	return newI
}

func TestAgentSuite(t *testing.T) {
	suite.Run(t, &AgentSuite{
		agentNamespace:   "hub-agent",
		traefikNamespace: "traefik",
	})
}

// getLocalIP returns the non loopback local IP of the host.
func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, address := range addrs {
		// check the address type and if it is not a loopback the display it
		if ipnet, ok := address.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return ""
}

func (s *AgentSuite) SetupSuite() {
	s.K8SSuite.SetupSuite()

	var err error
	l, err := net.Listen("tcp", fmt.Sprintf("%s:", getLocalIP()))
	if err != nil {
		s.T().Fatalf("can't create listener: %s", err)
	}

	var router chi.Router
	router, s.hubMock = getHubRouter(s.T(), l)
	s.hub = httptest.NewUnstartedServer(router)
	err = s.hub.Listener.Close()
	if err != nil {
		s.T().Fatalf("can't close listener: %s", err)
	}
	s.hub.Listener = l
	s.hub.Start()

	err = s.hubMock.createCA()
	if err != nil {
		s.T().Fatalf("can't create CA: %s", err)
	}

	s.ctx, err = envfuncs.CreateNamespace(s.agentNamespace)(s.ctx, s.testConf)
	if err != nil {
		s.T().Fatalf("can't create namespace: %s", err)
	}

	s.ctx, err = envfuncs.CreateNamespace(s.traefikNamespace)(s.ctx, s.testConf)
	if err != nil {
		s.T().Fatalf("can't create namespace: %s", err)
	}

	hubImage := os.Getenv(hubImageEnvVar)
	if hubImage != "" {
		s.ctx, err = envfuncs.LoadDockerImageToCluster(s.clusterName, hubImage)(s.ctx, s.testConf)
		if err != nil {
			s.T().Fatalf("can't import image: %s", err)
		}
		s.T().Logf("Image %s imported", hubImage)
	}

	err = s.installTraefik()
	if err != nil {
		s.T().Fatalf("can't install traefik: %s", err)
	}

	err = s.installHubAgent(hubImage)
	if err != nil {
		s.T().Fatalf("can't install hub-agent: %s", err)
	}
}

func (s *AgentSuite) TearDownSuite() {
	if s.hubMock.brokerListener != nil {
		_ = s.hubMock.brokerListener.Close()
	}
	s.K8SSuite.TearDownSuite()
}

func (s *AgentSuite) installTraefik() error {
	manager := helm.New(s.testConf.KubeconfigFile())
	err := manager.RunRepo(helm.WithArgs("add", "traefik", "https://traefik.github.io/charts"))
	if err != nil {
		s.T().Error("failed to add traefik helm chart repo (it may already exist)")
	}

	err = manager.RunRepo(helm.WithArgs("update"))
	if err != nil {
		return fmt.Errorf("can't update helm repo: %w", err)
	}

	err = manager.RunInstall(helm.WithName("traefik"),
		helm.WithNamespace(s.traefikNamespace),
		helm.WithReleaseName("traefik/traefik"),
		helm.WithArgs("--set", "hub.enabled=true", "--set", "ports.web=null", "--set", "ports.websecure=null"))
	if err != nil {
		return fmt.Errorf("can't install helm release: %w", err)
	}

	return nil
}

func (s *AgentSuite) installHubAgent(image string) error {
	platformURL := s.hub.URL

	manager := helm.New(s.testConf.KubeconfigFile())
	err := manager.RunRepo(helm.WithArgs("add", "traefik", "https://traefik.github.io/charts"))
	if err != nil {
		s.T().Error("failed to add traefik helm chart repo (it may already exist)")
	}

	err = manager.RunRepo(helm.WithArgs("update"))
	if err != nil {
		return fmt.Errorf("can't update helm repo: %w", err)
	}

	args := []string{
		"--set", "token=XXX",
		"--set", "controllerDeployment.traefik.metricsURL=http://traefik-hub.traefik.svc.cluster.local:9100/metrics",
		"--set", "controllerDeployment.traefik.metricsURL=http://traefik-hub.traefik.svc.cluster.local:9100/metrics",
		"--set", "tunnelDeployment.traefik.tunnelHost=traefik-hub.traefik.svc.cluster.local",
		"--set-json", fmt.Sprintf(`'controllerDeployment.args=["--platform-url=%s"]'`, platformURL),
		"--set-json", fmt.Sprintf(`'tunnelDeployment.args=["--platform-url=%s"]'`, platformURL),
	}
	if image != "" {
		args = append(args,
			"--set", fmt.Sprintf("image.name=%s", strings.Split(image, ":")[0]),
			"--set", fmt.Sprintf("image.tag=%s", strings.Split(image, ":")[1]),
			"--set", "image.pullPolicy=IfNotPresent",
		)
	}

	err = manager.RunInstall(helm.WithName("hub-agent"),
		helm.WithNamespace(s.agentNamespace),
		helm.WithReleaseName("traefik/hub-agent"),
		helm.WithArgs(args...))

	if err != nil {
		return fmt.Errorf("can't install helm release: %w", err)
	}

	s.checkDeploymentsRunning()

	return nil
}

func (s *AgentSuite) checkDeploymentsRunning() {
	tests := []struct {
		desc string
		want *appsv1.Deployment
	}{
		{
			desc: "hub-agent-controller",
			want: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hub-agent-controller",
					Namespace: s.agentNamespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(1),
				},
			},
		},
		{
			desc: "hub-agent-auth-server",
			want: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hub-agent-auth-server",
					Namespace: s.agentNamespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(3),
				},
			},
		},
		{
			desc: "hub-agent-tunnel",
			want: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hub-agent-tunnel",
					Namespace: s.agentNamespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(1),
				},
			},
		},
	}

	for _, test := range tests {
		test := test
		s.T().Run(test.desc, func(t *testing.T) {
			_, client := s.kubeClient()
			err := testutils.WaitForDeploymentComplete(client, test.want, func(format string, args ...interface{}) {}, pollInterval, pollTimeout)
			if err != nil {
				s.Fail("hub-agent not running")
			}
		})
	}
}

func (s *AgentSuite) TestEdgeIngress() {
	cfg, client := s.kubeClient()
	hubClient, err := hubclientset.NewForConfig(cfg)
	s.Nil(err)

	_, err = client.CoreV1().Pods("default").Create(s.ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "whoami",
			Namespace: "default",
			Labels: map[string]string{
				"app.kubernetes.io/name": "whoami",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "whoami",
					Image: "ghcr.io/traefik/whoami:v1.9.0",
					Args: []string{
						"-port",
						"8080",
					},
					Ports: []corev1.ContainerPort{
						{
							ContainerPort: 8080,
						},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}, metav1.CreateOptions{})
	s.Nil(err)

	_, err = client.CoreV1().Services("default").Create(s.ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "whoami",
			Namespace: "default",
			Labels: map[string]string{
				"app.kubernetes.io/name": "whoami",
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Selector: map[string]string{
				"app.kubernetes.io/name": "whoami",
			},
			Ports: []corev1.ServicePort{
				{
					Port:       8080,
					TargetPort: intstr.FromInt(8080),
				},
			},
		},
	}, metav1.CreateOptions{})
	s.Nil(err)

	_, err = hubClient.HubV1alpha1().EdgeIngresses("default").Create(s.ctx, &hubv1alpha1.EdgeIngress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "whoami",
			Namespace: "default",
		},
		Spec: hubv1alpha1.EdgeIngressSpec{
			Service: hubv1alpha1.EdgeIngressService{
				Name: "whoami",
				Port: 8080,
			},
		},
	}, metav1.CreateOptions{})
	s.Nil(err)

	watcher, err := hubClient.HubV1alpha1().EdgeIngresses("default").Watch(s.ctx, metav1.ListOptions{
		Watch: true,
	})
	s.Nil(err)

	_ = s.waitForCondition(watcher, 120*time.Second, waitCondition{
		desc: "edge-ingress to be up",
		fn: func(obj runtime.Object) bool {
			item := obj.(*hubv1alpha1.EdgeIngress)
			if item != nil && item.Status.Connection == hubv1alpha1.EdgeIngressConnectionUp {
				return true
			}
			return false
		},
	})

	time.Sleep(1 * time.Second)

	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(s.hubMock.caCertPEM)
	tlsConfig := &tls.Config{
		RootCAs:    caCertPool,
		MinVersion: tls.VersionTLS12,
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == "test.localhost:443" {
			addr = s.hubMock.tunnelAddr
		}

		dialer := &net.Dialer{Timeout: 2 * time.Second}
		return dialer.DialContext(ctx, network, addr)
	}}
	httpClient := &http.Client{Transport: transport}

	resp, err := httpClient.Get("https://test.localhost/api")
	if err != nil {
		s.T().Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		s.T().Fatal(err)
	}

	whoamiAPI := struct {
		Hostname string `json:"hostname"`
	}{}

	if err = json.Unmarshal(body, &whoamiAPI); err != nil {
		s.T().Fatal(err)
	}

	s.Equal("whoami", whoamiAPI.Hostname)
}
