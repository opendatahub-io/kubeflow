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
	"math/rand"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var letterRunes = []rune("abcdefghijklmnopqrstuvwxyz0123456789")

func randStringRunes(n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return string(b)
}

var _ = Describe("isFeastEnabled", func() {
	It("should return false when label is not present", func() {
		notebook := &nbv1.Notebook{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-notebook",
				Namespace: "default",
				Labels:    map[string]string{},
			},
		}
		result := isFeastEnabled(notebook)
		Expect(result).To(BeFalse())
	})

	It("should return true when label is set to 'true'", func() {
		notebook := &nbv1.Notebook{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-notebook",
				Namespace: "default",
				Labels: map[string]string{
					feastLabelKey: "true",
				},
			},
		}
		result := isFeastEnabled(notebook)
		Expect(result).To(BeTrue())
	})

	It("should return false when label is set to 'false'", func() {
		notebook := &nbv1.Notebook{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-notebook",
				Namespace: "default",
				Labels: map[string]string{
					feastLabelKey: "false",
				},
			},
		}
		result := isFeastEnabled(notebook)
		Expect(result).To(BeFalse())
	})

	It("should return false when label has invalid value", func() {
		notebook := &nbv1.Notebook{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-notebook",
				Namespace: "default",
				Labels: map[string]string{
					feastLabelKey: "yes",
				},
			},
		}
		result := isFeastEnabled(notebook)
		Expect(result).To(BeFalse())
	})

	It("should handle nil labels", func() {
		notebook := &nbv1.Notebook{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-notebook",
				Namespace: "default",
				Labels:    nil,
			},
		}
		result := isFeastEnabled(notebook)
		Expect(result).To(BeFalse())
	})
})

var _ = Describe("mountFeastConfig", func() {
	It("should add volume and volume mount to notebook container", func() {
		notebookName := "test-notebook"
		configMapName := "test-notebook-feast-config"

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

		err := mountFeastConfig(notebook, configMapName)
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

		err := mountFeastConfig(notebook, configMapName)
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

		err := mountFeastConfig(notebook, configMapName)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("notebook image container not found"))
	})

	It("should handle multiple containers and update only the matching one", func() {
		notebookName := "test-notebook"
		configMapName := "test-notebook-feast-config"

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

		err := mountFeastConfig(notebook, configMapName)
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

var _ = Describe("unmountFeastConfig", func() {
	It("should remove volume and volume mount from notebook", func() {
		notebookName := "test-notebook"
		configMapName := "test-notebook-feast-config"

		// Create notebook with Feast config already mounted
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
											Name: configMapName,
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
										MountPath: feastConfigMountPath,
										ReadOnly:  true,
									},
								},
							},
						},
					},
				},
			},
		}

		// Verify volume and mount exist before unmounting
		Expect(isFeastMounted(notebook)).To(BeTrue())

		// Unmount
		unmountFeastConfig(notebook)

		// Verify volume was removed
		volumes := notebook.Spec.Template.Spec.Volumes
		for _, v := range volumes {
			Expect(v.Name).ToNot(Equal(feastConfigVolumeName), "Feast config volume should be removed")
		}

		// Verify volume mount was removed
		containers := notebook.Spec.Template.Spec.Containers
		Expect(containers).To(HaveLen(1))
		for _, vm := range containers[0].VolumeMounts {
			Expect(vm.Name).ToNot(Equal(feastConfigVolumeName), "Feast config volume mount should be removed")
		}

		// Verify isFeastMounted returns false after unmounting
		Expect(isFeastMounted(notebook)).To(BeFalse())
	})

	It("should handle notebook without Feast config gracefully", func() {
		notebookName := "test-notebook-no-feast"

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

		// Should not panic when unmounting from notebook without Feast config
		Expect(func() { unmountFeastConfig(notebook) }).ToNot(Panic())
		Expect(isFeastMounted(notebook)).To(BeFalse())
	})
})

var _ = Describe("Feast Config Integration Tests", func() {
	var testNamespace string

	BeforeEach(func() {
		// Create unique test namespace for each test to avoid termination issues
		testNamespace = "feast-test-" + randStringRunes(5)
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: testNamespace,
			},
		}
		err := cli.Create(ctx, ns)
		Expect(err).ToNot(HaveOccurred())
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

	When("Label is enabled and ConfigMap exists", func() {
		It("should mount Feast config successfully", func() {
			notebookName := "test-notebook-feast-enabled"
			configMapName := notebookName + feastConfigMapSuffix

			// Create ConfigMap with expected naming convention
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
			err := cli.Create(ctx, configMap)
			if err != nil && !apierrs.IsAlreadyExists(err) {
				Expect(err).ToNot(HaveOccurred())
			}

			// Create Notebook with Feast label enabled
			notebook := &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Name:      notebookName,
					Namespace: testNamespace,
					Labels: map[string]string{
						feastLabelKey: "true", // Enable Feast mounting
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
			err = cli.Create(ctx, notebook)
			if err != nil && !apierrs.IsAlreadyExists(err) {
				Expect(err).ToNot(HaveOccurred())
			}

			// Replicate webhook flow: Check label first
			if isFeastEnabled(notebook) {
				err := NewFeastConfig(ctx, cli, notebook)
				Expect(err).ToNot(HaveOccurred())
			} else {
				Fail("Feast should be enabled but isFeastEnabled returned false")
			}

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
			Expect(foundVolume).To(BeTrue(), "Feast config volume should be added when label is enabled")

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
		})
	})

	When("Label is enabled but ConfigMap does not exist", func() {
		It("should mount volume reference (pod will fail to start if ConfigMap missing)", func() {
			notebookName := "test-notebook-feast-no-cm"

			// Create Notebook with Feast label enabled but no ConfigMap
			notebook := &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Name:      notebookName,
					Namespace: testNamespace,
					Labels: map[string]string{
						feastLabelKey: "true", // Enable Feast mounting
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

			// Replicate webhook flow: Check label first
			if isFeastEnabled(notebook) {
				err := NewFeastConfig(ctx, cli, notebook)
				// Should not error - volume reference is added regardless of ConfigMap existence
				Expect(err).ToNot(HaveOccurred())
			} else {
				Fail("Feast should be enabled but isFeastEnabled returned false")
			}

			// Verify that volume reference was added (even though ConfigMap doesn't exist)
			// Note: Pod will fail to start at runtime if ConfigMap is missing
			volumes := notebook.Spec.Template.Spec.Volumes
			foundVolume := false
			for _, v := range volumes {
				if v.Name == feastConfigVolumeName {
					foundVolume = true
					Expect(v.ConfigMap).ToNot(BeNil())
					Expect(v.ConfigMap.Name).To(Equal(notebookName + feastConfigMapSuffix))
					break
				}
			}
			Expect(foundVolume).To(BeTrue(), "Feast config volume reference should be added even if ConfigMap doesn't exist yet")
		})
	})

	When("Label is disabled (any reason)", func() {
		It("should skip Feast mounting when label is not 'true'", func() {
			// Test that isFeastEnabled returns false for various non-"true" label states
			// Detailed label behavior is tested in isFeastEnabled unit tests
			notebookName := "test-notebook-feast-disabled"

			notebook := &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Name:      notebookName,
					Namespace: testNamespace,
					// No label - Feast mounting disabled
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

			// Replicate webhook flow: Check label first
			Expect(isFeastEnabled(notebook)).To(BeFalse())

			// Verify no volume is added when label is disabled
			volumes := notebook.Spec.Template.Spec.Volumes
			for _, v := range volumes {
				Expect(v.Name).ToNot(Equal(feastConfigVolumeName), "Feast config volume should not be added when label is disabled")
			}
		})
	})

	When("Label is disabled after being enabled (unmount scenarios)", func() {
		It("should unmount Feast config when label changes or is removed", func() {
			notebookName := "test-notebook-feast-unmount"
			configMapName := notebookName + feastConfigMapSuffix

			// Create ConfigMap
			configMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: testNamespace,
				},
				Data: map[string]string{
					"feature_store.yaml": "project: feast_project",
				},
			}
			err := cli.Create(ctx, configMap)
			if err != nil && !apierrs.IsAlreadyExists(err) {
				Expect(err).ToNot(HaveOccurred())
			}

			// Create Notebook with Feast enabled initially
			notebook := &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Name:      notebookName,
					Namespace: testNamespace,
					Labels: map[string]string{
						feastLabelKey: "true", // Initially enabled
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

			// Step 1: Mount Feast config (label = true)
			if isFeastEnabled(notebook) {
				err := NewFeastConfig(ctx, cli, notebook)
				Expect(err).ToNot(HaveOccurred())
			}
			Expect(isFeastMounted(notebook)).To(BeTrue(), "Feast should be mounted when label is true")

			// Step 2: Disable label (simulate user changing label to false or removing it)
			notebook.Labels[feastLabelKey] = "false"

			// Replicate webhook flow: Check if should unmount
			if isFeastEnabled(notebook) {
				Fail("Label should be disabled")
			} else if isFeastMounted(notebook) {
				unmountFeastConfig(notebook)
			}

			// Verify volume was unmounted
			Expect(isFeastMounted(notebook)).To(BeFalse(), "Feast should be unmounted when label is disabled")
			for _, v := range notebook.Spec.Template.Spec.Volumes {
				Expect(v.Name).ToNot(Equal(feastConfigVolumeName), "Feast config volume should be removed")
			}

			// Verify volume mount was removed from container
			for _, container := range notebook.Spec.Template.Spec.Containers {
				if container.Name == notebookName {
					for _, vm := range container.VolumeMounts {
						Expect(vm.Name).ToNot(Equal(feastConfigVolumeName), "Feast config volume mount should be removed")
					}
				}
			}
		})
	})

	When("Volume is already mounted but label is disabled on creation", func() {
		It("should unmount Feast config (handle edge case)", func() {
			notebookName := "test-notebook-feast-edge-case"

			// Create Notebook with Feast volume pre-mounted but label disabled
			// (edge case: maybe from a previous state or manual edit)
			notebook := &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Name:      notebookName,
					Namespace: testNamespace,
					Labels: map[string]string{
						feastLabelKey: "false", // Label disabled
					},
				},
				Spec: nbv1.NotebookSpec{
					Template: nbv1.NotebookTemplateSpec{
						Spec: corev1.PodSpec{
							Volumes: []corev1.Volume{
								{
									Name: feastConfigVolumeName, // Volume exists
									VolumeSource: corev1.VolumeSource{
										ConfigMap: &corev1.ConfigMapVolumeSource{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: "some-config",
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
											MountPath: feastConfigMountPath,
										},
									},
								},
							},
						},
					},
				},
			}

			// Verify precondition: volume is mounted but label is disabled
			Expect(isFeastMounted(notebook)).To(BeTrue())
			Expect(isFeastEnabled(notebook)).To(BeFalse())

			// Replicate webhook flow: should unmount
			if isFeastEnabled(notebook) {
				Fail("Should not mount when label is disabled")
			} else if isFeastMounted(notebook) {
				unmountFeastConfig(notebook)
			}

			// Verify unmounted
			Expect(isFeastMounted(notebook)).To(BeFalse(), "Feast should be unmounted to match label state")
		})
	})
})
