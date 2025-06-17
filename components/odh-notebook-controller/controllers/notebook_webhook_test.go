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
	"time"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	imagev1 "github.com/openshift/api/image/v1"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
			imageStream *imagev1.ImageStream
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
				imageStream: &imagev1.ImageStream{
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
				},
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
				imageStream: &imagev1.ImageStream{
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
					IMAGE_STREAM_TAG_NOT_FOUND_EVENT,
				},
			},
			{
				name: "ImageStream in the same namespace as the Notebook",
				imageStream: &imagev1.ImageStream{
					TypeMeta: metav1.TypeMeta{
						Kind:       "ImageStream",
						APIVersion: "image.openshift.io/v1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:      "some-image",
						Namespace: Namespace,
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
								Created:              toMetav1Time("2025-05-14T02:36:21Z"),
								DockerImageReference: "quay.io/modh/odh-generic-data-science-notebook@sha256:5999547f847ca841fe067ff84e2972d2cbae598066c2418e236448e115c1728e",
								Image:                "sha256:5999547f847ca841fe067ff84e2972d2cbae598066c2418e236448e115c1728e",
								Generation:           2,
							}},
							Conditions: []imagev1.TagEventCondition{{
								Type:               "ImportSuccess",
								Status:             "False",
								LastTransitionTime: toMetav1Time("2025-05-14T02:36:21Z"),
								Reason:             "NotFound",
							}},
						}},
					},
				},
				notebook: &nbv1.Notebook{
					ObjectMeta: metav1.ObjectMeta{
						Name:      Name,
						Namespace: Namespace,
						Annotations: map[string]string{
							"notebooks.opendatahub.io/last-image-selection": "some-image:some-tag",
							"opendatahub.io/workbench-image-namespace":      Namespace,
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
				// valid new image is picked from user namespace.
				expectedImage: "quay.io/modh/odh-generic-data-science-notebook@sha256:5999547f847ca841fe067ff84e2972d2cbae598066c2418e236448e115c1728e",
				unexpectedEvents: []string{
					IMAGE_STREAM_NOT_FOUND_EVENT,
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

func toMetav1Time(timeString string) metav1.Time {
	parsedTime, err := time.Parse(time.RFC3339, timeString)
	if err != nil {
		return metav1.Time{}
	}
	return metav1.NewTime(parsedTime)
}
