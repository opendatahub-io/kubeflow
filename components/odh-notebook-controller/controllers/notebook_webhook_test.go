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
	"fmt"
	"time"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	imagev1 "github.com/openshift/api/image/v1"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("The Openshift Notebook webhook", func() {
	When("Creating a Notebook with internal registry disabled", func() {
		const (
			Name      = "test-notebook-with-last-image-selection"
			Namespace = "default"
		)

		BeforeEach(func() {
			// namespaces in envtest cannot be deleted https://github.com/kubernetes-sigs/controller-runtime/issues/880
			err := cli.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: odhNotebookControllerTestNamespace}}, &client.CreateOptions{})
			if err != nil && !apierrs.IsAlreadyExists(err) {
				Expect(err).ToNot(HaveOccurred())
			}
		})

		var newImageStream = func(name, namespace, tag, dockerImageReference string) *imagev1.ImageStream {
			return &imagev1.ImageStream{
				TypeMeta: metav1.TypeMeta{
					Kind:       "ImageStream",
					APIVersion: "image.openshift.io/v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
				},
				Spec: imagev1.ImageStreamSpec{
					LookupPolicy: imagev1.ImageLookupPolicy{
						Local: true,
					},
				},
				Status: imagev1.ImageStreamStatus{
					Tags: []imagev1.NamedTagEventList{{
						Tag: tag,
						Items: []imagev1.TagEvent{{
							DockerImageReference: dockerImageReference,
						}},
					}},
				},
			}
		}

		var newNotebook = func(annotations map[string]string, initialImage string) *nbv1.Notebook {
			return &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Name:        Name,
					Namespace:   Namespace,
					Annotations: annotations,
				},
				Spec: nbv1.NotebookSpec{
					Template: nbv1.NotebookTemplateSpec{
						Spec: corev1.PodSpec{Containers: []corev1.Container{{
							Name:  Name,
							Image: initialImage,
						}}},
					},
				},
			}
		}

		// https://go.dev/wiki/TableDrivenTests
		testCases := []struct {
			name string

			imageStreams []*imagev1.ImageStream
			notebook     *nbv1.Notebook

			// currently we expect that Notebook CR is always created,
			// and when unable to resolve ImageStream, image: is left alone
			expectedImage string
			// see https://www.youtube.com/watch?v=prLRI3VEVq4 for Observability Driven Development intro
			expectedEvents   []string
			unexpectedEvents []string
		}{
			{
				name: "ImageStream with all that is needful is correctly resolved",

				// The first test case does not use the CR creation helpers
				// so that everything going in is fully visible
				imageStreams: []*imagev1.ImageStream{{
					TypeMeta: metav1.TypeMeta{
						Kind:       "ImageStream",
						APIVersion: "image.openshift.io/v1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:      "some-image",
						Namespace: "redhat-ods-applications",
					},
					Spec: imagev1.ImageStreamSpec{
						LookupPolicy: imagev1.ImageLookupPolicy{
							Local: true,
						},
					},
					Status: imagev1.ImageStreamStatus{
						Tags: []imagev1.NamedTagEventList{{
							Tag: "some-tag",
							Items: []imagev1.TagEvent{{
								Created:              toMetav1Time("2024-10-03T08:10:22Z"),
								DockerImageReference: "quay.io/modh/odh-generic-data-science-notebook@sha256:76e6af79c601a323f75a58e7005de0beac66b8cccc3d2b67efb6d11d85f0cfa1",
								Image:                "sha256:76e6af79c601a323f75a58e7005de0beac66b8cccc3d2b67efb6d11d85f0cfa1",
								Generation:           2,
							}},
							Conditions: []imagev1.TagEventCondition{{
								Type:               "ImportSuccess",
								Status:             "False",
								LastTransitionTime: toMetav1Time("2025-03-11T08:50:51Z"),
								Reason:             "NotFound",
							}},
						}},
					},
				}},
				notebook: &nbv1.Notebook{
					ObjectMeta: metav1.ObjectMeta{
						Name:      Name,
						Namespace: Namespace,
						Annotations: map[string]string{
							"notebooks.opendatahub.io/last-image-selection": "some-image:some-tag",
							// dashboard gives an empty string here to mean the image is from the operator's namespace
							"opendatahub.io/workbench-image-namespace": "",
						},
					},
					Spec: nbv1.NotebookSpec{
						Template: nbv1.NotebookTemplateSpec{
							Spec: corev1.PodSpec{Containers: []corev1.Container{{
								Name:  Name,
								Image: ":some-tag",
							}}}},
					},
				},

				expectedImage: "quay.io/modh/odh-generic-data-science-notebook@sha256:76e6af79c601a323f75a58e7005de0beac66b8cccc3d2b67efb6d11d85f0cfa1",
				unexpectedEvents: []string{
					IMAGE_STREAM_NOT_FOUND_EVENT,
				},
			},
			{
				name: "ImageStream with a tag without items (RHOAIENG-13916)",

				imageStreams: []*imagev1.ImageStream{{
					TypeMeta: metav1.TypeMeta{
						Kind:       "ImageStream",
						APIVersion: "image.openshift.io/v1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:      "some-image",
						Namespace: "redhat-ods-applications",
					},
					Spec: imagev1.ImageStreamSpec{
						LookupPolicy: imagev1.ImageLookupPolicy{
							Local: true,
						},
					},
					Status: imagev1.ImageStreamStatus{
						Tags: []imagev1.NamedTagEventList{{
							Tag:   "some-tag",
							Items: nil,
							Conditions: []imagev1.TagEventCondition{{
								Type:               "ImportSuccess",
								Status:             "False",
								LastTransitionTime: toMetav1Time("2025-03-11T08:50:51Z"),
								Reason:             "",
							}},
						}},
					},
				}},
				notebook: newNotebook(map[string]string{
					"notebooks.opendatahub.io/last-image-selection": "some-image:some-tag",
				}, ":some-tag"),

				// there is no update to the Notebook
				expectedImage: ":some-tag",
				expectedEvents: []string{
					IMAGE_STREAM_TAG_NOT_FOUND_EVENT,
				},
			},
			//
			// Tests for RHOAIENG-23609 project-scoped workbench imagestreams feature,
			// tasks RHOAIENG-23765, and RHOAIENG-25707
			//
			{
				name: "ImageStream in the same namespace as the Notebook",

				imageStreams: []*imagev1.ImageStream{
					newImageStream("some-image", Namespace, "some-tag", "quay.io/modh/odh-generic-data-science-notebook@sha256:5999547f847ca841fe067ff84e2972d2cbae598066c2418e236448e115c1728e"),
				},
				notebook: newNotebook(map[string]string{
					"notebooks.opendatahub.io/last-image-selection": "some-image:some-tag",
					"opendatahub.io/workbench-image-namespace":      Namespace,
				}, ":some-tag"),

				expectedImage: "quay.io/modh/odh-generic-data-science-notebook@sha256:5999547f847ca841fe067ff84e2972d2cbae598066c2418e236448e115c1728e",
				unexpectedEvents: []string{
					IMAGE_STREAM_NOT_FOUND_EVENT,
				},
			},
			//
			// The following test cases require
			// multiple ImageStreams to be created.
			//
			{
				name: "last-image-selection is specified, workbench-image-namespace is also specified",

				imageStreams: []*imagev1.ImageStream{
					newImageStream("some-image", Namespace, "some-tag", "quay.io/workbench-namespace-imagestream:latest"),
					newImageStream("some-image", odhNotebookControllerTestNamespace, "some-tag", "quay.io/operator-namespace-imagestream:latest"),
				},
				notebook: newNotebook(map[string]string{
					"notebooks.opendatahub.io/last-image-selection": "some-image:some-tag",
					"opendatahub.io/workbench-image-namespace":      Namespace,
				}, ":some-tag"),

				// image is picked from notebook namespace
				expectedImage: "quay.io/workbench-namespace-imagestream:latest",
				unexpectedEvents: []string{
					IMAGE_STREAM_NOT_FOUND_EVENT,
				},
			},
			{
				name: "last-image-selection is specified, workbench-image-namespace is not specified",

				imageStreams: []*imagev1.ImageStream{
					newImageStream("some-image", Namespace, "some-tag", "quay.io/workbench-namespace-imagestream:latest"),
					newImageStream("some-image", odhNotebookControllerTestNamespace, "some-tag", "quay.io/operator-namespace-imagestream:latest"),
				},
				notebook: newNotebook(map[string]string{
					"notebooks.opendatahub.io/last-image-selection": "some-image:some-tag",
					"opendatahub.io/workbench-image-namespace":      "",
				}, ":some-tag"),

				// image is picked from operator namespace
				expectedImage: "quay.io/operator-namespace-imagestream:latest",
				unexpectedEvents: []string{
					IMAGE_STREAM_NOT_FOUND_EVENT,
				},
			},
			//
			// Something that Dashboard should never do, but we want to check it nevertheless
			//
			{
				name: "last-image-selection is not specified",

				imageStreams: []*imagev1.ImageStream{
					newImageStream("some-image", Namespace, "some-tag", "quay.io/modh/odh-generic-data-science-notebook@sha256:5999547f847ca841fe067ff84e2972d2cbae598066c2418e236448e115c1728e"),
				},
				notebook: newNotebook(map[string]string{
					"opendatahub.io/workbench-image-namespace": Namespace,
				}, ":some-tag"),

				// there is no update to the Notebook
				expectedImage: ":some-tag",
				unexpectedEvents: []string{
					IMAGE_STREAM_NOT_FOUND_EVENT,
				},
			},
		}

		BeforeEach(func() {
			Expect(tracings.TraceProvider.ForceFlush(ctx)).To(Succeed())
			tracings.SpanExporter.Reset()
		})

		for _, testCase := range testCases {
			Context(fmt.Sprintf("The Notebook webhook test case: %s", testCase.name), func() {
				BeforeEach(func() {
					By("Creating the requisite ImageStream resources successfully")
					for _, imageStream := range testCase.imageStreams {
						Expect(cli.Create(ctx, imageStream, &client.CreateOptions{})).To(Succeed())
					}
				})
				It("Should create a Notebook resource successfully", func() {
					// if our webhook panics, then cli.Create will err
					By("Creating a Notebook resource successfully")
					Expect(cli.Create(ctx, testCase.notebook, &client.CreateOptions{})).To(Succeed())

					By("Checking that the webhook modified the notebook CR with the expected image")
					Expect(testCase.notebook.Spec.Template.Spec.Containers[0].Image).To(Equal(testCase.expectedImage))

					By("Checking telemetry events")
					Expect(tracings.TraceProvider.ForceFlush(ctx)).To(Succeed())
					spans := tracings.SpanExporter.GetSpans()
					events := make([]string, 0)
					for _, span := range spans {
						for _, event := range span.Events {
							events = append(events, event.Name)
						}
					}
					Expect(events).To(ContainElements(testCase.expectedEvents))
					for _, unexpectedEvent := range testCase.unexpectedEvents {
						Expect(events).ToNot(ContainElement(unexpectedEvent))
					}
				})
				AfterEach(func() {
					By("Deleting the created resources")
					Expect(cli.Delete(ctx, testCase.notebook, &client.DeleteOptions{})).To(Succeed())
					for _, imageStream := range testCase.imageStreams {
						Expect(cli.Delete(ctx, imageStream, &client.DeleteOptions{})).To(Succeed())
					}
				})
			})
		}
	})
})

var _ = Describe("RBAC Proxy Resource Configuration Integration", func() {
	Context("RBAC proxy injection with resource configuration", func() {
		It("should inject complete RBAC proxy with default resources and all required components", func() {
			notebook := &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-notebook",
					Namespace: "default",
				},
				Spec: nbv1.NotebookSpec{
					Template: nbv1.NotebookTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test-notebook",
									Image: "test-image",
								},
							},
						},
					},
				},
			}

			rbac := RbacConfig{
				ProxyImage: "test-rbac-image",
			}

			err := InjectRbacProxy(notebook, rbac)
			Expect(err).ToNot(HaveOccurred())

			// Verify RBAC proxy container injection
			var rbacContainer corev1.Container
			found := false
			for _, container := range notebook.Spec.Template.Spec.Containers {
				if container.Name == "kube-rbac-proxy" {
					rbacContainer = container
					found = true
					break
				}
			}
			Expect(found).To(BeTrue())

			// Verify container configuration
			Expect(rbacContainer.Image).To(Equal("test-rbac-image"))
			Expect(rbacContainer.ImagePullPolicy).To(Equal(corev1.PullAlways))

			// Verify default resources are applied
			Expect(rbacContainer.Resources.Requests.Cpu().String()).To(Equal("100m"))
			Expect(rbacContainer.Resources.Requests.Memory().String()).To(Equal("64Mi"))
			Expect(rbacContainer.Resources.Limits.Cpu().String()).To(Equal("100m"))
			Expect(rbacContainer.Resources.Limits.Memory().String()).To(Equal("64Mi"))

			// Verify ports configuration
			Expect(rbacContainer.Ports).To(HaveLen(1))
			Expect(rbacContainer.Ports[0].Name).To(Equal("kube-rbac-proxy"))
			Expect(rbacContainer.Ports[0].ContainerPort).To(Equal(int32(8443)))

			// Verify volume mounts
			Expect(rbacContainer.VolumeMounts).To(HaveLen(2))
			mountNames := make(map[string]bool)
			for _, mount := range rbacContainer.VolumeMounts {
				mountNames[mount.Name] = true
			}
			Expect(mountNames["rbac-proxy-config"]).To(BeTrue())
			Expect(mountNames["rbac-tls-certificates"]).To(BeTrue())

			// Verify volumes were added
			Expect(notebook.Spec.Template.Spec.Volumes).To(HaveLen(2))
			volumeNames := make(map[string]bool)
			for _, volume := range notebook.Spec.Template.Spec.Volumes {
				volumeNames[volume.Name] = true
			}
			Expect(volumeNames["rbac-proxy-config"]).To(BeTrue())
			Expect(volumeNames["rbac-tls-certificates"]).To(BeTrue())

			// Verify service account
			Expect(notebook.Spec.Template.Spec.ServiceAccountName).To(Equal("test-notebook"))

			// Verify probes are configured
			Expect(rbacContainer.LivenessProbe).ToNot(BeNil())
			Expect(rbacContainer.ReadinessProbe).ToNot(BeNil())
		})

		It("should inject RBAC proxy with custom resources and maintain existing containers", func() {
			notebook := &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-notebook",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationAuthSidecarCPURequest:    "200m",
						AnnotationAuthSidecarMemoryRequest: "128Mi",
						AnnotationAuthSidecarCPULimit:      "500m",
						AnnotationAuthSidecarMemoryLimit:   "256Mi",
					},
				},
				Spec: nbv1.NotebookSpec{
					Template: nbv1.NotebookTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test-notebook",
									Image: "test-image",
								},
								{
									Name:  "sidecar",
									Image: "sidecar-image",
								},
							},
							Volumes: []corev1.Volume{
								{
									Name: "existing-volume",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
							},
						},
					},
				},
			}

			rbac := RbacConfig{
				ProxyImage: "test-rbac-image",
			}

			err := InjectRbacProxy(notebook, rbac)
			Expect(err).ToNot(HaveOccurred())

			// Verify total container count (2 original + 1 kube-rbac-proxy)
			Expect(notebook.Spec.Template.Spec.Containers).To(HaveLen(3))

			// Find RBAC proxy container
			var rbacContainer corev1.Container
			found := false
			for _, container := range notebook.Spec.Template.Spec.Containers {
				if container.Name == "kube-rbac-proxy" {
					rbacContainer = container
					found = true
					break
				}
			}
			Expect(found).To(BeTrue())

			// Verify custom resources are applied
			Expect(rbacContainer.Resources.Requests.Cpu().String()).To(Equal("200m"))
			Expect(rbacContainer.Resources.Requests.Memory().String()).To(Equal("128Mi"))
			Expect(rbacContainer.Resources.Limits.Cpu().String()).To(Equal("500m"))
			Expect(rbacContainer.Resources.Limits.Memory().String()).To(Equal("256Mi"))

			// Verify original containers are preserved
			originalContainerNames := make(map[string]bool)
			for _, container := range notebook.Spec.Template.Spec.Containers {
				originalContainerNames[container.Name] = true
			}
			Expect(originalContainerNames["test-notebook"]).To(BeTrue())
			Expect(originalContainerNames["sidecar"]).To(BeTrue())

			// Verify volumes include both original and RBAC volumes
			Expect(notebook.Spec.Template.Spec.Volumes).To(HaveLen(3)) // 1 original + 2 RBAC volumes
			volumeNames := make(map[string]bool)
			for _, volume := range notebook.Spec.Template.Spec.Volumes {
				volumeNames[volume.Name] = true
			}
			Expect(volumeNames["existing-volume"]).To(BeTrue())
			Expect(volumeNames["rbac-proxy-config"]).To(BeTrue())
			Expect(volumeNames["rbac-tls-certificates"]).To(BeTrue())
		})

		It("should fail injection early and preserve original notebook when resource validation fails", func() {
			originalNotebook := &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-notebook",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationAuthSidecarCPURequest: "500m",
						AnnotationAuthSidecarCPULimit:   "200m", // limit < request - validation failure
					},
				},
				Spec: nbv1.NotebookSpec{
					Template: nbv1.NotebookTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test-notebook",
									Image: "test-image",
								},
							},
						},
					},
				},
			}

			// Create a deep copy to verify no mutations on failure
			notebook := originalNotebook.DeepCopy()

			rbac := RbacConfig{
				ProxyImage: "test-rbac-image",
			}

			err := InjectRbacProxy(notebook, rbac)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid RBAC proxy resource configuration"))

			// Verify no RBAC proxy container was added on failure
			rbacFound := false
			for _, container := range notebook.Spec.Template.Spec.Containers {
				if container.Name == "kube-rbac-proxy" {
					rbacFound = true
					break
				}
			}
			Expect(rbacFound).To(BeFalse())

			// Verify no RBAC volumes were added on failure
			for _, volume := range notebook.Spec.Template.Spec.Volumes {
				Expect(volume.Name).ToNot(ContainSubstring("rbac"))
			}

			// Verify original containers remain unchanged
			Expect(notebook.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(notebook.Spec.Template.Spec.Containers[0].Name).To(Equal("test-notebook"))

			// Verify service account was not modified
			Expect(notebook.Spec.Template.Spec.ServiceAccountName).To(Equal(""))
		})

		It("should update existing kube-rbac-proxy container with new configuration while preserving container count", func() {
			notebook := &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-notebook",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationAuthSidecarCPURequest:  "300m",
						AnnotationAuthSidecarCPULimit:    "600m",
						AnnotationAuthSidecarMemoryLimit: "512Mi",
					},
				},
				Spec: nbv1.NotebookSpec{
					Template: nbv1.NotebookTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test-notebook",
									Image: "test-image",
								},
								{
									Name:  "kube-rbac-proxy",
									Image: "old-rbac-image",
									Args:  []string{"--old-arg=value"},
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("50m"),
											corev1.ResourceMemory: resource.MustParse("32Mi"),
										},
									},
									Ports: []corev1.ContainerPort{
										{
											Name:          "old-port",
											ContainerPort: 8080,
										},
									},
								},
							},
							Volumes: []corev1.Volume{
								{
									Name: "existing-rbac-config",
									VolumeSource: corev1.VolumeSource{
										ConfigMap: &corev1.ConfigMapVolumeSource{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: "old-config",
											},
										},
									},
								},
							},
						},
					},
				},
			}

			rbac := RbacConfig{
				ProxyImage: "new-rbac-image",
			}

			err := InjectRbacProxy(notebook, rbac)
			Expect(err).ToNot(HaveOccurred())

			// Verify container count remains the same (update, not add)
			Expect(notebook.Spec.Template.Spec.Containers).To(HaveLen(2))

			// Find the updated RBAC proxy container
			var rbacContainer corev1.Container
			found := false
			for _, container := range notebook.Spec.Template.Spec.Containers {
				if container.Name == "kube-rbac-proxy" {
					rbacContainer = container
					found = true
					break
				}
			}
			Expect(found).To(BeTrue())

			// Verify complete container replacement with new configuration
			Expect(rbacContainer.Image).To(Equal("new-rbac-image"))
			Expect(rbacContainer.ImagePullPolicy).To(Equal(corev1.PullAlways))

			// Verify resource updates (custom + defaults)
			Expect(rbacContainer.Resources.Requests.Cpu().String()).To(Equal("300m"))
			Expect(rbacContainer.Resources.Requests.Memory().String()).To(Equal("64Mi")) // default
			Expect(rbacContainer.Resources.Limits.Cpu().String()).To(Equal("600m"))
			Expect(rbacContainer.Resources.Limits.Memory().String()).To(Equal("512Mi")) // custom

			// Verify args were completely replaced (not appended)
			Expect(rbacContainer.Args).ToNot(ContainElement("--old-arg=value"))
			Expect(len(rbacContainer.Args)).To(BeNumerically(">", 5)) // Should have full RBAC arg set

			// Verify ports were replaced with RBAC proxy port
			Expect(rbacContainer.Ports).To(HaveLen(1))
			Expect(rbacContainer.Ports[0].Name).To(Equal("kube-rbac-proxy"))
			Expect(rbacContainer.Ports[0].ContainerPort).To(Equal(int32(8443)))

			// Verify volumes were properly managed (existing volumes updated/replaced as needed)
			volumeNames := make(map[string]bool)
			for _, volume := range notebook.Spec.Template.Spec.Volumes {
				volumeNames[volume.Name] = true
			}
			// Should have the RBAC volumes (old rbac-config should be replaced/updated)
			Expect(volumeNames["rbac-proxy-config"]).To(BeTrue())
			Expect(volumeNames["rbac-tls-certificates"]).To(BeTrue())
		})
	})
})

func toMetav1Time(timeString string) metav1.Time {
	parsedTime, err := time.Parse(time.RFC3339, timeString)
	Expect(err).ToNot(HaveOccurred())
	return metav1.NewTime(parsedTime)
}
