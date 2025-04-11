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
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"

	"k8s.io/apimachinery/pkg/util/intstr"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	oauthv1 "github.com/openshift/api/oauth/v1"
	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	OAuthServicePort     = 443
	OAuthServicePortName = "oauth-proxy"
	// OAuthProxyImage uses sha256 manifest list digest value of v4.14 image for AMD64 as default to be compatible with imagePullPolicy: IfNotPresent, overridable
	// taken from https://catalog.redhat.com/software/containers/openshift4/ose-oauth-proxy/5cdb2133bed8bd5717d5ae64?image=66cefc14401df6ff4664ec43&architecture=amd64&container-tabs=overview
	// and kept in sync with the manifests here and in ClusterServiceVersion metadata of opendatahub operator
	OAuthProxyImage = "registry.redhat.io/openshift4/ose-oauth-proxy@sha256:4f8d66597feeb32bb18699326029f9a71a5aca4a57679d636b876377c2e95695"

	// Strings used in secret generation
	letterRunes = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

	// Complexity of generated secrets, this will be stored in a const for now, but in the future
	// there is the possibility of creating a specific file to manage secrets, change the size of
	// the complexity if required, etc. For now, a complexity of 16 will suffice.
	SECRET_DEFAULT_COMPLEXITY = 16
)

type OAuthConfig struct {
	ProxyImage string
}

type OAuthClientConfig struct {
	Name string
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

// The NewNotebookOAuthClientSecret will generate a random secret that will be used
// by the OAuth proxy sidecar container to automatically accept the required permissions
// for the user to access a notebook instead of showing up a page where the user needs to
// aggree with content he might not fully understand
// More info: https://issues.redhat.com/browse/RHOAIENG-11155
func NewNotebookOAuthClientSecret(notebook *nbv1.Notebook) *corev1.Secret {
	// Generate the client secret for the OAuth proxy
	randomValue := make([]byte, SECRET_DEFAULT_COMPLEXITY)
	for i := 0; i < SECRET_DEFAULT_COMPLEXITY; i++ {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(letterRunes))))
		if err != nil {
			fmt.Printf("Error generating secret: %v\n", err)
		}
		randomValue[i] = letterRunes[num.Int64()]
	}

	// Create a Kubernetes secret to store the cookie secret
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      notebook.Name + "-oauth-client",
			Namespace: notebook.Namespace,
			Labels: map[string]string{
				"notebook-name": notebook.Name,
			},
		},
		StringData: map[string]string{
			"secret": string(randomValue),
		},
	}
}

// ReconcileOAuthSecret will manage the OAuth secret reconciliation required by
// the notebook OAuth proxy
func (r *OpenshiftNotebookReconciler) ReconcileOAuthSecret(notebook *nbv1.Notebook, ctx context.Context) error {
	// Generate the desired OAuth secret
	desiredOAuthSecret := NewNotebookOAuthSecret(notebook)
	_ = r.createSecret(notebook, ctx, desiredOAuthSecret)

	// Generate the desired OAuthClientSecret
	desiredOAuthClientSecret := NewNotebookOAuthClientSecret(notebook)
	_ = r.createSecret(notebook, ctx, desiredOAuthClientSecret)

	return nil
}

func (r *OpenshiftNotebookReconciler) ReconcileOAuthClient(notebook *nbv1.Notebook, ctx context.Context) error {
	log := logf.FromContext(ctx)

	route := &routev1.Route{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      notebook.Name,
		Namespace: notebook.Namespace,
	}, route)
	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("Route not found, cannot create OAuthClient yet", "route", notebook.Name)
			return nil
		}
		log.Error(err, "Failed to get Route for OAuthClient")
		return err
	}

	err = r.createOAuthClient(notebook, ctx)
	if err != nil {
		log.Error(err, "Unable to handle OAuthClient creation / update")
		return err
	}

	return nil
}

func (r *OpenshiftNotebookReconciler) createSecret(notebook *nbv1.Notebook, ctx context.Context, desiredSecret *corev1.Secret) error {
	log := logf.FromContext(ctx)

	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      desiredSecret.Name,
		Namespace: notebook.Namespace,
	}, secret)

	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("Creating ", "secret", desiredSecret.Name)

			// Add .metatada.ownerReferences to the OAuth client secret to be deleted by
			// the Kubernetes garbage collector if the notebook is deleted
			err = ctrl.SetControllerReference(notebook, desiredSecret, r.Scheme)
			if err != nil {
				log.Error(err, "Unable to add OwnerReference to secret ", desiredSecret.Name)
				return err
			}

			// Create the OAuth client secret in the Openshift cluster
			err = r.Create(ctx, desiredSecret)
			if err != nil && !apierrs.IsAlreadyExists(err) {
				log.Error(err, "Unable to create secret ", desiredSecret.Name)
				return err
			}
		} else {
			log.Error(err, "Unable to fetch secret")
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

func (r *OpenshiftNotebookReconciler) createOAuthClient(notebook *nbv1.Notebook, ctx context.Context) error {
	log := logf.FromContext(ctx)

	// Get the route that will be used in the OAuthClient
	oauthClientRoute := &routev1.Route{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      notebook.Name,
		Namespace: notebook.Namespace,
	}, oauthClientRoute)
	if err != nil {
		log.Error(err, "Unable to retrieve route", "route", notebook.Name)
		return err
	}

	// Get the secret that will be used in the OAuthClient
	secret := &corev1.Secret{}
	err = r.Get(ctx, types.NamespacedName{
		Name:      notebook.Name + "-oauth-client",
		Namespace: notebook.Namespace,
	}, secret)
	if err != nil {
		log.Error(err, "Unable to retrieve secret", "secret", notebook.Name)
		return err
	}

	stringData := string(secret.Data["secret"])

	oauthClient := &oauthv1.OAuthClient{
		TypeMeta: metav1.TypeMeta{
			Kind:       "OAuthClient",
			APIVersion: "oauth.openshift.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: notebook.Name + "-" + notebook.Namespace + "-oauth-client",
			Labels: map[string]string{
				"notebook-owner": notebook.Name,
			},
		},
		Secret:       stringData,
		RedirectURIs: []string{"https://" + oauthClientRoute.Spec.Host},
		GrantMethod:  oauthv1.GrantHandlerAuto,
	}

	err = r.Create(ctx, oauthClient)
	if err != nil {
		if apierrs.IsAlreadyExists(err) {
			data, err := json.Marshal(oauthClient)
			if err != nil {
				return fmt.Errorf("failed to create OAuth Client: %w", err)
			}
			if err = r.Patch(ctx, oauthClient, client.RawPatch(types.ApplyPatchType, data),
				client.ForceOwnership, client.FieldOwner("odh-notebook-controller")); err != nil {
				return fmt.Errorf("failed to patch existing OAuthClient CR: %w", err)
			}
			return nil
		}
	}

	return nil
}
