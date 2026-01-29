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

package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.

	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/opendatahub-io/kubeflow/components/odh-notebook-controller/controllers"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	dspav1 "github.com/opendatahub-io/data-science-pipelines-operator/api/v1"
	configv1 "github.com/openshift/api/config/v1"
	imagev1 "github.com/openshift/api/image/v1"
	oauthv1 "github.com/openshift/api/oauth/v1"
	routev1 "github.com/openshift/api/route/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(nbv1.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	utilruntime.Must(gatewayv1beta1.Install(scheme))
	utilruntime.Must(configv1.AddToScheme(scheme))
	utilruntime.Must(imagev1.AddToScheme(scheme))
	utilruntime.Must(dspav1.AddToScheme(scheme))
	utilruntime.Must(oauthv1.AddToScheme(scheme))
	utilruntime.Must(routev1.AddToScheme(scheme))

	//+kubebuilder:scaffold:scheme
}

// stripConfigMapData removes data payloads and managed fields from cached ConfigMaps.
// ConfigMap data is read via direct API calls (DisableFor) when needed.
// Note: SetManagedFields is called here because DefaultTransform does not apply
// to types that have a per-type Transform override in ByObject.
func stripConfigMapData(i interface{}) (interface{}, error) {
	if cm, ok := i.(*corev1.ConfigMap); ok {
		cm.Data = nil
		cm.BinaryData = nil
		cm.SetManagedFields(nil)
	}
	return i, nil
}

// stripSecretData removes data payloads and managed fields from cached Secrets.
// Secret data is read via direct API calls (DisableFor) when needed.
// Note: SetManagedFields is called here because DefaultTransform does not apply
// to types that have a per-type Transform override in ByObject.
func stripSecretData(i interface{}) (interface{}, error) {
	if s, ok := i.(*corev1.Secret); ok {
		s.Data = nil
		s.StringData = nil
		s.SetManagedFields(nil)
	}
	return i, nil
}

func getControllerNamespace() (string, error) {
	// Try to get the namespace from the service account secret
	// or from the K8S_NAMESPACE environment variable in local
	if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := string(data); len(ns) > 0 {
			return ns, nil
		}
	} else if ns := os.Getenv("K8S_NAMESPACE"); len(ns) > 0 {
		return ns, nil
	}

	return "", fmt.Errorf("unable to determine the namespace")
}

func main() {
	var metricsAddr, probeAddr, kubeRbacProxyImage, webhookCertDir string
	var webhookPort int
	var enableLeaderElection, enableDebugLogging bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the probe endpoint binds to.")
	flag.StringVar(&kubeRbacProxyImage, "kube-rbac-proxy-image", "",
		"Image of the kube-rbac-proxy sidecar container. (required)")
	// specified explicitly, since on macOS the default temporary directory often resolves to a path under /var/folders/...
	// this default path in /tmp/ is already hardcoded in the Makefile and manifests used for ktunnel deployment
	flag.StringVar(&webhookCertDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs",
		"Directory that contains the server key and certificate for the webhook server.")
	flag.IntVar(&webhookPort, "webhook-port", 8443,
		"Port that the webhook server serves at.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&enableDebugLogging, "debug-log", false, "Enable debug logging mode.")
	opts := zap.Options{
		Development: enableDebugLogging,
		TimeEncoder: zapcore.TimeEncoderOfLayout(time.RFC3339),
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	// Setup logger
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Validate required flags
	if kubeRbacProxyImage == "" {
		setupLog.Error(fmt.Errorf("missing required flag"), "kube-rbac-proxy-image flag must be set")
		flag.Usage()
		os.Exit(1)
	}

	// Setup controller manager
	mgrConfig := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "odh-notebook-controller",
		WebhookServer: webhook.NewServer(webhook.Options{
			Port:    webhookPort,
			CertDir: webhookCertDir,
		}),
		Cache: cache.Options{
			DefaultTransform: cache.TransformStripManagedFields(),
			ByObject: map[client.Object]cache.ByObject{
				// Strip data payloads from ConfigMaps and Secrets in the
				// informer cache. The controller watches ConfigMaps for
				// external resources (odh-config, trusted CA bundles) that
				// cannot be filtered by label, so we keep the informer but
				// minimize cached object size. Actual data reads go through
				// direct API calls via DisableFor.
				&corev1.ConfigMap{}: {Transform: stripConfigMapData},
				&corev1.Secret{}:    {Transform: stripSecretData},
			},
		},
		Client: client.Options{
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{
					&corev1.ConfigMap{},
					&corev1.Secret{},
				},
			},
		},
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrConfig)
	if err != nil {
		setupLog.Error(err, "Unable to start manager")
		os.Exit(1)
	}

	// Setup notebook controller
	// determine and set the controller namespace
	namespace, err := getControllerNamespace()
	if err != nil {
		setupLog.Error(err, "Error during determining controller / main namespace")
		os.Exit(1)
	}
	setupLog.Info("Controller is running in namespace", "namespace", namespace)
	if err = (&controllers.OpenshiftNotebookReconciler{
		Client:        mgr.GetClient(),
		Log:           ctrl.Log.WithName("controllers").WithName("odh-notebook-controller"),
		Namespace:     namespace,
		Scheme:        mgr.GetScheme(),
		Config:        mgr.GetConfig(),
		EventRecorder: mgr.GetEventRecorderFor("odh-notebook-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "odh-notebook-controller")
		os.Exit(1)
	}

	// Setup notebook mutating webhook
	dynamicClient, err := dynamic.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to create dynamic client")
		os.Exit(1)
	}
	hookServer := mgr.GetWebhookServer()
	notebookWebhook := &webhook.Admission{
		Handler: &controllers.NotebookWebhook{
			Log:           ctrl.Log.WithName("controllers").WithName("odh-notebook-webhook"),
			Client:        mgr.GetClient(),
			Config:        mgr.GetConfig(),
			DynamicClient: dynamicClient,
			Namespace:     namespace,
			KubeRbacProxyConfig: controllers.KubeRbacProxyConfig{
				ProxyImage: kubeRbacProxyImage,
			},
			Decoder: admission.NewDecoder(mgr.GetScheme()),
		},
	}
	hookServer.Register("/mutate-notebook-v1", notebookWebhook)

	// Setup notebook validating webhook
	notebookValidatingWebhook := &webhook.Admission{
		Handler: &controllers.NotebookValidatingWebhook{
			Log:     ctrl.Log.WithName("controllers").WithName("odh-notebook-validating-webhook"),
			Client:  mgr.GetClient(),
			Decoder: admission.NewDecoder(mgr.GetScheme()),
		},
	}
	hookServer.Register("/validate-notebook-v1", notebookValidatingWebhook)

	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
