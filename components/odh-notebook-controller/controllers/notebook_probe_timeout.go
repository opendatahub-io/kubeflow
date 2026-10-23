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
	"strconv"
	"time"

	"github.com/go-logr/logr"
	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	notebookControllerSettingsConfigMap = "notebook-controller-setting-config"
	workbenchProbeTimeoutKey            = "WORKBENCH_PROBE_TIMEOUT_SECONDS"
)

func readWorkbenchProbeTimeout(ctx context.Context, reader client.Reader, log logr.Logger, namespace string) (int32, bool, error) {
	configMap := &corev1.ConfigMap{}
	key := types.NamespacedName{Name: notebookControllerSettingsConfigMap, Namespace: namespace}
	err := retry.OnError(wait.Backoff{Steps: 3, Duration: 100 * time.Millisecond, Factor: 2}, func(err error) bool {
		if apierrs.IsNotFound(err) || ctx.Err() != nil {
			return false
		}
		log.Info("Retrying workbench probe timeout ConfigMap read", "configMap", key.String(), "error", err)
		return true
	}, func() error {
		return reader.Get(ctx, key, configMap)
	})
	if err != nil {
		if apierrs.IsNotFound(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("read ConfigMap %s for %s: %w", key, workbenchProbeTimeoutKey, err)
	}

	raw, configured := configMap.Data[workbenchProbeTimeoutKey]
	if !configured {
		return 0, false, nil
	}

	parsed, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || parsed <= 0 {
		return 0, false, fmt.Errorf("invalid %s in ConfigMap %s: %q must be a positive int32", workbenchProbeTimeoutKey, key, raw)
	}

	return int32(parsed), true, nil
}

func applyProbeTimeout(notebook *nbv1.Notebook, containerName string, timeout int32, log logr.Logger) {
	for i := range notebook.Spec.Template.Spec.Containers {
		container := &notebook.Spec.Template.Spec.Containers[i]
		if container.Name != containerName {
			continue
		}
		if container.LivenessProbe != nil {
			container.LivenessProbe.TimeoutSeconds = timeout
		}
		if container.ReadinessProbe != nil {
			container.ReadinessProbe.TimeoutSeconds = timeout
		}
		return
	}

	log.Info("Skipping probe timeout: target container not found", "container", containerName, "notebook", notebook.Name, "namespace", notebook.Namespace)
}
