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

	"k8s.io/apimachinery/pkg/util/intstr"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	// RBAC proxy configuration
	RbacServicePort     = 8443
	RbacServicePortName = "kube-rbac-proxy"
)

type RbacConfig struct {
	ProxyImage string
}

// ReconcileNotebookServiceAccount will manage the service account reconciliation
// required by the notebook for RBAC proxy
func (r *OpenshiftNotebookReconciler) ReconcileNotebookServiceAccount(notebook *nbv1.Notebook, ctx context.Context) error {
	// Initialize logger format
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	// Generate the desired service account
	desiredServiceAccount := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      notebook.Name,
			Namespace: notebook.Namespace,
			Labels: map[string]string{
				"notebook-name": notebook.Name,
			},
		},
	}

	// Create the service account if it does not already exist
	foundServiceAccount := &corev1.ServiceAccount{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      desiredServiceAccount.GetName(),
		Namespace: notebook.GetNamespace(),
	}, foundServiceAccount)
	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("Creating Service Account")
			// Add .metatada.ownerReferences to the service account to be deleted by
			// the Kubernetes garbage collector if the notebook is deleted
			err = ctrl.SetControllerReference(notebook, desiredServiceAccount, r.Scheme)
			if err != nil {
				log.Error(err, "Unable to add OwnerReference to the Service Account")
				return err
			}
			// Create the service account in the Openshift cluster
			err = r.Create(ctx, desiredServiceAccount)
			if err != nil && !apierrs.IsAlreadyExists(err) {
				log.Error(err, "Unable to create the Service Account")
				return err
			}
		} else {
			log.Error(err, "Unable to fetch the Service Account")
			return err
		}
	}

	return nil
}

// NewNotebookRbacService defines the desired RBAC service object for kube-rbac-proxy
func NewNotebookRbacService(notebook *nbv1.Notebook) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      notebook.Name + "-rbac",
			Namespace: notebook.Namespace,
			Labels: map[string]string{
				"notebook-name": notebook.Name,
			},
			Annotations: map[string]string{
				"service.beta.openshift.io/serving-cert-secret-name": notebook.Name + "-rbac-tls",
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				Name:       RbacServicePortName,
				Port:       RbacServicePort,
				TargetPort: intstr.FromString(RbacServicePortName),
				Protocol:   corev1.ProtocolTCP,
			}},
			Selector: map[string]string{
				"statefulset": notebook.Name,
			},
		},
	}
}

// ReconcileRbacService will manage the RBAC service reconciliation required
// by the notebook RBAC proxy
func (r *OpenshiftNotebookReconciler) ReconcileRbacService(notebook *nbv1.Notebook, ctx context.Context) error {
	// Initialize logger format
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	// Generate the desired RBAC service
	desiredService := NewNotebookRbacService(notebook)

	// Create the RBAC service if it does not already exist
	foundService := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      desiredService.GetName(),
		Namespace: notebook.GetNamespace(),
	}, foundService)
	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("Creating RBAC Service")
			// Add .metatada.ownerReferences to the RBAC service to be deleted by
			// the Kubernetes garbage collector if the notebook is deleted
			err = ctrl.SetControllerReference(notebook, desiredService, r.Scheme)
			if err != nil {
				log.Error(err, "Unable to add OwnerReference to the RBAC Service")
				return err
			}
			// Create the RBAC service in the Openshift cluster
			err = r.Create(ctx, desiredService)
			if err != nil && !apierrs.IsAlreadyExists(err) {
				log.Error(err, "Unable to create the RBAC Service")
				return err
			}
		} else {
			log.Error(err, "Unable to fetch the RBAC Service")
			return err
		}
	}

	return nil
}

// NewNotebookRbacHTTPRoute defines the desired RBAC HTTPRoute object for kube-rbac-proxy
func NewNotebookRbacHTTPRoute(notebook *nbv1.Notebook, isGenerateName bool) *gatewayv1.HTTPRoute {
	httpRoute := NewNotebookHTTPRoute(notebook, isGenerateName)

	// Update the backend to point to the RBAC service instead of the main service
	httpRoute.Spec.Rules[0].BackendRefs[0].Name = gatewayv1.ObjectName(notebook.Name + "-rbac")
	httpRoute.Spec.Rules[0].BackendRefs[0].Port = (*gatewayv1.PortNumber)(&[]gatewayv1.PortNumber{8443}[0])

	return httpRoute
}

// ReconcileRbacHTTPRoute will manage the creation, update and deletion of the RBAC HTTPRoute
// when the notebook is reconciled.
func (r *OpenshiftNotebookReconciler) ReconcileRbacHTTPRoute(
	notebook *nbv1.Notebook, ctx context.Context) error {
	return r.reconcileHTTPRoute(notebook, ctx, NewNotebookRbacHTTPRoute)
}

// NewNotebookRbacConfigMap defines the desired RBAC ConfigMap object for kube-rbac-proxy
func NewNotebookRbacConfigMap(notebook *nbv1.Notebook) *corev1.ConfigMap {
	rbacConfig := fmt.Sprintf(`authorization:
  resourceAttributes:
    verb: get
    resource: notebooks
    apiGroup: kubeflow.org
    resourceName: %s
    namespace: %s`, notebook.Name, notebook.Namespace)

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      notebook.Name + "-rbac-config",
			Namespace: notebook.Namespace,
			Labels: map[string]string{
				"notebook-name": notebook.Name,
			},
		},
		Data: map[string]string{
			RbacProxyConfigFileName: rbacConfig,
		},
	}
}

// ReconcileRbacConfigMap will manage the RBAC ConfigMap reconciliation required
// by the notebook RBAC proxy
func (r *OpenshiftNotebookReconciler) ReconcileRbacConfigMap(notebook *nbv1.Notebook, ctx context.Context) error {
	// Initialize logger format
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	// Generate the desired RBAC ConfigMap
	desiredConfigMap := NewNotebookRbacConfigMap(notebook)

	// Create the RBAC ConfigMap if it does not already exist
	foundConfigMap := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      desiredConfigMap.GetName(),
		Namespace: notebook.GetNamespace(),
	}, foundConfigMap)
	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("Creating RBAC ConfigMap")
			// Add .metatada.ownerReferences to the RBAC ConfigMap to be deleted by
			// the Kubernetes garbage collector if the notebook is deleted
			err = ctrl.SetControllerReference(notebook, desiredConfigMap, r.Scheme)
			if err != nil {
				log.Error(err, "Unable to add OwnerReference to the RBAC ConfigMap")
				return err
			}
			// Create the RBAC ConfigMap in the cluster
			err = r.Create(ctx, desiredConfigMap)
			if err != nil && !apierrs.IsAlreadyExists(err) {
				log.Error(err, "Unable to create the RBAC ConfigMap")
				return err
			}
		} else {
			log.Error(err, "Unable to fetch the RBAC ConfigMap")
			return err
		}
	}

	return nil
}

// NewNotebookRbacClusterRoleBinding defines the desired ClusterRoleBinding object for kube-rbac-proxy authentication
// This creates one ClusterRoleBinding per namespace that grants auth-delegator permissions to all ServiceAccounts in that namespace
func NewNotebookRbacClusterRoleBinding(notebook *nbv1.Notebook) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("odh-notebooks-%s-auth-delegator", notebook.Namespace),
			Labels: map[string]string{
				"opendatahub.io/component": "notebook-controller",
				"opendatahub.io/namespace": notebook.Namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "system:auth-delegator",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:     "Group",
				Name:     fmt.Sprintf("system:serviceaccounts:%s", notebook.Namespace),
				APIGroup: "rbac.authorization.k8s.io",
			},
		},
	}
}

// ReconcileRbacClusterRoleBinding will manage the ClusterRoleBinding reconciliation required
// by the notebook RBAC proxy for authentication (tokenreviews and subjectaccessreviews)
func (r *OpenshiftNotebookReconciler) ReconcileRbacClusterRoleBinding(notebook *nbv1.Notebook, ctx context.Context) error {
	// Initialize logger format
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	// Generate the desired ClusterRoleBinding
	desiredClusterRoleBinding := NewNotebookRbacClusterRoleBinding(notebook)

	// Create the ClusterRoleBinding if it does not already exist
	foundClusterRoleBinding := &rbacv1.ClusterRoleBinding{}
	err := r.Get(ctx, types.NamespacedName{
		Name: desiredClusterRoleBinding.GetName(),
	}, foundClusterRoleBinding)
	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("Creating RBAC ClusterRoleBinding")
			// Note: ClusterRoleBindings cannot have ownerReferences to namespaced resources
			// so we'll need to clean them up manually when the notebook is deleted
			err = r.Create(ctx, desiredClusterRoleBinding)
			if err != nil && !apierrs.IsAlreadyExists(err) {
				log.Error(err, "Unable to create the RBAC ClusterRoleBinding")
				return err
			}
		} else {
			log.Error(err, "Unable to fetch the RBAC ClusterRoleBinding")
			return err
		}
	}

	return nil
}

// CleanupRbacClusterRoleBinding removes the ClusterRoleBinding associated with the namespace
// if this is the last RBAC-enabled notebook in the namespace
func (r *OpenshiftNotebookReconciler) CleanupRbacClusterRoleBinding(notebook *nbv1.Notebook, ctx context.Context) error {
	// Initialize logger format
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	// Check if there are other RBAC-enabled notebooks in this namespace
	notebookList := &nbv1.NotebookList{}
	err := r.List(ctx, notebookList, client.InNamespace(notebook.Namespace))
	if err != nil {
		log.Error(err, "Unable to list notebooks in namespace")
		return err
	}

	// Count RBAC-enabled notebooks (excluding the current one being deleted/disabled)
	rbacNotebookCount := 0
	for _, nb := range notebookList.Items {
		// Skip the current notebook if it's being deleted or if it's the same notebook
		if nb.Name == notebook.Name {
			continue
		}
		if RbacInjectionIsEnabled(nb.ObjectMeta) {
			rbacNotebookCount++
		}
	}

	// Only delete the ClusterRoleBinding if this is the last RBAC-enabled notebook in the namespace
	if rbacNotebookCount == 0 {
		clusterRoleBindingName := fmt.Sprintf("odh-notebooks-%s-auth-delegator", notebook.Namespace)

		clusterRoleBinding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: clusterRoleBindingName,
			},
		}

		err := r.Delete(ctx, clusterRoleBinding)
		if err != nil && !apierrs.IsNotFound(err) {
			log.Error(err, "Unable to delete RBAC ClusterRoleBinding")
			return err
		}

		if err == nil {
			log.Info("Deleted RBAC ClusterRoleBinding for namespace", "clusterRoleBinding", clusterRoleBindingName)
		}
	} else {
		log.Info("Keeping RBAC ClusterRoleBinding as other RBAC-enabled notebooks exist in namespace", "count", rbacNotebookCount)
	}

	return nil
}
