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

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	feastConfigMapSuffix  = "-feast-config"
	feastConfigVolumeName = "odh-feast-config"
	feastConfigMountPath  = "/opt/app-root/src/feast-config"
	feastLabelKey         = "opendatahub.io/feast-integration"
)

// isFeastEnabled checks if Feast config mounting is enabled via label.
// Returns true if the label is set to "true", false otherwise.
func isFeastEnabled(notebook *nbv1.Notebook) bool {
	if notebook.Labels == nil {
		return false
	}
	labelValue := notebook.Labels[feastLabelKey]
	return labelValue == "true"
}

// isFeastMounted checks if the Feast config volume is currently mounted.
func isFeastMounted(notebook *nbv1.Notebook) bool {
	for _, volume := range notebook.Spec.Template.Spec.Volumes {
		if volume.Name == feastConfigVolumeName {
			return true
		}
	}
	return false
}

// mountFeastConfig mounts the Feast config on the notebook.
func mountFeastConfig(notebook *nbv1.Notebook, configMapName string) error {

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

// unmountFeastConfig removes the Feast config volume and volume mount from the notebook.
func unmountFeastConfig(notebook *nbv1.Notebook) {
	// Remove feast config volume
	notebookVolumes := &notebook.Spec.Template.Spec.Volumes
	for i, volume := range *notebookVolumes {
		if volume.Name == feastConfigVolumeName {
			*notebookVolumes = append((*notebookVolumes)[:i], (*notebookVolumes)[i+1:]...)
			break
		}
	}

	// Remove feast config volume mount from notebook container
	notebookContainers := &notebook.Spec.Template.Spec.Containers
	for i, container := range *notebookContainers {
		if container.Name == notebook.Name {
			for j, volumeMount := range container.VolumeMounts {
				if volumeMount.Name == feastConfigVolumeName {
					(*notebookContainers)[i].VolumeMounts = append(
						container.VolumeMounts[:j],
						container.VolumeMounts[j+1:]...,
					)
					break
				}
			}
			break
		}
	}
}

// NewFeastConfig creates a new Feast config.
func NewFeastConfig(ctx context.Context, cli client.Client, notebook *nbv1.Notebook) error {

	feastConfigMapName := notebook.Name + feastConfigMapSuffix

	feastConfigMapObj := &corev1.ConfigMap{}
	err := cli.Get(ctx, types.NamespacedName{Name: feastConfigMapName, Namespace: notebook.Namespace}, feastConfigMapObj)
	if err != nil {
		return fmt.Errorf("feast ConfigMap %s not found in namespace %s, skipping mount: %v", feastConfigMapName, notebook.Namespace, err)
	}

	// mount the Feast config volume
	err = mountFeastConfig(notebook, feastConfigMapName)
	if err != nil {
		return fmt.Errorf("error mounting Feast config volume: %v", err)
	}
	return nil
}
