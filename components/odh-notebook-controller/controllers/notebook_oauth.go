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

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	oauthv1 "github.com/openshift/api/oauth/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// Finalizer to handle OAuthClient cleanup since it's cluster-scoped and can't use owner references
	NotebookOAuthClientFinalizer = "notebook-oauth-client-finalizer.opendatahub.io"
)

// removeOAuthClientFinalizer removes the OAuth client finalizer from the notebook
func (r *OpenshiftNotebookReconciler) removeOAuthClientFinalizer(notebook *nbv1.Notebook, ctx context.Context) error {
	log := logf.FromContext(ctx)

	// Check if finalizer exists before attempting removal
	if !controllerutil.ContainsFinalizer(notebook, NotebookOAuthClientFinalizer) {
		log.Info("OAuth client finalizer not present, nothing to remove")
		return nil
	}

	// Remove the finalizer using the standard controller-runtime helper
	controllerutil.RemoveFinalizer(notebook, NotebookOAuthClientFinalizer)
	err := r.Update(ctx, notebook)
	if err != nil {
		log.Error(err, "Unable to remove OAuth client finalizer from notebook")
		return err
	}

	log.Info("Removed OAuth client finalizer from notebook")
	return nil
}

// hasOAuthClientFinalizer checks if the notebook has the OAuth client finalizer
func (r *OpenshiftNotebookReconciler) hasOAuthClientFinalizer(notebook *nbv1.Notebook) bool {
	for _, finalizer := range notebook.Finalizers {
		if finalizer == NotebookOAuthClientFinalizer {
			return true
		}
	}
	return false
}

// deleteOAuthClient deletes the OAuthClient associated with the notebook
func (r *OpenshiftNotebookReconciler) deleteOAuthClient(notebook *nbv1.Notebook, ctx context.Context) error {
	log := logf.FromContext(ctx)

	oauthClientName := notebook.Name + "-" + notebook.Namespace + "-oauth-client"
	oauthClient := &oauthv1.OAuthClient{}

	err := r.Get(ctx, types.NamespacedName{Name: oauthClientName}, oauthClient)
	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("OAuthClient not found, nothing to delete", "oauthClient", oauthClientName)
			return nil
		}
		log.Error(err, "Unable to fetch OAuthClient for deletion", "oauthClient", oauthClientName)
		return err
	}

	err = r.Delete(ctx, oauthClient)
	if err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("OAuthClient already deleted", "oauthClient", oauthClientName)
			return nil
		}
		log.Error(err, "Unable to delete OAuthClient", "oauthClient", oauthClientName)
		return err
	} else {
		log.Info("Successfully deleted OAuthClient", "oauthClient", oauthClientName)
	}

	return nil
}
