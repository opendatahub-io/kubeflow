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
	"reflect"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

// NewRoleBinding defines the desired RoleBinding or ClusterRoleBinding object.
// Parameters:
//   - notebook:        The Notebook resource instance for which the RoleBinding or ClusterRoleBinding is being created.
//   - rolebindingName: The name to assign to the RoleBinding or ClusterRoleBinding object.
//   - roleRefKind:     The kind of role reference to bind to, which can be either Role or ClusterRole.
//   - roleRefName:     The name of the Role or ClusterRole to reference.
func NewRoleBinding(notebook *nbv1.Notebook, rolebindingName, roleRefKind, roleRefName string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rolebindingName,
			Namespace: notebook.Namespace,
			Labels: map[string]string{
				"notebook-name": notebook.Name,
			},
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      notebook.Name,
				Namespace: notebook.Namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			Kind:     roleRefKind,
			Name:     roleRefName,
			APIGroup: "rbac.authorization.k8s.io",
		},
	}
}

// checkRoleExists checks if a Role or ClusterRole exists in the namespace.
func (r *OpenshiftNotebookReconciler) checkRoleExists(ctx context.Context, roleRefKind, roleRefName, namespace string) (bool, error) {
	if roleRefKind == "ClusterRole" {
		// Check ClusterRole if roleRefKind is ClusterRole
		clusterRole := &rbacv1.ClusterRole{}
		err := r.Get(ctx, types.NamespacedName{Name: roleRefName}, clusterRole)
		if err != nil {
			if apierrs.IsNotFound(err) {
				// ClusterRole not found
				return false, nil
			}
			return false, err // Some other error occurred
		}
	} else {
		// Check Role if roleRefKind is Role
		role := &rbacv1.Role{}
		err := r.Get(ctx, types.NamespacedName{Name: roleRefName, Namespace: namespace}, role)
		if err != nil {
			if apierrs.IsNotFound(err) {
				// Role not found
				return false, nil
			}
			return false, err // Some other error occurred
		}
	}
	return true, nil // Role or ClusterRole exists
}

// reconcileRoleBinding manages creation, update, and deletion of RoleBindings and ClusterRoleBindings
func (r *OpenshiftNotebookReconciler) reconcileRoleBinding(notebook *nbv1.Notebook, ctx context.Context, rolebindingName, roleRefKind, roleRefName string) error {

	log := r.Log.WithValues("notebook", types.NamespacedName{Name: notebook.Name, Namespace: notebook.Namespace})

	// Check if the Role or ClusterRole exists before proceeding
	roleExists, err := r.checkRoleExists(ctx, roleRefKind, roleRefName, notebook.Namespace)
	if err != nil {
		log.Error(err, "Error checking if Role exists", "Role.Kind", roleRefKind, "Role.Name", roleRefName)
		return err
	}
	if !roleExists {
		return nil // Skip if dspa Role is not found on the namespace
	}

	// Create a new RoleBinding based on provided parameters
	roleBinding := NewRoleBinding(notebook, rolebindingName, roleRefKind, roleRefName)

	// Check if the RoleBinding already exists
	found := &rbacv1.RoleBinding{}
	err = r.Get(ctx, types.NamespacedName{Name: rolebindingName, Namespace: notebook.Namespace}, found)
	if err != nil && apierrs.IsNotFound(err) {
		log.Info("Creating RoleBinding", "RoleBinding.Namespace", roleBinding.Namespace, "RoleBinding.Name", roleBinding.Name)

		// Add .metatada.ownerReferences to the Rolebinding to be deleted by
		// the Kubernetes garbage collector if the notebook is deleted
		if err := ctrl.SetControllerReference(notebook, roleBinding, r.Scheme); err != nil {
			log.Error(err, "Failed to set controller reference for RoleBinding")
			return err
		}
		err = r.Create(ctx, roleBinding)
		if err != nil {
			log.Error(err, "Failed to create RoleBinding", "RoleBinding.Namespace", roleBinding.Namespace, "RoleBinding.Name", roleBinding.Name)
			return err
		}
		return nil
	} else if err != nil {
		log.Error(err, "Failed to get RoleBinding")
		return err
	}

	// Update RoleBinding if the subjects differ
	if !reflect.DeepEqual(roleBinding.Subjects, found.Subjects) {
		log.Info("Updating RoleBinding", "RoleBinding.Namespace", roleBinding.Namespace, "RoleBinding.Name", roleBinding.Name)
		err = r.Update(ctx, roleBinding)
		if err != nil {
			log.Error(err, "Failed to update RoleBinding", "RoleBinding.Namespace", roleBinding.Namespace, "RoleBinding.Name", roleBinding.Name)
			return err
		}
	}

	return nil
}

// ReconcileRoleBindings will manage multiple RoleBinding and ClusterRoleBinding reconciliations
func (r *OpenshiftNotebookReconciler) ReconcileRoleBindings(notebook *nbv1.Notebook, ctx context.Context) error {

	roleBindingName := "elyra-pipelines-" + notebook.Name
	// Reconcile a RoleBinding for pipelines for the notebook service account
	if err := r.reconcileRoleBinding(notebook, ctx, roleBindingName, "Role", "ds-pipeline-user-access-dspa"); err != nil {
		return err
	}

	return nil
}
