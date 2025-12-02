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
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

var _ = Describe("validateFeastAnnotation", func() {
	It("should return empty string when annotation is not present", func() {
		notebook := &nbv1.Notebook{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-notebook",
				Namespace:   "default",
				Annotations: map[string]string{},
			},
		}
		result, err := validateFeastAnnotation(notebook)
		Expect(err).ToNot(HaveOccurred())
		Expect(result).To(Equal(""))
	})

	It("should return annotation value when present", func() {
		notebook := &nbv1.Notebook{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-notebook",
				Namespace: "default",
				Annotations: map[string]string{
					feastAnnotationKey: "test-notebook-feast-config",
				},
			},
		}
		result, err := validateFeastAnnotation(notebook)
		Expect(err).ToNot(HaveOccurred())
		Expect(result).To(Equal("test-notebook-feast-config"))
	})

	It("should handle nil annotations", func() {
		notebook := &nbv1.Notebook{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-notebook",
				Namespace:   "default",
				Annotations: nil,
			},
		}
		result, err := validateFeastAnnotation(notebook)
		Expect(err).ToNot(HaveOccurred())
		Expect(result).To(Equal(""))
	})
})

var _ = Describe("mountFeastConfig", func() {
	It("should add volume and volume mount to notebook container", func() {
		notebookName := "test-notebook"
		configMapName := "test-notebook-feast-config"
		feastConfigMap := map[string]string{
			"feature_store.yaml": "project: feast_project\nregistry: registry_config",
			"config.yaml":        "some_config: value",
		}

		notebook := &nbv1.Notebook{
			ObjectMeta: metav1.ObjectMeta{
				Name:      notebookName,
				Namespace: "default",
			},
			Spec: nbv1.NotebookSpec{
				Template: nbv1.NotebookTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  notebookName,
								Image: "test-image:latest",
							},
						},
					},
				},
			},
		}

		err := mountFeastConfig(notebook, configMapName, feastConfigMap)
		Expect(err).ToNot(HaveOccurred())

		// Verify volume was added
		volumes := notebook.Spec.Template.Spec.Volumes
		var foundVolume *corev1.Volume
		for i := range volumes {
			if volumes[i].Name == feastConfigVolumeName {
				foundVolume = &volumes[i]
				break
			}
		}
		Expect(foundVolume).ToNot(BeNil(), "Feast config volume should be added")
		Expect(foundVolume.ConfigMap).ToNot(BeNil())
		Expect(foundVolume.ConfigMap.Name).To(Equal(configMapName))
		Expect(*foundVolume.ConfigMap.Optional).To(BeTrue())
		// Items should be nil to allow all ConfigMap keys to be mounted dynamically
		Expect(foundVolume.ConfigMap.Items).To(BeNil())

		// Verify volume mount was added to container
		containers := notebook.Spec.Template.Spec.Containers
		Expect(containers).To(HaveLen(1))
		volumeMounts := containers[0].VolumeMounts
		var foundMount *corev1.VolumeMount
		for i := range volumeMounts {
			if volumeMounts[i].Name == feastConfigVolumeName {
				foundMount = &volumeMounts[i]
				break
			}
		}
		Expect(foundMount).ToNot(BeNil(), "Feast config volume mount should be added")
		Expect(foundMount.MountPath).To(Equal(feastConfigMountPath))
		Expect(foundMount.ReadOnly).To(BeTrue())
	})

	It("should update existing volume and volume mount", func() {
		notebookName := "test-notebook"
		configMapName := "test-notebook-feast-config"
		oldConfigMapName := "old-config-map"
		feastConfigMap := map[string]string{
			"feature_store.yaml": "project: feast_project",
		}

		notebook := &nbv1.Notebook{
			ObjectMeta: metav1.ObjectMeta{
				Name:      notebookName,
				Namespace: "default",
			},
			Spec: nbv1.NotebookSpec{
				Template: nbv1.NotebookTemplateSpec{
					Spec: corev1.PodSpec{
						Volumes: []corev1.Volume{
							{
								Name: feastConfigVolumeName,
								VolumeSource: corev1.VolumeSource{
									ConfigMap: &corev1.ConfigMapVolumeSource{
										LocalObjectReference: corev1.LocalObjectReference{
											Name: oldConfigMapName,
										},
									},
								},
							},
						},
						Containers: []corev1.Container{
							{
								Name:  notebookName,
								Image: "test-image:latest",
								VolumeMounts: []corev1.VolumeMount{
									{
										Name:      feastConfigVolumeName,
										MountPath: "/old/path",
										ReadOnly:  false,
									},
								},
							},
						},
					},
				},
			},
		}

		err := mountFeastConfig(notebook, configMapName, feastConfigMap)
		Expect(err).ToNot(HaveOccurred())

		// Verify volume was updated
		volumes := notebook.Spec.Template.Spec.Volumes
		Expect(volumes).To(HaveLen(1))
		Expect(volumes[0].Name).To(Equal(feastConfigVolumeName))
		Expect(volumes[0].ConfigMap.Name).To(Equal(configMapName))

		// Verify volume mount was updated
		volumeMounts := notebook.Spec.Template.Spec.Containers[0].VolumeMounts
		Expect(volumeMounts).To(HaveLen(1))
		Expect(volumeMounts[0].Name).To(Equal(feastConfigVolumeName))
		Expect(volumeMounts[0].MountPath).To(Equal(feastConfigMountPath))
		Expect(volumeMounts[0].ReadOnly).To(BeTrue())
	})

	It("should return error when container not found", func() {
		notebookName := "test-notebook"
		configMapName := "test-notebook-feast-config"
		feastConfigMap := map[string]string{
			"feature_store.yaml": "project: feast_project",
		}

		notebook := &nbv1.Notebook{
			ObjectMeta: metav1.ObjectMeta{
				Name:      notebookName,
				Namespace: "default",
			},
			Spec: nbv1.NotebookSpec{
				Template: nbv1.NotebookTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "different-container-name",
								Image: "test-image:latest",
							},
						},
					},
				},
			},
		}

		err := mountFeastConfig(notebook, configMapName, feastConfigMap)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("notebook image container not found"))
	})

	It("should handle multiple containers and update only the matching one", func() {
		notebookName := "test-notebook"
		configMapName := "test-notebook-feast-config"
		feastConfigMap := map[string]string{
			"feature_store.yaml": "project: feast_project",
		}

		notebook := &nbv1.Notebook{
			ObjectMeta: metav1.ObjectMeta{
				Name:      notebookName,
				Namespace: "default",
			},
			Spec: nbv1.NotebookSpec{
				Template: nbv1.NotebookTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "sidecar-container",
								Image: "sidecar:latest",
							},
							{
								Name:  notebookName,
								Image: "test-image:latest",
							},
							{
								Name:  "another-sidecar",
								Image: "another-sidecar:latest",
							},
						},
					},
				},
			},
		}

		err := mountFeastConfig(notebook, configMapName, feastConfigMap)
		Expect(err).ToNot(HaveOccurred())

		// Verify only the matching container got the volume mount
		containers := notebook.Spec.Template.Spec.Containers
		Expect(containers).To(HaveLen(3))

		// First container should not have the mount
		Expect(containers[0].VolumeMounts).To(HaveLen(0))

		// Second container (matching name) should have the mount
		Expect(containers[1].VolumeMounts).To(HaveLen(1))
		Expect(containers[1].VolumeMounts[0].Name).To(Equal(feastConfigVolumeName))

		// Third container should not have the mount
		Expect(containers[2].VolumeMounts).To(HaveLen(0))
	})
})

var _ = Describe("NewFeastConfig integration tests", func() {
	const (
		testNamespace = "feast-test-ns"
	)

	BeforeEach(func() {
		// Create test namespace
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: testNamespace,
			},
		}
		err := cli.Create(ctx, ns)
		if err != nil && !apierrs.IsAlreadyExists(err) {
			Expect(err).ToNot(HaveOccurred())
		}
	})

	AfterEach(func() {
		// Clean up test namespace
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: testNamespace,
			},
		}
		_ = cli.Delete(ctx, ns)
	})

	When("ConfigMap exists with valid data", func() {
		It("should mount Feast config successfully", func() {
			notebookName := "test-notebook-feast-valid"
			configMapName := notebookName + feastConfigMapSuffix

			// Create ConfigMap
			configMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: testNamespace,
				},
				Data: map[string]string{
					"feature_store.yaml": "project: feast_project\nregistry: registry_config",
					"config.yaml":        "some_config: value",
				},
			}
			Expect(cli.Create(ctx, configMap)).To(Succeed())

			// Create Notebook
			notebook := &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Name:      notebookName,
					Namespace: testNamespace,
					Annotations: map[string]string{
						feastAnnotationKey: configMapName,
					},
				},
				Spec: nbv1.NotebookSpec{
					Template: nbv1.NotebookTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  notebookName,
									Image: "test-image:latest",
								},
							},
						},
					},
				},
			}
			Expect(cli.Create(ctx, notebook)).To(Succeed())

			// Call NewFeastConfig
			err := NewFeastConfig(ctx, cli, notebook, ctrl.Log.WithName("test"))
			Expect(err).ToNot(HaveOccurred())

			// Verify volume was added
			volumes := notebook.Spec.Template.Spec.Volumes
			var foundVolume bool
			for _, v := range volumes {
				if v.Name == feastConfigVolumeName {
					foundVolume = true
					Expect(v.ConfigMap).ToNot(BeNil())
					Expect(v.ConfigMap.Name).To(Equal(configMapName))
					break
				}
			}
			Expect(foundVolume).To(BeTrue(), "Feast config volume should be added")

			// Verify volume mount was added
			containers := notebook.Spec.Template.Spec.Containers
			var foundMount bool
			for _, vm := range containers[0].VolumeMounts {
				if vm.Name == feastConfigVolumeName {
					foundMount = true
					Expect(vm.MountPath).To(Equal(feastConfigMountPath))
					Expect(vm.ReadOnly).To(BeTrue())
					break
				}
			}
			Expect(foundMount).To(BeTrue(), "Feast config volume mount should be added")

			// Cleanup
			Expect(cli.Delete(ctx, notebook)).To(Succeed())
			Expect(cli.Delete(ctx, configMap)).To(Succeed())
		})
	})

	When("ConfigMap does not exist", func() {
		It("should return nil without error", func() {
			notebookName := "test-notebook-feast-missing-cm"
			configMapName := notebookName + feastConfigMapSuffix

			notebook := &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Name:      notebookName,
					Namespace: testNamespace,
					Annotations: map[string]string{
						feastAnnotationKey: configMapName,
					},
				},
				Spec: nbv1.NotebookSpec{
					Template: nbv1.NotebookTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  notebookName,
									Image: "test-image:latest",
								},
							},
						},
					},
				},
			}

			err := NewFeastConfig(ctx, cli, notebook, ctrl.Log.WithName("test"))
			Expect(err).ToNot(HaveOccurred())
		})
	})

	When("ConfigMap exists but is empty", func() {
		It("should return error", func() {
			notebookName := "test-notebook-feast-empty-cm"
			configMapName := notebookName + feastConfigMapSuffix

			// Create empty ConfigMap
			configMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: testNamespace,
				},
				Data: map[string]string{},
			}
			Expect(cli.Create(ctx, configMap)).To(Succeed())

			notebook := &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Name:      notebookName,
					Namespace: testNamespace,
					Annotations: map[string]string{
						feastAnnotationKey: configMapName,
					},
				},
				Spec: nbv1.NotebookSpec{
					Template: nbv1.NotebookTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  notebookName,
									Image: "test-image:latest",
								},
							},
						},
					},
				},
			}

			err := NewFeastConfig(ctx, cli, notebook, ctrl.Log.WithName("test"))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("feast config map is empty"))

			// Cleanup
			Expect(cli.Delete(ctx, configMap)).To(Succeed())
		})
	})

	When("Annotation does not match expected format", func() {
		It("should return error for invalid annotation format", func() {
			notebookName := "test-notebook-feast-invalid"
			invalidConfigMapName := "wrong-name"

			notebook := &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Name:      notebookName,
					Namespace: testNamespace,
					Annotations: map[string]string{
						feastAnnotationKey: invalidConfigMapName,
					},
				},
				Spec: nbv1.NotebookSpec{
					Template: nbv1.NotebookTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  notebookName,
									Image: "test-image:latest",
								},
							},
						},
					},
				},
			}

			err := NewFeastConfig(ctx, cli, notebook, ctrl.Log.WithName("test"))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("feast config annotation is not in valid format"))
		})
	})
})
