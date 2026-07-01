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

package chaostests

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	nbv1beta1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1beta1"
	"github.com/kubeflow/kubeflow/components/notebook-controller/controllers"
	controllermetrics "github.com/kubeflow/kubeflow/components/notebook-controller/pkg/metrics"
	"github.com/opendatahub-io/operator-chaos/pkg/sdk"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	chaosTimeout  = 30 * time.Second
	chaosInterval = 200 * time.Millisecond
)

var chaosMetrics = &controllermetrics.Metrics{
	NotebookCreation:     prometheus.NewCounterVec(prometheus.CounterOpts{Name: "chaos_notebook_create_total"}, []string{"namespace"}),
	NotebookFailCreation: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "chaos_notebook_create_failed_total"}, []string{"namespace"}),
}

func newChaosReconciler(faultCfg *sdk.FaultConfig) *controllers.NotebookReconciler {
	chaosClient := sdk.NewChaosClient(cli, faultCfg)
	return &controllers.NotebookReconciler{
		Client:        chaosClient,
		Log:           ctrl.Log.WithName("chaos-test"),
		Scheme:        scheme.Scheme,
		Metrics:       chaosMetrics,
		EventRecorder: record.NewFakeRecorder(10),
	}
}

var _ = Describe("Notebook controller chaos resilience (isolated)", func() {

	ctx := context.Background()

	var (
		chaosNamespace    *corev1.Namespace
		typeNamespaceName types.NamespacedName
	)

	createChaosNotebook := func(name string) {
		chaosNamespace = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: name},
		}
		typeNamespaceName = types.NamespacedName{Name: name, Namespace: name}

		Expect(cli.Create(ctx, chaosNamespace)).To(Succeed())

		notebook := &nbv1beta1.Notebook{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: name,
			},
			Spec: nbv1beta1.NotebookSpec{
				Template: nbv1beta1.NotebookTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  name,
							Image: testNotebookImage,
						}},
					},
				},
			},
		}
		Expect(cli.Create(ctx, notebook)).To(Succeed())
	}

	AfterEach(func() {
		if chaosNamespace != nil {
			notebook := &nbv1beta1.Notebook{}
			err := cli.Get(ctx, typeNamespaceName, notebook)
			if err == nil {
				notebook.Finalizers = nil
				if updateErr := cli.Update(ctx, notebook); updateErr != nil {
					GinkgoT().Logf("cleanup: failed to clear finalizers: %v", updateErr)
				}
				if deleteErr := cli.Delete(ctx, notebook); deleteErr != nil {
					GinkgoT().Logf("cleanup: failed to delete notebook: %v", deleteErr)
				}
			}
			if deleteErr := cli.Delete(ctx, chaosNamespace); deleteErr != nil {
				GinkgoT().Logf("cleanup: failed to delete namespace: %v", deleteErr)
			}
			chaosNamespace = nil
		}
	})

	// --- Get faults ---

	It("should handle Get errors with requeue", func() {
		createChaosNotebook("chaos-get-errors")

		faultCfg := sdk.NewFaultConfig(map[sdk.Operation]sdk.FaultSpec{
			sdk.OpGet: {ErrorRate: 1.0, Error: "chaos: connection refused"},
		})
		reconciler := newChaosReconciler(faultCfg)

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespaceName})
		Expect(err).To(HaveOccurred(), "expected a chaos error on Get")
		var chaosErr *sdk.ChaosError
		Expect(errors.As(err, &chaosErr)).To(BeTrue(), "error should be a ChaosError")
		Expect(chaosErr.Operation).To(Equal(sdk.OpGet))
	})

	It("should converge after transient Get errors clear", func() {
		createChaosNotebook("chaos-get-transient")

		faultCfg := sdk.NewFaultConfig(map[sdk.Operation]sdk.FaultSpec{
			sdk.OpGet: {ErrorRate: 1.0, Error: "chaos: transient connection refused"},
		})
		reconciler := newChaosReconciler(faultCfg)

		By("Verifying reconciler fails while faults are active")
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespaceName})
		Expect(err).To(HaveOccurred())

		By("Clearing faults and verifying convergence")
		faultCfg.Deactivate()
		Eventually(func() error {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespaceName})
			return err
		}, chaosTimeout, chaosInterval).Should(Succeed())
	})

	// --- Create faults ---

	It("should handle Create failures and report errors", func() {
		createChaosNotebook("chaos-create-fail")

		faultCfg := sdk.NewFaultConfig(map[sdk.Operation]sdk.FaultSpec{
			sdk.OpCreate: {ErrorRate: 1.0, Error: "chaos: quota exceeded"},
		})
		reconciler := newChaosReconciler(faultCfg)

		var lastErr error
		Eventually(func() bool {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespaceName})
			if err != nil {
				lastErr = err
				return true
			}
			return false
		}, chaosTimeout, chaosInterval).Should(BeTrue(),
			"reconciler should eventually hit a Create error")

		var chaosErr *sdk.ChaosError
		Expect(errors.As(lastErr, &chaosErr)).To(BeTrue(), "error should be a ChaosError")
		Expect(chaosErr.Operation).To(Equal(sdk.OpCreate))
	})

	It("should converge after transient Create failures clear", func() {
		createChaosNotebook("chaos-create-transient")

		faultCfg := sdk.NewFaultConfig(map[sdk.Operation]sdk.FaultSpec{
			sdk.OpCreate: {ErrorRate: 1.0, Error: "chaos: quota exceeded"},
		})
		reconciler := newChaosReconciler(faultCfg)

		By("Verifying reconciler hits Create errors while faults are active")
		Eventually(func() bool {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespaceName})
			if err != nil {
				var chaosErr *sdk.ChaosError
				return errors.As(err, &chaosErr) && chaosErr.Operation == sdk.OpCreate
			}
			return false
		}, chaosTimeout, chaosInterval).Should(BeTrue(),
			"reconciler should hit a Create error")

		By("Clearing faults and verifying convergence")
		faultCfg.Deactivate()
		Eventually(func() error {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespaceName})
			return err
		}, chaosTimeout, chaosInterval).Should(Succeed(),
			"reconciler should converge once Create failures stop")
	})

	// --- List faults ---

	It("should handle List failures gracefully", func() {
		createChaosNotebook("chaos-list-fail")

		faultCfg := sdk.NewFaultConfig(map[sdk.Operation]sdk.FaultSpec{
			sdk.OpList: {ErrorRate: 1.0, Error: "chaos: list timeout"},
		})
		reconciler := newChaosReconciler(faultCfg)

		var lastErr error
		Eventually(func() bool {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespaceName})
			if err != nil {
				lastErr = err
				return true
			}
			return false
		}, chaosTimeout, chaosInterval).Should(BeTrue(),
			"reconciler should eventually hit a List error")

		var chaosErr *sdk.ChaosError
		Expect(errors.As(lastErr, &chaosErr)).To(BeTrue(), "error should be a ChaosError")
		Expect(chaosErr.Operation).To(Equal(sdk.OpList))
	})

	It("should converge after transient List errors clear", func() {
		createChaosNotebook("chaos-list-transient")

		faultCfg := sdk.NewFaultConfig(map[sdk.Operation]sdk.FaultSpec{
			sdk.OpList: {ErrorRate: 1.0, Error: "chaos: transient list timeout"},
		})
		reconciler := newChaosReconciler(faultCfg)

		By("Verifying reconciler fails while faults are active")
		var lastErr error
		Eventually(func() bool {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespaceName})
			if err != nil {
				lastErr = err
				return true
			}
			return false
		}, chaosTimeout, chaosInterval).Should(BeTrue())

		var chaosErr *sdk.ChaosError
		Expect(errors.As(lastErr, &chaosErr)).To(BeTrue())
		Expect(chaosErr.Operation).To(Equal(sdk.OpList))

		By("Clearing faults and verifying convergence")
		faultCfg.Deactivate()
		Eventually(func() error {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespaceName})
			return err
		}, chaosTimeout, chaosInterval).Should(Succeed(),
			"reconciler should converge once List errors stop")
	})

	// --- Intermittent multi-operation faults ---

	It("should tolerate intermittent API errors and eventually converge", func() {
		createChaosNotebook("chaos-intermittent")

		intermittentError := "chaos: intermittent failure"
		faultCfg := sdk.NewFaultConfig(map[sdk.Operation]sdk.FaultSpec{
			sdk.OpGet:    {ErrorRate: 0.15, Error: intermittentError},
			sdk.OpList:   {ErrorRate: 0.15, Error: intermittentError},
			sdk.OpCreate: {ErrorRate: 0.15, Error: intermittentError},
		})
		reconciler := newChaosReconciler(faultCfg)

		Eventually(func() error {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespaceName})
			return err
		}, chaosTimeout, chaosInterval).Should(Succeed(),
			"reconciler should eventually converge despite intermittent errors")
	})
})
