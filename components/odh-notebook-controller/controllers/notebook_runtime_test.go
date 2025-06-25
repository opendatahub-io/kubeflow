package controllers

import (
	"context"
	"fmt"
	"strings"
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
	resourceReconciliationTimeout     = time.Second * 30
	resourceReconciliationCheckPeriod = time.Second * 2
	testNamespace                     = "default"
	runtimeImagesCMName               = "pipeline-runtime-images"
	expectedMountName                 = "runtime-images"
	expectedMountPath                 = "/opt/app-root/pipeline-runtimes/"
)

// Check common Notebook volume and volume mount expectations
func expectNotebookVolumesAndMounts(ctx context.Context, notebookName, namespace string, expectedCM bool) {
	typedNotebook := &nbv1.Notebook{}
	Eventually(func(g Gomega) error {
		err := cli.Get(ctx, client.ObjectKey{Name: notebookName, Namespace: namespace}, typedNotebook)
		if err != nil {
			return err
		}

		c := typedNotebook.Spec.Template.Spec.Containers[0]

		foundMount := false
		for _, vm := range c.VolumeMounts {
			if vm.Name == expectedMountName && vm.MountPath == expectedMountPath {
				foundMount = true
				break // Found it, no need to check further
			}
		}

		// Check volumeMounts
		if expectedCM {
			g.Expect(foundMount).To(BeTrue(), "expected VolumeMount not found")
		} else {
			g.Expect(foundMount).To(BeFalse(), "unexpected VolumeMount found")
		}

		foundVolume := false
		for _, v := range typedNotebook.Spec.Template.Spec.Volumes {
			if v.Name == expectedMountName && v.ConfigMap != nil && v.ConfigMap.Name == runtimeImagesCMName {
				foundVolume = true
				break // Found it
			}
		}

		// Check volumes
		if expectedCM {
			g.Expect(foundVolume).To(BeTrue(), "expected ConfigMap volume not found")
		} else {
			g.Expect(foundVolume).To(BeFalse(), "unexpected ConfigMap volume found")
		}

		return nil
	}, resourceReconciliationTimeout, resourceReconciliationCheckPeriod).Should(Succeed())
}

var _ = Describe("Runtime Images ConfigMap Mounting", func() {
	ctx := context.Background()

	When("Runtime images ConfigMap is empty or irrelevant", func() {
		testCases := []struct {
			name        string
			configMap   *corev1.ConfigMap
			description string
		}{
			{
				name:        "ConfigMap without data",
				description: "the Notebook should not have the ConfigMap mounted",
				configMap:   createRuntimeConfigMap(runtimeImagesCMName, testNamespace, map[string]string{}),
			},
		}

		for _, tc := range testCases {
			Context(fmt.Sprintf("with a %s", tc.name), func() {
				notebookName := strings.ToLower(fmt.Sprintf("test-notebook-runtime-%s", strings.ReplaceAll(tc.name, " ", "-")))
				notebook := createNotebook(notebookName, testNamespace)

				BeforeEach(func() {
					err := cli.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: odhNotebookControllerTestNamespace}}, &client.CreateOptions{})
					if err != nil && !apierrs.IsAlreadyExists(err) {
						Expect(err).ToNot(HaveOccurred())
					}

					_ = cli.Delete(ctx, notebook, &client.DeleteOptions{})
					_ = cli.Delete(ctx, tc.configMap, &client.DeleteOptions{})

					By(fmt.Sprintf("Waiting for the Notebook %q to be deleted", notebookName))
					Eventually(func() bool {
						err := cli.Get(ctx, client.ObjectKey{Name: notebookName, Namespace: testNamespace}, &nbv1.Notebook{})
						return apierrs.IsNotFound(err)
					}).WithOffset(1).Should(BeTrue(), fmt.Sprintf("expected Notebook %q to be deleted", notebookName))

					By(fmt.Sprintf("Waiting for ConfigMap %q to be deleted", tc.configMap.Name))
					Eventually(func() bool {
						err := cli.Get(ctx, client.ObjectKey{Name: tc.configMap.Name, Namespace: tc.configMap.Namespace}, &corev1.ConfigMap{})
						return apierrs.IsNotFound(err)
					}).WithOffset(1).Should(BeTrue(), fmt.Sprintf("expected ConfigMap %q to be deleted", tc.configMap.Name))
				})

				It(fmt.Sprintf("should behave as expected: %s", tc.description), func() {
					By("Creating the ConfigMap directly")
					Expect(cli.Create(ctx, tc.configMap)).To(Succeed())

					By("Creating the Notebook")
					Expect(cli.Create(ctx, notebook)).To(Succeed())

					// Call the helper without the Gomega parameter
					expectNotebookVolumesAndMounts(ctx, notebookName, testNamespace, false)
				})

				AfterEach(func() {
					By("Deleting created resources after test")
					_ = cli.Delete(ctx, notebook, &client.DeleteOptions{})
					_ = cli.Delete(ctx, tc.configMap, &client.DeleteOptions{})
				})
			})
		}
	})

	When("ImageStream data leads to a runtime images ConfigMap", func() {
		testCases := []struct {
			name                  string
			imageStream           *imagev1.ImageStream
			expectedConfigMapData map[string]string
			expectCMMounted       bool
		}{
			{
				name: "ImageStream with two valid tags",
				imageStream: createRuntimeImageStream("some-image", odhNotebookControllerTestNamespace, []imagev1.TagReference{
					{
						Name: "some-tag",
						Annotations: map[string]string{
							"opendatahub.io/runtime-image-metadata": `[{"display_name":"Python 3.11 (UBI9)","metadata":{"tags":["some-tag"],"display_name":"Python 3.11 (UBI9)","pull_policy":"IfNotPresent"},"schema_name":"runtime-image"}]`,
						},
						From: &corev1.ObjectReference{Kind: "DockerImage", Name: "quay.io/opendatahub/test"},
					},
					{
						Name: "some-tag2",
						Annotations: map[string]string{
							"opendatahub.io/runtime-image-metadata": `[{"display_name":"Hohoho Python 3.12 (UBI9)","metadata":{"tags":["some-tag2"],"display_name":"Python 3.12 (UBI9)","pull_policy":"IfNotPresent"},"schema_name":"runtime-image"}]`,
						},
						From: &corev1.ObjectReference{Kind: "DockerImage", Name: "quay.io/opendatahub/test2"},
					},
				}),
				expectedConfigMapData: map[string]string{
					"python-3.11-ubi9.json":        `{"display_name":"Python 3.11 (UBI9)","metadata":{"display_name":"Python 3.11 (UBI9)","image_name":"quay.io/opendatahub/test","pull_policy":"IfNotPresent","tags":["some-tag"]},"schema_name":"runtime-image"}`,
					"hohoho-python-3.12-ubi9.json": `{"display_name":"Hohoho Python 3.12 (UBI9)","metadata":{"display_name":"Python 3.12 (UBI9)","image_name":"quay.io/opendatahub/test2","pull_policy":"IfNotPresent","tags":["some-tag2"]},"schema_name":"runtime-image"}`,
				},
				expectCMMounted: true,
			},
			{
				name: "ImageStream with one valid tag",
				imageStream: createRuntimeImageStream("some-image", odhNotebookControllerTestNamespace, []imagev1.TagReference{
					{
						Name: "some-tag",
						Annotations: map[string]string{
							"opendatahub.io/runtime-image-metadata": `[{"display_name":"Python 3.11 (UBI9)","metadata":{"tags":["some-tag"],"display_name":"Python 3.11 (UBI9)","pull_policy":"IfNotPresent"},"schema_name":"runtime-image"}]`,
						},
						From: &corev1.ObjectReference{Kind: "DockerImage", Name: "quay.io/modh/odh-pipeline-runtime-datascience-cpu-py311-ubi9@sha256:5aa8868be00f304084ce6632586c757bc56b28300779495d14b08bcfbcd3357f"},
					},
				}),
				expectedConfigMapData: map[string]string{
					"python-3.11-ubi9.json": `{"display_name":"Python 3.11 (UBI9)","metadata":{"display_name":"Python 3.11 (UBI9)","image_name":"quay.io/modh/odh-pipeline-runtime-datascience-cpu-py311-ubi9@sha256:5aa8868be00f304084ce6632586c757bc56b28300779495d14b08bcfbcd3357f","pull_policy":"IfNotPresent","tags":["some-tag"]},"schema_name":"runtime-image"}`,
				},
				expectCMMounted: true,
			},
			{
				name: "ImageStream with irrelevant data",
				imageStream: createRuntimeImageStream("some-image", odhNotebookControllerTestNamespace, []imagev1.TagReference{
					{
						Name: "some-tag",
						Annotations: map[string]string{
							"opendatahub.io/runtime-image-metadata-fake": `[{"display_name":"Python 3.11 (UBI9)","metadata":{"tags":["some-tag"],"display_name":"Python 3.11 (UBI9)","pull_policy":"IfNotPresent"},"schema_name":"runtime-image-fake"}]`,
						},
						From: &corev1.ObjectReference{Kind: "DockerImage", Name: "quay.io/opendatahub/test"},
					},
				}),
				expectedConfigMapData: nil, // Expect no ConfigMap data for this case
				expectCMMounted:       false,
			},
		}

		for _, tc := range testCases {
			Context(fmt.Sprintf("with %s", tc.name), func() {
				notebookName := strings.ToLower(fmt.Sprintf("test-notebook-runtime-%s", strings.ReplaceAll(tc.name, " ", "-")))
				notebook := createNotebook(notebookName, testNamespace)
				configMap := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      runtimeImagesCMName,
						Namespace: testNamespace,
					},
				}

				BeforeEach(func() {
					err := cli.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: odhNotebookControllerTestNamespace}}, &client.CreateOptions{})
					if err != nil && !apierrs.IsAlreadyExists(err) {
						Expect(err).ToNot(HaveOccurred())
					}

					_ = cli.Delete(ctx, notebook, &client.DeleteOptions{})
					_ = cli.Delete(ctx, tc.imageStream, &client.DeleteOptions{})
					_ = cli.Delete(ctx, configMap, &client.DeleteOptions{}) // Attempt to delete, ignore if not found

					By(fmt.Sprintf("Waiting for the Notebook %q to be deleted", notebookName))
					Eventually(func() bool {
						err := cli.Get(ctx, client.ObjectKey{Name: notebookName, Namespace: testNamespace}, &nbv1.Notebook{})
						return apierrs.IsNotFound(err)
					}).WithOffset(1).Should(BeTrue(), fmt.Sprintf("expected Notebook %q to be deleted", notebookName))

					By(fmt.Sprintf("Waiting for ImageStream %q to be deleted", tc.imageStream.Name))
					Eventually(func() bool {
						err := cli.Get(ctx, client.ObjectKey{Name: tc.imageStream.Name, Namespace: tc.imageStream.Namespace}, &imagev1.ImageStream{})
						return apierrs.IsNotFound(err)
					}).WithOffset(1).Should(BeTrue(), fmt.Sprintf("expected ImageStream %q to be deleted", tc.imageStream.Name))
				})

				It(fmt.Sprintf("should ensure the ConfigMap is %s and Notebook is %s",
					func() string {
						if tc.expectedConfigMapData != nil {
							return "created and correctly populated"
						}
						return "not created"
					}(),
					func() string {
						if tc.expectCMMounted {
							return "correctly mounted"
						}
						return "not mounted"
					}(),
				), func() {
					By("Creating the ImageStream")
					Expect(cli.Create(ctx, tc.imageStream)).To(Succeed())

					By("Creating the Notebook")
					Expect(cli.Create(ctx, notebook)).To(Succeed())

					if tc.expectedConfigMapData != nil {
						By("Fetching the ConfigMap for the runtime images and checking its content")
						Eventually(func(g Gomega) error {
							err := cli.Get(ctx, client.ObjectKey{Name: runtimeImagesCMName, Namespace: testNamespace}, configMap)
							g.Expect(err).ToNot(HaveOccurred(), "Expected ConfigMap to be created")
							g.Expect(configMap.GetName()).To(Equal(runtimeImagesCMName))
							g.Expect(configMap.Data).To(Equal(tc.expectedConfigMapData))
							return nil // Return nil on success
						}, resourceReconciliationTimeout, resourceReconciliationCheckPeriod).Should(Succeed())

						// Call the helper without the Gomega parameter
						expectNotebookVolumesAndMounts(ctx, notebookName, testNamespace, true)

					} else {
						By("Verifying the ConfigMap for runtime images is NOT created")
						Eventually(func() bool {
							err := cli.Get(ctx, client.ObjectKey{Name: runtimeImagesCMName, Namespace: testNamespace}, configMap)
							return apierrs.IsNotFound(err)
						}, resourceReconciliationTimeout, resourceReconciliationCheckPeriod).Should(BeTrue(), "Expected ConfigMap NOT to be created")

						// Call the helper without the Gomega parameter
						expectNotebookVolumesAndMounts(ctx, notebookName, testNamespace, false)
					}
				})

				AfterEach(func() {
					By("Deleting created resources after test")
					_ = cli.Delete(ctx, notebook, &client.DeleteOptions{})
					_ = cli.Delete(ctx, tc.imageStream, &client.DeleteOptions{})
					if tc.expectedConfigMapData != nil { // Only delete if it was expected to be created
						_ = cli.Delete(ctx, configMap, &client.DeleteOptions{})
					}
				})
			})
		}
	})
})
