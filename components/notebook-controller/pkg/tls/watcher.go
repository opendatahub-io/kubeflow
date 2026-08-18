/*
Copyright The Kubeflow Authors.

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

package tls

import (
	"context"
	"reflect"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var watcherLog = ctrl.Log.WithName("tls-profile-watcher")

const profileRetryInterval = 5 * time.Second

type ProfileWatcher struct {
	restConfig      *rest.Config
	lastProfile     interface{}
	onProfileChange func()
}

func NewProfileWatcher(cfg *rest.Config, initialProfile interface{}, onProfileChange func()) *ProfileWatcher {
	return &ProfileWatcher{
		restConfig:      cfg,
		lastProfile:     initialProfile,
		onProfileChange: onProfileChange,
	}
}

func (w *ProfileWatcher) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	dynClient, err := dynamic.NewForConfig(w.restConfig)
	if err != nil {
		watcherLog.Error(err, "Failed to create dynamic client for TLS profile watch")
		return reconcile.Result{RequeueAfter: profileRetryInterval}, nil
	}

	fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	obj, err := dynClient.Resource(apiServerGVR).Get(fetchCtx, "cluster", metav1.GetOptions{})
	if err != nil {
		watcherLog.Info("TLS profile fetch did not succeed, retrying", "retryAfter", profileRetryInterval, "error", err)
		return reconcile.Result{RequeueAfter: profileRetryInterval}, nil
	}

	rawSpec, _, _ := unstructured.NestedMap(obj.Object, "spec", "tlsSecurityProfile")
	var currentProfile interface{} = rawSpec

	if !reflect.DeepEqual(w.lastProfile, currentProfile) {
		watcherLog.Info("TLS security profile changed, triggering restart")
		w.lastProfile = currentProfile
		if w.onProfileChange != nil {
			w.onProfileChange()
		}
	}

	return reconcile.Result{}, nil
}

func (w *ProfileWatcher) NeedLeaderElection() bool {
	return false
}

func (w *ProfileWatcher) SetupWithManager(mgr ctrl.Manager) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "config.openshift.io", Version: "v1", Kind: "APIServer",
	})

	return ctrl.NewControllerManagedBy(mgr).
		Named("tls-profile-watcher").
		WithOptions(controller.Options{NeedLeaderElection: boolPtr(false)}).
		WatchesRawSource(source.Kind(mgr.GetCache(), obj,
			&handler.TypedEnqueueRequestForObject[*unstructured.Unstructured]{},
			predicate.TypedFuncs[*unstructured.Unstructured]{
				CreateFunc: func(e event.TypedCreateEvent[*unstructured.Unstructured]) bool {
					return e.Object.GetName() == "cluster"
				},
				UpdateFunc: func(e event.TypedUpdateEvent[*unstructured.Unstructured]) bool {
					return e.ObjectNew.GetName() == "cluster"
				},
				DeleteFunc: func(_ event.TypedDeleteEvent[*unstructured.Unstructured]) bool {
					return false
				},
				GenericFunc: func(_ event.TypedGenericEvent[*unstructured.Unstructured]) bool {
					return false
				},
			},
		)).
		Complete(w)
}

func boolPtr(b bool) *bool { return &b }
