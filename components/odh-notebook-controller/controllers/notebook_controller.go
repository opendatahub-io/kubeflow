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
	"errors"
	"reflect"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	"github.com/kubeflow/kubeflow/components/notebook-controller/pkg/culler"
	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	AnnotationInjectOAuth             = "notebooks.opendatahub.io/inject-oauth"
	AnnotationValueReconciliationLock = "odh-notebook-controller-lock"
	AnnotationLogoutUrl               = "notebooks.opendatahub.io/oauth-logout-url"
)

// OpenshiftNotebookReconciler holds the controller configuration.
type OpenshiftNotebookReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger
}

// ClusterRole permissions

// +kubebuilder:rbac:groups=kubeflow.org,resources=notebooks,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=kubeflow.org,resources=notebooks/status,verbs=get
// +kubebuilder:rbac:groups=kubeflow.org,resources=notebooks/finalizers,verbs=update
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=services;serviceaccounts;secrets;configmaps,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=config.openshift.io,resources=proxies,verbs=get;list;watch

// CompareNotebooks checks if two notebooks are equal, if not return false.
func CompareNotebooks(nb1 nbv1.Notebook, nb2 nbv1.Notebook) bool {
	return reflect.DeepEqual(nb1.ObjectMeta.Labels, nb2.ObjectMeta.Labels) &&
		reflect.DeepEqual(nb1.ObjectMeta.Annotations, nb2.ObjectMeta.Annotations) &&
		reflect.DeepEqual(nb1.Spec, nb2.Spec)
}

// OAuthInjectionIsEnabled returns true if the oauth sidecar injection
// annotation is present in the notebook.
func OAuthInjectionIsEnabled(meta metav1.ObjectMeta) bool {
	if meta.Annotations[AnnotationInjectOAuth] != "" {
		result, _ := strconv.ParseBool(meta.Annotations[AnnotationInjectOAuth])
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
				return errors.New("Pull secret not mounted")
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

	// Create Configmap
	err = r.createProxyConfigMap(notebook, ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Create the objects required by the OAuth proxy sidecar (see
	// notebook_oauth.go file)
	if OAuthInjectionIsEnabled(notebook.ObjectMeta) {
		// Call the OAuth Service Account reconciler
		err = r.ReconcileOAuthServiceAccount(notebook, ctx)
		if err != nil {
			return ctrl.Result{}, err
		}

		// Call the OAuth Service reconciler
		err = r.ReconcileOAuthService(notebook, ctx)
		if err != nil {
			return ctrl.Result{}, err
		}

		// Call the OAuth Secret reconciler
		err = r.ReconcileOAuthSecret(notebook, ctx)
		if err != nil {
			return ctrl.Result{}, err
		}

		// Call the OAuth Route reconciler
		err = r.ReconcileOAuthRoute(notebook, ctx)
		if err != nil {
			return ctrl.Result{}, err
		}
	} else {
		// Call the route reconciler (see notebook_route.go file)
		err = r.ReconcileRoute(notebook, ctx)
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

func (r *OpenshiftNotebookReconciler) createProxyConfigMap(notebook *nbv1.Notebook,
	ctx context.Context) error {

	trustedCAConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "trusted-ca",
			Namespace: notebook.Namespace,
			Labels:    map[string]string{"config.openshift.io/inject-trusted-cabundle": "true"},
		},
	}

	err := r.Client.Create(ctx, trustedCAConfigMap)
	if err != nil {
		if apierrs.IsAlreadyExists(err) {
			return nil
		}
	}
	return err
}

// SetupWithManager sets up the controller with the Manager.
func (r *OpenshiftNotebookReconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&nbv1.Notebook{}).
		Owns(&routev1.Route{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{})

	err := builder.Complete(r)
	if err != nil {
		return err
	}

	return nil
}
