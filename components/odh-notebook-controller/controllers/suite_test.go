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
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"go.opentelemetry.io/otel"

	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	dspav1 "github.com/opendatahub-io/data-science-pipelines-operator/api/v1"
	imagev1 "github.com/openshift/api/image/v1"
	oauthv1 "github.com/openshift/api/oauth/v1"
	routev1 "github.com/openshift/api/route/v1"
	v1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	otelShutdown   func(context.Context) error
	managerStopped = make(chan struct{})

	testNamespaces = []string{}

	// OAuth proxy image hardcoded placeholder value for tests
	oauthProxyImage string = "quay.io/odh-test-images/oauth-proxy-placeholder:latest"
)

const (
	timeout                            = time.Second * 10
	interval                           = time.Second * 2
	odhNotebookControllerTestNamespace = "redhat-ods-applications"
)

func TestAPIs(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	var err error
	ctx, cancel = context.WithCancel(context.Background())

	// Initialize logger
	opts := zap.Options{
		Development: true,
		TimeEncoder: zapcore.TimeEncoderOfLayout(time.RFC3339),
	}
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseFlagOptions(&opts)))

	// initialize tracer
	// Set up OpenTelemetry.
	otelShutdown, err = setupOTelSDK(ctx)
	Expect(err).ToNot(HaveOccurred())
	tracer = otel.Tracer("odh-notebook-controller/controllers/suite_test.go")

	// Initialize test environment:
	// https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/envtest#Environment.Start
	By("Bootstrapping test environment")
	envTest = &envtest.Environment{
		ControlPlane: envtest.ControlPlane{
			APIServer: &envtest.APIServer{},
		},
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
	if auditLogPath, found := os.LookupEnv("DEBUG_WRITE_AUDITLOG"); found {
		envTest.ControlPlane.APIServer.Configure().
			// https://kubernetes.io/docs/tasks/debug/debug-cluster/audit/#log-backend
			Append("audit-log-maxage", "1").
			Append("audit-log-maxbackup", "5").
			Append("audit-log-maxsize", "100"). // in MiB
			Append("audit-log-format", "json").
			Append("audit-policy-file", filepath.Join("..", "envtest-audit-policy.yaml")).
			Append("audit-log-path", auditLogPath)
		GinkgoT().Logf("DEBUG_WRITE_AUDITLOG is set, writing `envtest-audit-policy.yaml` auditlog to %s", auditLogPath)
	} else {
		GinkgoT().Logf("DEBUG_WRITE_AUDITLOG environment variable was not provided")
	}

	cfg, err = envTest.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	if kubeconfigPath, found := os.LookupEnv("DEBUG_WRITE_KUBECONFIG"); found {
		// https://github.com/rancher/fleet/blob/main/integrationtests/utils/kubeconfig.go
		user := envtest.User{Name: "MasterOfTheSystems", Groups: []string{"system:masters"}}
		authedUser, err := envTest.ControlPlane.AddUser(user, nil)
		Expect(err).NotTo(HaveOccurred())
		config, err := authedUser.KubeConfig()
		Expect(err).NotTo(HaveOccurred())
		err = os.WriteFile(kubeconfigPath, config, 0600)
		Expect(err).NotTo(HaveOccurred())
		GinkgoT().Logf("DEBUG_WRITE_KUBECONFIG is set, writing system:masters' Kubeconfig to %s", kubeconfigPath)
	} else {
		GinkgoT().Logf("DEBUG_WRITE_KUBECONFIG environment variable was not provided")
	}

	// Register API objects
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(nbv1.AddToScheme(scheme))
	utilruntime.Must(routev1.AddToScheme(scheme))
	utilruntime.Must(netv1.AddToScheme(scheme))
	utilruntime.Must(imagev1.AddToScheme(scheme))
	utilruntime.Must(dspav1.AddToScheme(scheme))
	utilruntime.Must(oauthv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme

	// Initialize Kubernetes client
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
		// Issue#429: waiting in tests only wastes time and prints pointless context-cancelled errors
		GracefulShutdownTimeout: ptr.To(time.Duration(0)),
		// pass in test context because why not
		BaseContext: func() context.Context {
			return ctx
		},
	})
	Expect(err).NotTo(HaveOccurred())

	// Setup notebook controller
	err = (&OpenshiftNotebookReconciler{
		Client:    mgr.GetClient(),
		Log:       ctrl.Log.WithName("controllers").WithName("odh-notebook-controller"),
		Scheme:    mgr.GetScheme(),
		Namespace: odhNotebookControllerTestNamespace,
		Config:    mgr.GetConfig(),
	}).SetupWithManager(mgr)
	Expect(err).ToNot(HaveOccurred())

	// Setup notebook mutating webhook
	hookServer := mgr.GetWebhookServer()
	notebookWebhook := &webhook.Admission{
		Handler: &NotebookWebhook{
			Log:       ctrl.Log.WithName("controllers").WithName("odh-notebook-webhook"),
			Client:    mgr.GetClient(),
			Config:    mgr.GetConfig(),
			Namespace: odhNotebookControllerTestNamespace,
			OAuthConfig: OAuthConfig{
				ProxyImage: oauthProxyImage,
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

	By("Shutting down OpenTelemetry")
	err = otelShutdown(context.Background())
	Expect(err).NotTo(HaveOccurred())
})
