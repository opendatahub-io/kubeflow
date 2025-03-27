package controllers

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"

	"github.com/go-logr/logr"
	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	configMapName = "pipeline-runtime-images"
	mountPath     = "/opt/app-root/pipeline-runtimes/"
	volumeName    = "runtime-images"
)

// getRuntimeConfigMap verifies if a ConfigMap exists in the namespace.
func (r *OpenshiftNotebookReconciler) getRuntimeConfigMap(ctx context.Context, configMapName, namespace string) (*corev1.ConfigMap, bool, error) {
	configMap := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: namespace}, configMap)
	if err != nil {
		if apierrs.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return configMap, true, nil
}

func (r *OpenshiftNotebookReconciler) syncRuntimeImagesConfigMap(ctx context.Context, notebookNamespace string, controllerNamespace string, config *rest.Config) error {

	log := r.Log.WithValues("namespace", notebookNamespace)

	// Create a dynamic client
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		log.Error(err, "Error creating dynamic client")
		return err
	}

	// Define GroupVersionResource for ImageStreams
	ims := schema.GroupVersionResource{
		Group:    "image.openshift.io",
		Version:  "v1",
		Resource: "imagestreams",
	}

	// Fetch ImageStreams from controllerNamespace namespace
	imagestreams, err := dynamicClient.Resource(ims).Namespace(controllerNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Error(err, "Failed to list ImageStreams", "Namespace", controllerNamespace)
		return err
	}

	// Prepare data for ConfigMap
	data := make(map[string]string)
	for _, item := range imagestreams.Items {
		labels := item.GetLabels()
		if labels["opendatahub.io/runtime-image"] == "true" {
			tags, found, err := unstructured.NestedSlice(item.Object, "spec", "tags")
			if err != nil || !found {
				log.Error(err, "Failed to extract tags from ImageStream", "ImageStream", item.GetName())
				continue
			}

			for _, tag := range tags {
				tagMap, ok := tag.(map[string]interface{})
				if !ok {
					continue
				}

				// Extract metadata annotation
				annotations, found, err := unstructured.NestedMap(tagMap, "annotations")
				if err != nil || !found {
					annotations = map[string]interface{}{}
				}
				metadataRaw, ok := annotations["opendatahub.io/runtime-image-metadata"].(string)
				if !ok || metadataRaw == "" {
					metadataRaw = "[]"
				}

				// Parse metadata
				metadataParsed := parseRuntimeImageMetadata(metadataRaw)
				displayName := extractDisplayName(metadataParsed)

				// Construct the key name
				if displayName != "" {
					formattedName := formatKeyName(displayName)
					data[formattedName] = metadataParsed
				}
			}
		}
	}

	// Check if the ConfigMap already exists
	existingConfigMap, configMapExists, err := r.getRuntimeConfigMap(ctx, configMapName, notebookNamespace)
	if err != nil {
		log.Error(err, "Error getting ConfigMap", "ConfigMap.Name", configMapName)
		return err
	}

	// If data is empty and ConfigMap does not exist, skip creating anything
	if len(data) == 0 && !configMapExists {
		log.Info("No runtime images found. Skipping creation of empty ConfigMap.")
		return nil
	}

	// If data is empty and ConfigMap does exist, decide what behavior we want:
	if len(data) == 0 && configMapExists {
		log.Info("Data is empty but the ConfigMap already exists. Leaving it as is.")
		// OR optionally delete it:
		// if err := r.Delete(ctx, existingConfigMap); err != nil {
		//	log.Error(err, "Failed to delete existing empty ConfigMap")
		//	return err
		//}
		return nil
	}

	// Create a new ConfigMap struct with the data
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: notebookNamespace,
			Labels:    map[string]string{"opendatahub.io/managed-by": "workbenches"},
		},
		Data: data,
	}

	// If the ConfigMap exists and data has changed, update it
	if configMapExists {
		if !reflect.DeepEqual(existingConfigMap.Data, data) {
			existingConfigMap.Data = data
			if err := r.Update(ctx, existingConfigMap); err != nil {
				log.Error(err, "Failed to update ConfigMap", "ConfigMap.Name", configMapName)
				return err
			}
			log.Info("Updated existing ConfigMap with new runtime images", "ConfigMap.Name", configMapName)
		} else {
			log.Info("ConfigMap already up-to-date", "ConfigMap.Name", configMapName)
		}
		return nil
	}

	// Otherwise, create the ConfigMap
	if err := r.Create(ctx, configMap); err != nil {
		log.Error(err, "Failed to create ConfigMap", "ConfigMap.Name", configMapName)
		return err
	}
	log.Info("Created new ConfigMap for runtime images", "ConfigMap.Name", configMapName)

	return nil
}

func extractDisplayName(metadata string) string {
	var metadataMap map[string]interface{}
	err := json.Unmarshal([]byte(metadata), &metadataMap)
	if err != nil {
		return ""
	}
	displayName, ok := metadataMap["display_name"].(string)
	if !ok {
		return ""
	}
	return displayName
}

func formatKeyName(displayName string) string {
	replacer := strings.NewReplacer(" ", "-", "(", "", ")", "")
	return strings.ToLower(replacer.Replace(displayName)) + ".json"
}

// parseRuntimeImageMetadata extracts the first object from the JSON array
func parseRuntimeImageMetadata(rawJSON string) string {
	var metadataArray []map[string]interface{}

	err := json.Unmarshal([]byte(rawJSON), &metadataArray)
	if err != nil || len(metadataArray) == 0 {
		return "{}" // Return empty JSON object if parsing fails
	}

	// Convert first object back to JSON
	metadataJSON, err := json.Marshal(metadataArray[0])
	if err != nil {
		return "{}"
	}

	return string(metadataJSON)
}

func (r *OpenshiftNotebookReconciler) EnsureNotebookConfigMap(notebook *nbv1.Notebook, ctx context.Context) error {
	return r.syncRuntimeImagesConfigMap(ctx, notebook.Namespace, r.Namespace, r.Config)
}

func MountPipelineRuntimeImages(notebook *nbv1.Notebook, log logr.Logger, client client.Client, ctx context.Context) error {

	// Retrieve the ConfigMap
	configMap := &corev1.ConfigMap{}
	err := client.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: notebook.Namespace}, configMap)
	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("ConfigMap does not exist", "ConfigMap", configMapName)
			return nil
		}
		log.Error(err, "Error retrieving ConfigMap", "ConfigMap", configMapName)
		return err
	}

	// Check if the ConfigMap is empty
	if len(configMap.Data) == 0 {
		log.Info("ConfigMap is empty, skipping volume mount", "ConfigMap", configMapName)
		return nil
	}

	// Define the volume
	configVolume := corev1.Volume{
		Name: volumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: configMapName,
				},
				Optional: ptr.To(true),
			},
		},
	}

	// Define the volume mount
	volumeMount := corev1.VolumeMount{
		Name:      volumeName,
		MountPath: mountPath,
	}

	// Append the volume if it does not already exist
	volumes := &notebook.Spec.Template.Spec.Volumes
	volumeExists := false
	for _, v := range *volumes {
		if v.Name == volumeName {
			volumeExists = true
			break
		}
	}
	if !volumeExists {
		*volumes = append(*volumes, configVolume)
	}

	log.Info("Injecting runtime-images volume into notebook", "notebook", notebook.Name, "namespace", notebook.Namespace)

	// Append the volume mount to all containers
	for i, container := range notebook.Spec.Template.Spec.Containers {
		mountExists := false
		for _, vm := range container.VolumeMounts {
			if vm.Name == volumeName {
				mountExists = true
				break
			}
		}
		if !mountExists {
			notebook.Spec.Template.Spec.Containers[i].VolumeMounts = append(container.VolumeMounts, volumeMount)
		}
	}

	return nil
}
