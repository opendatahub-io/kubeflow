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
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"reflect"

	"k8s.io/apimachinery/pkg/util/intstr"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	OAuthServicePort      = 443
	OAuthServicePortName  = "oauth-proxy"
	OAuthProxyImage       = "registry.redhat.io/openshift4/ose-oauth-proxy:latest"
	ODHDefaultNameSpace   = "opendatahub"
	RHODSDefaultNameSpace = "rhods-notebooks"
)

type OAuthConfig struct {
	ProxyImage string
}

// NewNotebookServiceAccount defines the desired service account object
func NewNotebookServiceAccount(notebook *nbv1.Notebook) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      notebook.Name,
			Namespace: notebook.Namespace,
			Labels: map[string]string{
				"notebook-name": notebook.Name,
			},
			Annotations: map[string]string{
				"serviceaccounts.openshift.io/oauth-redirectreference.first": "" +
					`{"kind":"OAuthRedirectReference","apiVersion":"v1",` +
					`"reference":{"kind":"Route","name":"` + notebook.Name + `"}}`,
			},
		},
	}
}

// CompareNotebookServiceAccounts checks if two service accounts are equal, if
// not return false
func CompareNotebookServiceAccounts(sa1 corev1.ServiceAccount, sa2 corev1.ServiceAccount) bool {
	// Two service accounts will be equal if the labels and annotations are
	// identical
	return reflect.DeepEqual(sa1.ObjectMeta.Labels, sa2.ObjectMeta.Labels) &&
		reflect.DeepEqual(sa1.ObjectMeta.Annotations, sa2.ObjectMeta.Annotations)
}

// ReconcileOAuthServiceAccount will manage the service account reconciliation
// required by the notebook OAuth proxy
func (r *OpenshiftNotebookReconciler) ReconcileOAuthServiceAccount(notebook *nbv1.Notebook, ctx context.Context) error {
	// Initialize logger format
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	// Generate the desired service account
	desiredServiceAccount := NewNotebookServiceAccount(notebook)

	// Create the service account if it does not already exist
	foundServiceAccount := &corev1.ServiceAccount{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      desiredServiceAccount.Name,
		Namespace: notebook.Namespace,
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

		// Add view roles to the Service Account if the workbench is DataScience project based
		if notebook.Namespace != ODHDefaultNameSpace && notebook.Namespace != RHODSDefaultNameSpace {

			nbrole := &rbacv1.Role{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-view", notebook.Name),
					Namespace: notebook.Namespace,
				},
				Rules: []rbacv1.PolicyRule{
					{
						Verbs:     []string{"get", "watch", "list"},
						APIGroups: []string{""},
						Resources: []string{"configmaps", "pods"},
					},
				},
			}

			// Add .metatada.ownerReferences to the Role to be deleted by
			// the Kubernetes garbage collector if the notebook is deleted
			err = ctrl.SetControllerReference(notebook, nbrole, r.Scheme)
			if err != nil {
				log.Error(err, "Unable to add OwnerReference to the Role")
				return err
			}

			// Create the Role
			log.Info("Creating Role")
			err = r.Create(ctx, nbrole)
			if err != nil && !apierrs.IsAlreadyExists(err) {
				log.Error(err, "Unable to create the Role")
				return err
			}

			nbrolebinding := &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-binding", nbrole.Name),
					Namespace: notebook.Namespace,
				},
				Subjects: []rbacv1.Subject{
					{
						Kind: "ServiceAccount",
						Name: desiredServiceAccount.Name,
					},
				},
				RoleRef: rbacv1.RoleRef{
					Kind: "Role",
					Name: nbrole.Name,
				},
			}

			// Add .metatada.ownerReferences to the Role Binding to be deleted by
			// the Kubernetes garbage collector if the notebook is deleted
			err = ctrl.SetControllerReference(notebook, nbrolebinding, r.Scheme)
			if err != nil {
				log.Error(err, "Unable to add OwnerReference to the RoleBinding")
				return err
			}

			// Create the RoleBinding
			log.Info("Creating RoleBinding")
			err = r.Create(ctx, nbrolebinding)
			if err != nil && !apierrs.IsAlreadyExists(err) {
				log.Error(err, "Unable to create the RoleBinding")
				return err
			}
		}

	}

	return nil
}

// NewNotebookOAuthService defines the desired OAuth service object
func NewNotebookOAuthService(notebook *nbv1.Notebook) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      notebook.Name + "-tls",
			Namespace: notebook.Namespace,
			Labels: map[string]string{
				"notebook-name": notebook.Name,
			},
			Annotations: map[string]string{
				"service.beta.openshift.io/serving-cert-secret-name": notebook.Name + "-tls",
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				Name:       OAuthServicePortName,
				Port:       OAuthServicePort,
				TargetPort: intstr.FromString(OAuthServicePortName),
				Protocol:   corev1.ProtocolTCP,
			}},
			Selector: map[string]string{
				"statefulset": notebook.Name,
			},
		},
	}
}

// CompareNotebookServices checks if two services are equal, if not return false
func CompareNotebookServices(s1 corev1.Service, s2 corev1.Service) bool {
	// Two services will be equal if the labels and annotations are identical
	return reflect.DeepEqual(s1.ObjectMeta.Labels, s2.ObjectMeta.Labels) &&
		reflect.DeepEqual(s1.ObjectMeta.Annotations, s2.ObjectMeta.Annotations)
}

// ReconcileOAuthService will manage the OAuth service reconciliation required
// by the notebook OAuth proxy
func (r *OpenshiftNotebookReconciler) ReconcileOAuthService(notebook *nbv1.Notebook, ctx context.Context) error {
	// Initialize logger format
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	// Generate the desired OAuth service
	desiredService := NewNotebookOAuthService(notebook)

	// Create the OAuth service if it does not already exist
	foundService := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      desiredService.GetName(),
		Namespace: notebook.GetNamespace(),
	}, foundService)
	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("Creating OAuth Service")
			// Add .metatada.ownerReferences to the OAuth service to be deleted by
			// the Kubernetes garbage collector if the notebook is deleted
			err = ctrl.SetControllerReference(notebook, desiredService, r.Scheme)
			if err != nil {
				log.Error(err, "Unable to add OwnerReference to the OAuth Service")
				return err
			}
			// Create the OAuth service in the Openshift cluster
			err = r.Create(ctx, desiredService)
			if err != nil && !apierrs.IsAlreadyExists(err) {
				log.Error(err, "Unable to create the OAuth Service")
				return err
			}
		} else {
			log.Error(err, "Unable to fetch the OAuth Service")
			return err
		}
	}

	return nil
}

// NewNotebookOAuthSecret defines the desired OAuth secret object
func NewNotebookOAuthSecret(notebook *nbv1.Notebook) *corev1.Secret {
	// Generate the cookie secret for the OAuth proxy
	cookieSeed := make([]byte, 16)
	rand.Read(cookieSeed)
	cookieSecret := base64.StdEncoding.EncodeToString(
		[]byte(base64.StdEncoding.EncodeToString(cookieSeed)))

	// Create a Kubernetes secret to store the cookie secret
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      notebook.Name + "-oauth-config",
			Namespace: notebook.Namespace,
			Labels: map[string]string{
				"notebook-name": notebook.Name,
			},
		},
		StringData: map[string]string{
			"cookie_secret": cookieSecret,
		},
	}
}

// ReconcileOAuthSecret will manage the OAuth secret reconciliation required by
// the notebook OAuth proxy
func (r *OpenshiftNotebookReconciler) ReconcileOAuthSecret(notebook *nbv1.Notebook, ctx context.Context) error {
	// Initialize logger format
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	// Generate the desired OAuth secret
	desiredSecret := NewNotebookOAuthSecret(notebook)

	// Create the OAuth secret if it does not already exist
	foundSecret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      desiredSecret.Name,
		Namespace: notebook.Namespace,
	}, foundSecret)
	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("Creating OAuth Secret")
			// Add .metatada.ownerReferences to the OAuth secret to be deleted by
			// the Kubernetes garbage collector if the notebook is deleted
			err = ctrl.SetControllerReference(notebook, desiredSecret, r.Scheme)
			if err != nil {
				log.Error(err, "Unable to add OwnerReference to the OAuth Secret")
				return err
			}
			// Create the OAuth secret in the Openshift cluster
			err = r.Create(ctx, desiredSecret)
			if err != nil && !apierrs.IsAlreadyExists(err) {
				log.Error(err, "Unable to create the OAuth Secret")
				return err
			}
		} else {
			log.Error(err, "Unable to fetch the OAuth Secret")
			return err
		}
	}

	return nil
}

// NewNotebookOAuthRoute defines the desired OAuth route object
func NewNotebookOAuthRoute(notebook *nbv1.Notebook) *routev1.Route {
	route := NewNotebookRoute(notebook)
	route.Spec.To.Name = notebook.Name + "-tls"
	route.Spec.Port.TargetPort = intstr.FromString(OAuthServicePortName)
	route.Spec.TLS.Termination = routev1.TLSTerminationReencrypt
	return route
}

// ReconcileOAuthRoute will manage the creation, update and deletion of the OAuth route
// when the notebook is reconciled.
func (r *OpenshiftNotebookReconciler) ReconcileOAuthRoute(
	notebook *nbv1.Notebook, ctx context.Context) error {
	return r.reconcileRoute(notebook, ctx, NewNotebookOAuthRoute)
}
