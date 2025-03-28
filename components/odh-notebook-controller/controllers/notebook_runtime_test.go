package controllers

import (
	"context"
	"fmt"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("When Creating a notebook should mount the configMap", func() {
	ctx := context.Background()

	When("Creating a Notebook", func() {

		const (
			Namespace = "default"
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
			ConfigMap         *unstructured.Unstructured
			expectedMountName string
			expectedMountPath string
		}{
			{
				name:         "ConfigMap with data",
				notebookName: "test-notebook-runtime-1",
				ConfigMap: &unstructured.Unstructured{
					Object: map[string]any{
						"kind":       "ConfigMap",
						"apiVersion": "v1",
						"metadata": map[string]any{
							"name":      "pipeline-runtime-images",
							"namespace": Namespace,
						},
						"data": map[string]string{
							"datascience.json": `{"image_name":"quay.io/opendatahub/test"}`,
						},
					},
				},
				expectedMountName: "runtime-images",
				expectedMountPath: "/opt/app-root/pipeline-runtimes/",
			},
			{
				name:         "ConfigMap without data",
				notebookName: "test-notebook-runtime-2",
				ConfigMap: &unstructured.Unstructured{
					Object: map[string]any{
						"kind":       "ConfigMap",
						"apiVersion": "v1",
						"metadata": map[string]any{
							"name":      "pipeline-runtime-images",
							"namespace": Namespace,
						},
						"data": map[string]string{},
					},
				},
				expectedMountName: "",
				expectedMountPath: "",
			},
		}

		for _, testCase := range testCases {
			testCase := testCase
			It(fmt.Sprintf("Should mount ConfigMap correctly: %s", testCase.name), func() {
				notebook := createNotebook(testCase.notebookName, Namespace)

				// cleanup first
				_ = cli.Delete(ctx, notebook, &client.DeleteOptions{})
				_ = cli.Delete(ctx, testCase.ConfigMap, &client.DeleteOptions{})

				// wait until deleted
				By("Waiting for the Notebook to be deleted")
				Eventually(func(g Gomega) {
					err := cli.Get(ctx, client.ObjectKey{Name: testCase.notebookName, Namespace: Namespace}, &nbv1.Notebook{})
					g.Expect(apierrs.IsNotFound(err)).To(BeTrue(), fmt.Sprintf("expected Notebook %q to be deleted", testCase.notebookName))
				}).WithOffset(1).Should(Succeed())

				By("Creating the ConfigMap")
				Expect(cli.Create(ctx, testCase.ConfigMap)).To(Succeed())

				By("Creating the Notebook")
				Expect(cli.Create(ctx, notebook)).To(Succeed())

				By("Fetching the created Notebook as typed object")
				typedNotebook := &nbv1.Notebook{}
				Eventually(func(g Gomega) {
					err := cli.Get(ctx, client.ObjectKey{Name: testCase.notebookName, Namespace: Namespace}, typedNotebook)
					g.Expect(err).ToNot(HaveOccurred())
				}).Should(Succeed())

				// Check volumeMounts
				c := typedNotebook.Spec.Template.Spec.Containers[0]
				foundMount := false
				for _, vm := range c.VolumeMounts {
					if vm.Name == testCase.expectedMountName && vm.MountPath == testCase.expectedMountPath {
						foundMount = true
					}
				}
				if testCase.expectedMountName != "" {
					Expect(foundMount).To(BeTrue(), "expected VolumeMount not found")
				} else {
					Expect(foundMount).To(BeFalse(), "unexpected VolumeMount found")
				}

				// Check volumes
				foundVolume := false
				for _, v := range typedNotebook.Spec.Template.Spec.Volumes {
					if v.Name == testCase.expectedMountName && v.ConfigMap != nil && v.ConfigMap.Name == "pipeline-runtime-images" {
						foundVolume = true
					}
				}
				if testCase.expectedMountName != "" {
					Expect(foundVolume).To(BeTrue(), "expected ConfigMap volume not found")
				} else {
					Expect(foundVolume).To(BeFalse(), "unexpected ConfigMap volume found")
				}

				By("Deleting the created resources")
				Expect(cli.Delete(ctx, notebook, &client.DeleteOptions{})).To(Succeed())
				Expect(cli.Delete(ctx, testCase.ConfigMap, &client.DeleteOptions{})).To(Succeed())
			})
		}
	})
})
