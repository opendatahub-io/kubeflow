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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
)

var _ = Describe("MLflow Integration", func() {
	const (
		Name                 = "test-notebook-mlflow"
		Namespace            = "default"
		testGatewayNamespace = "openshift-ingress"
		testGatewayHostname  = "gateway.example.com"
		testGatewayName      = "data-science-gateway"
		// Match timeout/interval from notebook_controller_test.go to avoid
		// flaky AfterEach cleanup on slow CI runners (finalizer removal
		// requires multiple UPDATE round-trips through both webhooks).
		duration = 10 * time.Second
		interval = 200 * time.Millisecond
	)

	var (
		expectedTrackingURI = fmt.Sprintf("https://%s/%s", testGatewayHostname, MLflowIdentifier)
	)

	var (
		notebook *nbv1.Notebook
		log      = ctrl.Log.WithName("test")
	)

	BeforeEach(func() {
		notebook = &nbv1.Notebook{
			ObjectMeta: metav1.ObjectMeta{
				Name:      Name,
				Namespace: Namespace,
			},
			Spec: nbv1.NotebookSpec{
				Template: nbv1.NotebookTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  Name,
								Image: "registry.redhat.io/ubi9/ubi:latest",
							},
						},
					},
				},
			},
		}
	})

	Describe("ReconcileMLflowIntegration", func() {
		var (
			testScheme *runtime.Scheme
			reconciler *OpenshiftNotebookReconciler
		)

		BeforeEach(func() {
			testScheme = runtime.NewScheme()
			utilruntime.Must(clientgoscheme.AddToScheme(testScheme))
			utilruntime.Must(nbv1.AddToScheme(testScheme))

			reconciler = &OpenshiftNotebookReconciler{
				Client:        cli,
				Log:           log,
				Config:        cfg,
				Scheme:        testScheme,
				EventRecorder: record.NewFakeRecorder(10),
			}
		})

		Context("when mlflow-instance annotation is not present", func() {
			BeforeEach(func() {
				// Create the notebook so it has a UID (matches production: we create RoleBinding with ownerReference to this)
				Expect(cli.Create(ctx, notebook)).To(Succeed())
			})

			AfterEach(func() {
				Eventually(func() error {
					nb := &nbv1.Notebook{}
					if err := cli.Get(ctx, types.NamespacedName{Name: Name, Namespace: Namespace}, nb); err != nil {
						return nil
					}
					return cli.Delete(ctx, nb)
				}, duration, interval).Should(Succeed())
			})

			It("should clean up existing RoleBinding when annotation is absent", func() {
				var currentNotebook nbv1.Notebook
				Expect(cli.Get(ctx, types.NamespacedName{Name: Name, Namespace: Namespace}, &currentNotebook)).To(Succeed())

				// Create RoleBinding with controller reference, as ReconcileMLflowRoleBinding does when label is enabled
				roleBinding := NewRoleBinding(&currentNotebook, mlflowRoleBindingName(&currentNotebook), "ClusterRole", MLflowClusterRoleName)
				Expect(ctrl.SetControllerReference(&currentNotebook, roleBinding, testScheme)).To(Succeed())
				Expect(cli.Create(ctx, roleBinding)).To(Succeed())

				// Notebook without annotation should trigger explicit cleanup (user disabled integration)
				result, err := reconciler.ReconcileMLflowIntegration(&currentNotebook, ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(ctrl.Result{}))

				// Verify RoleBinding was cleaned up by CleanupMLflowRoleBinding (explicit delete by name)
				roleBindingName := mlflowRoleBindingName(notebook)
				Eventually(func() bool {
					err := cli.Get(ctx, types.NamespacedName{Name: roleBindingName, Namespace: Namespace}, &rbacv1.RoleBinding{})
					return apierrors.IsNotFound(err)
				}, duration, interval).Should(BeTrue())
			})
		})

		Context("when mlflow-instance annotation is present but ClusterRole does not exist", func() {
			BeforeEach(func() {
				notebook.Annotations = map[string]string{
					MLflowInstanceAnnotation: MLflowIdentifier,
				}
				Expect(cli.Create(ctx, notebook)).To(Succeed())
			})

			AfterEach(func() {
				Eventually(func() error {
					nb := &nbv1.Notebook{}
					if err := cli.Get(ctx, types.NamespacedName{Name: Name, Namespace: Namespace}, nb); err != nil {
						return nil
					}
					return cli.Delete(ctx, nb)
				}, duration, interval).Should(Succeed())
			})

			It("should requeue without creating a RoleBinding", func() {
				var currentNotebook nbv1.Notebook
				Expect(cli.Get(ctx, types.NamespacedName{Name: Name, Namespace: Namespace}, &currentNotebook)).To(Succeed())

				result, err := reconciler.ReconcileMLflowIntegration(&currentNotebook, ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(30 * time.Second))

				// Verify no RoleBinding was created
				roleBindingName := mlflowRoleBindingName(notebook)
				err = cli.Get(ctx, types.NamespacedName{Name: roleBindingName, Namespace: Namespace}, &rbacv1.RoleBinding{})
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
			})
		})

		Context("when mlflow-instance annotation is present", func() {
			var mlflowClusterRole *rbacv1.ClusterRole

			BeforeEach(func() {
				// Set the mlflow instance annotation to enable integration (use default 'mlflow')
				notebook.Annotations = map[string]string{
					MLflowInstanceAnnotation: MLflowIdentifier,
				}

				// Create the MLflow ClusterRole (required — OpenShift rejects RoleBindings referencing non-existent ClusterRoles)
				mlflowClusterRole = &rbacv1.ClusterRole{
					ObjectMeta: metav1.ObjectMeta{
						Name: MLflowClusterRoleName,
					},
				}
				_ = cli.Create(ctx, mlflowClusterRole)

				// Create ServiceAccount for the notebook
				serviceAccount := &corev1.ServiceAccount{
					ObjectMeta: metav1.ObjectMeta{
						Name:      Name,
						Namespace: Namespace,
					},
				}
				_ = cli.Create(ctx, serviceAccount)

				// Create the notebook so it has a UID (required for controller reference)
				Expect(cli.Create(ctx, notebook)).To(Succeed())
			})

			AfterEach(func() {
				// Clean up RoleBinding if created
				roleBindingName := mlflowRoleBindingName(notebook)
				_ = cli.Delete(ctx, &rbacv1.RoleBinding{
					ObjectMeta: metav1.ObjectMeta{
						Name:      roleBindingName,
						Namespace: Namespace,
					},
				})
				// Clean up ClusterRole
				if mlflowClusterRole != nil {
					_ = cli.Delete(ctx, mlflowClusterRole)
				}
				// Clean up notebook
				_ = cli.Delete(ctx, notebook)
			})

			It("should create RoleBinding when annotation is present", func() {
				// Get the notebook from the API server to ensure it has a UID
				var currentNotebook nbv1.Notebook
				Expect(cli.Get(ctx, types.NamespacedName{Name: Name, Namespace: Namespace}, &currentNotebook)).To(Succeed())

				result, err := reconciler.ReconcileMLflowIntegration(&currentNotebook, ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(ctrl.Result{}))

				// Verify RoleBinding was created
				roleBindingName := mlflowRoleBindingName(notebook)
				Eventually(func() error {
					return cli.Get(ctx, types.NamespacedName{Name: roleBindingName, Namespace: Namespace}, &rbacv1.RoleBinding{})
				}, duration, interval).Should(Succeed())
			})
		})
	})

	Describe("HandleMLflowEnvVars", func() {
		Context("when mlflow-instance annotation is not set", func() {
			It("should NOT inject any MLflow environment variables", func() {
				HandleMLflowEnvVars(ctx, cli, notebook, log)

				container := findNotebookContainer(notebook)
				_, hasK8s := getEnvVarValue(container, MLflowK8sIntegrationEnvVar)
				Expect(hasK8s).To(BeFalse())
				_, hasAuth := getEnvVarValue(container, MLflowTrackingAuthEnvVar)
				Expect(hasAuth).To(BeFalse())
				_, hasURI := getEnvVarValue(container, MLflowTrackingURIEnvVar)
				Expect(hasURI).To(BeFalse())
			})
		})

		Context("when mlflow-instance annotation is present", func() {
			It("should inject MLFLOW_K8S_INTEGRATION as 'true' and MLFLOW_TRACKING_AUTH as 'kubernetes-namespaced'", func() {
				// Set the mlflow instance annotation to enable integration (use default 'mlflow')
				notebook.Annotations = map[string]string{
					MLflowInstanceAnnotation: MLflowIdentifier,
				}

				HandleMLflowEnvVars(ctx, cli, notebook, log)

				container := findNotebookContainer(notebook)
				k8sVal, k8sFound := getEnvVarValue(container, MLflowK8sIntegrationEnvVar)
				Expect(k8sFound).To(BeTrue())
				Expect(k8sVal).To(Equal("true"))

				authVal, authFound := getEnvVarValue(container, MLflowTrackingAuthEnvVar)
				Expect(authFound).To(BeTrue())
				Expect(authVal).To(Equal(MLflowTrackingAuthValue))
			})
		})

		Context("when mlflow-instance annotation is set to an empty value", func() {
			It("should NOT inject MLFLOW_K8S_INTEGRATION or MLFLOW_TRACKING_AUTH", func() {
				// Set the annotation to an empty/whitespace value (treated as disabled)
				notebook.Annotations = map[string]string{
					MLflowInstanceAnnotation: " ",
				}

				HandleMLflowEnvVars(ctx, cli, notebook, log)

				container := findNotebookContainer(notebook)
				_, hasK8s := getEnvVarValue(container, MLflowK8sIntegrationEnvVar)
				Expect(hasK8s).To(BeFalse())
				_, hasAuth := getEnvVarValue(container, MLflowTrackingAuthEnvVar)
				Expect(hasAuth).To(BeFalse())
			})
		})

		Context("when opendatahub.io/mlflow-instance annotation is set to MLflowIdentifier and Gateway is configured", func() {
			var (
				gateway *unstructured.Unstructured
			)

			BeforeEach(func() {
				// Create Gateway CR (openshift-ingress namespace is created in suite BeforeSuite)
				gateway = createGatewayCR(testGatewayName, testGatewayNamespace, testGatewayHostname)

				err := cli.Create(ctx, gateway)
				if err != nil && !apierrors.IsAlreadyExists(err) {
					Expect(err).NotTo(HaveOccurred())
				}

				// Set the mlflow instance annotation to enable integration (use default 'mlflow')
				notebook.Annotations = map[string]string{
					MLflowInstanceAnnotation: MLflowIdentifier,
				}
			})

			AfterEach(func() {
				if gateway != nil {
					_ = cli.Delete(ctx, gateway)
				}
			})

			It("should inject all MLflow environment variables", func() {
				HandleMLflowEnvVars(ctx, cli, notebook, log)

				container := findNotebookContainer(notebook)

				k8sVal, k8sFound := getEnvVarValue(container, MLflowK8sIntegrationEnvVar)
				Expect(k8sFound).To(BeTrue())
				Expect(k8sVal).To(Equal("true"))

				authVal, authFound := getEnvVarValue(container, MLflowTrackingAuthEnvVar)
				Expect(authFound).To(BeTrue())
				Expect(authVal).To(Equal(MLflowTrackingAuthValue))

				uriVal, uriFound := getEnvVarValue(container, MLflowTrackingURIEnvVar)
				Expect(uriFound).To(BeTrue())
				Expect(uriVal).To(Equal(expectedTrackingURI))
			})

			It("should construct tracking URI path as mlflow-<instanceName> when instance name is not MLflowIdentifier", func() {
				customInstanceName := "my-mlflow-instance"
				notebook.Annotations = map[string]string{
					MLflowInstanceAnnotation: customInstanceName,
				}
				expectedPathSegment := fmt.Sprintf("%s-%s", MLflowIdentifier, customInstanceName)
				expectedURI := fmt.Sprintf("https://%s/%s", testGatewayHostname, expectedPathSegment)

				HandleMLflowEnvVars(ctx, cli, notebook, log)

				container := findNotebookContainer(notebook)
				uriVal, uriFound := getEnvVarValue(container, MLflowTrackingURIEnvVar)
				Expect(uriFound).To(BeTrue())
				Expect(uriVal).To(Equal(expectedURI))
			})
		})
	})
})

// findNotebookContainer returns a pointer to the container whose name matches the notebook name.
// Fails the test if no matching container is found.
func findNotebookContainer(notebook *nbv1.Notebook) *corev1.Container {
	for i := range notebook.Spec.Template.Spec.Containers {
		if notebook.Spec.Template.Spec.Containers[i].Name == notebook.Name {
			return &notebook.Spec.Template.Spec.Containers[i]
		}
	}
	Fail("notebook image container not found: " + notebook.Name)
	return nil
}

// getEnvVarValue returns the value of the named environment variable and whether it was found.
func getEnvVarValue(container *corev1.Container, name string) (string, bool) {
	for _, env := range container.Env {
		if env.Name == name {
			return env.Value, true
		}
	}
	return "", false
}

// createGatewayCR creates an unstructured Gateway CR for testing
func createGatewayCR(name, namespace, hostname string) *unstructured.Unstructured {
	gateway := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "Gateway",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
			"spec": map[string]interface{}{
				"gatewayClassName": "openshift-default",
				"listeners": []interface{}{
					map[string]interface{}{
						"hostname": hostname,
						"name":     "https",
						"port":     int64(443),
						"protocol": "HTTPS",
					},
				},
			},
		},
	}
	gateway.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "gateway.networking.k8s.io",
		Version: "v1",
		Kind:    "Gateway",
	})
	return gateway
}
