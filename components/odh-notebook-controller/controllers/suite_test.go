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

package controllers

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	routev1 "github.com/openshift/api/route/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	//+kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

// +kubebuilder:docs-gen:collapse=Imports

var (
	cfg     *rest.Config
	cli     client.Client
	envTest *envtest.Environment

	ctx            context.Context
	cancel         context.CancelFunc
	managerStopped = make(chan struct{})

	testNamespaces = []string{}
)

const (
	timeout  = time.Second * 10
	interval = time.Second * 2
)

func TestAPIs(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	ctx, cancel = context.WithCancel(context.Background())

	// Initialize logger
	opts := zap.Options{
		Development: true,
		TimeEncoder: zapcore.TimeEncoderOfLayout(time.RFC3339),
	}
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseFlagOptions(&opts)))

	// Initiliaze test environment:
	// https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/envtest#Environment.Start
	By("Bootstrapping test environment")
	envTest = &envtest.Environment{
		CRDInstallOptions: envtest.CRDInstallOptions{
			Paths:              []string{filepath.Join("..", "config", "crd", "external")},
			ErrorIfPathMissing: true,
			CleanUpAfterUse:    false,
		},
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			Paths:                    []string{filepath.Join("..", "config", "webhook")},
			IgnoreErrorIfPathMissing: false,
		},
	}

	cfg, err := envTest.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	// Register API objects
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(nbv1.AddToScheme(scheme))
	utilruntime.Must(routev1.AddToScheme(scheme))
	utilruntime.Must(netv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme

	// Initiliaze Kubernetes client
	cli, err = client.New(cfg, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(cli).NotTo(BeNil())

	// Setup controller manager
	webhookInstallOptions := &envTest.WebhookInstallOptions
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:         scheme,
		LeaderElection: false,
		Metrics:        metricsserver.Options{BindAddress: "0"},
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    webhookInstallOptions.LocalServingHost,
			Port:    webhookInstallOptions.LocalServingPort,
			CertDir: webhookInstallOptions.LocalServingCertDir,
		}),
	})
	Expect(err).NotTo(HaveOccurred())

	// Setup notebook controller
	err = (&OpenshiftNotebookReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("notebook-controller"),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)
	Expect(err).ToNot(HaveOccurred())

	// Setup notebook mutating webhook
	hookServer := mgr.GetWebhookServer()
	notebookWebhook := &webhook.Admission{
		Handler: &NotebookWebhook{
			Log:    ctrl.Log.WithName("controllers").WithName("notebook-controller"),
			Client: mgr.GetClient(),
			Config: mgr.GetConfig(),
			OAuthConfig: OAuthConfig{
				ProxyImage: OAuthProxyImage,
			},
			Decoder: admission.NewDecoder(mgr.GetScheme()),
		},
	}
	hookServer.Register("/mutate-notebook-v1", notebookWebhook)

	// Start the manager
	go func() {
		defer GinkgoRecover()
		err = mgr.Start(ctx)
		managerStopped <- struct{}{}
		Expect(err).ToNot(HaveOccurred(), "Failed to run manager")
	}()

	// Wait for the webhook server to get ready
	dialer := &net.Dialer{Timeout: time.Second}
	addrPort := fmt.Sprintf("%s:%d", webhookInstallOptions.LocalServingHost, webhookInstallOptions.LocalServingPort)
	Eventually(func() error {
		conn, err := tls.DialWithDialer(dialer, "tcp", addrPort, &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			return err
		}
		conn.Close()
		return nil
	}).Should(Succeed())

	// Verify kubernetes client is working
	cli = mgr.GetClient()
	Expect(cli).ToNot(BeNil())

	for _, namespace := range testNamespaces {
		ns := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(cli.Create(ctx, ns)).To(Succeed())
	}

}, 60)

var _ = AfterSuite(func() {
	By("Stopping the manager")
	cancel()
	<-managerStopped // Issue#429: waiting to avoid shutdown errors being logged

	By("Tearing down the test environment")
	// TODO: Stop cert controller-runtime.certwatcher before manager
	err := envTest.Stop()
	Expect(err).NotTo(HaveOccurred())
})
