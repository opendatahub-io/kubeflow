package controllers

import (
	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	imagev1 "github.com/openshift/api/image/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Helper function to create a basic Notebook object
func createNotebook(name, namespace string) *nbv1.Notebook {
	return &nbv1.Notebook{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: nbv1.NotebookSpec{
			Template: nbv1.NotebookTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  name,
							Image: "registry.redhat.io/ubi8/ubi:latest",
						},
					},
				},
			},
		},
	}
}

// Helper function to create a basic ConfigMap for runtime images
func createRuntimeConfigMap(name, namespace string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: data,
	}
}

// Helper function to create a basic ImageStream
func createRuntimeImageStream(name, namespace string, tags []imagev1.TagReference) *imagev1.ImageStream {
	return &imagev1.ImageStream{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ImageStream",
			APIVersion: "image.openshift.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"opendatahub.io/runtime-image": "true",
			},
		},
		Spec: imagev1.ImageStreamSpec{
			LookupPolicy: imagev1.ImageLookupPolicy{
				Local: true,
			},
			Tags: tags,
		},
	}
}
