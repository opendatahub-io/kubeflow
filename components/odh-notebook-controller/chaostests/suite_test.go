/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package chaostests provides an isolated envtest environment (no controller
// manager) for deterministic ChaosClient SDK fault-injection tests.
// By not starting a manager, we guarantee that only the manually-invoked
// Reconcile() call touches the API server, eliminating race conditions
// between the test reconciler and a background manager.
package chaostests

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	dspav1 "github.com/opendatahub-io/data-science-pipelines-operator/api/v1"
	imagev1 "github.com/openshift/api/image/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"go.uber.org/zap/zapcore"
)

var (
	cfg     *rest.Config
	cli     client.Client
	envTest *envtest.Environment
	ctx     context.Context
	cancel  context.CancelFunc
)

const (
	controllerNamespace = "redhat-ods-applications"
	testNotebookImage   = "registry.redhat.io/ubi9/ubi:latest"
)

func TestChaos(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Chaos Suite")
}

var _ = BeforeSuite(func() {
	var err error
	ctx, cancel = context.WithCancel(context.Background())

	opts := zap.Options{
		Development: true,
		TimeEncoder: zapcore.TimeEncoderOfLayout(time.RFC3339),
	}
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseFlagOptions(&opts)))

	By("Bootstrapping isolated envtest (no controller manager)")
	envTest = &envtest.Environment{
		CRDInstallOptions: envtest.CRDInstallOptions{
			Paths:              []string{filepath.Join("..", "config", "crd", "external")},
			ErrorIfPathMissing: true,
			CleanUpAfterUse:    false,
		},
	}

	cfg, err = envTest.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(nbv1.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	utilruntime.Must(gatewayv1beta1.Install(scheme))
	utilruntime.Must(netv1.AddToScheme(scheme))
	utilruntime.Must(imagev1.AddToScheme(scheme))
	utilruntime.Must(dspav1.AddToScheme(scheme))

	cli, err = client.New(cfg, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(cli).NotTo(BeNil())

	centralNs := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: controllerNamespace},
	}
	Expect(cli.Create(ctx, centralNs)).To(Succeed())
}, 60)

var _ = AfterSuite(func() {
	cancel()
	By("Tearing down the test environment")
	err := envTest.Stop()
	Expect(err).NotTo(HaveOccurred())
})
