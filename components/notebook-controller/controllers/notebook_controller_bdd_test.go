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
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	nbv1beta1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1beta1"
)

var _ = Describe("Notebook controller", func() {

	// Define utility constants for object names and testing timeouts/durations and intervals.
	const (
		Name      = "test-notebook"
		Namespace = "default"
		timeout   = time.Second * 10
		interval  = time.Millisecond * 250

		testLabelName  = "testLabel"
		testLabelValue = "testLabelValue"
	)

	Context("When validating the notebook controller", func() {
		It("Should create replicas", func() {
			By("By creating a new Notebook")
			ctx := context.Background()
			notebook := &nbv1beta1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Name:      Name,
					Namespace: Namespace,
					Labels: map[string]string{
						testLabelName: testLabelValue,
					},
				},
				Spec: nbv1beta1.NotebookSpec{
					Template: nbv1beta1.NotebookTemplateSpec{
						Spec: v1.PodSpec{Containers: []v1.Container{{
							Name:  "busybox",
							Image: "busybox",
						}}}},
				}}
			Expect(k8sClient.Create(ctx, notebook)).Should(Succeed())

			notebookLookupKey := types.NamespacedName{Name: Name, Namespace: Namespace}
			createdNotebook := &nbv1beta1.Notebook{}

			Eventually(func() bool {
				err := k8sClient.Get(ctx, notebookLookupKey, createdNotebook)
				return err == nil
			}, timeout, interval).Should(BeTrue())
			/*
				Checking for the underlying statefulset.
				The satefulset controllers aren't running within envtest, when env test's aren't pointing to the live cluster.
				Only the API server is running within envtest. So cannot check actual pods / replicas.
			*/
			By("By checking that the Notebook has statefulset")
			Eventually(func() (bool, error) {
				sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
					Name:      Name,
					Namespace: Namespace,
				}}
				err := k8sClient.Get(ctx, notebookLookupKey, sts)
				if err != nil {
					return false, err
				}

				By("By checking that the StatefulSet has identical Labels as the Notebook")
				Expect(sts.GetLabels()).To(Equal(notebook.GetLabels()))
				Expect(sts.OwnerReferences).To(HaveLen(1))
				Expect(sts.OwnerReferences[0].APIVersion).To(Equal(nbv1.GroupVersion.String()))
				Expect(sts.OwnerReferences[0].UID).To(Equal(createdNotebook.UID))

				return true, nil
			}, timeout, interval).Should(BeTrue())

			By("By repairing a legacy StatefulSet owner reference on reconciliation")
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, notebookLookupKey, sts)).To(Succeed())
			sts.OwnerReferences[0].APIVersion = nbv1beta1.GroupVersion.String()
			Expect(k8sClient.Update(ctx, sts)).To(Succeed())
			Expect(k8sClient.Get(ctx, notebookLookupKey, createdNotebook)).To(Succeed())
			if createdNotebook.Annotations == nil {
				createdNotebook.Annotations = make(map[string]string)
			}
			createdNotebook.Annotations["owner-reference-test"] = "reconcile"
			Expect(k8sClient.Update(ctx, createdNotebook)).To(Succeed())
			Eventually(func() (string, error) {
				if err := k8sClient.Get(ctx, notebookLookupKey, sts); err != nil {
					return "", err
				}
				return sts.OwnerReferences[0].APIVersion, nil
			}, timeout, interval).Should(Equal(nbv1.GroupVersion.String()))
		})
	})
})
