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
	"encoding/json"
	"fmt"
	"net/http"

	configv1 "github.com/openshift/api/config/v1"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	"github.com/kubeflow/kubeflow/components/notebook-controller/pkg/culler"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

//+kubebuilder:webhook:path=/mutate-notebook-v1,mutating=true,failurePolicy=fail,sideEffects=None,groups=kubeflow.org,resources=notebooks,verbs=create;update,versions=v1,name=notebooks.opendatahub.io,admissionReviewVersions=v1

// NotebookWebhook holds the webhook configuration.
type NotebookWebhook struct {
	Client      client.Client
	decoder     *admission.Decoder
	OAuthConfig OAuthConfig
}

var proxyEnvVars = make(map[string]string, 4)

// InjectReconciliationLock injects the kubefllow notebook controller culling
// stop annotation to explicitly start the notebook pod when the ODH notebook
// controller finishes the reconciliation. Otherwise a race condition may happen
// while mounting the notebook service account pull secret into the pod.
//
// The ODH notebook controller will remove this annotation when the first
// reconcilitation is completed (see RemoveReconciliationLock).
func InjectReconciliationLock(meta *metav1.ObjectMeta) error {
	if meta.Annotations != nil {
		meta.Annotations[culler.STOP_ANNOTATION] = AnnotationValueReconciliationLock
	} else {
		meta.SetAnnotations(map[string]string{
			culler.STOP_ANNOTATION: AnnotationValueReconciliationLock,
		})
	}
	return nil
}

// InjectOAuthProxy injects the OAuth proxy sidecar container in the Notebook
// spec
func InjectOAuthProxy(notebook *nbv1.Notebook, oauth OAuthConfig) error {
	// https://pkg.go.dev/k8s.io/api/core/v1#Container
	proxyContainer := corev1.Container{
		Name:            "oauth-proxy",
		Image:           oauth.ProxyImage,
		ImagePullPolicy: corev1.PullAlways,
		Env: []corev1.EnvVar{{
			Name: "NAMESPACE",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.namespace",
				},
			},
		}},
		Args: []string{
			"--provider=openshift",
			"--https-address=:8443",
			"--http-address=",
			"--openshift-service-account=" + notebook.Name,
			"--cookie-secret-file=/etc/oauth/config/cookie_secret",
			"--cookie-expire=24h0m0s",
			"--tls-cert=/etc/tls/private/tls.crt",
			"--tls-key=/etc/tls/private/tls.key",
			"--upstream=http://localhost:8888",
			"--upstream-ca=/var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
			// "--skip-provider-button",
			"--email-domain=*",
			`--openshift-sar={"verb":"get","resource":"notebooks","resourceAPIGroup":"kubeflow.org",` +
				`"resourceName":"` + notebook.Name + `","namespace":"$(NAMESPACE)"}`,
		},
		Ports: []corev1.ContainerPort{{
			Name:          OAuthServicePortName,
			ContainerPort: 8443,
			Protocol:      corev1.ProtocolTCP,
		}},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   "/oauth/healthz",
					Port:   intstr.FromString(OAuthServicePortName),
					Scheme: corev1.URISchemeHTTPS,
				},
			},
			InitialDelaySeconds: 30,
			TimeoutSeconds:      1,
			PeriodSeconds:       5,
			SuccessThreshold:    1,
			FailureThreshold:    3,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   "/oauth/healthz",
					Port:   intstr.FromString(OAuthServicePortName),
					Scheme: corev1.URISchemeHTTPS,
				},
			},
			InitialDelaySeconds: 5,
			TimeoutSeconds:      1,
			PeriodSeconds:       5,
			SuccessThreshold:    1,
			FailureThreshold:    3,
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				"cpu":    resource.MustParse("100m"),
				"memory": resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{
				"cpu":    resource.MustParse("100m"),
				"memory": resource.MustParse("64Mi"),
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "oauth-config",
				MountPath: "/etc/oauth/config",
			},
			{
				Name:      "tls-certificates",
				MountPath: "/etc/tls/private",
			},
		},
	}

	// Add logout url if logout annotation is present in the notebook
	if notebook.ObjectMeta.Annotations[AnnotationLogoutUrl] != "" {
		proxyContainer.Args = append(proxyContainer.Args,
			"--logout-url="+notebook.ObjectMeta.Annotations[AnnotationLogoutUrl])
	}

	// // Add redirect url on oauth proxy configs
	// if notebook.ObjectMeta.Annotations[AnnotationLogoutUrl] != "" {
	// 	proxyContainer.Args = append(proxyContainer.Args,
	// 		"--redirect-url="+notebook.ObjectMeta.Annotations[AnnotationLogoutUrl]+"/oauth2/callback")
	// }

	// Add the sidecar container to the notebook
	notebookContainers := &notebook.Spec.Template.Spec.Containers
	proxyContainerExists := false
	for index, container := range *notebookContainers {
		if container.Name == "oauth-proxy" {
			(*notebookContainers)[index] = proxyContainer
			proxyContainerExists = true
			break
		}
	}
	if !proxyContainerExists {
		*notebookContainers = append(*notebookContainers, proxyContainer)
	}

	// Add the OAuth configuration volume:
	// https://pkg.go.dev/k8s.io/api/core/v1#Volume
	notebookVolumes := &notebook.Spec.Template.Spec.Volumes
	oauthVolumeExists := false
	oauthVolume := corev1.Volume{
		Name: "oauth-config",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  notebook.Name + "-oauth-config",
				DefaultMode: pointer.Int32Ptr(420),
			},
		},
	}
	for index, volume := range *notebookVolumes {
		if volume.Name == "oauth-config" {
			(*notebookVolumes)[index] = oauthVolume
			oauthVolumeExists = true
			break
		}
	}
	if !oauthVolumeExists {
		*notebookVolumes = append(*notebookVolumes, oauthVolume)
	}

	// Add the TLS certificates volume:
	// https://pkg.go.dev/k8s.io/api/core/v1#Volume
	tlsVolumeExists := false
	tlsVolume := corev1.Volume{
		Name: "tls-certificates",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  notebook.Name + "-tls",
				DefaultMode: pointer.Int32Ptr(420),
			},
		},
	}
	for index, volume := range *notebookVolumes {
		if volume.Name == "tls-certificates" {
			(*notebookVolumes)[index] = tlsVolume
			tlsVolumeExists = true
			break
		}
	}
	if !tlsVolumeExists {
		*notebookVolumes = append(*notebookVolumes, tlsVolume)
	}

	// Set a dedicated service account, do not use default
	notebook.Spec.Template.Spec.ServiceAccountName = notebook.Name
	return nil
}

// Handle transforms the Notebook objects.
func (w *NotebookWebhook) Handle(ctx context.Context, req admission.Request) admission.Response {
	notebook := &nbv1.Notebook{}

	err := w.decoder.Decode(req, notebook)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Inject the the reconciliation lock only on new notebook creation
	if req.Operation == admissionv1.Create {
		err = InjectReconciliationLock(&notebook.ObjectMeta)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}
	}

	// Inject the OAuth proxy if the annotation is present
	if OAuthInjectionIsEnabled(notebook.ObjectMeta) {
		err = InjectOAuthProxy(notebook, w.OAuthConfig)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}
	}

	// If cluster-wide-proxy is enabled add environment variables
	if w.ClusterWideProxyIsEnabled() {
		err = InjectProxyConfig(notebook)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}
	}

	// Create the mutated notebook object
	marshaledNotebook, err := json.Marshal(notebook)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledNotebook)
}

func (w *NotebookWebhook) ClusterWideProxyIsEnabled() bool {
	proxyResourceList := &configv1.ProxyList{}
	err := w.Client.List(context.TODO(), proxyResourceList)
	if err != nil {
		return false
	}

	for _, proxy := range proxyResourceList.Items {
		if proxy.Name == "cluster" {
			if proxy.Status.HTTPProxy != "" && proxy.Status.HTTPSProxy != "" &&
				proxy.Status.NoProxy != "" {
				// Update Proxy Env variables map
				proxyEnvVars["HTTP_PROXY"] = proxy.Status.HTTPProxy
				proxyEnvVars["HTTPS_PROXY"] = proxy.Status.HTTPSProxy
				proxyEnvVars["NO_PROXY"] = proxy.Status.NoProxy
				if proxy.Spec.TrustedCA.Name != "" {
					proxyEnvVars["PIP_CERT"] = "/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem"
					proxyEnvVars["REQUESTS_CA_BUNDLE"] = "/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem"
				}
				return true
			}
		}
	}
	return false

}

// InjectDecoder injects the decoder.
func (w *NotebookWebhook) InjectDecoder(d *admission.Decoder) error {
	w.decoder = d
	return nil
}

func InjectProxyConfig(notebook *nbv1.Notebook) error {
	notebookContainers := &notebook.Spec.Template.Spec.Containers
	var imgContainer corev1.Container

	// Add trusted-ca volume
	notebookVolumes := &notebook.Spec.Template.Spec.Volumes
	certVolumeExists := false
	certVolume := corev1.Volume{
		Name: "trusted-ca",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: "trusted-ca",
				},
				Items: []corev1.KeyToPath{{
					Key:  "ca-bundle.crt",
					Path: "tls-ca-bundle.pem",
				},
				},
			},
		},
	}
	for index, volume := range *notebookVolumes {
		if volume.Name == "trusted-ca" {
			(*notebookVolumes)[index] = certVolume
			certVolumeExists = true
			break
		}
	}
	if !certVolumeExists {
		*notebookVolumes = append(*notebookVolumes, certVolume)
	}

	// Update Notebook Image container with env variables and Volume Mounts
	for _, container := range *notebookContainers {
		// Update notebook image container with env Variables
		if container.Name == notebook.Name {
			var newVars []corev1.EnvVar
			imgContainer = container

			for key, val := range proxyEnvVars {
				keyExists := false
				for _, env := range imgContainer.Env {
					if key == env.Name {
						keyExists = true
						// Update if Proxy spec is updated
						if env.Value != val {
							env.Value = val
						}
					}
				}
				if !keyExists {
					newVars = append(newVars, corev1.EnvVar{Name: key, Value: val})
				}
			}

			// Update container only when required env variables are not present
			imgContainerExists := false
			if len(newVars) != 0 {
				imgContainer.Env = append(imgContainer.Env, newVars...)
			}

			// Create Volume mount
			volumeMountExists := false
			containerVolMounts := &imgContainer.VolumeMounts
			trustedCAVolMount := corev1.VolumeMount{
				Name:      "trusted-ca",
				ReadOnly:  true,
				MountPath: "/etc/pki/ca-trust/extracted/pem",
			}

			for index, volumeMount := range *containerVolMounts {
				if volumeMount.Name == "trusted-ca" {
					(*containerVolMounts)[index] = trustedCAVolMount
					volumeMountExists = true
					break
				}
			}
			if !volumeMountExists {
				*containerVolMounts = append(*containerVolMounts, trustedCAVolMount)
			}

			// Update container with Env and Volume Mount Changes
			for index, container := range *notebookContainers {
				if container.Name == notebook.Name {
					(*notebookContainers)[index] = imgContainer
					imgContainerExists = true
					break
				}
			}

			if !imgContainerExists {
				return fmt.Errorf("notebook image container not found %v", notebook.Name)
			}
			break
		}
	}
	return nil

}
