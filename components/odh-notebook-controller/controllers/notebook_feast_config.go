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
	"fmt"

	"github.com/go-logr/logr"
	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	feastConfigMapSuffix  = "-feast-config"
	feastConfigVolumeName = "odh-feast-config"
	feastConfigMountPath  = "/opt/app-root/config/feast"
	feastAnnotationKey    = "opendatahub.io/feast-config"
)

// validateFeastAnnotation validates the Feast annotation on the notebook.
func validateFeastAnnotation(notebook *nbv1.Notebook) (string, error) {
	if notebook.Annotations[feastAnnotationKey] == "" {
		return "", nil
	}
	return notebook.Annotations[feastAnnotationKey], nil
}

// mountFeastConfig mounts the Feast config on the notebook.
func mountFeastConfig(notebook *nbv1.Notebook, configMapName string, feastConfigMap map[string]string) error {

	// Add feast config volume
	notebookVolumes := &notebook.Spec.Template.Spec.Volumes
	feastConfigVolumeExists := false

	// Create the Feast config volume
	// Note: Not specifying Items means all keys from the ConfigMap will be mounted
	// and automatically updated when the ConfigMap changes
	feastConfigVolume := corev1.Volume{
		Name: feastConfigVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: configMapName,
				},
				Optional: ptr.To(true),
			},
		},
	}

	for index, volume := range *notebookVolumes {
		if volume.Name == feastConfigVolumeName {
			(*notebookVolumes)[index] = feastConfigVolume
			feastConfigVolumeExists = true
			break
		}
	}
	if !feastConfigVolumeExists {
		*notebookVolumes = append(*notebookVolumes, feastConfigVolume)
	}

	// Update Notebook Image container with Volume Mounts
	notebookContainers := &notebook.Spec.Template.Spec.Containers
	imgContainerExists := false

	for i, container := range *notebookContainers {
		if container.Name == notebook.Name {
			// Create Feast config Volume mount
			volumeMountExists := false
			feastConfigVolMount := corev1.VolumeMount{
				Name:      feastConfigVolumeName,
				ReadOnly:  true,
				MountPath: feastConfigMountPath,
			}

			for index, volumeMount := range container.VolumeMounts {
				if volumeMount.Name == feastConfigVolumeName {
					(*notebookContainers)[i].VolumeMounts[index] = feastConfigVolMount
					volumeMountExists = true
					break
				}
			}
			if !volumeMountExists {
				(*notebookContainers)[i].VolumeMounts = append(container.VolumeMounts, feastConfigVolMount)
			}
			imgContainerExists = true
			break
		}
	}

	if !imgContainerExists {
		return fmt.Errorf("notebook image container not found %v", notebook.Name)
	}
	return nil
}

// NewFeastConfig creates a new Feast config.
func NewFeastConfig(ctx context.Context, cli client.Client, notebook *nbv1.Notebook, log logr.Logger) error {

	// validate feast config annotation is same as feast config map name
	feastConfigMapName := notebook.Name + feastConfigMapSuffix
	if notebook.Annotations[feastAnnotationKey] != feastConfigMapName {
		return fmt.Errorf("feast config annotation is not in valid format, i.e <notebook-name>-feast-config")
	}

	feastConfigMap := make(map[string]string)
	// get the Feast config map
	feastConfigMapObj := &corev1.ConfigMap{}
	err := cli.Get(ctx, types.NamespacedName{Name: feastConfigMapName, Namespace: notebook.Namespace}, feastConfigMapObj)
	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Error(err, "Feast config map not found", "ConfigMap", feastConfigMapName)
			return nil
		}
		log.Error(err, "Error getting Feast config map", "ConfigMap", feastConfigMapName)
		return fmt.Errorf("error getting Feast config map: %v", err)
	} else {
		feastConfig := feastConfigMapObj.Data
		if len(feastConfig) == 0 {
			log.Error(fmt.Errorf("feast config map is empty"), "Feast config map is empty", "ConfigMap", feastConfigMapName)
			return fmt.Errorf("feast config map is empty: %v", feastConfigMapName)
		}
		// expand the data to build a map with key value pairs
		for key, value := range feastConfig {
			feastConfigMap[key] = value
		}
	}

	// mount the Feast config volume
	err = mountFeastConfig(notebook, feastConfigMapName, feastConfigMap)
	if err != nil {
		log.Error(err, "Error mounting Feast config volume", "ConfigMap", feastConfigMapName)
		return fmt.Errorf("error mounting Feast config volume: %v", err)
	}
	return nil
}
