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
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	configv1 "github.com/openshift/api/config/v1"
	imagev1 "github.com/openshift/api/image/v1"

	"github.com/go-logr/logr"
	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	"github.com/kubeflow/kubeflow/components/notebook-controller/pkg/culler"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

//+kubebuilder:webhook:path=/mutate-notebook-v1,mutating=true,failurePolicy=fail,sideEffects=None,groups=kubeflow.org,resources=notebooks,verbs=create;update,versions=v1,name=notebooks.opendatahub.io,admissionReviewVersions=v1

// NotebookWebhook holds the webhook configuration.
type NotebookWebhook struct {
	Log                 logr.Logger
	Client              client.Client
	Config              *rest.Config
	Decoder             admission.Decoder
	KubeRbacProxyConfig KubeRbacProxyConfig
	// controller namespace
	Namespace string
}

var proxyEnvVars = make(map[string]string, 3)

// https://github.com/open-telemetry/opentelemetry-go/pull/1674#issuecomment-793558199
// https://github.com/open-telemetry/opentelemetry-go/issues/4291#issuecomment-1629797725
var getWebhookTracer func() trace.Tracer = sync.OnceValue(func() trace.Tracer {
	return otel.GetTracerProvider().Tracer("opendatahub.io/kubeflow/components/odh-notebook-controller/controllers/notebook_webhook.go")
})

const (
	ContainerNameKubeRbacProxy = "kube-rbac-proxy"

	KubeRbacProxyConfigVolumeName = "kube-rbac-proxy-config"
	KubeRbacProxyConfigMountPath  = "/etc/kube-rbac-proxy"
	KubeRbacProxyConfigFileName   = "config-file.yaml"
	KubeRbacProxyConfigFilePath   = KubeRbacProxyConfigMountPath + "/" + KubeRbacProxyConfigFileName

	KubeRbacProxyTLSCertsVolumeName = "kube-rbac-proxy-tls-certificates"
	KubeRbacProxyTLSCertsMountPath  = "/etc/tls/private"
	KubeRbacProxyTLSCertFileName    = "tls.crt"
	KubeRbacProxyTLSCertFilePath    = KubeRbacProxyTLSCertsMountPath + "/" + KubeRbacProxyTLSCertFileName
	KubeRbacProxyTLSKeyFileName     = "tls.key"
	KubeRbacProxyTLSKeyFilePath     = KubeRbacProxyTLSCertsMountPath + "/" + KubeRbacProxyTLSKeyFileName

	IMAGE_STREAM_NOT_FOUND_EVENT     = "imagestream-not-found"
	IMAGE_STREAM_TAG_NOT_FOUND_EVENT = "imagestream-tag-not-found"

	WorkbenchImageNamespaceAnnotation = "opendatahub.io/workbench-image-namespace"
	LastImageSelectionAnnotation      = "notebooks.opendatahub.io/last-image-selection"

	KubeRbacProxyTLSCertVolumeSecretSuffix = "-kube-rbac-proxy-tls"
)

// InjectReconciliationLock injects the kubeflow notebook controller culling
// stop annotation to explicitly start the notebook pod when the ODH notebook
// controller finishes the reconciliation. Otherwise, a race condition may happen
// while mounting the notebook service account pull secret into the pod.
//
// The ODH notebook controller will remove this annotation when the first
// reconciliation is completed (see RemoveReconciliationLock).
func InjectReconciliationLock(meta *metav1.ObjectMeta) error {
	if meta.Annotations != nil {
		meta.Annotations[culler.STOP_ANNOTATION] = AnnotationValueReconciliationLock
	} else {
		meta.SetAnnotations(map[string]string{
			culler.STOP_ANNOTATION: AnnotationValueReconciliationLock,
		})
	}
	return nil
}

// resourceConfig holds parsed and validated OAuth proxy resource configuration
type resourceConfig struct {
	cpuRequest    resource.Quantity
	memoryRequest resource.Quantity
	cpuLimit      resource.Quantity
	memoryLimit   resource.Quantity
}

// parseAndValidateAuthSidecarResources parses OAuth proxy resource annotations, applies defaults,
// and validates that requests <= limits. This centralizes all resource parsing and validation logic.
func parseAndValidateAuthSidecarResources(notebook *nbv1.Notebook) (*resourceConfig, error) {
	// Start with defaults
	config := &resourceConfig{
		cpuRequest:    resource.MustParse(DefaultAuthSidecarCPURequest),
		memoryRequest: resource.MustParse(DefaultAuthSidecarMemoryRequest),
		cpuLimit:      resource.MustParse(DefaultAuthSidecarCPULimit),
		memoryLimit:   resource.MustParse(DefaultAuthSidecarMemoryLimit),
	}

	annotations := notebook.GetAnnotations()
	if annotations == nil {
		return config, nil
	}

	// Parse and validate each annotation, updating config if valid
	resourceMap := map[string]*resource.Quantity{
		AnnotationAuthSidecarCPURequest:    &config.cpuRequest,
		AnnotationAuthSidecarMemoryRequest: &config.memoryRequest,
		AnnotationAuthSidecarCPULimit:      &config.cpuLimit,
		AnnotationAuthSidecarMemoryLimit:   &config.memoryLimit,
	}

	for key, quantity := range resourceMap {
		if valStr, ok := annotations[key]; ok && valStr != "" {
			val, err := resource.ParseQuantity(strings.TrimSpace(valStr))
			if err != nil {
				return nil, fmt.Errorf("invalid value for annotation '%s': '%s': %w", key, valStr, err)
			}
			if val.Sign() < 0 {
				return nil, fmt.Errorf("annotation '%s' value '%s' cannot be negative", key, valStr)
			}
			*quantity = val
		}
	}

	// Validate that requests <= limits (including defaults)
	if config.cpuRequest.Cmp(config.cpuLimit) > 0 {
		return nil, fmt.Errorf("CPU request (%s) cannot be greater than CPU limit (%s)",
			config.cpuRequest.String(), config.cpuLimit.String())
	}

	if config.memoryRequest.Cmp(config.memoryLimit) > 0 {
		return nil, fmt.Errorf("memory request (%s) cannot be greater than memory limit (%s)",
			config.memoryRequest.String(), config.memoryLimit.String())
	}

	return config, nil
}

// InjectKubeRbacProxy injects the kube-rbac-proxy proxy sidecar container in the Notebook
// spec
func InjectKubeRbacProxy(notebook *nbv1.Notebook, kubeRbacProxyConfig KubeRbacProxyConfig) error {
	// Parse and validate kube-rbac-proxy resource annotations
	config, err := parseAndValidateAuthSidecarResources(notebook)
	if err != nil {
		return fmt.Errorf("invalid kube-rbac-proxy resource configuration: %w", err)
	}

	// Convert config to ResourceRequirements
	resourceRequirements := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    config.cpuRequest,
			corev1.ResourceMemory: config.memoryRequest,
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    config.cpuLimit,
			corev1.ResourceMemory: config.memoryLimit,
		},
	}

	// https://pkg.go.dev/k8s.io/api/core/v1#Container
	proxyContainer := corev1.Container{
		Name:            ContainerNameKubeRbacProxy,
		Image:           kubeRbacProxyConfig.ProxyImage,
		ImagePullPolicy: corev1.PullAlways,
		Args: []string{
			"--secure-listen-address=0.0.0.0:" + strconv.Itoa(NotebookKubeRbacProxyPort),
			"--upstream=http://127.0.0.1:" + strconv.Itoa(NotebookPort) + "/",
			"--logtostderr=true",
			"--v=10", // TODO - TBD, this is too verbose
			"--proxy-endpoints-port=" + strconv.Itoa(NotebookKubeRbacProxyHealthPort),
			"--config-file=" + KubeRbacProxyConfigFilePath,
			"--tls-cert-file=" + KubeRbacProxyTLSCertFilePath,
			"--tls-private-key-file=" + KubeRbacProxyTLSKeyFilePath,
			"--auth-header-fields-enabled=true",
			"--auth-header-user-field-name=X-Auth-Request-User",
			"--auth-header-groups-field-name=X-Auth-Request-Groups",
		},
		Ports: []corev1.ContainerPort{{
			Name:          KubeRbacProxyServicePortName,
			ContainerPort: NotebookKubeRbacProxyPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   "/healthz",
					Port:   intstr.FromInt32(NotebookKubeRbacProxyHealthPort),
					Scheme: corev1.URISchemeHTTPS,
				},
			},
			InitialDelaySeconds: 30,
			TimeoutSeconds:      1,
			PeriodSeconds:       5,
			SuccessThreshold:    1,
			FailureThreshold:    3,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   "/healthz",
					Port:   intstr.FromInt32(NotebookKubeRbacProxyHealthPort),
					Scheme: corev1.URISchemeHTTPS,
				},
			},
			InitialDelaySeconds: 5,
			TimeoutSeconds:      1,
			PeriodSeconds:       5,
			SuccessThreshold:    1,
			FailureThreshold:    3,
		},
		Resources: resourceRequirements,
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      KubeRbacProxyConfigVolumeName,
				MountPath: KubeRbacProxyConfigMountPath,
			},
			{
				Name:      KubeRbacProxyTLSCertsVolumeName,
				MountPath: KubeRbacProxyTLSCertsMountPath,
			},
		},
	}

	// Add the sidecar container to the notebook
	notebookContainers := &notebook.Spec.Template.Spec.Containers
	proxyContainerExists := false
	for index, container := range *notebookContainers {
		if container.Name == ContainerNameKubeRbacProxy {
			(*notebookContainers)[index] = proxyContainer
			proxyContainerExists = true
			break
		}
	}
	if !proxyContainerExists {
		*notebookContainers = append(*notebookContainers, proxyContainer)
	}

	// Add the kube-rbac-proxy configuration volume:
	// https://pkg.go.dev/k8s.io/api/core/v1#Volume
	notebookVolumes := &notebook.Spec.Template.Spec.Volumes
	kubeRbacProxyVolumeExists := false
	kubeRbacProxyVolume := corev1.Volume{
		Name: KubeRbacProxyConfigVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: notebook.Name + KubeRbacProxyConfigSuffix,
				},
				DefaultMode: ptr.To[int32](420),
			},
		},
	}
	for index, volume := range *notebookVolumes {
		if volume.Name == KubeRbacProxyConfigVolumeName {
			(*notebookVolumes)[index] = kubeRbacProxyVolume
			kubeRbacProxyVolumeExists = true
			break
		}
	}
	if !kubeRbacProxyVolumeExists {
		*notebookVolumes = append(*notebookVolumes, kubeRbacProxyVolume)
	}

	// Add the TLS certificates volume for kube-rbac-proxy:
	// https://pkg.go.dev/k8s.io/api/core/v1#Volume
	tlsVolumeExists := false
	tlsVolume := corev1.Volume{
		Name: KubeRbacProxyTLSCertsVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  notebook.Name + KubeRbacProxyTLSCertVolumeSecretSuffix,
				DefaultMode: ptr.To[int32](420),
			},
		},
	}
	for index, volume := range *notebookVolumes {
		if volume.Name == KubeRbacProxyTLSCertsVolumeName {
			(*notebookVolumes)[index] = tlsVolume
			tlsVolumeExists = true
			break
		}
	}
	if !tlsVolumeExists {
		*notebookVolumes = append(*notebookVolumes, tlsVolume)
	}

	// Set a dedicated service account, do not use default
	notebook.Spec.Template.Spec.ServiceAccountName = notebook.Name
	return nil
}

func (w *NotebookWebhook) ClusterWideProxyIsEnabled() bool {
	proxyResourceList := &configv1.ProxyList{}
	err := w.Client.List(context.TODO(), proxyResourceList)
	if err != nil {
		return false
	}

	for _, proxy := range proxyResourceList.Items {
		if proxy.Name == "cluster" {
			if proxy.Status.HTTPProxy != "" && proxy.Status.HTTPSProxy != "" &&
				proxy.Status.NoProxy != "" {
				// Update Proxy Env variables map
				proxyEnvVars["HTTP_PROXY"] = proxy.Status.HTTPProxy
				proxyEnvVars["HTTPS_PROXY"] = proxy.Status.HTTPSProxy
				proxyEnvVars["NO_PROXY"] = proxy.Status.NoProxy
				return true
			}
		}
	}
	return false

}

// Handle transforms the Notebook objects.
func (w *NotebookWebhook) Handle(ctx context.Context, req admission.Request) admission.Response {

	// Initialize logger format
	log := w.Log.WithValues("notebook", req.Name, "namespace", req.Namespace)
	ctx = logr.NewContext(ctx, log)

	// Initialize OpenTelemetry tracer.
	// This is a noop in production code and is (so far) only used for testing.
	ctx, span := getWebhookTracer().Start(ctx, "handleFunc", trace.WithNewRoot(), trace.WithAttributes(
		attribute.String("notebook", req.Name),
		attribute.String("namespace", req.Namespace),
		attribute.String("operation", string(req.Operation)),
	))
	defer span.End()

	notebook := &nbv1.Notebook{}

	err := w.Decoder.Decode(req, notebook)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Inject the reconciliation lock only on new notebook creation
	if req.Operation == admissionv1.Create {
		err = InjectReconciliationLock(&notebook.ObjectMeta)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}

	}

	// Check Imagestream Info both on create and update operations
	if req.Operation == admissionv1.Create || req.Operation == admissionv1.Update {
		// Check Imagestream Info
		err = SetContainerImageFromRegistry(ctx, w.Client, notebook, log, w.Namespace)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}

		// Mount ca bundle on notebook creation and update
		err = CheckAndMountCACertBundle(ctx, w.Client, notebook, log)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}

		// Mount ConfigMap pipeline-runtime-images as runtime-images
		err = MountPipelineRuntimeImages(ctx, w.Client, notebook, log)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}

		// Mount Secret ds-pipeline-config
		if strings.ToLower(strings.TrimSpace(os.Getenv("SET_PIPELINE_SECRET"))) == "true" {
			err = MountElyraRuntimeConfigSecret(ctx, w.Client, notebook, log)
			if err != nil {
				log.Error(err, "Unable to mount Elyra runtime config volume")
				return admission.Errored(http.StatusInternalServerError, err)
			}
		}

		// Mount Feast config if the label is enabled
		if isFeastEnabled(notebook) {
			err = NewFeastConfig(ctx, w.Client, notebook)
			if err != nil {
				log.Info("Unable to mount Feast config volume", "error", err)
			}
		}
	}

	// Inject the kube-rbac-proxy if the annotation is present
	if KubeRbacProxyInjectionIsEnabled(notebook.ObjectMeta) {
		// Inject kube-rbac-proxy
		err = InjectKubeRbacProxy(notebook, w.KubeRbacProxyConfig)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}
	}

	// If cluster-wide-proxy is enabled, and INJECT_CLUSTER_PROXY_ENV is set to true,
	// inject the environment variables.
	// RHOAIENG-26099: This is a temporary solution to inject the proxy environment variables
	// until the notebooks controller supports injecting the proxy environment variables
	injectClusterProxy := false
	if raw, ok := os.LookupEnv("INJECT_CLUSTER_PROXY_ENV"); ok {
		if val, err := strconv.ParseBool(strings.TrimSpace(raw)); err == nil {
			injectClusterProxy = val
		} else {
			log.Info("Failed to parse INJECT_CLUSTER_PROXY_ENV as boolean, defaulting to false", "value", raw, "error", err)
		}
	}
	if injectClusterProxy && w.ClusterWideProxyIsEnabled() {
		err = InjectProxyConfigEnvVars(notebook)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}
	}

	// RHOAIENG-14552: Running notebook cannot be updated carelessly, or we may end up restarting the pod when
	// the webhook runs after e.g. the kube-rbac-proxy image has been updated
	mutatedNotebook, needsRestart, err := w.maybeRestartRunningNotebook(ctx, req, notebook)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	updatePendingAnnotation := "notebooks.opendatahub.io/update-pending"
	// Initialize annotations to avoid panic error when map nil
	if mutatedNotebook.Annotations == nil {
		mutatedNotebook.Annotations = make(map[string]string)
	}
	if needsRestart != NoPendingUpdates {
		mutatedNotebook.ObjectMeta.Annotations[updatePendingAnnotation] = needsRestart.Reason
	} else {
		delete(mutatedNotebook.ObjectMeta.Annotations, updatePendingAnnotation)
	}

	// Create the mutated notebook object
	marshaledNotebook, err := json.Marshal(mutatedNotebook)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledNotebook)
}

// maybeRestartRunningNotebook evaluates whether the updates being made cause notebook pod to restart.
// If the restart is caused by updates made by the mutating webhook itself to already existing notebook,
// and the notebook is not stopped, then these updates will be blocked until the notebook is stopped.
// Returns the mutated notebook, a flag whether there's a pending restart (to apply blocked updates), and an error value.
func (w *NotebookWebhook) maybeRestartRunningNotebook(ctx context.Context, req admission.Request, mutatedNotebook *nbv1.Notebook) (*nbv1.Notebook, *UpdatesPending, error) {
	var err error
	log := logr.FromContextOrDiscard(ctx)

	ctx, span := getWebhookTracer().Start(ctx, "maybeRestartRunningNotebook")
	defer span.End()

	// Notebook that was just created can be updated
	if req.Operation == admissionv1.Create {
		log.Info("Not blocking update, notebook is being newly created")
		return mutatedNotebook, NoPendingUpdates, nil
	}

	// Stopped notebooks are ok to update
	if metav1.HasAnnotation(mutatedNotebook.ObjectMeta, "kubeflow-resource-stopped") {
		log.Info("Not blocking update, notebook is (to be) stopped")
		return mutatedNotebook, NoPendingUpdates, nil
	}

	// Restarting notebooks are also ok to update
	if metav1.HasAnnotation(mutatedNotebook.ObjectMeta, "notebooks.opendatahub.io/notebook-restart") {
		log.Info("Not blocking update, notebook is (to be) restarted")
		return mutatedNotebook, NoPendingUpdates, nil
	}

	// fetch the updated Notebook CR that was sent to the Webhook
	updatedNotebook := &nbv1.Notebook{}
	err = w.Decoder.Decode(req, updatedNotebook)
	if err != nil {
		log.Error(err, "Failed to fetch the updated Notebook CR")
		return nil, NoPendingUpdates, err
	}

	// fetch the original Notebook CR
	oldNotebook := &nbv1.Notebook{}
	err = w.Decoder.DecodeRaw(req.OldObject, oldNotebook)
	if err != nil {
		log.Error(err, "Failed to fetch the original Notebook CR")
		return nil, NoPendingUpdates, err
	}

	// The externally issued update already causes a restart, so we will just let all changes proceed
	if !equality.Semantic.DeepEqual(oldNotebook.Spec.Template.Spec, updatedNotebook.Spec.Template.Spec) {
		log.Info("Not blocking update, the externally issued update already modifies pod template, causing a restart")
		return mutatedNotebook, NoPendingUpdates, nil
	}

	// Nothing about the Pod definition is actually changing and we can proceed
	if equality.Semantic.DeepEqual(oldNotebook.Spec.Template.Spec, mutatedNotebook.Spec.Template.Spec) {
		log.Info("Not blocking update, the pod template is not being modified at all")
		return mutatedNotebook, NoPendingUpdates, nil
	}

	// Now we know we have to block the update
	// Keep the old values and mark the Notebook as UpdatesPending
	diff := getStructDiff(ctx, mutatedNotebook.Spec.Template.Spec, updatedNotebook.Spec.Template.Spec)
	log.Info("Update blocked, notebook pod template would be changed by the webhook", "diff", diff)
	mutatedNotebook.Spec.Template.Spec = updatedNotebook.Spec.Template.Spec
	return mutatedNotebook, &UpdatesPending{Reason: diff}, nil
}

func InjectProxyConfigEnvVars(notebook *nbv1.Notebook) error {
	notebookContainers := &notebook.Spec.Template.Spec.Containers
	var imgContainer corev1.Container

	// Update Notebook Image container with env variables from central cluster proxy config
	for _, container := range *notebookContainers {
		// Update notebook image container with env Variables
		if container.Name == notebook.Name {
			var newVars []corev1.EnvVar
			imgContainer = container

			for key, val := range proxyEnvVars {
				keyExists := false
				for _, env := range imgContainer.Env {
					if key == env.Name {
						keyExists = true
						// Update if Proxy spec is updated
						if env.Value != val {
							env.Value = val
						}
					}
				}
				if !keyExists {
					newVars = append(newVars, corev1.EnvVar{Name: key, Value: val})
				}
			}

			// Update container only when required env variables are not present
			imgContainerExists := false
			if len(newVars) != 0 {
				imgContainer.Env = append(imgContainer.Env, newVars...)
			}

			// Update container with Proxy Env Changes
			for index, container := range *notebookContainers {
				if container.Name == notebook.Name {
					(*notebookContainers)[index] = imgContainer
					imgContainerExists = true
					break
				}
			}

			if !imgContainerExists {
				return fmt.Errorf("notebook image container not found %v", notebook.Name)
			}
			break
		}
	}
	return nil
}

// CheckAndMountCACertBundle checks if the odh-trusted-ca-bundle ConfigMap is present
func CheckAndMountCACertBundle(ctx context.Context, cli client.Client, notebook *nbv1.Notebook, log logr.Logger) error {

	workbenchConfigMapName := "workbench-trusted-ca-bundle"
	odhConfigMapName := "odh-trusted-ca-bundle"

	// if the odh-trusted-ca-bundle ConfigMap is not present, skip the process
	// as operator might have disabled the feature.
	odhConfigMap := &corev1.ConfigMap{}
	odhErr := cli.Get(ctx, client.ObjectKey{Namespace: notebook.Namespace, Name: odhConfigMapName}, odhConfigMap)
	if odhErr != nil {
		log.Info("odh-trusted-ca-bundle ConfigMap is not present, not starting mounting process.")
		return nil
	}

	// if the workbench-trusted-ca-bundle ConfigMap is not present,
	// controller was not successful in creating the ConfigMap, skip the process
	workbenchConfigMap := &corev1.ConfigMap{}
	err := cli.Get(ctx, client.ObjectKey{Namespace: notebook.Namespace, Name: workbenchConfigMapName}, workbenchConfigMap)
	if err != nil {
		log.Info("workbench-trusted-ca-bundle ConfigMap is not present, start creating it...")
		// create the ConfigMap if it does not exist
		workbenchConfigMap = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      workbenchConfigMapName,
				Namespace: notebook.Namespace,
				Labels:    map[string]string{"opendatahub.io/managed-by": "workbenches"},
			},
			Data: map[string]string{
				"ca-bundle.crt": odhConfigMap.Data["ca-bundle.crt"],
			},
		}
		err = cli.Create(ctx, workbenchConfigMap)
		if err != nil {
			log.Info("Failed to create workbench-trusted-ca-bundle ConfigMap")
			return nil
		}
		log.Info("Created workbench-trusted-ca-bundle ConfigMap")
	}

	cm := workbenchConfigMap
	if cm.Name == workbenchConfigMapName {
		// Inject the trusted-ca volume and environment variables
		log.Info("Injecting trusted-ca volume and environment variables")
		return InjectCertConfig(notebook, workbenchConfigMapName)
	}
	return nil
}

func InjectCertConfig(notebook *nbv1.Notebook, configMapName string) error {

	// ConfigMap details
	configVolumeName := "trusted-ca"
	configMapMountPath := "/etc/pki/tls/custom-certs/ca-bundle.crt"
	configMapMountKey := "ca-bundle.crt"
	configMapMountValue := "ca-bundle.crt"
	configEnvVars := map[string]string{
		"PIP_CERT":                  configMapMountPath,
		"REQUESTS_CA_BUNDLE":        configMapMountPath,
		"SSL_CERT_FILE":             configMapMountPath,
		"PIPELINES_SSL_SA_CERTS":    configMapMountPath,
		"KF_PIPELINES_SSL_SA_CERTS": configMapMountPath,
		"GIT_SSL_CAINFO":            configMapMountPath,
	}

	notebookContainers := &notebook.Spec.Template.Spec.Containers
	var imgContainer corev1.Container

	// Add trusted-ca volume
	notebookVolumes := &notebook.Spec.Template.Spec.Volumes
	certVolumeExists := false
	certVolume := corev1.Volume{
		Name: configVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: configMapName,
				},
				Optional: ptr.To(true),
				Items: []corev1.KeyToPath{{
					Key:  configMapMountKey,
					Path: configMapMountValue,
				},
				},
			},
		},
	}
	for index, volume := range *notebookVolumes {
		if volume.Name == configVolumeName {
			(*notebookVolumes)[index] = certVolume
			certVolumeExists = true
			break
		}
	}
	if !certVolumeExists {
		*notebookVolumes = append(*notebookVolumes, certVolume)
	}

	// Update Notebook Image container with env variables and Volume Mounts
	for _, container := range *notebookContainers {
		// Update notebook image container with env Variables
		if container.Name == notebook.Name {
			var newVars []corev1.EnvVar
			imgContainer = container

			for key, val := range configEnvVars {
				keyExists := false
				for _, env := range imgContainer.Env {
					if key == env.Name {
						keyExists = true
						// Update if env value is updated
						if env.Value != val {
							env.Value = val
						}
					}
				}
				if !keyExists {
					newVars = append(newVars, corev1.EnvVar{Name: key, Value: val})
				}
			}

			// Update container only when required env variables are not present
			imgContainerExists := false
			if len(newVars) != 0 {
				imgContainer.Env = append(imgContainer.Env, newVars...)
			}

			// Create Volume mount
			volumeMountExists := false
			containerVolMounts := &imgContainer.VolumeMounts
			trustedCAVolMount := corev1.VolumeMount{
				Name:      configVolumeName,
				ReadOnly:  true,
				MountPath: configMapMountPath,
				SubPath:   configMapMountValue,
			}

			for index, volumeMount := range *containerVolMounts {
				if volumeMount.Name == configVolumeName {
					(*containerVolMounts)[index] = trustedCAVolMount
					volumeMountExists = true
					break
				}
			}
			if !volumeMountExists {
				*containerVolMounts = append(*containerVolMounts, trustedCAVolMount)
			}

			// Update container with Env and Volume Mount Changes
			for index, container := range *notebookContainers {
				if container.Name == notebook.Name {
					(*notebookContainers)[index] = imgContainer
					imgContainerExists = true
					break
				}
			}

			if !imgContainerExists {
				return fmt.Errorf("notebook image container not found %v", notebook.Name)
			}
			break
		}
	}
	return nil
}

// SetContainerImageFromRegistry checks if there is an internal registry and takes the corresponding actions to set the container.image value.
// If an internal registry is detected, it uses the default values specified in the Notebook Custom Resource (CR).
// Otherwise, it checks the last-image-selection annotation to find the image stream and fetches the image from status.dockerImageReference,
// assigning it to the container.image value.
func SetContainerImageFromRegistry(ctx context.Context, cli client.Client, notebook *nbv1.Notebook, log logr.Logger, controllerNamespace string) error {
	span := trace.SpanFromContext(ctx)

	annotations := notebook.GetAnnotations()
	if annotations != nil {
		if imageSelection, exists := annotations[LastImageSelectionAnnotation]; exists {

			containerFound := false
			// Iterate over containers to find the one matching the notebook name
			for _, container := range notebook.Spec.Template.Spec.Containers {
				if container.Name == notebook.Name {
					containerFound = true

					// Check if the container.Image value has an internal registry, if so, will pick up this without extra checks.
					// This value is constructed on the initialization of the Notebook CR (usually by odh-dashboard).
					if strings.Contains(container.Image, "image-registry.openshift-image-registry.svc:5000") {
						log.Info("Internal registry found. Will pick up the default value from image field.")
						return nil
					}
					log.Info("No internal registry found, let's pick up image reference from relevant ImageStream 'status.tags[].tag.dockerImageReference'")

					// Split the imageSelection to imagestream and tag
					imageSelected := strings.Split(imageSelection, ":")
					if len(imageSelected) != 2 {
						return fmt.Errorf("invalid image selection format")
					}

					imagestreamFound := false
					imageTagFound := false
					imagestreamName := imageSelected[0]
					imgSelection := &imagev1.ImageStream{}

					// in user-created Notebook CRs, the annotation may be missing
					// Dashboard creates it with an empty value (null in TypeScript) when the controllerNamespace is intended
					//  https://github.com/opendatahub-io/odh-dashboard/blob/2692224c3157f00a6fe93a2ca5bd267e3ff964ca/frontend/src/api/k8s/notebooks.ts#L215-L216
					imageNamespace, nsExists := annotations[WorkbenchImageNamespaceAnnotation]
					if !nsExists || strings.TrimSpace(imageNamespace) == "" {
						log.Info("Unable to find the namespace annotation in the Notebook CR, or the value of it is an empty string. Will search in controller namespace",
							"annotationName", WorkbenchImageNamespaceAnnotation,
							"controllerNamespace", controllerNamespace)
						imageNamespace = controllerNamespace
					}

					// Search for the ImageStream in the specified namespace
					// As default when no namespace specified, the ImageStream is searched for in the controller namespace
					err := cli.Get(ctx, types.NamespacedName{Name: imagestreamName, Namespace: imageNamespace}, imgSelection)
					if apierrs.IsNotFound(err) {
						span.AddEvent(IMAGE_STREAM_NOT_FOUND_EVENT)
						log.Info("Unable to find the ImageStream in the expected namespace",
							"imagestream", imagestreamName,
							"imageNamespace", imageNamespace)
					} else if err != nil {
						log.Error(err, "Error getting ImageStream", "imagestream", imagestreamName, "imageNamespace", imageNamespace)
					} else {
						// ImageStream found
						imagestreamFound = true
						log.Info("ImageStream found", "imagestream", imagestreamName, "namespace", imageNamespace)
					}

					if imagestreamFound {
						// Check if the ImageStream has a status and tags
						if imgSelection.Status.Tags == nil {
							log.Error(nil, "ImageStream has no status or tags", "name", imagestreamName, "namespace", imageNamespace)
							span.AddEvent(IMAGE_STREAM_TAG_NOT_FOUND_EVENT)
							return fmt.Errorf("ImageStream has no status or tags")
						}
						// Iterate through the tags to find the one matching the imageSelected
						for _, tag := range imgSelection.Status.Tags {
							// Check if the tag name matches the imageSelected
							if tag.Tag == imageSelected[1] {
								// Check if the items are present
								if len(tag.Items) > 0 {
									// Sort items by creationTimestamp to get the most recent one
									sort.Slice(tag.Items, func(i, j int) bool {
										iTime := tag.Items[i].Created
										jTime := tag.Items[j].Created
										return iTime.Time.After(jTime.Time) //nolint:QF1008 // Reason: We are comparing metav1.Time // Lexicographical comparison of RFC3339 timestamps
									})
									// Get the most recent item
									imageHash := tag.Items[0].DockerImageReference
									// Update the container image
									notebook.Spec.Template.Spec.Containers[0].Image = imageHash
									// Update the JUPYTER_IMAGE environment variable with the image selection for example "jupyter-datascience-notebook:2023.2"
									for i, envVar := range container.Env {
										if envVar.Name == "JUPYTER_IMAGE" {
											container.Env[i].Value = imageSelection
											break
										}
									}
									imageTagFound = true
									break
								}
							}
						}
						if !imageTagFound {
							log.Error(nil, "ImageStream is present but does not contain a dockerImageReference for the specified tag")
							span.AddEvent(IMAGE_STREAM_TAG_NOT_FOUND_EVENT)
						}
					}
				}
			}
			if !containerFound {
				return fmt.Errorf("no container found matching the notebook name %s", notebook.Name)
			}
		}
	}
	return nil
}
