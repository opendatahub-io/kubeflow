package controllers

import (
	"context"
	"encoding/json"
	"reflect"
	"regexp"
	"strings"

	"github.com/go-logr/logr"
	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	imagev1 "github.com/openshift/api/image/v1"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

func (r *OpenshiftNotebookReconciler) syncRuntimeImagesConfigMap(ctx context.Context, notebookNamespace string, controllerNamespace string) error {

	log := r.Log.WithValues("namespace", notebookNamespace)

	// Fetch ImageStreams from controllerNamespace namespace
	imageStreamList := &imagev1.ImageStreamList{}
	if err := r.List(ctx, imageStreamList, client.InNamespace(controllerNamespace)); err != nil {
		log.Error(err, "Failed to list ImageStreams", "Namespace", controllerNamespace)
		return err
	}

	// Prepare data for ConfigMap
	data := make(map[string]string)
	for _, imageStream := range imageStreamList.Items {
		if imageStream.Labels["opendatahub.io/runtime-image"] != "true" {
			continue
		}

		if len(imageStream.Spec.Tags) == 0 {
			log.Error(nil, "ImageStream labeled as runtime-image has no tags - possible misconfiguration", "ImageStream", imageStream.Name, "Namespace", controllerNamespace)
			continue
		}

		for _, tag := range imageStream.Spec.Tags {
			// Extract metadata annotation
			metadataRaw := tag.Annotations["opendatahub.io/runtime-image-metadata"]
			if metadataRaw == "" {
				metadataRaw = "[]"
			}

			// Extract image URL from the tag's From reference
			if tag.From == nil || tag.From.Name == "" {
				log.Error(nil, "Failed to extract image URL from ImageStream", "ImageStream", imageStream.Name, "Tag", tag.Name)
				continue
			}
			imageURL := tag.From.Name

			// Parse metadata
			metadataParsed := parseRuntimeImageMetadata(metadataRaw, imageURL)
			displayName := extractDisplayName(metadataParsed)

			// Construct the key name
			if displayName != "" {
				formattedName := formatKeyName(displayName)
				if formattedName != "" {
					data[formattedName] = metadataParsed
				} else {
					log.Error(nil, "Failed to construct ConfigMap key name", "ImageStream", imageStream.Name, "Tag", tag.Name)
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

var (
	invalidChars = regexp.MustCompile(`[^-._a-zA-Z0-9]+`)
	multiDash    = regexp.MustCompile(`-+`)
)

// formatKeyName sanitizes display names for use as ConfigMap keys.
// Returns empty string if input contains only invalid characters.
func formatKeyName(displayName string) string {
	s := invalidChars.ReplaceAllString(strings.ToLower(displayName), "-")
	s = multiDash.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return ""
	}
	return s + ".json"
}

// parseRuntimeImageMetadata extracts the first object from the JSON array
func parseRuntimeImageMetadata(rawJSON string, imageUrl string) string {
	var metadataArray []map[string]interface{}

	err := json.Unmarshal([]byte(rawJSON), &metadataArray)
	if err != nil || len(metadataArray) == 0 {
		return "{}" // Return empty JSON object if parsing fails
	}

	// Insert imageUrl into the metadataArray
	if metadataArray[0]["metadata"] != nil {
		metadata, ok := metadataArray[0]["metadata"].(map[string]interface{})
		if ok {
			metadata["image_name"] = imageUrl
		}
	}

	// Convert first object back to JSON
	metadataJSON, err := json.Marshal(metadataArray[0])
	if err != nil {
		return "{}"
	}

	return string(metadataJSON)
}

func (r *OpenshiftNotebookReconciler) EnsureNotebookConfigMap(notebook *nbv1.Notebook, ctx context.Context) error {
	return r.syncRuntimeImagesConfigMap(ctx, notebook.Namespace, r.Namespace)
}

func MountPipelineRuntimeImages(ctx context.Context, client client.Client, notebook *nbv1.Notebook, log logr.Logger) error {

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
