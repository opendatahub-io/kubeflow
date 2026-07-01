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

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	"github.com/opendatahub-io/kubeflow/components/odh-notebook-controller/controllers"

	"github.com/opendatahub-io/operator-chaos/pkg/sdk"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	chaosTimeout  = 30 * time.Second
	chaosInterval = 200 * time.Millisecond
)

func newChaosReconciler(faultCfg *sdk.FaultConfig) *controllers.OpenshiftNotebookReconciler {
	chaosClient := sdk.NewChaosClient(cli, faultCfg)
	return &controllers.OpenshiftNotebookReconciler{
		Client:        chaosClient,
		Scheme:        cli.Scheme(),
		Log:           ctrl.Log.WithName("chaos-test"),
		Namespace:     controllerNamespace,
		Config:        cfg,
		EventRecorder: record.NewFakeRecorder(10),
		MLflowEnabled: false,
		GatewayURL:    "",
	}
}

var _ = Describe("ODH Notebook controller chaos resilience (isolated)", func() {

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

		notebook := &nbv1.Notebook{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: name,
			},
			Spec: nbv1.NotebookSpec{
				Template: nbv1.NotebookTemplateSpec{
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
			notebook := &nbv1.Notebook{}
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

		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: typeNamespaceName,
		})

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
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: typeNamespaceName,
		})
		Expect(err).To(HaveOccurred())

		By("Clearing faults and verifying convergence")
		faultCfg.Deactivate()

		Eventually(func() error {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespaceName,
			})
			return err
		}, chaosTimeout, chaosInterval).Should(Succeed(),
			"reconciler should converge once Get errors stop")
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
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespaceName,
			})
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
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespaceName,
			})
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
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespaceName,
			})
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
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespaceName,
			})
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
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespaceName,
			})
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
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespaceName,
			})
			return err
		}, chaosTimeout, chaosInterval).Should(Succeed(),
			"reconciler should converge once List errors stop")
	})

	// --- Update faults ---

	It("should remain converged when Update faults are present but no drift exists", func() {
		createChaosNotebook("chaos-update-no-drift")
		reconciler := newChaosReconciler(nil)

		By("Running initial clean reconcile to create resources")
		Eventually(func() error {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespaceName,
			})
			return err
		}, chaosTimeout, 500*time.Millisecond).Should(Succeed())

		By("Injecting Update faults and verifying reconciler stays healthy")
		updateFaultCfg := sdk.NewFaultConfig(map[sdk.Operation]sdk.FaultSpec{
			sdk.OpUpdate: {ErrorRate: 1.0, Error: "chaos: the object has been modified"},
		})
		reconciler.Client = sdk.NewChaosClient(cli, updateFaultCfg)

		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: typeNamespaceName,
		})
		Expect(err).NotTo(HaveOccurred(),
			"reconciler should succeed when state is converged and no updates are needed")
	})

	// --- Delete faults (finalization path) ---

	It("should propagate Delete errors during finalization", func() {
		createChaosNotebook("chaos-delete-finalize")
		reconciler := newChaosReconciler(nil)

		By("Running initial reconcile to converge and add finalizers")
		Eventually(func() error {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespaceName,
			})
			return err
		}, chaosTimeout, 500*time.Millisecond).Should(Succeed())

		By("Marking notebook for deletion")
		notebook := &nbv1.Notebook{}
		Expect(cli.Get(ctx, typeNamespaceName, notebook)).To(Succeed())
		Expect(cli.Delete(ctx, notebook)).To(Succeed())

		By("Injecting Delete faults and reconciling the deletion")
		deleteFaultCfg := sdk.NewFaultConfig(map[sdk.Operation]sdk.FaultSpec{
			sdk.OpDelete: {ErrorRate: 1.0, Error: "chaos: delete blocked"},
		})
		reconciler.Client = sdk.NewChaosClient(cli, deleteFaultCfg)

		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: typeNamespaceName,
		})
		Expect(err).To(HaveOccurred(),
			"reconciler should propagate errors when Delete fails during finalization")
	})

	It("should complete finalization after transient Delete errors clear", func() {
		createChaosNotebook("chaos-delete-transient")
		reconciler := newChaosReconciler(nil)

		By("Running initial reconcile to converge and add finalizers")
		Eventually(func() error {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespaceName,
			})
			return err
		}, chaosTimeout, 500*time.Millisecond).Should(Succeed())

		By("Marking notebook for deletion")
		notebook := &nbv1.Notebook{}
		Expect(cli.Get(ctx, typeNamespaceName, notebook)).To(Succeed())
		Expect(cli.Delete(ctx, notebook)).To(Succeed())

		By("Injecting transient Delete faults")
		deleteFaultCfg := sdk.NewFaultConfig(map[sdk.Operation]sdk.FaultSpec{
			sdk.OpDelete: {ErrorRate: 1.0, Error: "chaos: transient delete failure"},
		})
		reconciler.Client = sdk.NewChaosClient(cli, deleteFaultCfg)

		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: typeNamespaceName,
		})
		Expect(err).To(HaveOccurred(), "deletion should fail while faults are active")

		By("Clearing faults and verifying finalization completes")
		deleteFaultCfg.Deactivate()

		Eventually(func() error {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespaceName,
			})
			return err
		}, chaosTimeout, chaosInterval).Should(Succeed(),
			"reconciler should complete finalization once Delete errors stop")
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
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespaceName,
			})
			return err
		}, chaosTimeout, chaosInterval).Should(Succeed(),
			"reconciler should eventually converge despite intermittent errors")
	})
})
