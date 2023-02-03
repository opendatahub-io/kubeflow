package main

import (
	"flag"
	"time"

	"github.com/opendatahub-io/kubeflow/components/odh-notebook-controller/controllers"
	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlruntime "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	signals "sigs.k8s.io/controller-runtime/pkg/manager/signals"
	webhook "sigs.k8s.io/controller-runtime/pkg/webhook"

	//placeholder, I do not know which shcemes are necessary yet
	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	configv1 "github.com/openshift/api/config/v1"
	routev1 "github.com/openshift/api/route/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

var (
	webhookLog = ctrl.Log.WithName("webhook")
	scheme     = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(nbv1.AddToScheme(scheme))
	utilruntime.Must(routev1.AddToScheme(scheme))
	utilruntime.Must(configv1.AddToScheme(scheme))

	//+kubebuilder:scaffold:scheme
}

func main() {

	// Set variable based on odh-nbc
	var metricsAddr string
	var certDir string
	var webhookPort int
	var oauthProxyImage string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"The address the metric endpoint binds to.")
	flag.IntVar(&webhookPort, "webhook-port", 8443,
		"Port that the webhook server serves at.")
	flag.StringVar(&certDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs",
		"The directory containing the cert files for the webhook.")
	flag.StringVar(&oauthProxyImage, "oauth-proxy-image", controllers.OAuthProxyImage,
		"Image of the OAuth proxy sidecar container.")

	// Setup logger
	opts := zap.Options{
		Development: true,
		TimeEncoder: zapcore.TimeEncoderOfLayout(time.RFC3339),
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Get k8s Config and Client
	config, err := ctrlruntime.GetConfig()
	if err != nil {
		webhookLog.Error(err, "Error creating the config object")
	}

	cliOpts := client.Options{
		Scheme: scheme,
	}

	// kc, err := k8sclient.NewForConfig(config)
	// if err != nil {
	// 	webhookLog.Error(err, "Failed to initialize Kubernetes client")
	// }
	cli, err := client.New(config, cliOpts)
	if err != nil {
		webhookLog.Error(err, "Failed to initialize Kubernetes client")
	}
	// Setup notebook mutating webhook
	hookServer := webhook.Server{
		Host:    "",
		Port:    webhookPort,
		CertDir: certDir,
	}
	notebookWebhook := &webhook.Admission{
		Handler: &controllers.NotebookWebhook{
			Client: cli,
			OAuthConfig: controllers.OAuthConfig{
				ProxyImage: oauthProxyImage,
			},
		},
	}
	hookServer.Register("/mutate-notebook-v1", notebookWebhook)
	err = hookServer.StartStandalone(signals.SetupSignalHandler(), scheme)
	if err != nil {
		webhookLog.Error(err, "Failed to start webhook")
	}

}
