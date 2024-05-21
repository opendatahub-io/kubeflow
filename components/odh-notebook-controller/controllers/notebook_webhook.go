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
	"sort"
	"strings"

	"github.com/go-logr/logr"
	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	"github.com/kubeflow/kubeflow/components/notebook-controller/pkg/culler"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

//+kubebuilder:webhook:path=/mutate-notebook-v1,mutating=true,failurePolicy=fail,sideEffects=None,groups=kubeflow.org,resources=notebooks,verbs=create;update,versions=v1,name=notebooks.opendatahub.io,admissionReviewVersions=v1

// NotebookWebhook holds the webhook configuration.
type NotebookWebhook struct {
	Log         logr.Logger
	Client      client.Client
	Config      *rest.Config
	Decoder     *admission.Decoder
	OAuthConfig OAuthConfig
}

// InjectReconciliationLock injects the kubeflow notebook controller culling
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
			"--email-domain=*",
			"--skip-provider-button",
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

	// Initialize logger format
	log := w.Log.WithValues("notebook", req.Name, "namespace", req.Namespace)

	notebook := &nbv1.Notebook{}

	err := w.Decoder.Decode(req, notebook)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Inject the reconciliation lock only on new notebook creation
	if req.Operation == admissionv1.Create {
		err = InjectReconciliationLock(&notebook.ObjectMeta)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}

		// Only Mount ca bundle on new notebook creation
		err = CheckAndMountCACertBundle(ctx, w.Client, notebook, log)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}

		// Check Imagestream Info
		err = SetContainerImageFromRegistry(ctx, w.Config, notebook, log)
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

	// Create the mutated notebook object
	marshaledNotebook, err := json.Marshal(notebook)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledNotebook)
}

// InjectDecoder injects the decoder.
func (w *NotebookWebhook) InjectDecoder(d *admission.Decoder) error {
	w.Decoder = d
	return nil
}

// CheckAndMountCACertBundle checks if the odh-trusted-ca-bundle ConfigMap is present
func CheckAndMountCACertBundle(ctx context.Context, cli client.Client, notebook *nbv1.Notebook, log logr.Logger) error {

	workbenchConfigMapName := "workbench-trusted-ca-bundle"
	odhConfigMapName := "odh-trusted-ca-bundle"

	// if the odh-trusted-ca-bundle ConfigMap is not present, skip the process
	// as operator might have disabled the feature.
	odhConfigMap := &corev1.ConfigMap{}
	odhErr := cli.Get(ctx, client.ObjectKey{Namespace: notebook.Namespace, Name: odhConfigMapName}, odhConfigMap)
	if odhErr != nil {
		log.Info("odh-trusted-ca-bundle ConfigMap is not present, not starting mounting process.")
		return nil
	}

	// if the workbench-trusted-ca-bundle ConfigMap is not present,
	// controller was not successful in creating the ConfigMap, skip the process
	workbenchConfigMap := &corev1.ConfigMap{}
	err := cli.Get(ctx, client.ObjectKey{Namespace: notebook.Namespace, Name: workbenchConfigMapName}, workbenchConfigMap)
	if err != nil {
		log.Info("workbench-trusted-ca-bundle ConfigMap is not present, start creating it...")
		// create the ConfigMap if it does not exist
		workbenchConfigMap = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      workbenchConfigMapName,
				Namespace: notebook.Namespace,
				Labels:    map[string]string{"opendatahub.io/managed-by": "workbenches"},
			},
			Data: map[string]string{
				"ca-bundle.crt": odhConfigMap.Data["ca-bundle.crt"],
			},
		}
		err = cli.Create(ctx, workbenchConfigMap)
		if err != nil {
			log.Info("Failed to create workbench-trusted-ca-bundle ConfigMap")
			return nil
		}
		log.Info("Created workbench-trusted-ca-bundle ConfigMap")
	}

	cm := workbenchConfigMap
	if cm.Name == workbenchConfigMapName {
		// Inject the trusted-ca volume and environment variables
		log.Info("Injecting trusted-ca volume and environment variables", notebook.Name, "namespace", notebook.Namespace)
		return InjectCertConfig(notebook, workbenchConfigMapName)
	}
	return nil
}

func InjectCertConfig(notebook *nbv1.Notebook, configMapName string) error {

	// ConfigMap details
	configVolumeName := "trusted-ca"
	configMapMountPath := "/etc/pki/tls/custom-certs/ca-bundle.crt"
	configMapMountKey := "ca-bundle.crt"
	configMapMountValue := "ca-bundle.crt"
	configEnvVars := map[string]string{
		"PIP_CERT":               configMapMountPath,
		"REQUESTS_CA_BUNDLE":     configMapMountPath,
		"SSL_CERT_FILE":          configMapMountPath,
		"PIPELINES_SSL_SA_CERTS": configMapMountPath,
		"GIT_SSL_CAINFO":         configMapMountPath,
	}

	notebookContainers := &notebook.Spec.Template.Spec.Containers
	var imgContainer corev1.Container

	// Add trusted-ca volume
	notebookVolumes := &notebook.Spec.Template.Spec.Volumes
	certVolumeExists := false
	certVolume := corev1.Volume{
		Name: configVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: configMapName,
				},
				Optional: pointer.Bool(true),
				Items: []corev1.KeyToPath{{
					Key:  configMapMountKey,
					Path: configMapMountValue,
				},
				},
			},
		},
	}
	for index, volume := range *notebookVolumes {
		if volume.Name == configVolumeName {
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

			for key, val := range configEnvVars {
				keyExists := false
				for _, env := range imgContainer.Env {
					if key == env.Name {
						keyExists = true
						// Update if env value is updated
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
				Name:      configVolumeName,
				ReadOnly:  true,
				MountPath: configMapMountPath,
				SubPath:   configMapMountValue,
			}

			for index, volumeMount := range *containerVolMounts {
				if volumeMount.Name == configVolumeName {
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

// SetContainerImageFromRegistry checks if there is an internal registry and takes the corresponding actions to set the container.image value.
// If an internal registry is detected, it uses the default values specified in the Notebook Custom Resource (CR).
// Otherwise, it checks the last-image-selection annotation to find the image stream and fetches the image from status.dockerImageReference,
// assigning it to the container.image value.
func SetContainerImageFromRegistry(ctx context.Context, config *rest.Config, notebook *nbv1.Notebook, log logr.Logger) error {
    // Create a dynamic client
    dynamicClient, err := dynamic.NewForConfig(config)
    if err != nil {
        log.Error(err, "Error creating dynamic client")
        return err
    }
    // Specify the GroupVersionResource for imagestreams
    ims := schema.GroupVersionResource{
        Group:    "image.openshift.io",
        Version:  "v1",
        Resource: "imagestreams",
    }

    annotations := notebook.GetAnnotations()
    if annotations != nil {
        if imageSelection, exists := annotations["notebooks.opendatahub.io/last-image-selection"]; exists {
            // Check if the image selection has an internal registry, if so  will pickup this. This value constructed on the initialization of the Notebook CR.
            if strings.Contains(notebook.Spec.Template.Spec.Containers[0].Image, "image-registry.openshift-image-registry.svc:5000") {
                log.Info("Internal registry found. Will pickup the default value from image field.")
                return nil
            } else {
                // Split the imageSelection to imagestream and tag
                parts := strings.Split(imageSelection, ":")
                if len(parts) != 2 {
                    log.Error(nil, "Invalid image selection format")
                    return fmt.Errorf("invalid image selection format")
                }

                imagestreamName := parts[0]
                tag := parts[1]

                // Specify the namespaces to search in
                namespaces := []string{"opendatahub", "redhat-ods-applications"}

                imagestreamFound := false

                for _, namespace := range namespaces {
                    // List imagestreams in the specified namespace
                    imagestreams, err := dynamicClient.Resource(ims).Namespace(namespace).List(ctx, metav1.ListOptions{})
                    if err != nil {
                        log.Error(err, "Cannot list imagestreams", "namespace", namespace)
                        continue
                    }

                    // Iterate through the imagestreams to find matches
                    for _, item := range imagestreams.Items {
                        metadata := item.Object["metadata"].(map[string]interface{})
                        name := metadata["name"].(string)

                        if name == imagestreamName {
                            status := item.Object["status"].(map[string]interface{})

                            log.Info("No Internal registry found, pick up imageHash from status.tag.dockerImageReference")

                            tags := status["tags"].([]interface{})
                            for _, t := range tags {
                                tagMap := t.(map[string]interface{})
                                tagName := tagMap["tag"].(string)
                                if tagName == tag {
                                    items := tagMap["items"].([]interface{})
                                    if len(items) > 0 {
                                        // Sort items by creationTimestamp to get the most recent one
                                        sort.Slice(items, func(i, j int) bool {
                                            iTime := items[i].(map[string]interface{})["created"].(string)
                                            jTime := items[j].(map[string]interface{})["created"].(string)
                                            return iTime > jTime // Lexicographical comparison of RFC3339 timestamps
                                        })
                                        imageHash := items[0].(map[string]interface{})["dockerImageReference"].(string)
                                        notebook.Spec.Template.Spec.Containers[0].Image = imageHash
                                        // Update the JUPYTER_IMAGE environment variable
                                        for i, envVar := range notebook.Spec.Template.Spec.Containers[0].Env {
                                            if envVar.Name == "JUPYTER_IMAGE" {
                                                notebook.Spec.Template.Spec.Containers[0].Env[i].Value = imageHash
                                                break
                                            }
                                        }
                                        imagestreamFound = true
                                        break
                                    }
                                }
                            }
                        }
                    }
                    if imagestreamFound {
                        break
                    }
                }

                if !imagestreamFound {
                    log.Info("Imagestream not found in any of the specified namespaces", "imagestreamName", imagestreamName, "tag", tag)
                }
            }
        }
    }

    return nil
}
