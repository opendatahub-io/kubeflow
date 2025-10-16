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
	// kube-rbac-proxy configuration
	KubeRbacProxyServicePort     = 8443
	KubeRbacProxyServicePortName = "kube-rbac-proxy"
	KubeRbacProxyConfigSuffix    = "-kube-rbac-proxy-config"
	KubeRbacProxyServiceSuffix   = "-rbac" // TODO
)

type KubeRbacProxyConfig struct {
	ProxyImage string
}

// ReconcileNotebookServiceAccount will manage the service account reconciliation
// required by the notebook for kube-rbac-proxy
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

// NewNotebookKubeRbacProxyService defines the desired service object for kube-rbac-proxy
func NewNotebookKubeRbacProxyService(notebook *nbv1.Notebook) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      notebook.Name + KubeRbacProxyServiceSuffix,
			Namespace: notebook.Namespace,
			Labels: map[string]string{
				"notebook-name": notebook.Name,
			},
			Annotations: map[string]string{
				"service.beta.openshift.io/serving-cert-secret-name": notebook.Name + KubeRbacProxyTLSCertVolumeSecretSuffix,
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				Name:       KubeRbacProxyServicePortName,
				Port:       KubeRbacProxyServicePort,
				TargetPort: intstr.FromString(KubeRbacProxyServicePortName),
				Protocol:   corev1.ProtocolTCP,
			}},
			Selector: map[string]string{
				"statefulset": notebook.Name,
			},
		},
	}
}

// ReconcileKubeRbacProxyService will manage the service reconciliation required
// by the notebook kube-rbac-proxy
func (r *OpenshiftNotebookReconciler) ReconcileKubeRbacProxyService(notebook *nbv1.Notebook, ctx context.Context) error {
	// Initialize logger format
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	// Generate the desired kube-rbac-proxy service
	desiredService := NewNotebookKubeRbacProxyService(notebook)

	// Create the kube-rbac-proxy service if it does not already exist
	foundService := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      desiredService.GetName(),
		Namespace: notebook.GetNamespace(),
	}, foundService)
	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("Creating kube-rbac-proxy Service")
			// Add .metatada.ownerReferences to the kube-rbac-proxy service to be deleted by
			// the Kubernetes garbage collector if the notebook is deleted
			err = ctrl.SetControllerReference(notebook, desiredService, r.Scheme)
			if err != nil {
				log.Error(err, "Unable to add OwnerReference to the kube-rbac-proxy Service")
				return err
			}
			// Create the kube-rbac-proxy service in the Openshift cluster
			err = r.Create(ctx, desiredService)
			if err != nil && !apierrs.IsAlreadyExists(err) {
				log.Error(err, "Unable to create the kube-rbac-proxy Service")
				return err
			}
		} else {
			log.Error(err, "Unable to fetch the kube-rbac-proxy Service")
			return err
		}
	}

	return nil
}

// NewNotebookKubeRbacProxyHTTPRoute defines the desired HTTPRoute object for kube-rbac-proxy
func NewNotebookKubeRbacProxyHTTPRoute(notebook *nbv1.Notebook, isGenerateName bool) *gatewayv1.HTTPRoute {
	httpRoute := NewNotebookHTTPRoute(notebook, isGenerateName)

	// Update the backend to point to the kube-rbac-proxy service instead of the main service
	httpRoute.Spec.Rules[0].BackendRefs[0].Name = gatewayv1.ObjectName(notebook.Name + KubeRbacProxyServiceSuffix)
	httpRoute.Spec.Rules[0].BackendRefs[0].Port = (*gatewayv1.PortNumber)(&[]gatewayv1.PortNumber{8443}[0])

	return httpRoute
}

// ReconcileKubeRbacProxyHTTPRoute will manage the creation, update and deletion of the kube-rbac-proxy HTTPRoute
// when the notebook is reconciled.
func (r *OpenshiftNotebookReconciler) ReconcileKubeRbacProxyHTTPRoute(
	notebook *nbv1.Notebook, ctx context.Context) error {
	return r.reconcileHTTPRoute(notebook, ctx, NewNotebookKubeRbacProxyHTTPRoute)
}

// NewNotebookKubeRbacProxyConfigMap defines the desired ConfigMap object for kube-rbac-proxy
func NewNotebookKubeRbacProxyConfigMap(notebook *nbv1.Notebook) *corev1.ConfigMap {
	kubeRbacProxyConfig := fmt.Sprintf(`authorization:
  resourceAttributes:
    verb: get
    resource: notebooks
    apiGroup: kubeflow.org
    resourceName: %s
    namespace: %s`, notebook.Name, notebook.Namespace)

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      notebook.Name + KubeRbacProxyConfigSuffix,
			Namespace: notebook.Namespace,
			Labels: map[string]string{
				"notebook-name": notebook.Name,
			},
		},
		Data: map[string]string{
			KubeRbacProxyConfigFileName: kubeRbacProxyConfig,
		},
	}
}

// ReconcileKubeRbacProxyConfigMap will manage the ConfigMap reconciliation required
// by the notebook kube-rbac-proxy
func (r *OpenshiftNotebookReconciler) ReconcileKubeRbacProxyConfigMap(notebook *nbv1.Notebook, ctx context.Context) error {
	// Initialize logger format
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	// Generate the desired kube-rbac-proxy ConfigMap
	desiredConfigMap := NewNotebookKubeRbacProxyConfigMap(notebook)

	// Create the kube-rbac-proxy ConfigMap if it does not already exist
	foundConfigMap := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      desiredConfigMap.GetName(),
		Namespace: notebook.GetNamespace(),
	}, foundConfigMap)
	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("Creating kube-rbac-proxy ConfigMap")
			// Add .metatada.ownerReferences to the kube-rbac-proxy ConfigMap to be deleted by
			// the Kubernetes garbage collector if the notebook is deleted
			err = ctrl.SetControllerReference(notebook, desiredConfigMap, r.Scheme)
			if err != nil {
				log.Error(err, "Unable to add OwnerReference to the kube-rbac-proxy ConfigMap")
				return err
			}
			// Create the kube-rbac-proxy ConfigMap in the cluster
			err = r.Create(ctx, desiredConfigMap)
			if err != nil && !apierrs.IsAlreadyExists(err) {
				log.Error(err, "Unable to create the kube-rbac-proxy ConfigMap")
				return err
			}
		} else {
			log.Error(err, "Unable to fetch the kube-rbac-proxy ConfigMap")
			return err
		}
	} else {
		// ConfigMap exists, check if it needs to be updated
		needsUpdate := false

		// Check if data differs
		if len(foundConfigMap.Data) != len(desiredConfigMap.Data) {
			needsUpdate = true
		} else {
			for key, value := range desiredConfigMap.Data {
				if foundConfigMap.Data[key] != value {
					needsUpdate = true
					break
				}
			}
		}

		// Check if labels differ
		if !needsUpdate {
			if len(foundConfigMap.Labels) != len(desiredConfigMap.Labels) {
				needsUpdate = true
			} else {
				for key, value := range desiredConfigMap.Labels {
					if foundConfigMap.Labels[key] != value {
						needsUpdate = true
						break
					}
				}
			}
		}

		if needsUpdate {
			log.Info("Reconciling kube-rbac-proxy ConfigMap")
			// Update the existing ConfigMap with desired values
			foundConfigMap.Data = desiredConfigMap.Data
			foundConfigMap.Labels = desiredConfigMap.Labels
			err = r.Update(ctx, foundConfigMap)
			if err != nil {
				log.Error(err, "Unable to reconcile the kube-rbac-proxy ConfigMap")
				return err
			}
		}
	}

	return nil
}

// TODO: We need to revisit in favor of https://issues.redhat.com/browse/RHOAIENG-36109
// NewNotebookKubeRbacProxyClusterRoleBinding defines the desired ClusterRoleBinding object for kube-rbac-proxy authentication
// This creates one ClusterRoleBinding per namespace that grants auth-delegator permissions to all ServiceAccounts in that namespace
func NewNotebookKubeRbacProxyClusterRoleBinding(notebook *nbv1.Notebook) *rbacv1.ClusterRoleBinding {
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

// ReconcileKubeRbacProxyClusterRoleBinding will manage the ClusterRoleBinding reconciliation required
// by the notebook kube-rbac-proxy for authentication (tokenreviews and subjectaccessreviews)
func (r *OpenshiftNotebookReconciler) ReconcileKubeRbacProxyClusterRoleBinding(notebook *nbv1.Notebook, ctx context.Context) error {
	// Initialize logger format
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	// Generate the desired ClusterRoleBinding
	desiredClusterRoleBinding := NewNotebookKubeRbacProxyClusterRoleBinding(notebook)

	// Create the ClusterRoleBinding if it does not already exist
	foundClusterRoleBinding := &rbacv1.ClusterRoleBinding{}
	err := r.Get(ctx, types.NamespacedName{
		Name: desiredClusterRoleBinding.GetName(),
	}, foundClusterRoleBinding)
	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("Creating kube-rbac-proxy ClusterRoleBinding")
			// Note: ClusterRoleBindings cannot have ownerReferences to namespaced resources
			// so we'll need to clean them up manually when the notebook is deleted
			err = r.Create(ctx, desiredClusterRoleBinding)
			if err != nil && !apierrs.IsAlreadyExists(err) {
				log.Error(err, "Unable to create the kube-rbac-proxy ClusterRoleBinding")
				return err
			}
		} else {
			log.Error(err, "Unable to fetch the kube-rbac-proxy ClusterRoleBinding")
			return err
		}
	}

	return nil
}

// CleanupKubeRbacProxyClusterRoleBinding removes the ClusterRoleBinding associated with the namespace
// if this is the last auth-enabled notebook in the namespace
func (r *OpenshiftNotebookReconciler) CleanupKubeRbacProxyClusterRoleBinding(notebook *nbv1.Notebook, ctx context.Context) error {
	// Initialize logger format
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	// Check if there are other auth-enabled notebooks in this namespace
	notebookList := &nbv1.NotebookList{}
	err := r.List(ctx, notebookList, client.InNamespace(notebook.Namespace))
	if err != nil {
		log.Error(err, "Unable to list notebooks in namespace")
		return err
	}

	// Count auth-enabled notebooks (excluding the current one being deleted/disabled)
	authNotebookCount := 0
	for _, nb := range notebookList.Items {
		// Skip the current notebook if it's being deleted or if it's the same notebook
		if nb.Name == notebook.Name {
			continue
		}
		if KubeRbacProxyInjectionIsEnabled(nb.ObjectMeta) {
			authNotebookCount++
		}
	}

	// Only delete the ClusterRoleBinding if this is the last auth-enabled notebook in the namespace
	if authNotebookCount == 0 {
		clusterRoleBindingName := fmt.Sprintf("odh-notebooks-%s-auth-delegator", notebook.Namespace)

		clusterRoleBinding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: clusterRoleBindingName,
			},
		}

		err := r.Delete(ctx, clusterRoleBinding)
		if err != nil && !apierrs.IsNotFound(err) {
			log.Error(err, "Unable to delete kube-rbac-proxy ClusterRoleBinding")
			return err
		}

		if err == nil {
			log.Info("Deleted kube-rbac-proxy ClusterRoleBinding for namespace", "clusterRoleBinding", clusterRoleBindingName)
		}
	} else {
		log.Info("Keeping kube-rbac-proxy ClusterRoleBinding as other auth-enabled notebooks exist in namespace", "count", authNotebookCount)
	}

	return nil
}
