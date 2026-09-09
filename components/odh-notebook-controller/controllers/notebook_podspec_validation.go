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
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

func validatePodSpecSecurity(podSpec *corev1.PodSpec, notebookName string) error {
	if podSpec == nil {
		return nil
	}

	if podSpec.HostNetwork {
		return fmt.Errorf("hostNetwork is not allowed in notebook pod spec")
	}
	if podSpec.HostPID {
		return fmt.Errorf("hostPID is not allowed in notebook pod spec")
	}
	if podSpec.HostIPC {
		return fmt.Errorf("hostIPC is not allowed in notebook pod spec")
	}

	if podSpec.ServiceAccountName != "" && podSpec.ServiceAccountName != notebookName {
		return fmt.Errorf("serviceAccountName %q is not allowed; notebooks use a dedicated service account named after the notebook", podSpec.ServiceAccountName)
	}

	for _, volume := range podSpec.Volumes {
		if volume.HostPath != nil {
			return fmt.Errorf("hostPath volume %q is not allowed in notebook pod spec", volume.Name)
		}
	}

	for _, container := range podSpec.Containers {
		if err := validateContainerSecurityContext(container.Name, container.SecurityContext); err != nil {
			return err
		}
	}
	for _, container := range podSpec.InitContainers {
		if err := validateContainerSecurityContext(container.Name, container.SecurityContext); err != nil {
			return err
		}
	}
	for _, container := range podSpec.EphemeralContainers {
		if err := validateContainerSecurityContext(container.Name, container.SecurityContext); err != nil {
			return err
		}
	}

	return nil
}

func validateContainerSecurityContext(containerName string, securityContext *corev1.SecurityContext) error {
	if securityContext != nil && securityContext.Privileged != nil && *securityContext.Privileged {
		return fmt.Errorf("privileged container %q is not allowed in notebook pod spec", containerName)
	}
	return nil
}
