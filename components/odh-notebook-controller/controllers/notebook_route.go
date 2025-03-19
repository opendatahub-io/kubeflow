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
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Route Url are defined with combination of Route.name and Route.namespace
// The max sub-domain length is 63 characters, to keep the bound is place
// we are using the generateName for the Route object. The const used for this
// would be max 48 chars for route.name and 15 chars for route.namespace
const (
	// RouteNameMaxLen is the max length of the route name
	RouteNameMaxLen = 48
	// RouteNamespaceMaxLen is the max length of the route namespace
	RouteNamespaceMaxLen = 15
)

// NewNotebookRoute defines the desired route object
func NewNotebookRoute(notebook *nbv1.Notebook, isgenerateName bool) *routev1.Route {

	routeMetadata := metav1.ObjectMeta{
		Name:      notebook.Name,
		Namespace: notebook.Namespace,
		Labels: map[string]string{
			"notebook-name": notebook.Name,
		},
	}

	// If the route name + namespace is greater than 63 characters, the route name would be created by generateName
	// ex: notebook-name 48 + namespace(rhods-notebooks) 15 = 63
	if isgenerateName {
		routeMetadata = metav1.ObjectMeta{
			GenerateName: "nb-",
			Namespace:    notebook.Namespace,
			Labels: map[string]string{
				"notebook-name": notebook.Name,
			},
		}
	}
	return &routev1.Route{
		ObjectMeta: routeMetadata,
		Spec: routev1.RouteSpec{
			To: routev1.RouteTargetReference{
				Kind:   "Service",
				Name:   notebook.Name,
				Weight: ptr.To[int32](100),
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
	ctx context.Context, newRoute func(*nbv1.Notebook, bool) *routev1.Route) error {
	// Initialize logger format
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	var isGenerateName bool = false
	// If the route name + namespace is greater than 63 characters, the route name would be created by generateName
	// ex: notebook-name 48 + namespace(rhods-notebooks) 15 = 63
	if len(notebook.Name) > RouteNameMaxLen && len(notebook.Namespace) >= RouteNamespaceMaxLen {
		log.Info("Route name is too long, using generateName")
		isGenerateName = true
		// Note: Also update service account redirect reference once route is created.
	}

	// Generate the desired route
	desiredRoute := newRoute(notebook, isGenerateName)

	// Create the route if it does not already exist
	foundRoute := &routev1.Route{}
	routeList := &routev1.RouteList{}
	justCreated := false

	// List the routes in the notebook namespace with the notebook name label
	opts := []client.ListOption{
		client.InNamespace(notebook.Namespace),
		client.MatchingLabels{"notebook-name": notebook.Name},
	}

	err := r.List(ctx, routeList, opts...)
	if err != nil {
		log.Error(err, "Unable to list the Route")
	}

	// Get the route from the list
	for _, nRoute := range routeList.Items {
		if metav1.IsControlledBy(&nRoute, notebook) {
			foundRoute = &nRoute
			break
		}
	}
	// If the route is not found, create it
	if foundRoute.Name == "" && foundRoute.Namespace == "" {
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
	}

	// Reconcile the route spec if it has been manually modified
	if !justCreated && !CompareNotebookRoutes(*desiredRoute, *foundRoute) {
		log.Info("Reconciling Route")
		// Retry the update operation when the ingress controller eventually
		// updates the resource version field
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			// Get the last route revision
			if err := r.Get(ctx, types.NamespacedName{
				Name:      foundRoute.Name,
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

	// Update service account redirect reference if justCreated and isGenerateName
	if justCreated && isGenerateName {
		// get the generated route name
		findRoute := &routev1.Route{}
		routeList := &routev1.RouteList{}
		opts := []client.ListOption{
			client.InNamespace(notebook.Namespace),
			client.MatchingLabels{"notebook-name": notebook.Name},
		}

		err := r.List(ctx, routeList, opts...)
		if err != nil {
			log.Error(err, "Unable to list the Route")
		}

		// Get the route from the list
		for _, nRoute := range routeList.Items {
			if metav1.IsControlledBy(&nRoute, notebook) {
				findRoute = &nRoute
				break
			}
		}
		// Update the service account if already exist
		foundSA := &corev1.ServiceAccount{}
		err = r.Get(ctx, types.NamespacedName{
			Name:      notebook.Name,
			Namespace: notebook.Namespace,
		}, foundSA)
		if err == nil && foundSA.Name != "" {
			log.Info("Updating Service Account")
			foundSA = InsertSecondRedirectReference(foundSA, findRoute.Name)
			err = r.Update(ctx, foundSA)
			if err != nil {
				log.Error(err, "Unable to update the Service Account")
				return err
			}
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

// InsertSecondRedirectReference inserts the second redirect reference into the ServiceAccount
func InsertSecondRedirectReference(sa *corev1.ServiceAccount, routeName string) *corev1.ServiceAccount {
	sa.ObjectMeta.Annotations["serviceaccounts.openshift.io/oauth-redirectreference.second"] = "" +
		`{"kind":"OAuthRedirectReference","apiVersion":"v1",` +
		`"reference":{"kind":"Route","name":"` + routeName + `"}}`
	return sa
}
