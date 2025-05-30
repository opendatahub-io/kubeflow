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
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/opendatahub-io/kubeflow/components/odh-notebook-controller/controllers"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	dspav1 "github.com/opendatahub-io/data-science-pipelines-operator/api/v1"
	configv1 "github.com/openshift/api/config/v1"
	imagev1 "github.com/openshift/api/image/v1"
	oauthv1 "github.com/openshift/api/oauth/v1"
	routev1 "github.com/openshift/api/route/v1"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(nbv1.AddToScheme(scheme))
	utilruntime.Must(routev1.AddToScheme(scheme))
	utilruntime.Must(configv1.AddToScheme(scheme))
	utilruntime.Must(oauthv1.AddToScheme(scheme))
	utilruntime.Must(imagev1.AddToScheme(scheme))
	utilruntime.Must(dspav1.AddToScheme(scheme))

	//+kubebuilder:scaffold:scheme
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
	var metricsAddr, probeAddr, oauthProxyImage string
	var webhookPort int
	var enableLeaderElection, enableDebugLogging bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the probe endpoint binds to.")
	flag.StringVar(&oauthProxyImage, "oauth-proxy-image", controllers.OAuthProxyImage,
		"Image of the OAuth proxy sidecar container.")
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

	// Setup controller manager
	mgrConfig := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "odh-notebook-controller",
		WebhookServer: webhook.NewServer(webhook.Options{
			Port: webhookPort,
		}),
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
		Client:    mgr.GetClient(),
		Log:       ctrl.Log.WithName("controllers").WithName("Notebook"),
		Namespace: namespace,
		Scheme:    mgr.GetScheme(),
		Config:    mgr.GetConfig(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Notebook")
		os.Exit(1)
	}

	// Setup notebook mutating webhook
	hookServer := mgr.GetWebhookServer()
	notebookWebhook := &webhook.Admission{
		Handler: &controllers.NotebookWebhook{
			Log:       ctrl.Log.WithName("controllers").WithName("Notebook"),
			Client:    mgr.GetClient(),
			Config:    mgr.GetConfig(),
			Namespace: namespace,
			OAuthConfig: controllers.OAuthConfig{
				ProxyImage: oauthProxyImage,
			},
			Decoder: admission.NewDecoder(mgr.GetScheme()),
		},
	}
	hookServer.Register("/mutate-notebook-v1", notebookWebhook)

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
