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

	"github.com/go-logr/logr"
	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TODO: Fix the name from ds-pipeline-config-by-nbcxxx to ds-pipeline-config once everything is in place
const (
	elyraRuntimeSecretName = "ds-pipeline-config-by-nbc"
	//TODO: Remove the last / once everything is ready
	elyraRuntimeMountPath  = "/opt/app-root/runtimes/"
	elyraRuntimeVolumeName = "elyra-dsp-details-by-nbc"
)

func extractElyraRuntimeConfigInfo(ctx context.Context, dynamicClient dynamic.Interface, client client.Client, notebook *nbv1.Notebook, log logr.Logger) (map[string]interface{}, error) {
	// Define GVRs
	dspa := schema.GroupVersionResource{
		Group:    "datasciencepipelinesapplications.opendatahub.io",
		Version:  "v1",
		Resource: "datasciencepipelinesapplications",
	}
	dashboard := schema.GroupVersionResource{
		Group:    "components.platform.opendatahub.io",
		Version:  "v1alpha1",
		Resource: "dashboards",
	}

	// Fetch DSPA CR
	dspaObj, err := dynamicClient.Resource(dspa).Namespace(notebook.Namespace).Get(ctx, "dspa", metav1.GetOptions{})
	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("DSPA CR not found; skipping Elyra config generation")
			return nil, nil
		}
		log.Error(err, "Failed to get DSPA CR")
		return nil, fmt.Errorf("error retrieving DSPA CR: %w", err)
	}

	// Fetch Dashboard CR
	dashboardObj, err := dynamicClient.Resource(dashboard).Get(ctx, "default-dashboard", metav1.GetOptions{})
	if err != nil {
		log.Error(err, "Failed to get Dashboard CR")
		return nil, fmt.Errorf("error retrieving Dashboard CR: %w", err)
	}

	// Extract dashboard URL
	status, ok := dashboardObj.Object["status"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid Dashboard CR: missing 'status'")
	}
	dashboardURL, ok := status["url"].(string)
	if !ok || dashboardURL == "" {
		return nil, fmt.Errorf("invalid Dashboard CR: missing or empty 'url'")
	}
	publicAPIEndpoint := fmt.Sprintf("https://%s/experiments/%s/", dashboardURL, notebook.Namespace)

	// Extract info from DSPA spec
	spec, ok := dspaObj.Object["spec"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid DSPA CR: missing 'spec'")
	}
	objectStorage, ok := spec["objectStorage"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid DSPA CR: missing 'objectStorage'")
	}
	externalStorage, ok := objectStorage["externalStorage"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid DSPA CR: missing 'externalStorage'")
	}

	// Extract host
	host, ok := externalStorage["host"].(string)
	if !ok || host == "" {
		return nil, fmt.Errorf("invalid DSPA CR: missing or invalid 'host'")
	}
	cosEndpoint := fmt.Sprintf("https://%s", host)

	// Extract bucket
	cosBucket, ok := externalStorage["bucket"].(string)
	if !ok || cosBucket == "" {
		return nil, fmt.Errorf("invalid DSPA CR: missing or invalid 'bucket'")
	}

	// Extract S3 credentials
	s3CredentialsSecret, ok := externalStorage["s3CredentialsSecret"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid DSPA CR: missing 's3CredentialsSecret'")
	}
	cosSecret, ok := s3CredentialsSecret["secretName"].(string)
	usernameKey, ok1 := s3CredentialsSecret["accessKey"].(string)
	passwordKey, ok2 := s3CredentialsSecret["secretKey"].(string)
	if !ok || !ok1 || !ok2 {
		return nil, fmt.Errorf("invalid DSPA CR: incomplete 's3CredentialsSecret'")
	}

	// Fetch secret for credentials
	dashboardSecret := &corev1.Secret{}
	err = client.Get(ctx, types.NamespacedName{Name: cosSecret, Namespace: notebook.Namespace}, dashboardSecret)
	if err != nil {
		log.Error(err, "Failed to get secret", "secretName", cosSecret)
		return nil, fmt.Errorf("failed to get secret '%s': %w", cosSecret, err)
	}

	// Extract values from the secret
	usernameVal, ok := dashboardSecret.Data[usernameKey]
	if !ok {
		return nil, fmt.Errorf("missing key '%s' in secret '%s'", usernameKey, cosSecret)
	}
	passwordVal, ok := dashboardSecret.Data[passwordKey]
	if !ok {
		return nil, fmt.Errorf("missing key '%s' in secret '%s'", passwordKey, cosSecret)
	}

	cosUsername := string(usernameVal)
	cosPassword := string(passwordVal)

	// Extract API Endpoint from DSPA status
	status, ok = dspaObj.Object["status"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid DSPA CR: missing 'status'")
	}
	components, ok := status["components"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid DSPA CR: missing 'components' in status")
	}
	apiServer, ok := components["apiServer"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid DSPA CR: missing 'apiServer' in components")
	}
	apiEndpoint, ok := apiServer["externalUrl"].(string)
	if !ok || apiEndpoint == "" {
		return nil, fmt.Errorf("invalid DSPA CR: missing or invalid 'externalUrl' for apiServer")
	}

	// Construct and return the DSPA config
	return map[string]interface{}{
		"display_name": "Data Science Pipeline",
		"schema_name":  "kfp",
		"metadata": map[string]interface{}{
			"tags":                []string{},
			"display_name":        "Data Science Pipeline",
			"engine":              "Argo",
			"runtime_type":        "KUBEFLOW_PIPELINES",
			"auth_type":           "KUBERNETES_SERVICE_ACCOUNT_TOKEN",
			"cos_auth_type":       "KUBERNETES_SECRET",
			"public_api_endpoint": publicAPIEndpoint,
			"api_endpoint":        apiEndpoint,
			"cos_endpoint":        cosEndpoint,
			"cos_bucket":          cosBucket,
			"cos_username":        cosUsername,
			"cos_password":        cosPassword,
			"cos_secret":          cosSecret,
			//TODO: Remove this once everything is in place
			"debug": "true",
		},
	}, nil
}

// NewElyraRuntimeConfigSecret defines the desired ElyraRuntimeConfig secret object
func (r *OpenshiftNotebookReconciler) NewElyraRuntimeConfigSecret(ctx context.Context, dynamicConfig *rest.Config, client client.Client, notebook *nbv1.Notebook, controllerNamespace string, log logr.Logger) *corev1.Secret {

	// Create a dynamic client to be able to fetch dspa and dasboard CRs
	dynamicClient, err := dynamic.NewForConfig(dynamicConfig)
	if err != nil {
		log.Error(err, "Failed to create dynamic client")
		return nil
	}

	dspData, err := extractElyraRuntimeConfigInfo(ctx, dynamicClient, client, notebook, log)
	if err != nil {
		// In case there is some issue on info fetching return error Info on the logs
		log.Error(err, "Failed to extract Elyra runtime config info")
		return nil
	}
	if dspData == nil {
		// In case No DSPA present in namespace skipping Elyra secret creation as DSPA is not present
		//log.Info("No DSPA present in namespace; skipping Elyra secret creation")
		return nil
	}

	// Marshal the map to JSON
	dspJSON, err := json.Marshal(dspData)
	if err != nil {
		log.Error(err, "Failed to marshal DSPA config to JSON")
		return nil
	}

	// Create a Kubernetes secret to store the Elyra runtime config data
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      elyraRuntimeSecretName,
			Namespace: notebook.Namespace,
			Labels:    map[string]string{"opendatahub.io/managed-by": "workbenches"},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"odh_dsp.json": dspJSON,
		},
	}
}

func MountElyraRuntimeConfigSecret(ctx context.Context, client client.Client, notebook *nbv1.Notebook, log logr.Logger) error {

	// Retrieve the Secret
	secret := &corev1.Secret{}
	err := client.Get(ctx, types.NamespacedName{Name: elyraRuntimeSecretName, Namespace: notebook.Namespace}, secret)
	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("Secret does not exist", "Secret", elyraRuntimeSecretName)
			return nil
		}
		log.Error(err, "Error retrieving Secret", "Secret", elyraRuntimeSecretName)
		return err
	}

	// Check if the ConfigMap is empty
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

	// Append the volume if it does not already exist
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
	}

	log.Info("Injecting Elyra runtime volume into notebook", "notebook", notebook.Name, "namespace", notebook.Namespace)

	// Append the volume mount to all containers
	for i, container := range notebook.Spec.Template.Spec.Containers {
		mountExists := false
		for _, vm := range container.VolumeMounts {
			if vm.Name == elyraRuntimeVolumeName {
				mountExists = true
				break
			}
		}
		if !mountExists {
			notebook.Spec.Template.Spec.Containers[i].VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
				Name:      elyraRuntimeVolumeName,
				MountPath: elyraRuntimeMountPath,
			})
		}
	}

	return nil
}

// ReconcileElyraRuntimeConfigSecret will manage the secret reconciliation
// required by the notebook Elyra capabilities
func (r *OpenshiftNotebookReconciler) ReconcileElyraRuntimeConfigSecret(notebook *nbv1.Notebook, ctx context.Context) error {

	// Initialize logger format
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	// TODO: These secret should be created only if identify DSPA object

	// Generate the desired Elyra runtime config secret
	desiredSecret := r.NewElyraRuntimeConfigSecret(ctx, r.Config, r.Client, notebook, r.Namespace, log)

	// Skip secret reconciliation if DSPA route was not found for now then should check for the dspa cr itself
	if desiredSecret == nil {
		log.Info("Skipping Elyra runtime config secret creation as no DSPA is configured in this namespace")
		return nil
	}

	// Create the Elyra runtime config secret if it does not already exist
	foundSecret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      desiredSecret.GetName(),
		Namespace: notebook.GetNamespace(),
	}, foundSecret)
	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("Creating Elyra runtime config secret")
			// Add metadata.ownerReferences so the secret is deleted when the notebook is deleted
			err = ctrl.SetControllerReference(notebook, desiredSecret, r.Scheme)
			if err != nil {
				log.Error(err, "Unable to add OwnerReference to the Elyra runtime config secret")
				return err
			}
			// Create the Elyra runtime config secret in the OpenShift cluster
			err = r.Create(ctx, desiredSecret)
			if err != nil && !apierrs.IsAlreadyExists(err) {
				log.Error(err, "Unable to create the Elyra runtime config secret")
				return err
			}
		} else {
			log.Error(err, "Unable to fetch the Elyra runtime config secret")
			return err
		}
	}

	return nil
}
