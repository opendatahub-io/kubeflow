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

	"github.com/go-logr/logr"
	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	elyraRuntimeConfigSecretName = "ds-pipeline-config"
	elyraRuntimeConfigMountPath  = "TODO"
	elyraRuntimeConfigVolumeName = "TODO"
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
func NewElyraRuntimeConfigSecret(notebook *nbv1.Notebook) *corev1.Secret {

	// TODO: query for DSPA

	// TODO: query for secret referenced by DSPA

	// TODO: extract hostname from notebook annotation (if it exists) and append URL experiments segment

	// Create a Kubernetes secret to store the Elyra runtime config data
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      elyraRuntimeConfigSecretName,
			Namespace: notebook.Namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"username": []byte("admin"),
			"password": []byte("s3cr3t"),
		},
	}
}

// ReconcileElyraRuntimeConfigSecret will manage the secret reconciliation
// required by the notebook Elyra capabilities
func (r *OpenshiftNotebookReconciler) ReconcileElyraRuntimeConfigSecret(notebook *nbv1.Notebook, ctx context.Context) error {

	// Initialize logger format
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	// Generate the desired Elyra runtime config secret
	desiredSecret := NewElyraRuntimeConfigSecret(notebook)

	// Create the Elyra runtime config secret if it does not already exist
	// TODO: rework this bit to create/update
	foundSecret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      desiredSecret.GetName(),
		Namespace: notebook.GetNamespace(),
	}, foundSecret)
	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("Creating Elyra runtime config secret")
			// Add .metatada.ownerReferences to the Elyra runtime config secret to be deleted by
			// the Kubernetes garbage collector if the notebook is deleted
			err = ctrl.SetControllerReference(notebook, desiredSecret, r.Scheme)
			if err != nil {
				log.Error(err, "Unable to add OwnerReference to the Elyra runtime config secret")
				return err
			}
			// Create the Elyra runtime config secret in the Openshift cluster
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
