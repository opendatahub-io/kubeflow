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
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.

	"go.uber.org/zap/zapcore"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"

	"github.com/opendatahub-io/kubeflow/components/odh-notebook-controller/controllers"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	dspav1 "github.com/opendatahub-io/data-science-pipelines-operator/api/v1"
	platformtls "github.com/opendatahub-io/odh-platform-utilities/framework/tls"
	configv1 "github.com/openshift/api/config/v1"
	imagev1 "github.com/openshift/api/image/v1"
	oauthv1 "github.com/openshift/api/oauth/v1"
	routev1 "github.com/openshift/api/route/v1"
	tlspkg "github.com/openshift/controller-runtime-common/pkg/tls"
	libgocrypto "github.com/openshift/library-go/pkg/crypto"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	k8serr "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

// intermediateCiphers is the Mozilla Intermediate cipher set, used as the
// hardened default on non-OpenShift clusters where the APIServer TLS profile
// is not available.
var intermediateCiphers = []uint16{
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
}

// nextProtos enables HTTP/2 with HTTP/1.1 fallback on the TLS servers.
var nextProtos = []string{"h2", "http/1.1"}

// runtimeCipherIDs is the set of cipher suite IDs this Go runtime can
// negotiate that an OpenShift TLS profile can name. RC4 suites are
// implemented by this runtime but appear in no profile (RFC 7465 prohibits
// RC4), so they are never included.
var runtimeCipherIDs = func() map[uint16]bool {
	ids := make(map[uint16]bool)
	for _, cs := range tls.CipherSuites() {
		ids[cs.ID] = true
	}
	profileNames := make(map[string]bool)
	for _, spec := range configv1.TLSProfiles {
		for _, name := range libgocrypto.OpenSSLToIANACipherSuites(spec.Ciphers) {
			profileNames[name] = true
		}
	}
	for _, cs := range tls.InsecureCipherSuites() {
		if profileNames[cs.Name] {
			ids[cs.ID] = true
		}
	}
	return ids
}()

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

// tlsBootstrap is the outcome of the startup TLS policy bootstrap: the TLS
// options for the webhook and metrics servers plus the state the security
// profile watcher needs.
type tlsBootstrap struct {
	opts                  []func(*tls.Config)
	profile               configv1.TLSProfileSpec
	hasOpenShiftConfigAPI bool
	adherence             configv1.TLSAdherencePolicy
	adherenceFetched      bool
	adherencePolicyRead   bool
}

// bootstrapTLS reads the cluster TLS profile and adherence policy and builds
// the TLS options for the webhook and metrics servers. Denied (401/403) and
// other non-transient read failures are returned as errors so the controller
// does not start with an unknown TLS posture.
func bootstrapTLS(ctx context.Context, bootstrapClient client.Client) (*tlsBootstrap, error) {
	boot := &tlsBootstrap{}
	profile, err := tlspkg.FetchAPIServerTLSProfile(ctx, bootstrapClient)
	if err != nil {
		switch {
		case apimeta.IsNoMatchError(err):
			setupLog.Info("TLS profile not available, using hardened defaults (non-OpenShift cluster)")
		case k8serr.IsNotFound(err):
			setupLog.Info("APIServer resource not found, using hardened defaults")
		case isTransientAPIError(err):
			boot.hasOpenShiftConfigAPI = true // register watcher so it self-heals when the API recovers
			setupLog.Info("Transient API error reading TLS profile, using hardened defaults", "error", err)
		default:
			return nil, fmt.Errorf("reading APIServer TLS profile: %w", err)
		}
		boot.opts = append(boot.opts, func(c *tls.Config) {
			c.MinVersion = tls.VersionTLS12
			c.CipherSuites = intermediateCiphers
			c.NextProtos = nextProtos
		})
	} else {
		boot.hasOpenShiftConfigAPI = true
		boot.profile = profile
	}

	// Fetch the TLS adherence policy (OpenShift only, requires the APIServer CRD)
	if boot.hasOpenShiftConfigAPI {
		if err := fetchTLSAdherence(ctx, bootstrapClient, boot); err != nil {
			return nil, err
		}
	}
	if err == nil {
		validated, err := validateProfileSpec(profile, boot.adherence, boot.adherencePolicyRead)
		if err != nil {
			return nil, fmt.Errorf("validating TLS profile: %w", err)
		}
		tlsConfigFn, unsupportedCiphers := tlspkg.NewTLSConfigFromProfile(validated)
		if len(unsupportedCiphers) > 0 {
			setupLog.Info("some ciphers from TLS profile are not supported by Go", "unsupported", unsupportedCiphers)
		}
		boot.opts = append(boot.opts, tlsConfigFn, func(c *tls.Config) {
			c.NextProtos = nextProtos
		})
	}
	return boot, nil
}

// fetchTLSAdherence reads the TLS adherence policy. Denied (401/403) and other
// non-transient reads are errors; NoMatchError and NotFound leave the policy
// unset; transient errors keep the watcher registered so it self-heals when
// the API recovers (OCP ref: cluster-machine-approver).
func fetchTLSAdherence(ctx context.Context, bootstrapClient client.Client, boot *tlsBootstrap) error {
	adherence, err := tlspkg.FetchAPIServerTLSAdherencePolicy(ctx, bootstrapClient)
	if err != nil {
		switch {
		case apimeta.IsNoMatchError(err):
			setupLog.Info("TLS adherence policy not available (non-OpenShift or pre-4.22 cluster)")
		case k8serr.IsNotFound(err):
			setupLog.Info("APIServer resource not found for TLS adherence policy, leaving it unset")
		case isTransientAPIError(err):
			boot.adherenceFetched = true
			setupLog.Info("Transient API error fetching TLS adherence policy, watcher will retry", "error", err)
		default:
			return fmt.Errorf("reading APIServer TLS adherence policy: %w", err)
		}
		return nil
	}
	boot.adherenceFetched = true
	boot.adherence = adherence
	boot.adherencePolicyRead = true
	return nil
}

// isTransientAPIError reports whether the API error may resolve on retry.
func isTransientAPIError(err error) bool {
	return k8serr.IsServiceUnavailable(err) ||
		k8serr.IsTimeout(err) ||
		k8serr.IsServerTimeout(err) ||
		k8serr.IsTooManyRequests(err) ||
		errors.Is(err, context.DeadlineExceeded)
}

// validateProfileSpec rejects TLS profiles that cannot be honored without a
// silent substitution. Empty cipher lists below TLS 1.3 are rejected under
// Strict or unknown adherence; all-unusable lists are always rejected.
func validateProfileSpec(profile configv1.TLSProfileSpec, policy configv1.TLSAdherencePolicy, policyRead bool) (configv1.TLSProfileSpec, error) {
	if profile.MinTLSVersion == "" {
		return profile, fmt.Errorf("TLS profile minimum version must be set")
	}
	minVersion, err := libgocrypto.TLSVersion(string(profile.MinTLSVersion))
	if err != nil {
		return profile, fmt.Errorf("invalid minimum TLS version %q: %w", profile.MinTLSVersion, err)
	}
	// Go 1.22+ defaults the server minimum to TLS 1.2 only when tls.Config.MinVersion
	// is unset; the explicit minimum set here needs no GODEBUG.
	if minVersion >= tls.VersionTLS13 {
		return profile, nil
	}
	if len(profile.Ciphers) == 0 {
		if !policyRead {
			return profile, fmt.Errorf("TLS profile has no cipher suites for minimum TLS version below 1.3 because adherence policy could not be read")
		}
		if platformtls.ShouldHonorClusterTLSProfile(policy) {
			return profile, fmt.Errorf("TLS profile has no cipher suites for minimum TLS version below 1.3 under adherence policy %q", policy)
		}
		return profile, nil
	}

	usable := make([]string, 0, len(profile.Ciphers))
	var dropped []string
	for _, cipher := range profile.Ciphers {
		if cipherUsableForTLS12(cipher) {
			usable = append(usable, cipher)
		} else {
			dropped = append(dropped, cipher)
		}
	}
	if len(usable) == 0 {
		return profile, fmt.Errorf("no cipher suite usable with minimum TLS version %q: requested ciphers %v", profile.MinTLSVersion, profile.Ciphers)
	}
	if len(dropped) > 0 {
		setupLog.Info("dropping TLS profile ciphers not usable for TLS <= 1.2 handshakes", "dropped", dropped)
		profile.Ciphers = usable
	}
	return profile, nil
}

// cipherUsableForTLS12 reports whether name, in IANA or OpenSSL form, resolves
// to a cipher suite ID that a TLS <= 1.2 handshake can negotiate. TLS 1.3-only
// suites (IDs 0x1300-0x13ff) and IDs the Go runtime does not implement never
// qualify.
func cipherUsableForTLS12(name string) bool {
	id, err := libgocrypto.CipherSuite(name)
	if err != nil {
		iana := libgocrypto.OpenSSLToIANACipherSuites([]string{name})
		if len(iana) != 1 {
			return false
		}
		if id, err = libgocrypto.CipherSuite(iana[0]); err != nil {
			return false
		}
	}
	return (id < 0x1300 || id > 0x13ff) && runtimeCipherIDs[id]
}

func main() {
	var metricsAddr, probeAddr, kubeRbacProxyImage, webhookCertDir string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookPort int
	var enableLeaderElection, enableDebugLogging bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"The address the metric endpoint binds to.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "", "Directory containing TLS cert/key for the metrics endpoint.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "Filename of the metrics TLS certificate.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "Filename of the metrics TLS private key.")
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

	// Fetch the cluster TLS security profile and adherence policy for the
	// webhook and metrics servers (OpenShift only)
	restCfg := ctrl.GetConfigOrDie()
	// Bound the bootstrap client's requests, including REST mapper discovery,
	// which the bootstrap context does not cover.
	bootstrapCfg := rest.CopyConfig(restCfg)
	bootstrapCfg.Timeout = 10 * time.Second
	bootstrapClient, err := client.New(bootstrapCfg, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to create bootstrap client for TLS profile")
		os.Exit(1)
	}

	bootstrapCtx, bootstrapCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer bootstrapCancel()
	tlsBoot, err := bootstrapTLS(bootstrapCtx, bootstrapClient)
	if err != nil {
		setupLog.Error(err, "refusing to start with unknown TLS posture")
		os.Exit(1)
	}

	// Setup controller manager
	metricsOpts := metricsserver.Options{
		BindAddress:    metricsAddr,
		SecureServing:  true,
		FilterProvider: filters.WithAuthenticationAndAuthorization,
		TLSOpts:        tlsBoot.opts,
	}
	if metricsCertPath != "" {
		metricsOpts.CertDir = metricsCertPath
		metricsOpts.CertName = metricsCertName
		metricsOpts.KeyName = metricsCertKey
	}
	cacheOptions, clientOptions := controllers.ManagerCacheOptions()
	mgrConfig := ctrl.Options{
		Cache:                  cacheOptions,
		Client:                 clientOptions,
		Scheme:                 scheme,
		Metrics:                metricsOpts,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "odh-notebook-controller",
		WebhookServer: webhook.NewServer(webhook.Options{
			Port:    webhookPort,
			CertDir: webhookCertDir,
			TLSOpts: tlsBoot.opts,
		}),
	}

	mgr, err := ctrl.NewManager(restCfg, mgrConfig)
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

	// Read MLflow configuration from environment variables (set once at startup)
	mlflowEnabled := strings.ToLower(strings.TrimSpace(os.Getenv("MLFLOW_ENABLED"))) == "true"
	gatewayURL := strings.TrimSpace(os.Getenv("GATEWAY_URL"))
	setupLog.Info("MLflow configuration", "mlflowEnabled", mlflowEnabled)

	if err = (&controllers.OpenshiftNotebookReconciler{
		Client:        mgr.GetClient(),
		Log:           ctrl.Log.WithName("controllers").WithName("odh-notebook-controller"),
		Namespace:     namespace,
		Scheme:        mgr.GetScheme(),
		Config:        mgr.GetConfig(),
		EventRecorder: mgr.GetEventRecorderFor("odh-notebook-controller"),
		MLflowEnabled: mlflowEnabled,
		GatewayURL:    gatewayURL,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "odh-notebook-controller")
		os.Exit(1)
	}

	// Setup notebook mutating webhook
	hookServer := mgr.GetWebhookServer()
	notebookWebhook := &webhook.Admission{
		Handler: &controllers.NotebookWebhook{
			Log:       ctrl.Log.WithName("controllers").WithName("odh-notebook-webhook"),
			Client:    mgr.GetClient(),
			APIReader: mgr.GetAPIReader(),
			Config:    mgr.GetConfig(),
			Namespace: namespace,
			KubeRbacProxyConfig: controllers.KubeRbacProxyConfig{
				ProxyImage: kubeRbacProxyImage,
			},
			Decoder:       admission.NewDecoder(mgr.GetScheme()),
			MLflowEnabled: mlflowEnabled,
			GatewayURL:    gatewayURL,
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

	// Register SecurityProfileWatcher on OpenShift: cancel context on TLS profile change so pod restarts
	ctx, cancel := context.WithCancel(ctrl.SetupSignalHandler())
	defer cancel()
	if tlsBoot.hasOpenShiftConfigAPI {
		watcher := &tlspkg.SecurityProfileWatcher{
			Client:                mgr.GetClient(),
			InitialTLSProfileSpec: tlsBoot.profile,
			OnProfileChange: func(_ context.Context, _, _ configv1.TLSProfileSpec) {
				setupLog.Info("TLS profile changed, initiating graceful shutdown to reload")
				cancel()
			},
		}
		if tlsBoot.adherenceFetched {
			watcher.InitialTLSAdherencePolicy = tlsBoot.adherence
		}
		watcher.OnAdherencePolicyChange = func(_ context.Context, _, _ configv1.TLSAdherencePolicy) {
			setupLog.Info("TLS adherence policy changed, initiating shutdown to reload")
			cancel()
		}
		if err := watcher.SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to register TLS security profile watcher")
			os.Exit(1)
		}
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
