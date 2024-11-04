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
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("The Openshift Notebook webhook", func() {
	ctx := context.Background()

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

		testCases := []struct {
			name        string
			imageStream *unstructured.Unstructured
			notebook    *nbv1.Notebook
			// currently we expect that Notebook CR is always created,
			// and when unable to resolve imagestream, image: is left alone
			expectedImage string
			// see https://www.youtube.com/watch?v=prLRI3VEVq4 for Observability Driven Development intro
			expectedEvents   []string
			unexpectedEvents []string
		}{
			{
				name: "ImageStream with all that is needful",
				imageStream: &unstructured.Unstructured{
					Object: map[string]any{
						"kind":       "ImageStream",
						"apiVersion": "image.openshift.io/v1",
						"metadata": map[string]any{
							"name":      "some-image",
							"namespace": "redhat-ods-applications",
						},
						"spec": map[string]any{
							"lookupPolicy": map[string]any{
								"local": true,
							},
						},
						"status": map[string]any{
							"tags": []any{
								map[string]any{
									"tag": "some-tag",
									"items": []map[string]any{
										{
											"created":              "2024-10-03T08:10:22Z",
											"dockerImageReference": "quay.io/modh/odh-generic-data-science-notebook@sha256:76e6af79c601a323f75a58e7005de0beac66b8cccc3d2b67efb6d11d85f0cfa1",
											"image":                "sha256:76e6af79c601a323f75a58e7005de0beac66b8cccc3d2b67efb6d11d85f0cfa1",
											"generation":           1,
										},
									},
									"conditions": []any{
										map[string]any{
											"type":               "ImportSuccess",
											"status":             "False",
											"lastTransitionTime": "2025-03-11T08:50:51Z",
											"reason":             "NotFound",
										}}}},
						},
					},
				},
				notebook: &nbv1.Notebook{
					ObjectMeta: metav1.ObjectMeta{
						Name:      Name,
						Namespace: Namespace,
						Annotations: map[string]string{
							"notebooks.opendatahub.io/last-image-selection": "some-image:some-tag",
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
					"imagestream-not-found",
				},
			},
			{
				name: "ImageStream with a tag without items (RHOAIENG-13916)",
				imageStream: &unstructured.Unstructured{
					Object: map[string]any{
						"kind":       "ImageStream",
						"apiVersion": "image.openshift.io/v1",
						"metadata": map[string]any{
							"name":      "some-image",
							"namespace": "redhat-ods-applications",
						},
						"spec": map[string]any{
							"lookupPolicy": map[string]any{
								"local": true,
							},
						},
						"status": map[string]any{
							"tags": []any{
								map[string]any{
									"tag":   "some-tag",
									"items": nil,
									"conditions": []any{
										map[string]any{
											"type":               "ImportSuccess",
											"status":             "False",
											"lastTransitionTime": "2025-03-11T08:50:51Z",
											"reason":             "NotFound",
										}}}},
						},
					},
				},
				notebook: &nbv1.Notebook{
					ObjectMeta: metav1.ObjectMeta{
						Name:      Name,
						Namespace: Namespace,
						Annotations: map[string]string{
							"notebooks.opendatahub.io/last-image-selection": "some-image:some-tag",
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
				// there is no update to the Notebook
				expectedImage: ":some-tag",
				expectedEvents: []string{
					"imagestream-not-found",
				},
			},
		}

		BeforeEach(func() {
			Expect(tracings.TraceProvider.ForceFlush(context.Background())).To(Succeed())
			tracings.SpanExporter.Reset()
		})

		for _, testCase := range testCases {
			Context(fmt.Sprintf("The Notebook webhook test case: %s", testCase.name), func() {
				It("Should create a Notebook resource successfully", func() {
					By("Creating a Notebook resource successfully")
					Expect(cli.Create(ctx, testCase.imageStream, &client.CreateOptions{})).To(Succeed())
					// if our webhook panics, then cli.Create will err
					Expect(cli.Create(ctx, testCase.notebook, &client.CreateOptions{})).To(Succeed())

					By("Checking that the webhook modified the notebook CR with the expected image")
					Expect(testCase.notebook.Spec.Template.Spec.Containers[0].Image).To(Equal(testCase.expectedImage))

					By("Checking telemetry events")
					Expect(tracings.TraceProvider.ForceFlush(context.Background())).To(Succeed())
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
					Expect(cli.Delete(ctx, testCase.imageStream, &client.DeleteOptions{})).To(Succeed())
				})
			})
		}
	})
})
