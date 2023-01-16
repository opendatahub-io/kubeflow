package main

import (
	"flag"
	"time"

	"github.com/opendatahub-io/kubeflow/components/odh-notebook-controller/controllers"
	"go.uber.org/zap/zapcore"
	k8sclient "k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlruntime "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	webhook "sigs.k8s.io/controller-runtime/pkg/webhook"
)

var (
	webhookLog = ctrl.Log.WithName("webhook")
)

func main() {

	// Set variable based on odh-nbc
	var metricsAddr string
	var webhookPort int
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"The address the metric endpoint binds to.")
	flag.IntVar(&webhookPort, "webhook-port", 8443,
		"Port that the webhook server serves at.")

	// Setup logger
	opts := zap.Options{
		Development: true,
		TimeEncoder: zapcore.TimeEncoderOfLayout(time.RFC3339),
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	var oauthProxyImage string
	flag.StringVar(&oauthProxyImage, "oauth-proxy-image", controllers.OAuthProxyImage,
		"Image of the OAuth proxy sidecar container.")

	// Get k8s Config and Client
	config, err := ctrlruntime.GetConfig()
	if err != nil {
		webhookLog.Error(err, "Error creating the config object")
	}

	kc, err := k8sclient.NewForConfig(config)
	if err != nil {
		webhookLog.Error(err, "Failed to initialize Kubernetes client")
	}

	// Setup notebook mutating webhook
	hookServer := webhook.Server{
		Host: metricsAddr,
		Port: webhookPort,
	}
	notebookWebhook := &webhook.Admission{
		Handler: &controllers.NotebookWebhook{
			Client: kc,
			OAuthConfig: controllers.OAuthConfig{
				ProxyImage: oauthProxyImage,
			},
		},
	}
	hookServer.Register("/mutate-notebook-v1", notebookWebhook)

}
