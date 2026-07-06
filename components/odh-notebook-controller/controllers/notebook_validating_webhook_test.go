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
	"time"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("The Openshift Notebook validating webhook", func() {
	When("Updating a Notebook's MLflow annotation", func() {
		const (
			Namespace = "default"
		)

		BeforeEach(func() {
			// Ensure namespace exists
			err := cli.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: Namespace}}, &client.CreateOptions{})
			if err != nil && !apierrs.IsAlreadyExists(err) {
				Expect(err).ToNot(HaveOccurred())
			}
		})

		newNotebook := func(name string, annotations map[string]string) *nbv1.Notebook {
			return &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Name:        name,
					Namespace:   Namespace,
					Annotations: annotations,
				},
				Spec: nbv1.NotebookSpec{
					Template: nbv1.NotebookTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{
								Name:  name,
								Image: "test-image:latest",
							}},
						},
					},
				},
			}
		}

		// updateNotebookWithRetry fetches the latest notebook and applies the update function,
		// retrying on conflict errors. Returns the final error (nil on success, or validation error).
		updateNotebookWithRetry := func(name string, updateFn func(*nbv1.Notebook)) error {
			var lastErr error
			for i := 0; i < 10; i++ {
				notebook := &nbv1.Notebook{}
				if err := cli.Get(ctx, types.NamespacedName{Name: name, Namespace: Namespace}, notebook); err != nil {
					return err
				}
				updateFn(notebook)
				lastErr = cli.Update(ctx, notebook)
				if lastErr == nil {
					return nil
				}
				if !apierrs.IsConflict(lastErr) {
					// Not a conflict error - could be validation rejection or other error
					return lastErr
				}
				// Conflict error - retry
				time.Sleep(100 * time.Millisecond)
			}
			return lastErr
		}

		It("Should allow removing MLflow annotation when notebook is stopped", func() {
			name := "test-mlflow-remove-stopped"
			// Create notebook with MLflow annotation and stopped annotation
			notebook := newNotebook(name, map[string]string{
				MLflowInstanceAnnotation:    "mlflow",
				"kubeflow-resource-stopped": "true",
			})
			Expect(cli.Create(ctx, notebook)).To(Succeed())

			// Remove MLflow annotation with retry logic
			err := updateNotebookWithRetry(name, func(nb *nbv1.Notebook) {
				delete(nb.Annotations, MLflowInstanceAnnotation)
			})
			Expect(err).ToNot(HaveOccurred())

			// Cleanup
			Eventually(func() error {
				nb := &nbv1.Notebook{}
				if err := cli.Get(ctx, types.NamespacedName{Name: name, Namespace: Namespace}, nb); err != nil {
					return nil // Already gone
				}
				return cli.Delete(ctx, nb)
			}, 10*time.Second).Should(Succeed())
		})

		It("Should allow adding MLflow annotation to a notebook", func() {
			name := "test-mlflow-add"
			// Create notebook without MLflow annotation
			// Note: mutating webhook adds kubeflow-resource-stopped on create
			notebook := newNotebook(name, map[string]string{})
			Expect(cli.Create(ctx, notebook)).To(Succeed())

			// Add MLflow annotation with retry logic
			err := updateNotebookWithRetry(name, func(nb *nbv1.Notebook) {
				if nb.Annotations == nil {
					nb.Annotations = make(map[string]string)
				}
				nb.Annotations[MLflowInstanceAnnotation] = "mlflow"
			})
			Expect(err).ToNot(HaveOccurred())

			// Cleanup
			Eventually(func() error {
				nb := &nbv1.Notebook{}
				if err := cli.Get(ctx, types.NamespacedName{Name: name, Namespace: Namespace}, nb); err != nil {
					return nil
				}
				return cli.Delete(ctx, nb)
			}, 10*time.Second).Should(Succeed())
		})

		It("Should allow updating notebook without changing MLflow annotation", func() {
			name := "test-mlflow-update-other"
			// Create notebook with MLflow annotation
			notebook := newNotebook(name, map[string]string{
				MLflowInstanceAnnotation: "mlflow",
			})
			Expect(cli.Create(ctx, notebook)).To(Succeed())

			// Update a different annotation with retry logic
			err := updateNotebookWithRetry(name, func(nb *nbv1.Notebook) {
				nb.Annotations["some-other-annotation"] = "value"
			})
			Expect(err).ToNot(HaveOccurred())

			// Cleanup
			Eventually(func() error {
				nb := &nbv1.Notebook{}
				if err := cli.Get(ctx, types.NamespacedName{Name: name, Namespace: Namespace}, nb); err != nil {
					return nil
				}
				return cli.Delete(ctx, nb)
			}, 10*time.Second).Should(Succeed())
		})

		It("Should deny removing MLflow annotation from a running notebook", func() {
			name := "test-mlflow-deny-remove"
			// Create notebook with MLflow annotation
			// First, create it stopped, then "start" it by removing the stopped annotation
			notebook := newNotebook(name, map[string]string{
				MLflowInstanceAnnotation: "mlflow",
			})
			Expect(cli.Create(ctx, notebook)).To(Succeed())

			// Wait for controller to stabilize, then remove the stopped annotation to simulate running
			err := updateNotebookWithRetry(name, func(nb *nbv1.Notebook) {
				delete(nb.Annotations, "kubeflow-resource-stopped")
			})
			Expect(err).ToNot(HaveOccurred())

			// Now try to remove MLflow annotation - should be denied
			err = updateNotebookWithRetry(name, func(nb *nbv1.Notebook) {
				delete(nb.Annotations, MLflowInstanceAnnotation)
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("cannot remove"))
			Expect(err.Error()).To(ContainSubstring(MLflowInstanceAnnotation))

			// Cleanup - need to stop the notebook first
			_ = updateNotebookWithRetry(name, func(nb *nbv1.Notebook) {
				nb.Annotations["kubeflow-resource-stopped"] = "true"
			})
			Eventually(func() error {
				nb := &nbv1.Notebook{}
				if err := cli.Get(ctx, types.NamespacedName{Name: name, Namespace: Namespace}, nb); err != nil {
					return nil
				}
				return cli.Delete(ctx, nb)
			}, 10*time.Second).Should(Succeed())
		})

		It("Should deny setting MLflow annotation to empty on a running notebook", func() {
			name := "test-mlflow-deny-empty"
			// Create notebook with MLflow annotation
			notebook := newNotebook(name, map[string]string{
				MLflowInstanceAnnotation: "mlflow",
			})
			Expect(cli.Create(ctx, notebook)).To(Succeed())

			// Remove the stopped annotation to simulate running
			err := updateNotebookWithRetry(name, func(nb *nbv1.Notebook) {
				delete(nb.Annotations, "kubeflow-resource-stopped")
			})
			Expect(err).ToNot(HaveOccurred())

			// Try to set MLflow annotation to empty - should be denied
			err = updateNotebookWithRetry(name, func(nb *nbv1.Notebook) {
				nb.Annotations[MLflowInstanceAnnotation] = ""
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("cannot remove"))

			// Cleanup
			_ = updateNotebookWithRetry(name, func(nb *nbv1.Notebook) {
				nb.Annotations["kubeflow-resource-stopped"] = "true"
			})
			Eventually(func() error {
				nb := &nbv1.Notebook{}
				if err := cli.Get(ctx, types.NamespacedName{Name: name, Namespace: Namespace}, nb); err != nil {
					return nil
				}
				return cli.Delete(ctx, nb)
			}, 10*time.Second).Should(Succeed())
		})

	})
})
