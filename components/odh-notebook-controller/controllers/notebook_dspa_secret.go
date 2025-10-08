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
	"reflect"

	"github.com/go-logr/logr"
	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	dspav1 "github.com/opendatahub-io/data-science-pipelines-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	elyraRuntimeSecretName = "ds-pipeline-config"
	elyraRuntimeMountPath  = "/opt/app-root/runtimes"
	elyraRuntimeVolumeName = "elyra-dsp-details"
	dashboardInstanceName  = "default-dashboard"
	dspaInstanceName       = "dspa"
	managedByKey           = "opendatahub.io/managed-by"
	managedByValue         = "workbenches"
)

func getDashboardInstance(ctx context.Context, dynamicClient dynamic.Interface, log logr.Logger) (map[string]interface{}, error) {
	dashboardGVR := schema.GroupVersionResource{
		Group:    "components.platform.opendatahub.io",
		Version:  "v1alpha1",
		Resource: "dashboards",
	}

	obj, err := dynamicClient.Resource(dashboardGVR).Get(ctx, dashboardInstanceName, metav1.GetOptions{})
	if err != nil {
		if apierrs.IsNotFound(err) {
			// Skipping as Dashboard CR not found (optional CR)
			return nil, nil
		}
		// Catch "no matches for kind" when CRD is missing
		if meta.IsNoMatchError(err) {
			log.Info("Dashboard CRD is not installed in the cluster - skipping")
			return nil, nil
		}
		log.Error(err, "Failed to retrieve Dashboard CR", "name", dashboardInstanceName)
		return nil, err
	}

	return obj.UnstructuredContent(), nil
}

func getDSPAInstance(ctx context.Context, k8sClient client.Client, namespace string, log logr.Logger) (*dspav1.DataSciencePipelinesApplication, error) {
	dspa := &dspav1.DataSciencePipelinesApplication{}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: dspaInstanceName, Namespace: namespace}, dspa)
	if err != nil {
		// Skipping as DSPA CR not found in namespace (optional CR)
		if apierrs.IsNotFound(err) {
			return nil, nil
		}
		// Catch "no matches for kind" when CRD is missing
		if meta.IsNoMatchError(err) {
			log.Info("DSPA CRD is not installed in the cluster - skipping")
			return nil, nil
		}
		log.Error(err, "Failed to retrieve DSPA CR", "name", dspaInstanceName, "namespace", namespace)
		return nil, err
	}
	return dspa, nil
}

// extractElyraRuntimeConfigInfo retrieves the essential configuration details from dspa and dashboard CRs used for pipeline execution.
func extractElyraRuntimeConfigInfo(ctx context.Context, dashboardInstance map[string]interface{}, dspaInstance *dspav1.DataSciencePipelinesApplication, client client.Client, notebook *nbv1.Notebook, log logr.Logger) (map[string]interface{}, error) {

	// Extract API Endpoint from DSPA status
	apiEndpoint := dspaInstance.Status.Components.APIServer.ExternalUrl

	// Extract info from DSPA spec
	spec := dspaInstance.Spec
	objectStorage := spec.ObjectStorage
	externalStorage := objectStorage.ExternalStorage

	// Validate required fields
	host := externalStorage.Host
	if host == "" {
		return nil, fmt.Errorf("invalid DSPA CR: missing or invalid 'host'")
	}
	cosEndpoint := fmt.Sprintf("https://%s", host)

	cosBucket := externalStorage.Bucket
	if cosBucket == "" {
		return nil, fmt.Errorf("invalid DSPA CR: missing or invalid 'bucket'")
	}

	s3CredentialsSecret := externalStorage.S3CredentialSecret
	cosSecret := s3CredentialsSecret.SecretName
	usernameKey := s3CredentialsSecret.AccessKey
	passwordKey := s3CredentialsSecret.SecretKey

	// Fetch secret containing credentials
	dspaCOSSecret := &corev1.Secret{}
	err := client.Get(ctx, types.NamespacedName{Name: cosSecret, Namespace: notebook.Namespace}, dspaCOSSecret)
	if err != nil {
		log.Error(err, "Failed to get secret", "secretName", cosSecret)
		return nil, fmt.Errorf("failed to get secret '%s': %w", cosSecret, err)
	}

	usernameVal, ok := dspaCOSSecret.Data[usernameKey]
	if !ok {
		return nil, fmt.Errorf("missing key '%s' in secret '%s'", usernameKey, cosSecret)
	}
	passwordVal, ok := dspaCOSSecret.Data[passwordKey]
	if !ok {
		return nil, fmt.Errorf("missing key '%s' in secret '%s'", passwordKey, cosSecret)
	}

	// Construct Elyra-compatible config
	metadata := map[string]interface{}{
		"tags":          []string{},
		"display_name":  "Pipeline",
		"engine":        "Argo",
		"runtime_type":  "KUBEFLOW_PIPELINES",
		"auth_type":     "KUBERNETES_SERVICE_ACCOUNT_TOKEN",
		"cos_auth_type": "KUBERNETES_SECRET",
		"api_endpoint":  apiEndpoint,
		"cos_endpoint":  cosEndpoint,
		"cos_bucket":    cosBucket,
		"cos_username":  string(usernameVal),
		"cos_password":  string(passwordVal),
		"cos_secret":    cosSecret,
	}

	// Extract optional Dashboard public API URL if exists and append it on the metadata
	if len(dashboardInstance) == 0 {
		log.Info("Dashboard CR: not present or empty")
	} else if status, ok := dashboardInstance["status"].(map[string]interface{}); !ok {
		log.Info("Dashboard CR: 'status' field is missing or invalid")
	} else if dashboardURL, ok := status["url"].(string); !ok || dashboardURL == "" {
		log.Info("Dashboard CR: 'url' field is missing or empty")
	} else {
		publicAPIEndpoint := fmt.Sprintf("https://%s/external/elyra/%s", dashboardURL, notebook.Namespace)
		metadata["public_api_endpoint"] = publicAPIEndpoint
	}

	// Return the full runtime config
	return map[string]interface{}{
		"display_name": "Pipeline",
		"schema_name":  "kfp",
		"metadata":     metadata,
	}, nil
}

// NewElyraRuntimeConfigSecret defines and handles the creation, watch and update to the desired ElyraRuntimeConfig secret object
func (r *OpenshiftNotebookReconciler) NewElyraRuntimeConfigSecret(ctx context.Context, dynamicConfig *rest.Config, c client.Client, notebook *nbv1.Notebook, controllerNamespace string, log logr.Logger) error {
	dynamicClient, err := dynamic.NewForConfig(dynamicConfig)
	if err != nil {
		log.Error(err, "Failed to create dynamic client")
		return err
	}

	// Dashboard is optional: retrieve it, but continue even when it's absent.
	dashboardInstance, err := getDashboardInstance(ctx, dynamicClient, log)
	if err != nil {
		return err
	}
	// Dashboard CR not found (optional cr)
	if dashboardInstance == nil {
		dashboardInstance = map[string]interface{}{}
	}

	dspaInstance, err := getDSPAInstance(ctx, c, notebook.Namespace, log)
	if err != nil {
		return err
	}
	// Neither DSPA nor Dashboard CRs are present - skipping Elyra secret creation
	if dspaInstance == nil && len(dashboardInstance) == 0 {
		return nil
	}
	// Skipping Elyra secret creation; DSPA CR not found (optional cr but madatory for secret creation)
	if dspaInstance == nil {
		return nil
	}

	// Generate DSPA-based Elyra config
	dspData, err := extractElyraRuntimeConfigInfo(ctx, dashboardInstance, dspaInstance, c, notebook, log)
	if err != nil {
		log.Error(err, "Failed to extract Elyra runtime config info")
		return err
	}
	if dspData == nil {
		return nil
	}

	dspJSON, err := json.Marshal(dspData)
	if err != nil {
		log.Error(err, "Failed to marshal DSPA config to JSON")
		return err
	}

	desiredSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      elyraRuntimeSecretName,
			Namespace: notebook.Namespace,
			Labels:    map[string]string{managedByKey: managedByValue},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"odh_dsp.json": dspJSON,
		},
	}

	// Set owner reference only on the secrets created by nbc (avoid blockOwnerDeletion issue)
	desiredSecret.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion:         dspav1.GroupVersion.String(),
			Kind:               "DataSciencePipelinesApplication",
			Name:               dspaInstance.Name,
			UID:                dspaInstance.UID,
			Controller:         ptr.To(true),
			BlockOwnerDeletion: ptr.To(false),
		},
	}

	// Check if the secret exists
	existingSecret := &corev1.Secret{}
	err = c.Get(ctx, types.NamespacedName{Name: elyraRuntimeSecretName, Namespace: notebook.Namespace}, existingSecret)
	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("Creating Elyra runtime config secret", "secret", elyraRuntimeSecretName, "notebook", notebook.Name, "namespace", notebook.Namespace)
			if err := c.Create(ctx, desiredSecret); err != nil && !apierrs.IsAlreadyExists(err) {
				log.Error(err, "Failed to create Elyra runtime config secret")
				return err
			}
			log.Info("Secret created successfully")
			return nil
		}
		log.Error(err, "Failed to get Elyra runtime config secret")
		return err
	}

	// Update Secret
	requiresUpdate := !reflect.DeepEqual(existingSecret.Data, desiredSecret.Data) ||
		existingSecret.Labels[managedByKey] != managedByValue

	if requiresUpdate {
		log.Info("Updating existing Elyra runtime config secret", "name", elyraRuntimeSecretName)
		// Set correct label and data
		existingSecret.Labels = desiredSecret.Labels
		existingSecret.Data = desiredSecret.Data

		if err := c.Update(ctx, existingSecret); err != nil {
			log.Error(err, "Failed to update existing Elyra runtime config secret")
			return err
		}
	}

	return nil
}

// MountElyraRuntimeConfigSecret injects the Elyra runtime configuration Secret as a volume mount into the Notebook pod.
// This function is invoked by the webhook during Notebook mutation.
func MountElyraRuntimeConfigSecret(ctx context.Context, client client.Client, notebook *nbv1.Notebook, log logr.Logger) error {

	// Retrieve the Secret
	secret := &corev1.Secret{}
	err := client.Get(ctx, types.NamespacedName{Name: elyraRuntimeSecretName, Namespace: notebook.Namespace}, secret)
	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("Secret is not available yet", "Secret", elyraRuntimeSecretName)
			return nil
		}
		log.Error(err, "Error retrieving Secret", "Secret", elyraRuntimeSecretName)
		return err
	}

	// Check that it's our managed secret and has expected data
	if secret.Labels[managedByKey] != managedByValue {
		log.Info("Skipping mounting secret not managed by workbenches", "secret", elyraRuntimeSecretName)
		return nil
	}
	if len(secret.Data) == 0 {
		log.Info("Secret is empty, skipping volume mount", "Secret", elyraRuntimeSecretName)
		return nil
	}

	// Define the volume
	secretVolume := corev1.Volume{
		Name: elyraRuntimeVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: elyraRuntimeSecretName,
				Optional:   ptr.To(true),
			},
		},
	}

	// Append the volume if it doesn't already exist
	volumes := &notebook.Spec.Template.Spec.Volumes
	volumeExists := false
	for _, v := range *volumes {
		if v.Name == elyraRuntimeVolumeName {
			volumeExists = true
			break
		}
	}
	if !volumeExists {
		*volumes = append(*volumes, secretVolume)
		log.Info("Added elyra-dsp-details volume to notebook", "notebook", notebook.Name, "namespace", notebook.Namespace)
	} else {
		log.Info("elyra-dsp-details volume already exists, skipping", "notebook", notebook.Name, "namespace", notebook.Namespace)
	}

	log.Info("Injecting elyra-dsp-details volume into notebook", "notebook", notebook.Name, "namespace", notebook.Namespace)

	// Append volume mount to container (ensure no duplication by name or mountPath)
	for i, container := range notebook.Spec.Template.Spec.Containers {
		mountExists := false
		for _, vm := range container.VolumeMounts {
			if vm.Name == elyraRuntimeVolumeName || vm.MountPath == elyraRuntimeMountPath {
				mountExists = true
				break
			}
		}
		if !mountExists {
			notebook.Spec.Template.Spec.Containers[i].VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
				Name:      elyraRuntimeVolumeName,
				MountPath: elyraRuntimeMountPath,
			})
			log.Info("Added elyra-dsp-details volume mount", "container", container.Name, "mountPath", elyraRuntimeMountPath)
		} else {
			log.Info("elyra-dsp-details volume mount already exists, skipping", "container", container.Name, "mountPath", elyraRuntimeMountPath)
		}
	}

	return nil
}

// ReconcileElyraRuntimeConfigSecret handles the reconciliation of the Elyra runtime config secret.
// This function is invoked by the ODH Notebook Controller and is required for enabling Elyra functionality in notebooks.
func (r *OpenshiftNotebookReconciler) ReconcileElyraRuntimeConfigSecret(notebook *nbv1.Notebook, ctx context.Context) error {
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)
	return r.NewElyraRuntimeConfigSecret(ctx, r.Config, r.Client, notebook, r.Namespace, log)
}
