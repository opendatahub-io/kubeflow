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
	"os"
	"reflect"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// HTTPRoute hostname generation constants
const (
	// HTTPRouteSubDomainMaxLen is the max length of the HTTPRoute subdomain
	HTTPRouteSubDomainMaxLen = 63
	// DefaultGatewayName is the default Gateway name to use for HTTPRoutes prepared by the opendatahub-operator
	DefaultGatewayName = "data-science-gateway"
	// DefaultGatewayNamespace is the default Gateway namespace prepared by the opendatahub-operator
	DefaultGatewayNamespace = "openshift-ingress"
)

// Environment variables for configuration:
// - NOTEBOOK_GATEWAY_NAME: Override the Gateway name (default: "data-science-gateway")
// - NOTEBOOK_GATEWAY_NAMESPACE: Override the Gateway namespace (default: "openshift-ingress")

// NewNotebookHTTPRoute defines the desired HTTPRoute object in the central namespace.
// The HTTPRoute is created in the controller's namespace and references
// the backend Service in the user's namespace using cross-namespace references.
func NewNotebookHTTPRoute(notebook *nbv1.Notebook, centralNamespace string) *gatewayv1.HTTPRoute {
	// Create a unique name combining namespace and notebook name to avoid conflicts
	// Format: nb-{user-namespace}-{notebook-name}
	httpRouteName := "nb-" + notebook.Namespace + "-" + notebook.Name

	routeMetadata := metav1.ObjectMeta{
		Name:      httpRouteName,
		Namespace: centralNamespace, // Central protected namespace
		Labels: map[string]string{
			"notebook-name":      notebook.Name,
			"notebook-namespace": notebook.Namespace, // Critical for filtering and cleanup
		},
	}

	// If the HTTPRoute name (namespace-notebook) is greater than 63 characters, use generateName
	if len(httpRouteName) > HTTPRouteSubDomainMaxLen {
		// Use a prefix that includes namespace info
		prefix := "nb-" + notebook.Namespace[:min(len(notebook.Namespace), 10)] + "-" + notebook.Name[:min(len(notebook.Name), 10)] + "-"
		routeMetadata = metav1.ObjectMeta{
			GenerateName: prefix,
			Namespace:    centralNamespace,
			Labels: map[string]string{
				"notebook-name":      notebook.Name,
				"notebook-namespace": notebook.Namespace,
			},
		}
	}

	// Get Gateway configuration from environment or use defaults
	gatewayName := os.Getenv("NOTEBOOK_GATEWAY_NAME")
	if gatewayName == "" {
		gatewayName = DefaultGatewayName
	}

	gatewayNamespace := os.Getenv("NOTEBOOK_GATEWAY_NAMESPACE")
	if gatewayNamespace == "" {
		gatewayNamespace = DefaultGatewayNamespace
	}

	// Generate notebook path: /notebook/{namespace}/{notebook-name}
	notebookPath := fmt.Sprintf("/notebook/%s/%s", notebook.Namespace, notebook.Name)

	pathPrefix := gatewayv1.PathMatchPathPrefix
	httpRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: routeMetadata,
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Name:      gatewayv1.ObjectName(gatewayName),
						Namespace: (*gatewayv1.Namespace)(&gatewayNamespace),
					},
				},
			},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{
							Path: &gatewayv1.HTTPPathMatch{
								Type:  &pathPrefix,
								Value: &notebookPath,
							},
						},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name:      gatewayv1.ObjectName(notebook.Name),
									Namespace: (*gatewayv1.Namespace)(&notebook.Namespace), // Cross-namespace reference
									Port:      (*gatewayv1.PortNumber)(&[]gatewayv1.PortNumber{8888}[0]),
								},
							},
						},
					},
				},
			},
		},
	}

	return httpRoute
}

// CompareNotebookHTTPRoutes checks if two HTTPRoutes are equal, if not return false
func CompareNotebookHTTPRoutes(r1 gatewayv1.HTTPRoute, r2 gatewayv1.HTTPRoute) bool {
	// Two HTTPRoutes will be equal if the labels and spec are identical
	return reflect.DeepEqual(r1.ObjectMeta.Labels, r2.ObjectMeta.Labels) &&
		reflect.DeepEqual(r1.Spec, r2.Spec)
}

// reconcileHTTPRoute will manage the creation, update and deletion of the HTTPRoute returned
// by the newHTTPRoute function. HTTPRoutes are now created in the central namespace (controller's namespace)
// instead of the user's namespace for security reasons.
func (r *OpenshiftNotebookReconciler) reconcileHTTPRoute(notebook *nbv1.Notebook,
	ctx context.Context, newHTTPRoute func(*nbv1.Notebook, string) *gatewayv1.HTTPRoute) error {
	// Initialize logger format
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	// Generate the desired HTTPRoute in the central namespace
	desiredHTTPRoute := newHTTPRoute(notebook, r.Namespace)

	// Create the HTTPRoute if it does not already exist
	foundHTTPRoute := &gatewayv1.HTTPRoute{}
	httpRouteList := &gatewayv1.HTTPRouteList{}
	justCreated := false

	// List the HTTPRoutes in the CENTRAL namespace (not user namespace) with matching labels
	opts := []client.ListOption{
		client.InNamespace(r.Namespace), // Central namespace = controller's namespace
		client.MatchingLabels{
			"notebook-name":      notebook.Name,
			"notebook-namespace": notebook.Namespace,
		},
	}

	err := r.List(ctx, httpRouteList, opts...)
	if err != nil {
		log.Error(err, "Unable to list the HTTPRoute")
		return err
	}

	// Get the HTTPRoute from the list
	// Note: We cannot use IsControlledBy since HTTPRoute is in different namespace
	// We rely on label matching instead
	if len(httpRouteList.Items) == 1 {
		foundHTTPRoute = &httpRouteList.Items[0]
	} else if len(httpRouteList.Items) > 1 {
		log.Error(fmt.Errorf("multiple HTTPRoutes found for notebook"), "Multiple HTTPRoutes found for notebook")
		return fmt.Errorf("multiple HTTPRoutes found for notebook")
	} else {
		// If the HTTPRoute is not found, create it
		log.Info("Creating HTTPRoute " + desiredHTTPRoute.Name + " in central namespace " + r.Namespace)
		// NOTE: We CANNOT use SetControllerReference because HTTPRoute is in a different namespace
		// than the Notebook. Cross-namespace owner references are not supported in Kubernetes.
		// We will use finalizers for cleanup instead.
		err = r.Create(ctx, desiredHTTPRoute)
		if err != nil && !apierrs.IsAlreadyExists(err) {
			log.Error(err, "Unable to create the HTTPRoute")
			return err
		}
		justCreated = true
		log.Info("Successfully created HTTPRoute in central namespace")
	}

	// Reconcile the HTTPRoute spec if it has been manually modified
	if !justCreated && !CompareNotebookHTTPRoutes(*desiredHTTPRoute, *foundHTTPRoute) {
		log.Info("Reconciling HTTPRoute")
		// Retry the update operation when there are conflicts
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			// Get the last HTTPRoute revision from the central namespace
			if err := r.Get(ctx, types.NamespacedName{
				Name:      foundHTTPRoute.Name,
				Namespace: r.Namespace, // Central namespace
			}, foundHTTPRoute); err != nil {
				return err
			}
			// Reconcile labels and spec field
			foundHTTPRoute.Spec = desiredHTTPRoute.Spec
			foundHTTPRoute.Labels = desiredHTTPRoute.Labels
			return r.Update(ctx, foundHTTPRoute)
		})
		if err != nil {
			log.Error(err, "Unable to reconcile the HTTPRoute")
			return err
		}
	}

	return nil
}

// ReconcileHTTPRoute will manage the creation, update and deletion of the
// HTTPRoute when the notebook is reconciled
func (r *OpenshiftNotebookReconciler) ReconcileHTTPRoute(
	notebook *nbv1.Notebook, ctx context.Context) error {
	return r.reconcileHTTPRoute(notebook, ctx, NewNotebookHTTPRoute)
}

// DeleteHTTPRouteForNotebook deletes the HTTPRoute for a notebook from the central namespace.
// This is called during notebook deletion as part of finalizer cleanup.
func (r *OpenshiftNotebookReconciler) DeleteHTTPRouteForNotebook(notebook *nbv1.Notebook, ctx context.Context) error {
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	// List HTTPRoutes in the central namespace that belong to this notebook
	httpRouteList := &gatewayv1.HTTPRouteList{}
	opts := []client.ListOption{
		client.InNamespace(r.Namespace), // Central namespace
		client.MatchingLabels{
			"notebook-name":      notebook.Name,
			"notebook-namespace": notebook.Namespace,
		},
	}

	err := r.List(ctx, httpRouteList, opts...)
	if err != nil {
		log.Error(err, "Unable to list HTTPRoutes for deletion")
		return err
	}

	// Delete all matching HTTPRoutes
	var deletionErrors []error
	for _, httpRoute := range httpRouteList.Items {
		log.Info("Deleting HTTPRoute from central namespace", "httpRoute", httpRoute.Name)
		err = r.Delete(ctx, &httpRoute)
		if err != nil && !apierrs.IsNotFound(err) {
			log.Error(err, "Unable to delete HTTPRoute", "httpRoute", httpRoute.Name)
			deletionErrors = append(deletionErrors, fmt.Errorf("failed to delete HTTPRoute %s: %w", httpRoute.Name, err))
		}
	}

	if len(deletionErrors) > 0 {
		return fmt.Errorf("failed to delete some HTTPRoutes: %v", deletionErrors)
	}

	log.Info("Successfully deleted HTTPRoute(s) for notebook")
	return nil
}

// EnsureConflictingHTTPRouteAbsent deletes any existing conflicting HTTPRoute for the notebook
// to prevent conflicts when switching between auth and non-auth modes.
func (r *OpenshiftNotebookReconciler) EnsureConflictingHTTPRouteAbsent(
	notebook *nbv1.Notebook, ctx context.Context, isAuthMode bool) error {
	// Initialize logger format
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	// List all HTTPRoutes for this notebook in the CENTRAL namespace
	httpRouteList := &gatewayv1.HTTPRouteList{}
	opts := []client.ListOption{
		client.InNamespace(r.Namespace), // Central namespace
		client.MatchingLabels{
			"notebook-name":      notebook.Name,
			"notebook-namespace": notebook.Namespace,
		},
	}

	err := r.List(ctx, httpRouteList, opts...)
	if err != nil {
		log.Error(err, "Unable to list HTTPRoutes for conflicting route cleanup")
		return err
	}

	// Check each HTTPRoute for this notebook (using label matching since no owner refs)
	for _, httpRoute := range httpRouteList.Items {
		// Determine if this HTTPRoute conflicts with the current mode
		shouldDelete := false

		if len(httpRoute.Spec.Rules) > 0 && len(httpRoute.Spec.Rules[0].BackendRefs) > 0 {
			backendName := string(httpRoute.Spec.Rules[0].BackendRefs[0].Name)
			backendPort := httpRoute.Spec.Rules[0].BackendRefs[0].Port

			isKubeRbacProxyRoute := (backendName == notebook.Name+KubeRbacProxyServiceSuffix) || (backendPort != nil && *backendPort == 8443)
			isRegularRoute := (backendName == notebook.Name) || (backendPort != nil && *backendPort == 8888)

			// Delete conflicting routes:
			// - If switching TO auth mode, delete regular routes
			// - If switching FROM auth mode, delete kube-rbac-proxy routes
			if isAuthMode && isRegularRoute {
				shouldDelete = true
				log.Info("Deleting regular HTTPRoute to switch to auth mode", "httpRoute", httpRoute.Name)
			} else if !isAuthMode && isKubeRbacProxyRoute {
				shouldDelete = true
				log.Info("Deleting kube-rbac-proxy HTTPRoute to switch to non-auth mode", "httpRoute", httpRoute.Name)
			}
		}

		if shouldDelete {
			err = r.Delete(ctx, &httpRoute)
			if err != nil && !apierrs.IsNotFound(err) {
				log.Error(err, "Unable to delete conflicting HTTPRoute", "httpRoute", httpRoute.Name)
				return err
			}
		}
	}

	return nil
}
