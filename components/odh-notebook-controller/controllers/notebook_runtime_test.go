package controllers

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	imagev1 "github.com/openshift/api/image/v1"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	resource_reconciliation_timeout      = time.Second * 30
	resource_reconciliation_check_period = time.Second * 2
	Namespace                            = "default"
	RuntimeImagesCMName                  = "pipeline-runtime-images"
	expectedMountName                    = "runtime-images"
	expectedMountPath                    = "/opt/app-root/pipeline-runtimes/"
)

var _ = Describe("Runtime images ConfigMap should be mounted", func() {
	When("Empty ConfigMap for runtime images", func() {

		BeforeEach(func() {
			err := cli.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: odhNotebookControllerTestNamespace}}, &client.CreateOptions{})
			if err != nil && !apierrs.IsAlreadyExists(err) {
				Expect(err).ToNot(HaveOccurred())
			}
		})

		testCases := []struct {
			name         string
			notebook     *nbv1.Notebook
			notebookName string
			ConfigMap    *corev1.ConfigMap
		}{
			{
				name:         "ConfigMap without data",
				notebookName: "test-notebook-runtime-empty-cf",
				ConfigMap: &corev1.ConfigMap{
					TypeMeta: metav1.TypeMeta{
						Kind:       "ConfigMap",
						APIVersion: "v1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:      RuntimeImagesCMName,
						Namespace: Namespace,
					},
					Data: map[string]string{},
				},
			},
		}

		for _, testCase := range testCases {
			Context(fmt.Sprintf("The Notebook runtime pipeline images ConfigMap test case: %s", testCase.name), func() {
				notebook := createNotebook(testCase.notebookName, Namespace)
				configMap := &corev1.ConfigMap{}
				It(fmt.Sprintf("Should mount ConfigMap correctly: %s", testCase.name), func() {

					// cleanup first
					_ = cli.Delete(ctx, notebook, &client.DeleteOptions{})
					_ = cli.Delete(ctx, testCase.ConfigMap, &client.DeleteOptions{})

					// wait until deleted
					By("Waiting for the Notebook to be deleted")
					Eventually(func(g Gomega) {
						err := cli.Get(ctx, client.ObjectKey{Name: testCase.notebookName, Namespace: Namespace}, &nbv1.Notebook{})
						g.Expect(apierrs.IsNotFound(err)).To(BeTrue(), fmt.Sprintf("expected Notebook %q to be deleted", testCase.notebookName))
					}).WithOffset(1).Should(Succeed())

					// test code start
					By("Create the ConfigMap directly")
					Expect(cli.Create(ctx, testCase.ConfigMap)).To(Succeed())

					By("Creating the Notebook")
					Expect(cli.Create(ctx, notebook)).To(Succeed())

					By("Fetching the ConfigMap for the runtime images")
					Eventually(func(g Gomega) {
						err := cli.Get(ctx, client.ObjectKey{Name: RuntimeImagesCMName, Namespace: Namespace}, configMap)
						g.Expect(err).ToNot(HaveOccurred())
					}, resource_reconciliation_timeout, resource_reconciliation_check_period).Should(Succeed())

					// Check volumeMounts
					By("Fetching the created Notebook CR as typed object and volumeMounts check")
					typedNotebook := &nbv1.Notebook{}
					Eventually(func(g Gomega) {
						err := cli.Get(ctx, client.ObjectKey{Name: testCase.notebookName, Namespace: Namespace}, typedNotebook)
						g.Expect(err).ToNot(HaveOccurred())

						c := typedNotebook.Spec.Template.Spec.Containers[0]

						foundMount := false
						for _, vm := range c.VolumeMounts {
							if vm.Name == expectedMountName && vm.MountPath == expectedMountPath {
								foundMount = true
							}
						}

						g.Expect(foundMount).To(BeFalse(), "unexpected VolumeMount found")
					}, resource_reconciliation_timeout, resource_reconciliation_check_period).Should(Succeed())

					// Check volumes
					foundVolume := false
					for _, v := range typedNotebook.Spec.Template.Spec.Volumes {
						if v.Name == expectedMountName && v.ConfigMap != nil && v.ConfigMap.Name == RuntimeImagesCMName {
							foundVolume = true
						}
					}
					Expect(foundVolume).To(BeFalse(), "unexpected ConfigMap volume found")
				})
				AfterEach(func() {
					By("Deleting the created resources")
					Expect(cli.Delete(ctx, notebook, &client.DeleteOptions{})).To(Succeed())
					Expect(cli.Delete(ctx, configMap, &client.DeleteOptions{})).To(Succeed())
				})
			})
		}
	})

	When("Creating a Notebook", func() {

		BeforeEach(func() {
			err := cli.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: odhNotebookControllerTestNamespace}}, &client.CreateOptions{})
			if err != nil && !apierrs.IsAlreadyExists(err) {
				Expect(err).ToNot(HaveOccurred())
			}
		})

		testCases := []struct {
			name                  string
			notebook              *nbv1.Notebook
			notebookName          string
			expectedConfigMapData map[string]string
			imageStream           *imagev1.ImageStream
		}{
			{
				name:         "ImageStream with two tags",
				notebookName: "test-notebook-runtime-1",
				expectedConfigMapData: map[string]string{
					"python-3.11-ubi9.json":        `{"display_name":"Python 3.11 (UBI9)","metadata":{"display_name":"Python 3.11 (UBI9)","image_name":"quay.io/opendatahub/test","pull_policy":"IfNotPresent","tags":["some-tag"]},"schema_name":"runtime-image"}`,
					"hohoho-python-3.12-ubi9.json": `{"display_name":"Hohoho Python 3.12 (UBI9)","metadata":{"display_name":"Python 3.12 (UBI9)","image_name":"quay.io/opendatahub/test2","pull_policy":"IfNotPresent","tags":["some-tag2"]},"schema_name":"runtime-image"}`,
				},
				imageStream: &imagev1.ImageStream{
					TypeMeta: metav1.TypeMeta{
						Kind:       "ImageStream",
						APIVersion: "image.openshift.io/v1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:      "some-image",
						Namespace: "redhat-ods-applications",
						Labels: map[string]string{
							"opendatahub.io/runtime-image": "true",
						},
					},
					Spec: imagev1.ImageStreamSpec{
						LookupPolicy: imagev1.ImageLookupPolicy{
							Local: true,
						},
						Tags: []imagev1.TagReference{
							{
								Name: "some-tag",
								Annotations: map[string]string{
									"opendatahub.io/runtime-image-metadata": `
										[
											{
												"display_name": "Python 3.11 (UBI9)",
												"metadata": {
													"tags": [
														"some-tag"
													],
													"display_name": "Python 3.11 (UBI9)",
													"pull_policy": "IfNotPresent"
												},
												"schema_name": "runtime-image"
											}
										]
									`,
								},
								From: &corev1.ObjectReference{
									Kind: "DockerImage",
									Name: "quay.io/opendatahub/test",
								},
							},
							{
								Name: "some-tag2",
								Annotations: map[string]string{
									"opendatahub.io/runtime-image-metadata": `
										[
											{
												"display_name": "Hohoho Python 3.12 (UBI9)",
												"metadata": {
													"tags": [
														"some-tag2"
													],
													"display_name": "Python 3.12 (UBI9)",
													"pull_policy": "IfNotPresent"
												},
												"schema_name": "runtime-image"
											}
										]
									`,
								},
								From: &corev1.ObjectReference{
									Kind: "DockerImage",
									Name: "quay.io/opendatahub/test2",
								},
							},
						},
					},
				},
			},
			{
				name:         "ImageStream with one tag",
				notebookName: "test-notebook-runtime-2",
				expectedConfigMapData: map[string]string{
					"python-3.11-ubi9.json": `{"display_name":"Python 3.11 (UBI9)","metadata":{"display_name":"Python 3.11 (UBI9)","image_name":"quay.io/modh/odh-pipeline-runtime-datascience-cpu-py311-ubi9@sha256:5aa8868be00f304084ce6632586c757bc56b28300779495d14b08bcfbcd3357f","pull_policy":"IfNotPresent","tags":["some-tag"]},"schema_name":"runtime-image"}`,
				},
				imageStream: &imagev1.ImageStream{
					TypeMeta: metav1.TypeMeta{
						Kind:       "ImageStream",
						APIVersion: "image.openshift.io/v1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:      "some-image",
						Namespace: "redhat-ods-applications",
						Labels: map[string]string{
							"opendatahub.io/runtime-image": "true",
						},
					},
					Spec: imagev1.ImageStreamSpec{
						LookupPolicy: imagev1.ImageLookupPolicy{
							Local: true,
						},
						Tags: []imagev1.TagReference{
							{
								Name: "some-tag",
								Annotations: map[string]string{
									"opendatahub.io/runtime-image-metadata": `
										[
											{
												"display_name": "Python 3.11 (UBI9)",
												"metadata": {
													"tags": [
														"some-tag"
													],
													"display_name": "Python 3.11 (UBI9)",
													"pull_policy": "IfNotPresent"
												},
												"schema_name": "runtime-image"
											}
										]
									`,
								},
								From: &corev1.ObjectReference{
									Kind: "DockerImage",
									Name: "quay.io/modh/odh-pipeline-runtime-datascience-cpu-py311-ubi9@sha256:5aa8868be00f304084ce6632586c757bc56b28300779495d14b08bcfbcd3357f",
								},
							},
						},
					},
				},
			},
			{
				name:                  "ImageStream with irrelevant data",
				notebookName:          "test-notebook-runtime-3",
				expectedConfigMapData: nil,
				imageStream: &imagev1.ImageStream{
					TypeMeta: metav1.TypeMeta{
						Kind:       "ImageStream",
						APIVersion: "image.openshift.io/v1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:      "some-image",
						Namespace: "redhat-ods-applications",
						Labels: map[string]string{
							"opendatahub.io/runtime-image": "true",
						},
					},
					Spec: imagev1.ImageStreamSpec{
						LookupPolicy: imagev1.ImageLookupPolicy{
							Local: true,
						},
						Tags: []imagev1.TagReference{
							{
								Name: "some-tag",
								Annotations: map[string]string{
									"opendatahub.io/runtime-image-metadata-fake": `
										[
											{
												"display_name": "Python 3.11 (UBI9)",
												"metadata": {
													"tags": [
														"some-tag"
													],
													"display_name": "Python 3.11 (UBI9)",
													"pull_policy": "IfNotPresent"
												},
												"schema_name": "runtime-image-fake"
											}
										]
									`,
								},
								From: &corev1.ObjectReference{
									Kind: "DockerImage",
									Name: "quay.io/opendatahub/test",
								},
							},
						},
					},
				},
			},
		}

		for _, testCase := range testCases {
			Context(fmt.Sprintf("The Notebook runtime pipeline images ConfigMap test case: %s", testCase.name), func() {
				notebook := createNotebook(testCase.notebookName, Namespace)
				configMap := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      RuntimeImagesCMName,
						Namespace: Namespace,
					},
				}
				It(fmt.Sprintf("Should mount ConfigMap correctly: %s", testCase.name), func() {

					// cleanup first
					_ = cli.Delete(ctx, notebook, &client.DeleteOptions{})
					_ = cli.Delete(ctx, testCase.imageStream, &client.DeleteOptions{})
					_ = cli.Delete(ctx, configMap, &client.DeleteOptions{})

					// wait until deleted
					By("Waiting for the Notebook to be deleted")
					Eventually(func(g Gomega) {
						err := cli.Get(ctx, client.ObjectKey{Name: testCase.notebookName, Namespace: Namespace}, &nbv1.Notebook{})
						g.Expect(apierrs.IsNotFound(err)).To(BeTrue(), fmt.Sprintf("expected Notebook %q to be deleted", testCase.notebookName))
					}).WithOffset(1).Should(Succeed())

					// test code start
					By("Create the ImageStream")
					Expect(cli.Create(ctx, testCase.imageStream)).To(Succeed())

					By("Creating the Notebook")
					Expect(cli.Create(ctx, notebook)).To(Succeed())

					By("Fetching the ConfigMap for the runtime images")
					if testCase.expectedConfigMapData != nil {
						Eventually(func(g Gomega) {
							err := cli.Get(ctx, client.ObjectKey{Name: RuntimeImagesCMName, Namespace: Namespace}, configMap)
							g.Expect(err).ToNot(HaveOccurred())
						}, resource_reconciliation_timeout, resource_reconciliation_check_period).Should(Succeed())

						// Let's check the content of the ConfigMap created from the given ImageStream.
						Expect(configMap.GetName()).To(Equal(RuntimeImagesCMName))
						Expect(configMap.Data).To(Equal(testCase.expectedConfigMapData))

						// ----------------------------------------------------------------
						// Test workaround for the RHOAIENG-24545 - we need to manually modify
						// the workbench so that the expected resource is mounted properly
						// kubeflow-resource-stopped: '2025-06-25T13:53:46Z'
						By("Running the workaround for RHOAIENG-24545")
						notebook.Spec.Template.Spec.ServiceAccountName = "foo"
						Expect(cli.Update(ctx, notebook)).Should(Succeed())
						// end of workaround
						// ----------------------------------------------------------------

						// Check volumeMounts
						By("Fetching the created Notebook CR as typed object and volumeMounts check")
						typedNotebook := &nbv1.Notebook{}
						Eventually(func(g Gomega) {
							err := cli.Get(ctx, client.ObjectKey{Name: testCase.notebookName, Namespace: Namespace}, typedNotebook)
							g.Expect(err).ToNot(HaveOccurred())

							c := typedNotebook.Spec.Template.Spec.Containers[0]

							foundMount := false
							for _, vm := range c.VolumeMounts {
								if vm.Name == expectedMountName && vm.MountPath == expectedMountPath {
									foundMount = true
								}
							}
							g.Expect(foundMount).To(BeTrue(), "expected VolumeMount not found")
						}, resource_reconciliation_timeout, resource_reconciliation_check_period).Should(Succeed())

						// Check volumes
						foundVolume := false
						for _, v := range typedNotebook.Spec.Template.Spec.Volumes {
							if v.Name == expectedMountName && v.ConfigMap != nil && v.ConfigMap.Name == RuntimeImagesCMName {
								foundVolume = true
							}
						}
						Expect(foundVolume).To(BeTrue(), "expected ConfigMap volume not found")
					} else {
						// The data in the given ImageStream weren't supposed to create a runtime image configmap
						Eventually(func(g Gomega) {
							err := cli.Get(ctx, client.ObjectKey{Name: RuntimeImagesCMName, Namespace: Namespace}, configMap)
							g.Expect(err).ToNot(HaveOccurred())
						}, resource_reconciliation_timeout, resource_reconciliation_check_period).ShouldNot(Succeed())

						// Check volumeMounts
						By("Fetching the created Notebook CR as typed object and volumeMounts check")
						typedNotebook := &nbv1.Notebook{}
						Eventually(func(g Gomega) {
							err := cli.Get(ctx, client.ObjectKey{Name: testCase.notebookName, Namespace: Namespace}, typedNotebook)
							g.Expect(err).ToNot(HaveOccurred())

							c := typedNotebook.Spec.Template.Spec.Containers[0]

							foundMount := false
							for _, vm := range c.VolumeMounts {
								if vm.Name == expectedMountName && vm.MountPath == expectedMountPath {
									foundMount = true
								}
							}
							g.Expect(foundMount).To(BeTrue(), "expected VolumeMount not found")
						}, resource_reconciliation_timeout, resource_reconciliation_check_period).ShouldNot(Succeed())

						// Check volumes
						foundVolume := false
						for _, v := range typedNotebook.Spec.Template.Spec.Volumes {
							if v.Name == expectedMountName && v.ConfigMap != nil && v.ConfigMap.Name == RuntimeImagesCMName {
								foundVolume = true
							}
						}
						Expect(foundVolume).To(BeFalse(), "expected ConfigMap volume not found")
					}
				})
				AfterEach(func() {
					By("Deleting the created resources")
					Expect(cli.Delete(ctx, notebook, &client.DeleteOptions{})).To(Succeed())
					Expect(cli.Delete(ctx, testCase.imageStream, &client.DeleteOptions{})).To(Succeed())
					if testCase.expectedConfigMapData != nil {
						Expect(cli.Delete(ctx, configMap, &client.DeleteOptions{})).To(Succeed())
					}
				})
			})
		}
	})
})
