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
	routev1 "github.com/openshift/api/route/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
)

// NewNotebookRoute defines the desired route object
func NewNotebookRoute(notebook *nbv1.Notebook) *routev1.Route {
	return &routev1.Route{
		ObjectMeta: metav1.ObjectMeta{
			Name:      notebook.Name,
			Namespace: notebook.Namespace,
			Labels: map[string]string{
				"notebook-name": notebook.Name,
			},
		},
		Spec: routev1.RouteSpec{
			// Path: "/notebook/" + notebook.Namespace + "/" + notebook.Name,
			To: routev1.RouteTargetReference{
				Kind:   "Service",
				Name:   notebook.Name,
				Weight: pointer.Int32Ptr(100),
			},
			Port: &routev1.RoutePort{
				TargetPort: intstr.FromString("http-" + notebook.Name),
			},
			TLS: &routev1.TLSConfig{
				Termination:                   routev1.TLSTerminationEdge,
				InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyRedirect,
			},
			WildcardPolicy: routev1.WildcardPolicyNone,
		},
		Status: routev1.RouteStatus{
			Ingress: []routev1.RouteIngress{},
		},
	}
}

// CompareNotebookRoutes checks if two routes are equal, if not return false
func CompareNotebookRoutes(r1 routev1.Route, r2 routev1.Route) bool {
	// Omit the host field since it is reconciled by the ingress controller
	r1.Spec.Host, r2.Spec.Host = "", ""

	// Two routes will be equal if the labels and spec are identical
	return reflect.DeepEqual(r1.ObjectMeta.Labels, r2.ObjectMeta.Labels) &&
		reflect.DeepEqual(r1.Spec, r2.Spec)
}

// Reconcile will manage the creation, update and deletion of the route returned
// by the newRoute function
func (r *OpenshiftNotebookReconciler) reconcileRoute(notebook *nbv1.Notebook,
	ctx context.Context, newRoute func(*nbv1.Notebook) *routev1.Route) error {
	// Initialize logger format
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	// Generate the desired route
	desiredRoute := newRoute(notebook)

	// Create the route if it does not already exist
	foundRoute := &routev1.Route{}
	justCreated := false
	err := r.Get(ctx, types.NamespacedName{
		Name:      desiredRoute.Name,
		Namespace: notebook.Namespace,
	}, foundRoute)
	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("Creating Route")
			// Add .metatada.ownerReferences to the route to be deleted by the
			// Kubernetes garbage collector if the notebook is deleted
			err = ctrl.SetControllerReference(notebook, desiredRoute, r.Scheme)
			if err != nil {
				log.Error(err, "Unable to add OwnerReference to the Route")
				return err
			}
			// Create the route in the Openshift cluster
			err = r.Create(ctx, desiredRoute)
			if err != nil && !apierrs.IsAlreadyExists(err) {
				log.Error(err, "Unable to create the Route")
				return err
			}
			justCreated = true
		} else {
			log.Error(err, "Unable to fetch the Route")
			return err
		}
	}

	// Reconcile the route spec if it has been manually modified
	if !justCreated && !CompareNotebookRoutes(*desiredRoute, *foundRoute) {
		log.Info("Reconciling Route")
		// Retry the update operation when the ingress controller eventually
		// updates the resource version field
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			// Get the last route revision
			if err := r.Get(ctx, types.NamespacedName{
				Name:      desiredRoute.Name,
				Namespace: notebook.Namespace,
			}, foundRoute); err != nil {
				return err
			}
			// Reconcile labels and spec field
			foundRoute.Spec = desiredRoute.Spec
			foundRoute.ObjectMeta.Labels = desiredRoute.ObjectMeta.Labels
			return r.Update(ctx, foundRoute)
		})
		if err != nil {
			log.Error(err, "Unable to reconcile the Route")
			return err
		}
	}

	return nil
}

// ReconcileRoute will manage the creation, update and deletion of the
// TLS route when the notebook is reconciled
func (r *OpenshiftNotebookReconciler) ReconcileRoute(
	notebook *nbv1.Notebook, ctx context.Context) error {
	return r.reconcileRoute(notebook, ctx, NewNotebookRoute)
}
