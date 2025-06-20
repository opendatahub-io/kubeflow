package controllers

import (
	"context"
	"fmt"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	imagev1 "github.com/openshift/api/image/v1"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("When Creating a notebook should mount the configMap", func() {
	ctx := context.Background()

	When("Creating a Notebook", func() {

		const (
			Namespace           = "default"
			RuntimeImagesCMName = "pipeline-runtime-images"
		)

		BeforeEach(func() {
			err := cli.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: odhNotebookControllerTestNamespace}}, &client.CreateOptions{})
			if err != nil && !apierrs.IsAlreadyExists(err) {
				Expect(err).ToNot(HaveOccurred())
			}
		})

		testCases := []struct {
			name              string
			notebook          *nbv1.Notebook
			notebookName      string
			ConfigMap         *corev1.ConfigMap
			imageStream       *imagev1.ImageStream
			expectedMountName string
			expectedMountPath string
		}{
			{
				name:         "ConfigMap with data",
				notebookName: "test-notebook-runtime-1",
				ConfigMap: &corev1.ConfigMap{
					TypeMeta: metav1.TypeMeta{
						Kind:       "ConfigMap",
						APIVersion: "v1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:      RuntimeImagesCMName,
						Namespace: Namespace,
					},
					Data: map[string]string{
						"datascience.json": `{"image_name":"quay.io/opendatahub/test"}`,
					},
				},
				imageStream:       nil,
				expectedMountName: "runtime-images",
				expectedMountPath: "/opt/app-root/pipeline-runtimes/",
			},
			{
				name:         "ConfigMap without data",
				notebookName: "test-notebook-runtime-2",
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
				imageStream:       nil,
				expectedMountName: "",
				expectedMountPath: "",
			},
			{
				name:         "ConfigMap created from an actual ImageStream",
				notebookName: "test-notebook-runtime-3",
				ConfigMap:    nil,
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
				expectedMountName: "runtime-images",
				expectedMountPath: "/opt/app-root/pipeline-runtimes/",
			},
		}

		for _, testCase := range testCases {
			Context(fmt.Sprintf("The Notebook runtime pipeline images ConfigMap test case: %s", testCase.name), func() {
				notebook := createNotebook(testCase.notebookName, Namespace)
				configMap := &corev1.ConfigMap{}
				It(fmt.Sprintf("Should mount ConfigMap correctly: %s", testCase.name), func() {

					// cleanup first
					_ = cli.Delete(ctx, notebook, &client.DeleteOptions{})
					_ = cli.Delete(ctx, testCase.imageStream, &client.DeleteOptions{})
					_ = cli.Delete(ctx, testCase.ConfigMap, &client.DeleteOptions{})

					// }

					// wait until deleted
					By("Waiting for the Notebook to be deleted")
					Eventually(func(g Gomega) {
						err := cli.Get(ctx, client.ObjectKey{Name: testCase.notebookName, Namespace: Namespace}, &nbv1.Notebook{})
						g.Expect(apierrs.IsNotFound(err)).To(BeTrue(), fmt.Sprintf("expected Notebook %q to be deleted", testCase.notebookName))
					}).WithOffset(1).Should(Succeed())

					// test code start
					if testCase.ConfigMap != nil {
						By("Create the ConfigMap directly")
						Expect(cli.Create(ctx, testCase.ConfigMap)).To(Succeed())
					} else {
						By("Create the ImageStream")
						Expect(cli.Create(ctx, testCase.imageStream)).To(Succeed())
					}

					By("Creating the Notebook")
					Expect(cli.Create(ctx, notebook)).To(Succeed())

					By("Fetching the ConfigMap for the runtime images")
					Eventually(func(g Gomega) {
						err := cli.Get(ctx, client.ObjectKey{Name: RuntimeImagesCMName, Namespace: Namespace}, configMap)
						g.Expect(err).ToNot(HaveOccurred())
					}, timeout, interval).Should(Succeed())

					if testCase.ConfigMap == nil {
						// Let's check the content of the ConfigMap created from the given ImageStream.
						// TODO
						Expect(configMap.GetName()).To(Equal(RuntimeImagesCMName))
						expectedData := map[string]string{
							"python-3.11-ubi9.json": `{"display_name":"Python 3.11 (UBI9)","metadata":{"display_name":"Python 3.11 (UBI9)","image_name":"quay.io/modh/odh-pipeline-runtime-datascience-cpu-py311-ubi9@sha256:5aa8868be00f304084ce6632586c757bc56b28300779495d14b08bcfbcd3357f","pull_policy":"IfNotPresent","tags":["some-tag"]},"schema_name":"runtime-image"}`,
						}
						Expect(configMap.Data).To(Equal(expectedData))
					} else {
						// It doesn't make sense to do any ConfigMap check in this case as we created it directly in the test.
					}

					// Check volumeMounts
					By("Fetching the created Notebook CR as typed object and volumeMounts check")
					typedNotebook := &nbv1.Notebook{}
					Eventually(func(g Gomega) {
						err := cli.Get(ctx, client.ObjectKey{Name: testCase.notebookName, Namespace: Namespace}, typedNotebook)
						g.Expect(err).ToNot(HaveOccurred())

						c := typedNotebook.Spec.Template.Spec.Containers[0]

						foundMount := false
						for _, vm := range c.VolumeMounts {
							if vm.Name == testCase.expectedMountName && vm.MountPath == testCase.expectedMountPath {
								foundMount = true
							}
						}

						if testCase.expectedMountName != "" {
							g.Expect(foundMount).To(BeTrue(), "expected VolumeMount not found")
						} else {
							g.Expect(foundMount).To(BeFalse(), "unexpected VolumeMount found")
						}
					}, timeout, interval).Should(Succeed())

					// Check volumes
					foundVolume := false
					for _, v := range typedNotebook.Spec.Template.Spec.Volumes {
						if v.Name == testCase.expectedMountName && v.ConfigMap != nil && v.ConfigMap.Name == RuntimeImagesCMName {
							foundVolume = true
						}
					}
					if testCase.expectedMountName != "" {
						Expect(foundVolume).To(BeTrue(), "expected ConfigMap volume not found")
					} else {
						Expect(foundVolume).To(BeFalse(), "unexpected ConfigMap volume found")
					}
				})
				AfterEach(func() {
					By("Deleting the created resources")
					Expect(cli.Delete(ctx, notebook, &client.DeleteOptions{})).To(Succeed())
					if testCase.imageStream != nil {
						Expect(cli.Delete(ctx, testCase.imageStream, &client.DeleteOptions{})).To(Succeed())
					}
					Expect(cli.Delete(ctx, configMap, &client.DeleteOptions{})).To(Succeed())
				})
			})
		}
	})
})
