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
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type probeTimeoutReader struct {
	client.Reader
	get func(context.Context, client.ObjectKey, client.Object) error
}

func (r probeTimeoutReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	return r.get(ctx, key, obj)
}

func TestReadWorkbenchProbeTimeoutRetries(t *testing.T) {
	transientErr := errors.New("temporary API failure")
	for _, tt := range []struct {
		name           string
		failures       int
		readErr        error
		cancel         bool
		wantCalls      int
		wantConfigured bool
		wantErr        error
	}{
		{name: "transient recovery on third read", failures: 2, readErr: transientErr, wantCalls: 3, wantConfigured: true},
		{name: "persistent failure stops after three reads", failures: 4, readErr: transientErr, wantCalls: 3, wantErr: transientErr},
		{name: "not found is not retried", failures: 4, readErr: apierrs.NewNotFound(schema.GroupResource{Resource: "configmaps"}, notebookControllerSettingsConfigMap), wantCalls: 1},
		{name: "canceled context is not retried", failures: 4, readErr: context.Canceled, cancel: true, wantCalls: 1, wantErr: context.Canceled},
	} {
		t.Run(tt.name, func(t *testing.T) {
			readCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancel {
				cancel()
			}
			calls := 0
			reader := probeTimeoutReader{get: func(_ context.Context, key client.ObjectKey, obj client.Object) error {
				calls++
				if key.Name != notebookControllerSettingsConfigMap || key.Namespace != Namespace {
					t.Fatalf("unexpected ConfigMap key: %v", key)
				}
				if calls <= tt.failures {
					return tt.readErr
				}
				obj.(*corev1.ConfigMap).Data = map[string]string{workbenchProbeTimeoutKey: "5"}
				return nil
			}}
			timeout, configured, err := readWorkbenchProbeTimeout(readCtx, reader, logr.Discard(), Namespace)
			if calls != tt.wantCalls {
				t.Errorf("got %d reads, want %d", calls, tt.wantCalls)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want %v", err, tt.wantErr)
			}
			if configured != tt.wantConfigured {
				t.Errorf("configured = %v, want %v", configured, tt.wantConfigured)
			}
			var wantTimeout int32
			if tt.wantConfigured {
				wantTimeout = 5
			}
			if timeout != wantTimeout {
				t.Errorf("timeout = %d, want %d", timeout, wantTimeout)
			}
		})
	}
}
