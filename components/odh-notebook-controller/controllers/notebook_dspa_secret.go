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
	routev1 "github.com/openshift/api/route/v1"
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

const (
	elyraRuntimeConfigSecretName = "ds-pipeline-config-by-nbc"
	elyraRuntimeConfigMountPath  = "/opt/app-root/runtimes"
	elyraRuntimeConfigVolumeName = "elyra-dsp-details"
)

func MountElyraRuntimeConfigSecret(ctx context.Context, client client.Client, notebook *nbv1.Notebook, log logr.Logger) error {

	// Retrieve the Secret
	secret := &corev1.Secret{}
	err := client.Get(ctx, types.NamespacedName{Name: elyraRuntimeConfigSecretName, Namespace: notebook.Namespace}, secret)
	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("Secret does not exist", "Secret", elyraRuntimeConfigSecretName)
			return nil
		}
		log.Error(err, "Error retrieving Secret", "Secret", elyraRuntimeConfigSecretName)
		return err
	}

	// Check if the ConfigMap is empty
	if len(secret.Data) == 0 {
		log.Info("Secret is empty, skipping volume mount", "Secret", elyraRuntimeConfigSecretName)
		return nil
	}

	// Define the volume
	configVolume := corev1.Volume{
		Name: elyraRuntimeConfigVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: elyraRuntimeConfigSecretName,
				},
				Optional: ptr.To(true),
			},
		},
	}

	// Append the volume if it does not already exist
	volumes := &notebook.Spec.Template.Spec.Volumes
	volumeExists := false
	for _, v := range *volumes {
		if v.Name == elyraRuntimeConfigVolumeName {
			volumeExists = true
			break
		}
	}
	if !volumeExists {
		*volumes = append(*volumes, configVolume)
	}

	log.Info("Injecting Elyra runtime config volume into notebook", "notebook", notebook.Name, "namespace", notebook.Namespace)

	// Append the volume mount to all containers
	for i, container := range notebook.Spec.Template.Spec.Containers {
		mountExists := false
		for _, vm := range container.VolumeMounts {
			if vm.Name == elyraRuntimeConfigVolumeName {
				mountExists = true
				break
			}
		}
		if !mountExists {
			notebook.Spec.Template.Spec.Containers[i].VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
				Name:      elyraRuntimeConfigVolumeName,
				MountPath: elyraRuntimeConfigMountPath,
			})
		}
	}

	return nil
}

// NewElyraRuntimeConfigSecret defines the desired ElyraRuntimeConfig secret object
func (r *OpenshiftNotebookReconciler) NewElyraRuntimeConfigSecret(ctx context.Context, dynamicConfig *rest.Config, client client.Client, notebook *nbv1.Notebook, controllerNamespace string, log logr.Logger) *corev1.Secret {
	// Create a dynamic client
	dynamicClient, err := dynamic.NewForConfig(dynamicConfig)
	if err != nil {
		log.Error(err, "Failed to create dynamic client")
		return nil
	}

	// Define the DSPA GroupVersionResource
	dspaGVR := schema.GroupVersionResource{
		Group:    "datasciencepipelinesapplications.opendatahub.io",
		Version:  "v1",
		Resource: "datasciencepipelinesapplications", // plural form
	}

	// Fetch DSPA CR from the same namespace as the notebook
	dspaName := "dspa" // Replace with the actual name of your DSPA CR if needed
	dspaObj, err := dynamicClient.Resource(dspaGVR).Namespace(notebook.Namespace).Get(ctx, dspaName, metav1.GetOptions{})
	if err != nil {
		log.Error(err, "Failed to get DSPA CR")
		return nil
	}

	// Extract the access key from DSPA
	spec := dspaObj.Object["spec"].(map[string]interface{})
	objectStorage := spec["objectStorage"].(map[string]interface{})
	externalStorage := objectStorage["externalStorage"].(map[string]interface{})
	host := externalStorage["host"].(string) // Here we fetch the 'host'	// externalStorage := objectStorage["externalStorage"].(map[string]interface{})
	// s3CredentialsSecret := externalStorage["s3CredentialsSecret"].(map[string]interface{})
	// accessKey := s3CredentialsSecret["accessKey"].(string)

	// Query the dashboards' route
	dashboardRoute := &routev1.Route{}
	err = client.Get(ctx, types.NamespacedName{Name: "rhods-dashboard", Namespace: controllerNamespace}, dashboardRoute)
	if err != nil {
		log.Error(err, "Failed to get rhods-dashboard Route")
		return nil
	}
	publicAPIEndpoint := fmt.Sprintf("https://%s/experiments/%s/", dashboardRoute.Spec.Host, notebook.Namespace)

	// Check if DSPA Route exists in this namespace
	dspaRoute := &routev1.Route{}
	err = client.Get(ctx, types.NamespacedName{
		Name:      "ds-pipeline-dspa",
		Namespace: notebook.Namespace,
	}, dspaRoute)

	if err != nil {
		if apierrs.IsNotFound(err) {
			// Expected: this namespace may not use DSPA, skip
			return nil
		}
		log.Error(err, "Error while fetching DSPA route")
		return nil
	}
	// If found, construct API endpoint
	APIEndpoint := fmt.Sprintf("https://%s", dspaRoute.Spec.Host)

	// Construct the data to marshal
	dspData := map[string]interface{}{
		"display_name": "Data Science Pipeline",
		"schema_name":  "kfp",
		"metadata": map[string]interface{}{
			"tags":                []string{}, // or "[]", for string
			"display_name":        "Data Science Pipeline",
			"engine":              "Argo",
			"runtime_type":        "KUBEFLOW_PIPELINES",
			"auth_type":           "KUBERNETES_SERVICE_ACCOUNT_TOKEN",
			"cos_auth_type":       "KUBERNETES_SECRET",
			"public_api_endpoint": publicAPIEndpoint,
			"api_endpoint":        APIEndpoint,
			"cos_endpoint":        host,
		},
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
			Name:      elyraRuntimeConfigSecretName,
			Namespace: notebook.Namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"odh_dsp.json": dspJSON,
		},
	}
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
