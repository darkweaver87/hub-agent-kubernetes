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
	"flag"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
	"k8s.io/apimachinery/pkg/runtime"
	watchv1 "k8s.io/apimachinery/pkg/watch"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
)

const (
	clusterNamePrefix = "test-hub"
)

type K8SSuite struct {
	suite.Suite
	testEnv     env.Environment
	testConf    *envconf.Config
	clusterName string
	ctx         context.Context
}

func (s *K8SSuite) SetupSuite() {
	if testing.Verbose() {
		flags := flag.FlagSet{}
		klog.InitFlags(&flags)
		err := flags.Set("v", "4")
		if err != nil {
			s.T().Fatal(err)
		}
	}

	s.testEnv = env.New()
	s.testConf = envconf.New()
	s.ctx = context.Background()
	s.clusterName = envconf.RandomName(fmt.Sprintf("%s-%s", clusterNamePrefix, strings.ToLower(s.T().Name())), 32)

	var err error

	s.ctx, err = envfuncs.CreateKindClusterWithConfig(s.clusterName, "kindest/node:v1.25.3", "resources/kind.yaml")(s.ctx, s.testConf)

	if err != nil {
		s.T().Fatal(err)
	}
}

func (s *K8SSuite) TearDownSuite() {
	var err error
	s.ctx, err = envfuncs.DestroyKindCluster(s.clusterName)(s.ctx, s.testConf)
	if err != nil {
		s.T().Fatal(err)
	}
}

func (s *K8SSuite) kubeClient() (*rest.Config, *clientset.Clientset) {
	config, err := clientcmd.BuildConfigFromFlags("", s.testConf.KubeconfigFile())
	if err != nil {
		s.T().Fatal(err)
	}

	client, err := clientset.NewForConfig(config)
	if err != nil {
		s.T().Fatal(err)
	}

	return config, client
}

type waitCondition struct {
	desc string
	fn   func(obj runtime.Object) bool
}

func (s *K8SSuite) waitForCondition(watcher watchv1.Interface, timeout time.Duration, cond waitCondition) runtime.Object {
	timer := time.NewTimer(timeout)
	var event watchv1.Event
	var ok bool

	for {
		select {
		case event, ok = <-watcher.ResultChan():
			if !ok {
				watcher = nil
			}
			if event.Type == watchv1.Modified {
				if cond.fn(event.Object) {
					watcher.Stop()
					return event.Object
				}
			}
		case <-timer.C:
			s.T().Fatalf("timeout waiting for %s !", cond.desc)
		}

		if watcher == nil {
			break
		}
	}

	return nil
}
