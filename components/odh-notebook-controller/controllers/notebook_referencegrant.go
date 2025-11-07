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
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

const (
	// ReferenceGrantName is the consistent name for ReferenceGrant per namespace
	ReferenceGrantName = "notebook-httproute-access"
)

// NewNotebookReferenceGrant creates a ReferenceGrant that allows HTTPRoutes from the
// central application namespace to reference Services in the user's namespace where
// Notebooks are created.
func NewNotebookReferenceGrant(namespace string, centralNamespace string) *gatewayv1beta1.ReferenceGrant {
	return &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ReferenceGrantName,
			Namespace: namespace, // User namespace where Services live
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "odh-notebook-controller",
				"opendatahub.io/component":     "notebook-controller",
			},
		},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{
				{
					Group:     gatewayv1.GroupName,
					Kind:      "HTTPRoute",
					Namespace: gatewayv1.Namespace(centralNamespace),
				},
			},
			To: []gatewayv1beta1.ReferenceGrantTo{
				{
					Group: "", // Core group for Services
					Kind:  "Service",
					// Name is optional - if omitted, allows all Services in this namespace
					// We leave it unset to allow any Workbench service in the namespace
					// This may be revisited in the future if we need to be more specific
					// https://issues.redhat.com/browse/RHOAIENG-38217
				},
			},
		},
	}
}

// CompareNotebookReferenceGrants checks if two ReferenceGrants are equal, if not return false
func CompareNotebookReferenceGrants(rg1 gatewayv1beta1.ReferenceGrant, rg2 gatewayv1beta1.ReferenceGrant) bool {
	// Two ReferenceGrants will be equal if the labels and specs are identical
	return reflect.DeepEqual(rg1.Labels, rg2.Labels) &&
		reflect.DeepEqual(rg1.Spec, rg2.Spec)
}

// ReconcileReferenceGrant ensures a ReferenceGrant exists in the Notebook's namespace
// to allow HTTPRoutes from the central namespace to reference backend Services.
// Only one ReferenceGrant per namespace is needed, shared by all Notebooks in that namespace.
func (r *OpenshiftNotebookReconciler) ReconcileReferenceGrant(notebook *nbv1.Notebook, ctx context.Context) error {
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	// Generate the desired ReferenceGrant
	desiredRefGrant := NewNotebookReferenceGrant(notebook.Namespace, r.Namespace)

	// Check if ReferenceGrant already exists
	foundRefGrant := &gatewayv1beta1.ReferenceGrant{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      ReferenceGrantName,
		Namespace: notebook.Namespace,
	}, foundRefGrant)

	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("Creating ReferenceGrant to allow cross-namespace HTTPRoute backend references")
			// Create the ReferenceGrant
			// Note: We cannot use OwnerReference since ReferenceGrant is in user namespace
			// and Notebook could be deleted. We'll use finalizers for cleanup.
			err = r.Create(ctx, desiredRefGrant)
			if err != nil && !apierrs.IsAlreadyExists(err) {
				log.Error(err, "Unable to create ReferenceGrant")
				return err
			}
			log.Info("Successfully created ReferenceGrant")
		} else {
			log.Error(err, "Unable to fetch ReferenceGrant")
			return err
		}
	} else {
		// ReferenceGrant exists - verify it matches the desired state
		if !CompareNotebookReferenceGrants(*desiredRefGrant, *foundRefGrant) {
			log.Info("Updating ReferenceGrant to match desired spec and labels")
			foundRefGrant.Spec = desiredRefGrant.Spec
			foundRefGrant.Labels = desiredRefGrant.Labels
			err = r.Update(ctx, foundRefGrant)
			if err != nil {
				log.Error(err, "Unable to update ReferenceGrant")
				return err
			}
			log.Info("Successfully updated ReferenceGrant")
		}
	}

	return nil
}

// DeleteReferenceGrantIfLastNotebook removes the ReferenceGrant from a namespace
// if the given notebook is the last one in that namespace.
func (r *OpenshiftNotebookReconciler) DeleteReferenceGrantIfLastNotebook(notebook *nbv1.Notebook, ctx context.Context) error {
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	// Check if this is the last notebook in the namespace
	isLast, err := r.isLastNotebookInNamespace(notebook, ctx)
	if err != nil {
		log.Error(err, "Unable to determine if this is the last notebook in namespace")
		return err
	}

	if !isLast {
		log.Info("Other notebooks still exist in this namespace, keeping ReferenceGrant")
		return nil
	}

	// This is the last notebook, delete the ReferenceGrant
	log.Info("This is the last notebook in namespace, deleting ReferenceGrant")
	refGrant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ReferenceGrantName,
			Namespace: notebook.Namespace,
		},
	}

	err = r.Delete(ctx, refGrant)
	if err != nil && !apierrs.IsNotFound(err) {
		log.Error(err, "Unable to delete ReferenceGrant")
		return err
	}

	log.Info("Successfully deleted ReferenceGrant")
	return nil
}

// isLastNotebookInNamespace checks if the given notebook is the last (or only)
// notebook in its namespace that is not being deleted.
func (r *OpenshiftNotebookReconciler) isLastNotebookInNamespace(notebook *nbv1.Notebook, ctx context.Context) (bool, error) {
	notebookList := &nbv1.NotebookList{}
	err := r.List(ctx, notebookList, client.InNamespace(notebook.Namespace))
	if err != nil {
		return false, err
	}

	// Count notebooks that are not being deleted and are not this notebook
	activeCount := 0
	for _, nb := range notebookList.Items {
		// Skip the current notebook and any notebooks being deleted
		if nb.Name != notebook.Name && nb.DeletionTimestamp == nil {
			activeCount++
		}
	}

	// If activeCount is 0, this is the last notebook
	return activeCount == 0, nil
}
