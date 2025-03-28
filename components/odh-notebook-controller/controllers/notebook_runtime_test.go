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
			Name      = "test-notebook-with-runtime-images"
			Namespace = "default"
		)

		BeforeEach(func() {
			err := cli.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: odhNotebookControllerTestNamespace}}, &client.CreateOptions{})
			if err != nil && !apierrs.IsAlreadyExists(err) {
				Expect(err).ToNot(HaveOccurred())
			}
		})

		testCases := []struct {
			name      string
			ConfigMap *unstructured.Unstructured
			// currently we expect that Notebook CR is always created,
			// and when unable to resolve imagestream, image: is left alone
			expectedData map[string]string
		}{
			{
				name: "ConfigMap with data",
				ConfigMap: &unstructured.Unstructured{
					Object: map[string]any{
						"kind":       "ConfigMap",
						"apiVersion": "v1",
						"metadata": map[string]any{
							"name":      "pipeline-runtime-images",
							"namespace": "redhat-ods-applications",
						},
						"data": map[string]string{
							"datascience-with-python-3.11-ubi9.json": `{"display_name":"Datascience with Python 3.11 (UBI9)","metadata":{"display_name":"Datascience with Python 3.11 (UBI9)","image_name":"quay.io/opendatahub/workbench-images@sha256:304d3b2ea846832f27312ef6776064a1bf3797c645b6fea0b292a7ef6416458e","pull_policy":"IfNotPresent","tags":["datascience"]},"schema_name":"runtime-image"}`,
						},
					},
				},
				expectedData: map[string]string{
					"datascience-with-python-3.11-ubi9.json": `{"display_name":"Datascience with Python 3.11 (UBI9)","metadata":{"display_name":"Datascience with Python 3.11 (UBI9)","image_name":"quay.io/opendatahub/workbench-images@sha256:304d3b2ea846832f27312ef6776064a1bf3797c645b6fea0b292a7ef6416458e","pull_policy":"IfNotPresent","tags":["datascience"]},"schema_name":"runtime-image"}`,
				},
			},
			{
				name: "ConfigMap without data",
				ConfigMap: &unstructured.Unstructured{
					Object: map[string]any{
						"kind":       "ConfigMap",
						"apiVersion": "v1",
						"metadata": map[string]any{
							"name":      "pipeline-runtime-images",
							"namespace": "redhat-ods-applications",
						},
						"data": map[string]string{},
					},
				},
				expectedData: map[string]string{},
			},
		}

		for _, testCase := range testCases {
			testCase := testCase // create a copy to get correct capture in the `It` closure, https://go.dev/blog/loopvar-preview
			It(fmt.Sprintf("Should create a Notebook resource successfully: %s", testCase.name), func() {

				notebook := createNotebook(Name, Namespace)

				By("Creating a Notebook resource successfully")
				Expect(cli.Create(ctx, notebook)).Should(Succeed())
				By("Creating a Configmap successfully")
				Expect(cli.Create(ctx, testCase.ConfigMap, &client.CreateOptions{})).To(Succeed())

				// By("Checking that the webhook modified the notebook CR with the expected image")
				// Expect(notebook.Spec.Template.Spec.Containers[0].Image).To(Equal(testCase.expectedData))

				By("Deleting the created resources")
				Expect(cli.Delete(ctx, notebook, &client.DeleteOptions{})).To(Succeed())
				Expect(cli.Delete(ctx, testCase.ConfigMap, &client.DeleteOptions{})).To(Succeed())
			})
		}

	})

})
