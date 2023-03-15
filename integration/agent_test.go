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
	"bytes"
	"context"
	"fmt"
	"io"
	"math/big"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
	hubv1alpha1 "github.com/traefik/hub-agent-kubernetes/pkg/crd/api/hub/v1alpha1"
	hubclientset "github.com/traefik/hub-agent-kubernetes/pkg/crd/generated/client/hub/clientset/versioned"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

func (s *AgentSuite) SetupSuite() {
	s.K8SSuite.SetupSuite()

	var err error
	s.hub = httptest.NewServer(getHubRouter())

	s.ctx, err = envfuncs.CreateNamespace(s.agentNamespace)(s.ctx, s.testConf)
	if err != nil {
		s.T().Fatalf("can't create namespace: %s", err)
	}

	s.ctx, err = envfuncs.CreateNamespace(s.traefikNamespace)(s.ctx, s.testConf)
	if err != nil {
		s.T().Fatalf("can't create namespace: %s", err)
	}

	err = s.setupForward(context.Background())
	if err != nil {
		s.T().Fatalf("can't setupForward: %s", err)
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
	platformURL := fmt.Sprintf("http://%s.%s.svc:%d", kurunServiceName, kurunNamespace, kurunServicePort)

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

	_, err = hubClient.HubV1alpha1().EdgeIngresses("default").Create(s.ctx, &hubv1alpha1.EdgeIngress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ingress",
			Namespace: "default",
		},
		Spec: hubv1alpha1.EdgeIngressSpec{
			Service: hubv1alpha1.EdgeIngressService{
				Name: "hub-forward",
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

	traefikSvc, err := client.CoreV1().Services(s.traefikNamespace).Get(s.ctx, "traefik-hub", metav1.GetOptions{})
	s.Nil(err)

	pod, err := client.CoreV1().Pods("default").Create(s.ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ingress",
			Namespace: "default",
			Labels: map[string]string{
				"test-ingress": "test-ingress",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "test-ingress",
					Image: "curlimages/curl",
					Args: []string{
						"https://test.localhost:9901/test-ingress",
						"-k",
						"--resolv",
						"test.localhost:9901:" + traefikSvc.Spec.ClusterIP,
					},
					TTY: false,
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}, metav1.CreateOptions{})
	s.Nil(err)

	watcher, err = client.CoreV1().Pods(pod.Namespace).Watch(s.ctx, metav1.ListOptions{
		Watch: true,
	})
	s.Nil(err)

	obj := s.waitForCondition(watcher, 60*time.Second, waitCondition{
		desc: "pod to run",
		fn: func(obj runtime.Object) bool {
			item := obj.(*corev1.Pod)
			if item != nil && (item.Status.Phase == corev1.PodFailed || item.Status.Phase == corev1.PodSucceeded) {
				return true
			}
			return false
		},
	})
	pod = obj.(*corev1.Pod)
	s.Equal(corev1.PodSucceeded, pod.Status.Phase)

	req := client.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{})
	podLogs, err := req.Stream(s.ctx)
	s.Nil(err)
	defer func() { _ = podLogs.Close() }()

	buf := new(bytes.Buffer)
	_, err = io.Copy(buf, podLogs)
	s.Nil(err)
	str := buf.String()

	lines := strings.Split(str, "\n")
	s.Equal(lines[len(lines)-1], "It works !")
}
