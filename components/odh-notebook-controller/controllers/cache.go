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
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ManagerCacheOptions keeps metadata for watches and reads ConfigMap and Secret data directly.
func ManagerCacheOptions() (cache.Options, client.Options) {
	return cache.Options{
			DefaultTransform: cache.TransformStripManagedFields(),
			ByObject: map[client.Object]cache.ByObject{
				&corev1.ConfigMap{}: {Transform: stripConfigMapData},
				&corev1.Secret{}:    {Transform: stripSecretData},
			},
		}, client.Options{
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{&corev1.ConfigMap{}, &corev1.Secret{}},
			},
		}
}

// stripConfigMapData removes data payloads and managed fields from cached ConfigMaps.
// ConfigMap data is read via direct API calls (DisableFor) when needed.
// Note: SetManagedFields is called here because DefaultTransform does not apply
// to types that have a per-type Transform override in ByObject.
func stripConfigMapData(i interface{}) (interface{}, error) {
	if cm, ok := i.(*corev1.ConfigMap); ok {
		cm.Data = nil
		cm.BinaryData = nil
		if cm.Annotations != nil {
			delete(cm.Annotations, "kubectl.kubernetes.io/last-applied-configuration")
		}
		cm.SetManagedFields(nil)
	}
	return i, nil
}

// stripSecretData removes data payloads and managed fields from cached Secrets.
// Secret data is read via direct API calls (DisableFor) when needed.
// Note: SetManagedFields is called here because DefaultTransform does not apply
// to types that have a per-type Transform override in ByObject.
func stripSecretData(i interface{}) (interface{}, error) {
	if s, ok := i.(*corev1.Secret); ok {
		s.Data = nil
		s.StringData = nil
		if s.Annotations != nil {
			delete(s.Annotations, "kubectl.kubernetes.io/last-applied-configuration")
		}
		s.SetManagedFields(nil)
	}
	return i, nil
}
