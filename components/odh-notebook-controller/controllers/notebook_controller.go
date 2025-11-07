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
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	netv1 "k8s.io/api/networking/v1"

	"github.com/go-logr/logr"
	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	"github.com/kubeflow/kubeflow/components/notebook-controller/pkg/culler"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

const (
	AnnotationInjectAuth               = "notebooks.opendatahub.io/inject-auth"
	AnnotationValueReconciliationLock  = "odh-notebook-controller-lock"
	AnnotationAuthSidecarCPURequest    = "notebooks.opendatahub.io/auth-sidecar-cpu-request"
	AnnotationAuthSidecarMemoryRequest = "notebooks.opendatahub.io/auth-sidecar-memory-request"
	AnnotationAuthSidecarCPULimit      = "notebooks.opendatahub.io/auth-sidecar-cpu-limit"
	AnnotationAuthSidecarMemoryLimit   = "notebooks.opendatahub.io/auth-sidecar-memory-limit"
	DefaultAuthSidecarCPURequest       = "100m"
	DefaultAuthSidecarMemoryRequest    = "64Mi"
	DefaultAuthSidecarCPULimit         = "100m"
	DefaultAuthSidecarMemoryLimit      = "64Mi"
)

const (
	// Finalizer names for cross-namespace resource cleanup
	HTTPRouteFinalizerName      = "notebook.opendatahub.io/httproute-cleanup"
	ReferenceGrantFinalizerName = "notebook.opendatahub.io/referencegrant-cleanup"
	KubeRbacProxyFinalizerName  = "notebook.opendatahub.io/kube-rbac-proxy-cleanup"
)

const (
	OdhConfigMapName        = "odh-trusted-ca-bundle"    // Use ODH Trusted CA Bundle Contains ca-bundle.crt and odh-ca-bundle.crt
	SelfSignedConfigMapName = "kube-root-ca.crt"         // Self-Signed Certs Contains ca.crt
	ServiceCAConfigMapName  = "openshift-service-ca.crt" // Service CA Bundle Contains service-ca.crt
)

// OpenshiftNotebookReconciler holds the controller configuration.
type OpenshiftNotebookReconciler struct {
	client.Client
	Namespace string
	Scheme    *runtime.Scheme
	Log       logr.Logger
	Config    *rest.Config
}

// ClusterRole permissions
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create
// +kubebuilder:rbac:groups=kubeflow.org,resources=notebooks,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=kubeflow.org,resources=notebooks/status,verbs=get
// +kubebuilder:rbac:groups=kubeflow.org,resources=notebooks/finalizers,verbs=update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=referencegrants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;serviceaccounts;secrets;configmaps,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=config.openshift.io,resources=proxies,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies/finalizers,verbs=update;patch
// +kubebuilder:rbac:groups=oauth.openshift.io,resources=oauthclients,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;update;patch;delete
// TODO kept here for the datascience pipelines application server - check whether this is final or not
// +kubebuilder:rbac:groups="route.openshift.io",resources=routes,verbs=get
// +kubebuilder:rbac:groups="image.openshift.io",resources=imagestreams,verbs=list;get;watch
// +kubebuilder:rbac:groups="datasciencepipelinesapplications.opendatahub.io",resources=datasciencepipelinesapplications,verbs=get;list;watch
// +kubebuilder:rbac:groups="datasciencepipelinesapplications.opendatahub.io",resources=datasciencepipelinesapplications/api,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups="gateway.networking.k8s.io",resources=gateways,verbs=get;list;watch

// CompareNotebooks checks if two notebooks are equal, if not return false.
func CompareNotebooks(nb1 nbv1.Notebook, nb2 nbv1.Notebook) bool {
	return reflect.DeepEqual(nb1.ObjectMeta.Labels, nb2.ObjectMeta.Labels) &&
		reflect.DeepEqual(nb1.ObjectMeta.Annotations, nb2.ObjectMeta.Annotations) &&
		reflect.DeepEqual(nb1.Spec, nb2.Spec)
}

// KubeRbacProxyInjectionIsEnabled returns true if the kube-rbac-proxy sidecar injection
// annotation is present in the notebook.
func KubeRbacProxyInjectionIsEnabled(meta metav1.ObjectMeta) bool {
	if meta.Annotations[AnnotationInjectAuth] != "" {
		result, _ := strconv.ParseBool(meta.Annotations[AnnotationInjectAuth])
		return result
	} else {
		return false
	}
}

// ReconciliationLockIsEnabled returns true if the reconciliation lock
// annotation is present in the notebook.
func ReconciliationLockIsEnabled(meta metav1.ObjectMeta) bool {
	if meta.Annotations[culler.STOP_ANNOTATION] != "" {
		return meta.Annotations[culler.STOP_ANNOTATION] == AnnotationValueReconciliationLock
	} else {
		return false
	}
}

// RemoveReconciliationLock waits until the image pull secret is mounted in the
// notebook service account to remove the reconciliation lock annotation.
func (r *OpenshiftNotebookReconciler) RemoveReconciliationLock(notebook *nbv1.Notebook,
	ctx context.Context) error {
	// Wait until the image pull secret is mounted in the notebook service
	// account
	retry.OnError(wait.Backoff{
		Steps:    3,
		Duration: 1 * time.Second,
		Factor:   5.0,
	}, func(error) bool { return true },
		func() error {
			serviceAccount := &corev1.ServiceAccount{}
			if err := r.Get(ctx, types.NamespacedName{
				Name:      notebook.Name,
				Namespace: notebook.Namespace,
			}, serviceAccount); err != nil {
				return err
			}
			if len(serviceAccount.ImagePullSecrets) == 0 {
				return errors.New("pull secret not mounted")
			}
			return nil
		},
	)

	// Remove the reconciliation lock annotation
	patch := client.RawPatch(types.MergePatchType,
		[]byte(`{"metadata":{"annotations":{"`+culler.STOP_ANNOTATION+`":null}}}`))
	return r.Patch(ctx, notebook, patch)
}

// Reconcile performs the reconciling of the Openshift objects for a Kubeflow
// Notebook.
func (r *OpenshiftNotebookReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Initialize logger format
	log := r.Log.WithValues("notebook", req.Name, "namespace", req.Namespace)

	// Get the notebook object when a reconciliation event is triggered (create,
	// update, delete)
	notebook := &nbv1.Notebook{}
	err := r.Get(ctx, req.NamespacedName, notebook)
	if err != nil && apierrs.IsNotFound(err) {
		log.Info("Stop Notebook reconciliation")
		return ctrl.Result{}, nil
	} else if err != nil {
		log.Error(err, "Unable to fetch the Notebook")
		return ctrl.Result{}, err
	}

	// Handle deletion with finalizer
	if notebook.DeletionTimestamp != nil {

		// In RHOAI 2.25, there were used OAuthClient CR for each workbench.
		// We used finalizers to remove them on the workbench destroy operation.
		// Since RHOAI 3.0, the OAuthClient CR aren't created for workbenches anymore,
		// but we're keeping this cleanup code here for any users migrating from 2.x -> 3.x release.
		// Check if we have the OAuth client finalizer
		if r.hasOAuthClientFinalizer(notebook) {
			log.Info("Cleaning up OAuthClient before notebook deletion")
			// Delete the OAuthClient
			err := r.deleteOAuthClient(notebook, ctx)
			if err != nil {
				log.Error(err, "Failed to delete OAuthClient")
				return ctrl.Result{}, err
			}
			// Remove the finalizer
			err = r.removeOAuthClientFinalizer(notebook, ctx)
			if err != nil {
				log.Error(err, "Failed to remove OAuth client finalizer")
				return ctrl.Result{}, err
			}
			log.Info("Successfully cleaned up OAuthClient and removed finalizer")
		}

		// Track which finalizers need to be removed and collect any cleanup errors
		finalizersToRemove := []string{}
		cleanupErrors := []error{}

		// Clean up HTTPRoute from central namespace
		if controllerutil.ContainsFinalizer(notebook, HTTPRouteFinalizerName) {
			log.Info("Cleaning up HTTPRoute from central namespace")
			err := r.DeleteHTTPRouteForNotebook(notebook, ctx)
			if err != nil {
				log.Error(err, "Failed to delete HTTPRoute")
				cleanupErrors = append(cleanupErrors, err)
			} else {
				finalizersToRemove = append(finalizersToRemove, HTTPRouteFinalizerName)
				log.Info("Successfully cleaned up HTTPRoute")
			}
		}

		// Clean up ReferenceGrant if this is the last notebook in the namespace
		if controllerutil.ContainsFinalizer(notebook, ReferenceGrantFinalizerName) {
			log.Info("Checking if ReferenceGrant cleanup is needed")
			err := r.DeleteReferenceGrantIfLastNotebook(notebook, ctx)
			if err != nil {
				log.Error(err, "Failed to delete ReferenceGrant")
				cleanupErrors = append(cleanupErrors, err)
			} else {
				finalizersToRemove = append(finalizersToRemove, ReferenceGrantFinalizerName)
				log.Info("Successfully handled ReferenceGrant cleanup")
			}
		}

		// Notebook is being deleted - clean up kube-rbac-proxy resources
		kubeRbacProxyCleanupSuccess := true
		if KubeRbacProxyInjectionIsEnabled(notebook.ObjectMeta) {
			// Clean up ClusterRoleBinding before allowing deletion
			err = r.CleanupKubeRbacProxyClusterRoleBinding(notebook, ctx)
			if err != nil {
				log.Error(err, "Failed to cleanup kube-rbac-proxy ClusterRoleBinding during deletion")
				kubeRbacProxyCleanupSuccess = false
				cleanupErrors = append(cleanupErrors, err)
			}
		}

		// Remove kube-rbac-proxy finalizer only if cleanup succeeded (or wasn't needed)
		if controllerutil.ContainsFinalizer(notebook, KubeRbacProxyFinalizerName) && kubeRbacProxyCleanupSuccess {
			finalizersToRemove = append(finalizersToRemove, KubeRbacProxyFinalizerName)
		}

		// Remove finalizers for successfully cleaned up resources with retry logic
		// This is done even if some cleanups failed to allow partial progress and avoid race conditions
		if len(finalizersToRemove) > 0 {
			err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
				// Get the latest version of the notebook to avoid conflicts
				currentNotebook := &nbv1.Notebook{}
				if err := r.Get(ctx, types.NamespacedName{Name: notebook.Name, Namespace: notebook.Namespace}, currentNotebook); err != nil {
					return err
				}

				// Remove all finalizers that were successfully cleaned up
				modified := false
				for _, finalizer := range finalizersToRemove {
					if controllerutil.ContainsFinalizer(currentNotebook, finalizer) {
						controllerutil.RemoveFinalizer(currentNotebook, finalizer)
						modified = true
					}
				}

				// Only update if we actually removed finalizers
				if modified {
					return r.Update(ctx, currentNotebook)
				}
				return nil
			})

			if err != nil {
				log.Error(err, "Failed to remove finalizers after retries", "finalizers", finalizersToRemove)
				return ctrl.Result{}, err
			}
			log.Info("Successfully removed finalizers for completed cleanups", "finalizers", finalizersToRemove)
		}

		// If there were any cleanup errors, return and retry for the remaining resources
		if len(cleanupErrors) > 0 {
			// Combine all errors into a single error message for better visibility
			var combinedErr error
			if len(cleanupErrors) == 1 {
				combinedErr = cleanupErrors[0]
			} else {
				// Multiple errors - combine them
				errMsg := fmt.Sprintf("multiple cleanup failures (%d errors): ", len(cleanupErrors))
				for i, err := range cleanupErrors {
					if i > 0 {
						errMsg += "; "
					}
					errMsg += err.Error()
				}
				combinedErr = errors.New(errMsg)
			}
			log.Info("Some cleanup operations failed, will retry on next reconciliation", "errors", len(cleanupErrors))
			return ctrl.Result{}, combinedErr
		}

		return ctrl.Result{}, nil
	}

	// Add finalizers for HTTPRoute and ReferenceGrant cleanup
	// Check which finalizers need to be added
	finalizersToAdd := []string{}
	if !controllerutil.ContainsFinalizer(notebook, HTTPRouteFinalizerName) {
		finalizersToAdd = append(finalizersToAdd, HTTPRouteFinalizerName)
	}
	if !controllerutil.ContainsFinalizer(notebook, ReferenceGrantFinalizerName) {
		finalizersToAdd = append(finalizersToAdd, ReferenceGrantFinalizerName)
	}

	// Add finalizer if kube-rbac-proxy is enabled and finalizer is not present
	if KubeRbacProxyInjectionIsEnabled(notebook.ObjectMeta) && !controllerutil.ContainsFinalizer(notebook, KubeRbacProxyFinalizerName) {
		finalizersToAdd = append(finalizersToAdd, KubeRbacProxyFinalizerName)
	}

	// Add all finalizers with retry logic to handle concurrent modifications
	if len(finalizersToAdd) > 0 {
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			// Get the latest version of the notebook to avoid conflicts
			currentNotebook := &nbv1.Notebook{}
			if err := r.Get(ctx, types.NamespacedName{Name: notebook.Name, Namespace: notebook.Namespace}, currentNotebook); err != nil {
				return err
			}

			// Add all missing finalizers
			modified := false
			for _, finalizer := range finalizersToAdd {
				if !controllerutil.ContainsFinalizer(currentNotebook, finalizer) {
					controllerutil.AddFinalizer(currentNotebook, finalizer)
					modified = true
				}
			}

			// Only update if we actually added finalizers
			if modified {
				return r.Update(ctx, currentNotebook)
			}
			return nil
		})

		if err != nil {
			log.Error(err, "Failed to add finalizers after retries", "finalizers", finalizersToAdd)
			return ctrl.Result{}, err
		}
		log.Info("Successfully added finalizers", "finalizers", finalizersToAdd)
		return ctrl.Result{Requeue: true}, nil
	}

	// Create Configmap with the ODH notebook certificate
	// With the ODH 2.8 Operator, user can provide their own certificate
	// from DSCI initializer, that provides the certs in a ConfigMap odh-trusted-ca-bundle
	// create a separate ConfigMap for the notebook which append the user provided certs
	// with cluster self-signed certs.
	err = r.CreateNotebookCertConfigMap(notebook, ctx)
	if err != nil {
		return ctrl.Result{}, err
	} else {
		// If createNotebookCertConfigMap returns nil,
		// and still the ConfigMap workbench-trusted-ca-bundle is not found,
		// reconcile notebook to unset the env variable.
		if r.IsConfigMapDeleted(notebook, ctx) {
			// Unset the env variable in the notebook
			err = r.UnsetNotebookCertConfig(notebook, ctx)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// Call the Network Policies reconciler
	err = r.ReconcileAllNetworkPolicies(notebook, ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Create/Watch and Update the pipeline-runtime-image ConfigMap on Notebook's Namespace
	err = r.EnsureNotebookConfigMap(notebook, ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Call the Rolebinding reconciler
	if strings.ToLower(strings.TrimSpace(os.Getenv("SET_PIPELINE_RBAC"))) == "true" {
		err = r.ReconcileRoleBindings(notebook, ctx)
		if err != nil {
			log.Error(err, "Unable to Reconcile Rolebinding")
			return ctrl.Result{}, err
		}
	}

	// Call the Elyra pipeline secret reconciler
	if strings.ToLower(strings.TrimSpace(os.Getenv("SET_PIPELINE_SECRET"))) == "true" {
		err = r.ReconcileElyraRuntimeConfigSecret(notebook, ctx)
		if err != nil {
			log.Error(err, "Unable to Reconcile Elyra runtime config secret")
			return ctrl.Result{}, err
		}
	}

	// Reconcile ReferenceGrant to allow cross-namespace HTTPRoute backend references
	// This must be done before creating HTTPRoutes
	err = r.ReconcileReferenceGrant(notebook, ctx)
	if err != nil {
		log.Error(err, "Unable to reconcile ReferenceGrant")
		return ctrl.Result{}, err
	}

	// Create the objects required by the kube-rbac-proxy sidecar if annotation is present
	if KubeRbacProxyInjectionIsEnabled(notebook.ObjectMeta) {
		// Ensure any existing regular HTTPRoute is cleaned up before creating kube-rbac-proxy objects
		err = r.EnsureConflictingHTTPRouteAbsent(notebook, ctx, true)
		if err != nil {
			return ctrl.Result{}, err
		}

		// Create the objects required by the kube-rbac-proxy sidecar
		err = r.ReconcileNotebookServiceAccount(notebook, ctx)
		if err != nil {
			return ctrl.Result{}, err
		}

		// Call the kube-rbac-proxy ClusterRoleBinding reconciler
		err = r.ReconcileKubeRbacProxyClusterRoleBinding(notebook, ctx)
		if err != nil {
			return ctrl.Result{}, err
		}

		// Call the kube-rbac-proxy ConfigMap reconciler
		err = r.ReconcileKubeRbacProxyConfigMap(notebook, ctx)
		if err != nil {
			return ctrl.Result{}, err
		}

		// Call the kube-rbac-proxy Service reconciler
		err = r.ReconcileKubeRbacProxyService(notebook, ctx)
		if err != nil {
			return ctrl.Result{}, err
		}

		// Call the kube-rbac-proxy HTTPRoute reconciler
		err = r.ReconcileKubeRbacProxyHTTPRoute(notebook, ctx)
		if err != nil {
			return ctrl.Result{}, err
		}
	} else {
		// Ensure any existing kube-rbac-proxy HTTPRoute is cleaned up before creating regular HTTPRoute
		err = r.EnsureConflictingHTTPRouteAbsent(notebook, ctx, false)
		if err != nil {
			return ctrl.Result{}, err
		}

		// Clean up any existing kube-rbac-proxy ClusterRoleBinding when switching away from auth mode
		err = r.CleanupKubeRbacProxyClusterRoleBinding(notebook, ctx)
		if err != nil {
			return ctrl.Result{}, err
		}

		// Call the regular HTTPRoute reconciler (see notebook_route.go file)
		err = r.ReconcileHTTPRoute(notebook, ctx)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	// Remove the reconciliation lock annotation
	if ReconciliationLockIsEnabled(notebook.ObjectMeta) {
		log.Info("Removing reconciliation lock")
		err = r.RemoveReconciliationLock(notebook, ctx)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// createNotebookCertConfigMap creates a ConfigMap workbench-trusted-ca-bundle
// that contains the root certificates from the ConfigMap odh-trusted-ca-bundle
// and the self-signed certificates from the ConfigMap kube-root-ca.crt
// The ConfigMap workbench-trusted-ca-bundle is used by the notebook to trust
// the root and self-signed certificates.
func (r *OpenshiftNotebookReconciler) CreateNotebookCertConfigMap(notebook *nbv1.Notebook,
	ctx context.Context) error {

	// Initialize logger format
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	rootCertPool := [][]byte{} // Root certificate pool

	configMapList := []string{OdhConfigMapName, SelfSignedConfigMapName, ServiceCAConfigMapName}
	configMapFileNames := map[string][]string{
		OdhConfigMapName:        {"ca-bundle.crt", "odh-ca-bundle.crt"},
		SelfSignedConfigMapName: {"ca.crt"},
		ServiceCAConfigMapName:  {"service-ca.crt"},
	}

	for _, configMapName := range configMapList {

		configMap := &corev1.ConfigMap{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: notebook.Namespace, Name: configMapName}, configMap); err != nil {
			// if configmap odh-trusted-ca-bundle is not found,
			// no need to create the workbench-trusted-ca-bundle
			if apierrs.IsNotFound(err) && configMapName == OdhConfigMapName {
				return nil
			}
			log.Info("Unable to fetch ConfigMap", "configMap", configMapName)
			continue
		}

		// Search for the certificate in the ConfigMap
		for _, certFile := range configMapFileNames[configMapName] {

			certData, ok := configMap.Data[certFile]
			// RHOAIENG-15743: opendatahub-operator#1339 started adding '\n' unconditionally, which
			// is breaking our `== ""` checks below. Trim the certData again.
			certData = strings.TrimSpace(certData)
			// If ca-bundle.crt is not found in the ConfigMap odh-trusted-ca-bundle
			// no need to create the workbench-trusted-ca-bundle, as it is created
			// by annotation inject-ca-bundle: "true"
			if !ok || certFile == "ca-bundle.crt" && certData == "" {
				return nil
			}
			if !ok || certData == "" {
				continue
			}

			// Attempt to decode PEM encoded certificate
			block, _ := pem.Decode([]byte(certData))
			if block != nil && block.Type == "CERTIFICATE" {
				// Attempt to parse the certificate
				_, err := x509.ParseCertificate(block.Bytes)
				if err != nil {
					log.Error(err, "Error parsing certificate", "configMap", configMap.Name, "certFile", certFile)
					continue
				}
				// Add the certificate to the pool
				rootCertPool = append(rootCertPool, []byte(certData))
			} else if len(certData) > 0 {
				log.Info("Invalid certificate format", "configMap", configMap.Name, "certFile", certFile)
			}
		}
	}

	if len(rootCertPool) > 0 {
		desiredTrustedCAConfigMap := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "workbench-trusted-ca-bundle",
				Namespace: notebook.Namespace,
				Labels:    map[string]string{"opendatahub.io/managed-by": "workbenches"},
			},
			Data: map[string]string{
				"ca-bundle.crt": string(bytes.Join(rootCertPool, []byte("\n"))),
			},
		}

		foundTrustedCAConfigMap := &corev1.ConfigMap{}
		err := r.Get(ctx, client.ObjectKey{
			Namespace: desiredTrustedCAConfigMap.Namespace,
			Name:      desiredTrustedCAConfigMap.Name,
		}, foundTrustedCAConfigMap)
		if err != nil {
			if apierrs.IsNotFound(err) {
				r.Log.Info("Creating workbench-trusted-ca-bundle configmap", "namespace", notebook.Namespace, "notebook", notebook.Name)
				err = r.Create(ctx, desiredTrustedCAConfigMap)
				if err != nil && !apierrs.IsAlreadyExists(err) {
					r.Log.Error(err, "Unable to create the workbench-trusted-ca-bundle ConfigMap")
					return err
				} else {
					r.Log.Info("Created workbench-trusted-ca-bundle ConfigMap", "namespace", notebook.Namespace, "notebook", notebook.Name)
				}
			}
		} else if !reflect.DeepEqual(foundTrustedCAConfigMap.Data, desiredTrustedCAConfigMap.Data) {
			// some data has changed, update the ConfigMap
			r.Log.Info("Updating workbench-trusted-ca-bundle ConfigMap", "namespace", notebook.Namespace, "notebook", notebook.Name)
			foundTrustedCAConfigMap.Data = desiredTrustedCAConfigMap.Data
			err = r.Update(ctx, foundTrustedCAConfigMap)
			if err != nil {
				r.Log.Error(err, "Unable to update the workbench-trusted-ca-bundle ConfigMap")
				return err
			}
		}
	}
	return nil
}

// IsConfigMapDeleted check if configmap is deleted
// and the notebook is using the configmap as a volume
func (r *OpenshiftNotebookReconciler) IsConfigMapDeleted(notebook *nbv1.Notebook, ctx context.Context) bool {

	// Initialize logger format
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	var workbenchConfigMapExists bool
	workbenchConfigMapExists = false

	foundTrustedCAConfigMap := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{
		Namespace: notebook.Namespace,
		Name:      "workbench-trusted-ca-bundle",
	}, foundTrustedCAConfigMap)
	if err == nil {
		workbenchConfigMapExists = true
	}

	if !workbenchConfigMapExists {
		for _, volume := range notebook.Spec.Template.Spec.Volumes {
			if volume.ConfigMap != nil && volume.ConfigMap.Name == "workbench-trusted-ca-bundle" {
				log.Info("workbench-trusted-ca-bundle ConfigMap is deleted and used by the notebook as a volume")
				return true
			}
		}
	}
	return false
}

// UnsetEnvVars removes the environment variables from the notebook container
func (r *OpenshiftNotebookReconciler) UnsetNotebookCertConfig(notebook *nbv1.Notebook, ctx context.Context) error {

	// Initialize logger format
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	// Get the notebook object
	envVars := []string{"PIP_CERT", "REQUESTS_CA_BUNDLE", "SSL_CERT_FILE", "PIPELINES_SSL_SA_CERTS", "GIT_SSL_CAINFO", "KF_PIPELINES_SSL_SA_CERTS"}
	notebookSpecChanged := false
	patch := client.MergeFrom(notebook.DeepCopy())
	copyNotebook := notebook.DeepCopy()

	notebookContainers := &copyNotebook.Spec.Template.Spec.Containers
	notebookVolumes := &copyNotebook.Spec.Template.Spec.Volumes
	var imgContainer corev1.Container

	// Unset the env variables in the notebook
	for _, container := range *notebookContainers {
		// Update notebook image container with env Variables
		if container.Name == notebook.Name {
			imgContainer = container
			for _, key := range envVars {
				for index, env := range imgContainer.Env {
					if key == env.Name {
						imgContainer.Env = append(imgContainer.Env[:index], imgContainer.Env[index+1:]...)
					}
				}
			}
			// Unset VolumeMounts in the notebook
			for index, volumeMount := range imgContainer.VolumeMounts {
				if volumeMount.Name == "trusted-ca" {
					imgContainer.VolumeMounts = append(imgContainer.VolumeMounts[:index], imgContainer.VolumeMounts[index+1:]...)
				}
			}
			// Update container with Env and Volume Mount Changes
			for index, container := range *notebookContainers {
				if container.Name == notebook.Name {
					(*notebookContainers)[index] = imgContainer
					notebookSpecChanged = true
					break
				}
			}
			break
		}
	}

	// Unset Volume in the notebook
	for index, volume := range *notebookVolumes {
		if volume.ConfigMap != nil && volume.ConfigMap.Name == "workbench-trusted-ca-bundle" {
			*notebookVolumes = append((*notebookVolumes)[:index], (*notebookVolumes)[index+1:]...)
			notebookSpecChanged = true
			break
		}
	}

	if notebookSpecChanged {
		// Update the notebook with the new container
		err := r.Patch(ctx, copyNotebook, patch)
		if err != nil {
			log.Error(err, "Unable to update the notebook for removing the env variables")
			return err
		}
		log.Info("Removed the env variables from the notebook", "notebook", notebook.Name, "namespace", notebook.Namespace)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *OpenshiftNotebookReconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&nbv1.Notebook{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&netv1.NetworkPolicy{}).
		Owns(&rbacv1.RoleBinding{}).

		// Watch for HTTPRoutes in the central namespace
		// When an HTTPRoute is deleted or modified, trigger reconcile for the corresponding notebook
		Watches(&gatewayv1.HTTPRoute{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
				// Only watch HTTPRoutes in the central namespace (controller's namespace)
				if o.GetNamespace() != r.Namespace {
					return []reconcile.Request{}
				}

				// Get the notebook labels from the HTTPRoute
				notebookName := o.GetLabels()["notebook-name"]
				notebookNamespace := o.GetLabels()["notebook-namespace"]

				if notebookName == "" || notebookNamespace == "" {
					return []reconcile.Request{}
				}

				// Trigger reconcile for the notebook
				return []reconcile.Request{
					{
						NamespacedName: types.NamespacedName{
							Name:      notebookName,
							Namespace: notebookNamespace,
						},
					},
				}
			}),
		).

		// Watch for ReferenceGrants in user namespaces
		// When a ReferenceGrant is deleted or modified, trigger reconcile for one notebook in that namespace
		// (one is sufficient since ReferenceGrant is shared by all notebooks in the namespace)
		Watches(&gatewayv1beta1.ReferenceGrant{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
				log := r.Log.WithValues("namespace", o.GetNamespace(), "name", o.GetName())

				// Only watch ReferenceGrants with the expected name
				if o.GetName() != ReferenceGrantName {
					return []reconcile.Request{}
				}

				// Skip ReferenceGrants in the central namespace (they shouldn't exist there)
				if o.GetNamespace() == r.Namespace {
					return []reconcile.Request{}
				}

				// List all notebooks in the namespace where the ReferenceGrant exists
				var nbList nbv1.NotebookList
				if err := r.List(ctx, &nbList, client.InNamespace(o.GetNamespace())); err != nil {
					log.Error(err, "Unable to list Notebooks when attempting to handle ReferenceGrant event")
					return []reconcile.Request{}
				}

				// Trigger reconcile for only the first notebook (sufficient to fix shared ReferenceGrant)
				if len(nbList.Items) > 0 {
					log.Info("Triggering reconcile for one notebook due to ReferenceGrant change", "notebook", nbList.Items[0].Name)
					return []reconcile.Request{
						{
							NamespacedName: types.NamespacedName{
								Name:      nbList.Items[0].Name,
								Namespace: nbList.Items[0].Namespace,
							},
						},
					}
				}

				return []reconcile.Request{}
			}),
		).

		// Watch for all the required ConfigMaps
		// odh-trusted-ca-bundle, kube-root-ca.crt, workbench-trusted-ca-bundle
		// and reconcile the workbench-trusted-ca-bundle ConfigMap,
		Watches(&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
				log := r.Log.WithValues("namespace", o.GetNamespace(), "name", o.GetName())

				// If the ConfigMap name matches on of our interested ConfigMaps
				// trigger a reconcile event for first notebook in the namespace
				if o.GetName() == OdhConfigMapName || o.GetName() == SelfSignedConfigMapName || o.GetName() == ServiceCAConfigMapName {
					// List all the notebooks in the namespace and trigger a reconcile event
					var nbList nbv1.NotebookList
					if err := r.List(ctx, &nbList, client.InNamespace(o.GetNamespace())); err != nil {
						log.Error(err, "Unable to list Notebooks when attempting to handle Global CA Bundle event.")
						return []reconcile.Request{}
					}

					// As there is only one configmap workbench-trusted-ca-bundle per namespace
					// and is used by all the notebooks in the namespace, we can trigger
					// reconcile event only for the first notebook in the list.
					for _, nb := range nbList.Items {
						return []reconcile.Request{
							{
								NamespacedName: types.NamespacedName{
									Name:      nb.Name,
									Namespace: o.GetNamespace(),
								},
							},
						}
					}
				}

				// If the ConfigMap is workbench-trusted-ca-bundle
				// trigger a reconcile event for all the notebooks in the namespace
				// containing the ConfigMap workbench-trusted-ca-bundle as a volume.
				if o.GetName() == "workbench-trusted-ca-bundle" {
					// List all the notebooks in the namespace and trigger a reconcile event
					var nbList nbv1.NotebookList
					if err := r.List(ctx, &nbList, client.InNamespace(o.GetNamespace())); err != nil {
						log.Error(err, "Unable to list Notebook's when attempting to handle Global CA Bundle event.")
						return []reconcile.Request{}
					}

					// For all the notebooks that mounted the ConfigMap workbench-trusted-ca-bundle
					// as a volume, trigger a reconcile event.
					reconcileRequests := []reconcile.Request{}
					for _, nb := range nbList.Items {
						for _, volume := range nb.Spec.Template.Spec.Volumes {
							if volume.ConfigMap != nil && volume.ConfigMap.Name == o.GetName() {
								namespacedName := types.NamespacedName{
									Name:      nb.Name,
									Namespace: o.GetNamespace(),
								}
								reconcileRequests = append(reconcileRequests, reconcile.Request{NamespacedName: namespacedName})
							}
						}
					}
					return reconcileRequests
				}

				return []reconcile.Request{}
			}),
		)
	err := builder.Complete(r)
	if err != nil {
		return err
	}
	return nil
}
