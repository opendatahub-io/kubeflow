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
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	"github.com/kubeflow/kubeflow/components/notebook-controller/pkg/culler"
)

var _ = Describe("The Openshift Notebook controller", func() {
	// Define utility constants for testing timeouts/durations and intervals.
	const (
		duration = 10 * time.Second
		interval = 2 * time.Second
	)

	When("Creating a Notebook", func() {
		const (
			Name      = "test-notebook"
			Namespace = "default"
		)

		notebook := createNotebook(Name, Namespace)

		expectedRoute := routev1.Route{
			ObjectMeta: metav1.ObjectMeta{
				Name:      Name,
				Namespace: Namespace,
				Labels: map[string]string{
					"notebook-name": Name,
				},
			},
			Spec: routev1.RouteSpec{
				To: routev1.RouteTargetReference{
					Kind:   "Service",
					Name:   Name,
					Weight: pointer.Int32Ptr(100),
				},
				Port: &routev1.RoutePort{
					TargetPort: intstr.FromString("http-" + Name),
				},
				TLS: &routev1.TLSConfig{
					Termination:                   routev1.TLSTerminationEdge,
					InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyRedirect,
				},
				WildcardPolicy: routev1.WildcardPolicyNone,
			},
			Status: routev1.RouteStatus{
				Ingress: []routev1.RouteIngress{},
			},
		}

		route := &routev1.Route{}

		It("Should create a Route to expose the traffic externally", func() {
			ctx := context.Background()

			By("By creating a new Notebook")
			Expect(cli.Create(ctx, notebook)).Should(Succeed())
			time.Sleep(interval)

			By("By checking that the controller has created the Route")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, route)
			}, duration, interval).Should(Succeed())
			Expect(CompareNotebookRoutes(*route, expectedRoute)).Should(BeTrueBecause(cmp.Diff(*route, expectedRoute)))
		})

		It("Should reconcile the Route when modified", func() {
			By("By simulating a manual Route modification")
			patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"to":{"name":"foo"}}}`))
			Expect(cli.Patch(ctx, route, patch)).Should(Succeed())
			time.Sleep(interval)

			By("By checking that the controller has restored the Route spec")
			Eventually(func() (string, error) {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				err := cli.Get(ctx, key, route)
				if err != nil {
					return "", err
				}
				return route.Spec.To.Name, nil
			}, duration, interval).Should(Equal(Name))
			Expect(CompareNotebookRoutes(*route, expectedRoute)).Should(BeTrueBecause(cmp.Diff(*route, expectedRoute)))
		})

		It("Should recreate the Route when deleted", func() {
			By("By deleting the notebook route")
			Expect(cli.Delete(ctx, route)).Should(Succeed())
			time.Sleep(interval)

			By("By checking that the controller has recreated the Route")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, route)
			}, duration, interval).Should(Succeed())
			Expect(CompareNotebookRoutes(*route, expectedRoute)).Should(BeTrueBecause(cmp.Diff(*route, expectedRoute)))
		})

		It("Should delete the Openshift Route", func() {
			// Testenv cluster does not implement Kubernetes GC:
			// https://book.kubebuilder.io/reference/envtest.html#testing-considerations
			// To test that the deletion lifecycle works, test the ownership
			// instead of asserting on existence.
			expectedOwnerReference := metav1.OwnerReference{
				APIVersion:         "kubeflow.org/v1",
				Kind:               "Notebook",
				Name:               Name,
				UID:                notebook.GetObjectMeta().GetUID(),
				Controller:         pointer.BoolPtr(true),
				BlockOwnerDeletion: pointer.BoolPtr(true),
			}

			By("By checking that the Notebook owns the Route object")
			Expect(route.GetObjectMeta().GetOwnerReferences()).To(ContainElement(expectedOwnerReference))

			By("By deleting the recently created Notebook")
			Expect(cli.Delete(ctx, notebook)).Should(Succeed())
			time.Sleep(interval)

			By("By checking that the Notebook is deleted")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, notebook)
			}, duration, interval).Should(HaveOccurred())
		})

	})

	// New test case for RoleBinding reconciliation
	When("Reconcile RoleBindings is called for a Notebook", func() {
		const (
			name      = "test-notebook-rolebinding"
			namespace = "default"
		)
		notebook := createNotebook(name, namespace)

		// Define the role and role-binding names and types used in the reconciliation
		roleRefName := "ds-pipeline-user-access-dspa"
		roleBindingName := "elyra-pipelines-" + name

		BeforeEach(func() {
			// Skip the tests if SET_PIPELINE_RBAC is not set to "true"
			fmt.Printf("SET_PIPELINE_RBAC is: %s\n", os.Getenv("SET_PIPELINE_RBAC"))
			if os.Getenv("SET_PIPELINE_RBAC") != "true" {
				Skip("Skipping RoleBinding reconciliation tests as SET_PIPELINE_RBAC is not set to 'true'")
			}
		})

		It("Should create a RoleBinding when the referenced Role exists", func() {
			ctx := context.Background()

			By("Creating a Notebook and ensuring the Role exists")
			Expect(cli.Create(ctx, notebook)).Should(Succeed())
			time.Sleep(interval)

			// Simulate the Role required by RoleBinding
			role := &rbacv1.Role{
				ObjectMeta: metav1.ObjectMeta{
					Name:      roleRefName,
					Namespace: namespace,
				},
			}
			Expect(cli.Create(ctx, role)).Should(Succeed())
			defer func() {
				if err := cli.Delete(ctx, role); err != nil {
					GinkgoT().Logf("Failed to delete Role: %v", err)
				}
			}()

			By("Checking that the RoleBinding is created")
			roleBinding := &rbacv1.RoleBinding{}
			Eventually(func() error {
				return cli.Get(ctx, types.NamespacedName{Name: roleBindingName, Namespace: namespace}, roleBinding)
			}, duration, interval).Should(Succeed())

			Expect(roleBinding.RoleRef.Name).To(Equal(roleRefName))
			Expect(roleBinding.Subjects[0].Name).To(Equal(name))
			Expect(roleBinding.Subjects[0].Kind).To(Equal("ServiceAccount"))
		})

		It("Should delete the RoleBinding when the Notebook is deleted", func() {
			ctx := context.Background()

			By("Ensuring the RoleBinding exists")
			roleBinding := &rbacv1.RoleBinding{}
			Eventually(func() error {
				return cli.Get(ctx, types.NamespacedName{Name: roleBindingName, Namespace: namespace}, roleBinding)
			}, duration, interval).Should(Succeed())

			By("Deleting the Notebook")
			Expect(cli.Delete(ctx, notebook)).Should(Succeed())

			By("Ensuring the RoleBinding is deleted")
			Eventually(func() error {
				return cli.Get(ctx, types.NamespacedName{Name: roleBindingName, Namespace: namespace}, roleBinding)
			}, duration, interval).Should(Succeed())
		})

	})

	// New test case for notebook creation
	When("Creating a Notebook, test certificate is mounted", func() {
		const (
			Name      = "test-notebook"
			Namespace = "default"
		)

		It("Should mount a trusted-ca when it exists on the given namespace", func() {
			ctx := context.Background()
			logger := logr.Discard()

			By("By simulating the existence of odh-trusted-ca-bundle ConfigMap")
			// Create a ConfigMap similar to odh-trusted-ca-bundle for simulation
			workbenchTrustedCACertBundle := "workbench-trusted-ca-bundle"
			trustedCACertBundle := createOAuthConfigmap(
				"odh-trusted-ca-bundle",
				"default",
				map[string]string{
					"config.openshift.io/inject-trusted-cabundle": "true",
				},
				// NOTE: use valid short CA certs and make them each be different
				// $ openssl req -nodes -x509 -newkey ed25519 -days 365 -set_serial 1 -out /dev/stdout -subj "/"
				map[string]string{
					"ca-bundle.crt":     "-----BEGIN CERTIFICATE-----\nMIGrMF+gAwIBAgIBATAFBgMrZXAwADAeFw0yNDExMTMyMzI3MzdaFw0yNTExMTMy\nMzI3MzdaMAAwKjAFBgMrZXADIQDEMMlJ1P0gyxEV7A8PgpNosvKZgE4ttDDpu/w9\n35BHzjAFBgMrZXADQQDHT8ulalOcI6P5lGpoRcwLzpa4S/5pyqtbqw2zuj7dIJPI\ndNb1AkbARd82zc9bF+7yDkCNmLIHSlDORUYgTNEL\n-----END CERTIFICATE-----",
					"odh-ca-bundle.crt": "-----BEGIN CERTIFICATE-----\nMIGrMF+gAwIBAgIBATAFBgMrZXAwADAeFw0yNDExMTMyMzI2NTlaFw0yNTExMTMy\nMzI2NTlaMAAwKjAFBgMrZXADIQB/v02zcoIIcuan/8bd7cvrBuCGTuVZBrYr1RdA\n0k58yzAFBgMrZXADQQBKsL1tkpOZ6NW+zEX3mD7bhmhxtODQHnANMXEXs0aljWrm\nAxDrLdmzsRRYFYxe23OdXhWqPs8SfO8EZWEvXoME\n-----END CERTIFICATE-----",
				})

			// Create the ConfigMap
			Expect(cli.Create(ctx, trustedCACertBundle)).Should(Succeed())
			defer func() {
				// Clean up the ConfigMap after the test
				if err := cli.Delete(ctx, trustedCACertBundle); err != nil {
					// Log the error without failing the test
					logger.Info("Error occurred during deletion of ConfigMap: %v", err)
				}
			}()

			By("By creating a new Notebook")
			notebook := createNotebook(Name, Namespace)
			Expect(cli.Create(ctx, notebook)).Should(Succeed())
			time.Sleep(interval)

			By("By checking that trusted-ca bundle is mounted")
			// Assert that the volume mount and volume are added correctly
			volumeMountPath := "/etc/pki/tls/custom-certs/ca-bundle.crt"
			expectedVolumeMount := corev1.VolumeMount{
				Name:      "trusted-ca",
				MountPath: volumeMountPath,
				SubPath:   "ca-bundle.crt",
				ReadOnly:  true,
			}
			// Check if the volume mount is present and matches the expected one
			Expect(notebook.Spec.Template.Spec.Containers[0].VolumeMounts).To(ContainElement(expectedVolumeMount))

			expectedVolume := corev1.Volume{
				Name: "trusted-ca",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: workbenchTrustedCACertBundle},
						Optional:             pointer.Bool(true),
						Items: []corev1.KeyToPath{
							{
								Key:  "ca-bundle.crt",
								Path: "ca-bundle.crt",
							},
						},
					},
				},
			}
			// Check if the volume is present and matches the expected one
			Expect(notebook.Spec.Template.Spec.Volumes).To(ContainElement(expectedVolume))

			// Check the content in workbench-trusted-ca-bundle matches what we expect:
			//   - have 2 certificates there in ca-bundle.crt
			//   - both certificates are valid
			configMapName := "workbench-trusted-ca-bundle"
			checkCertConfigMap(ctx, notebook.Namespace, configMapName, "ca-bundle.crt", 2)
		})

	})

	// New test case for notebook update
	When("Updating a Notebook", func() {
		const (
			Name      = "test-notebook-update"
			Namespace = "default"
		)

		notebook := createNotebook(Name, Namespace)

		It("Should update the Notebook specification", func() {
			ctx := context.Background()

			By("By creating a new Notebook")
			Expect(cli.Create(ctx, notebook)).Should(Succeed())
			time.Sleep(interval)

			By("By updating the Notebook's image")
			key := types.NamespacedName{Name: Name, Namespace: Namespace}
			Expect(cli.Get(ctx, key, notebook)).Should(Succeed())

			updatedImage := "registry.redhat.io/ubi8/ubi:updated"
			notebook.Spec.Template.Spec.Containers[0].Image = updatedImage
			Expect(cli.Update(ctx, notebook)).Should(Succeed())
			time.Sleep(interval)

			By("By checking that the Notebook's image is updated")
			Eventually(func() string {
				Expect(cli.Get(ctx, key, notebook)).Should(Succeed())
				return notebook.Spec.Template.Spec.Containers[0].Image
			}, duration, interval).Should(Equal(updatedImage))
		})

		It("When notebook CR is updated, should mount a trusted-ca if it exists on the given namespace", func() {
			ctx := context.Background()
			logger := logr.Discard()

			By("By simulating the existence of odh-trusted-ca-bundle ConfigMap")
			// Create a ConfigMap similar to odh-trusted-ca-bundle for simulation
			workbenchTrustedCACertBundle := "workbench-trusted-ca-bundle"
			trustedCACertBundle := createOAuthConfigmap(
				"odh-trusted-ca-bundle",
				"default",
				map[string]string{
					"config.openshift.io/inject-trusted-cabundle": "true",
				},
				map[string]string{
					"ca-bundle.crt":     "-----BEGIN CERTIFICATE-----\nMIGrMF+gAwIBAgIBATAFBgMrZXAwADAeFw0yNDExMTMyMzI4MjZaFw0yNTExMTMy\nMzI4MjZaMAAwKjAFBgMrZXADIQD77pLvWIX0WmlkYthRZ79oIf7qrGO7yECf668T\nSB42vTAFBgMrZXADQQDs76j81LPh+lgnnf4L0ROUqB66YiBx9SyDTjm83Ya4KC+2\nLEP6Mw1//X2DX89f1chy7RxCpFS3eXb7U/p+GPwA\n-----END CERTIFICATE-----",
					"odh-ca-bundle.crt": "-----BEGIN CERTIFICATE-----\nMIGrMF+gAwIBAgIBATAFBgMrZXAwADAeFw0yNDExMTMyMzI4NDJaFw0yNTExMTMy\nMzI4NDJaMAAwKjAFBgMrZXADIQAw01381TUVSxaCvjQckcw3RTcg+bsVMgNZU8eF\nXa/f3jAFBgMrZXADQQBeJZHSiMOYqa/tXUrQTfNIcklHuvieGyBRVSrX3bVUV2uM\nDBkZLsZt65rCk1A8NG+xkA6j3eIMAA9vBKJ0ht8F\n-----END CERTIFICATE-----",
				})
			// Create the ConfigMap
			Expect(cli.Create(ctx, trustedCACertBundle)).Should(Succeed())
			defer func() {
				// Clean up the ConfigMap after the test
				if err := cli.Delete(ctx, trustedCACertBundle); err != nil {
					// Log the error without failing the test
					logger.Info("Error occurred during deletion of ConfigMap: %v", err)
				}
			}()

			By("By updating the Notebook's image")
			key := types.NamespacedName{Name: Name, Namespace: Namespace}
			Expect(cli.Get(ctx, key, notebook)).Should(Succeed())

			updatedImage := "registry.redhat.io/ubi8/ubi:updated"
			notebook.Spec.Template.Spec.Containers[0].Image = updatedImage
			Expect(cli.Update(ctx, notebook)).Should(Succeed())
			time.Sleep(interval)

			By("By checking that trusted-ca bundle is mounted")
			// Assert that the volume mount and volume are added correctly
			volumeMountPath := "/etc/pki/tls/custom-certs/ca-bundle.crt"
			expectedVolumeMount := corev1.VolumeMount{
				Name:      "trusted-ca",
				MountPath: volumeMountPath,
				SubPath:   "ca-bundle.crt",
				ReadOnly:  true,
			}
			Expect(notebook.Spec.Template.Spec.Containers[0].VolumeMounts).To(ContainElement(expectedVolumeMount))

			expectedVolume := corev1.Volume{
				Name: "trusted-ca",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: workbenchTrustedCACertBundle},
						Optional:             pointer.Bool(true),
						Items: []corev1.KeyToPath{
							{
								Key:  "ca-bundle.crt",
								Path: "ca-bundle.crt",
							},
						},
					},
				},
			}
			Expect(notebook.Spec.Template.Spec.Volumes).To(ContainElement(expectedVolume))

			// Check the content in workbench-trusted-ca-bundle matches what we expect:
			//   - have 2 certificates there in ca-bundle.crt
			//   - both certificates are valid
			configMapName := "workbench-trusted-ca-bundle"
			checkCertConfigMap(ctx, notebook.Namespace, configMapName, "ca-bundle.crt", 2)
		})
	})

	When("Creating a Notebook, test Networkpolicies", func() {
		const (
			Name      = "test-notebook-np"
			Namespace = "default"
		)

		notebook := createNotebook(Name, Namespace)

		npProtocol := corev1.ProtocolTCP
		testPodNamespace := odhNotebookControllerTestNamespace

		expectedNotebookNetworkPolicy := netv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      notebook.Name + "-ctrl-np",
				Namespace: notebook.Namespace,
			},
			Spec: netv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"notebook-name": notebook.Name,
					},
				},
				Ingress: []netv1.NetworkPolicyIngressRule{
					{
						Ports: []netv1.NetworkPolicyPort{
							{
								Protocol: &npProtocol,
								Port: &intstr.IntOrString{
									IntVal: NotebookPort,
								},
							},
						},
						From: []netv1.NetworkPolicyPeer{
							{
								// Since for unit tests the controller does not run in a cluster pod,
								// it cannot detect its own pod's namespace. Therefore, we define it
								// to be `redhat-ods-applications` (in suite_test.go)
								NamespaceSelector: &metav1.LabelSelector{
									MatchLabels: map[string]string{
										"kubernetes.io/metadata.name": testPodNamespace,
									},
								},
							},
						},
					},
				},
				PolicyTypes: []netv1.PolicyType{
					netv1.PolicyTypeIngress,
				},
			},
		}

		expectedNotebookOAuthNetworkPolicy := createOAuthNetworkPolicy(notebook.Name, notebook.Namespace, npProtocol, NotebookOAuthPort)

		notebookNetworkPolicy := &netv1.NetworkPolicy{}
		notebookOAuthNetworkPolicy := &netv1.NetworkPolicy{}

		It("Should create network policies to restrict undesired traffic", func() {
			ctx := context.Background()

			By("By creating a new Notebook")
			Expect(cli.Create(ctx, notebook)).Should(Succeed())
			time.Sleep(interval)

			By("By checking that the controller has created Network policy to allow only controller traffic")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name + "-ctrl-np", Namespace: Namespace}
				return cli.Get(ctx, key, notebookNetworkPolicy)
			}, duration, interval).Should(Succeed())
			Expect(CompareNotebookNetworkPolicies(*notebookNetworkPolicy, expectedNotebookNetworkPolicy)).Should(BeTrueBecause(cmp.Diff(*notebookNetworkPolicy, expectedNotebookNetworkPolicy)))

			By("By checking that the controller has created Network policy to allow all requests on OAuth port")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name + "-oauth-np", Namespace: Namespace}
				return cli.Get(ctx, key, notebookOAuthNetworkPolicy)
			}, duration, interval).Should(Succeed())
			Expect(CompareNotebookNetworkPolicies(*notebookOAuthNetworkPolicy, expectedNotebookOAuthNetworkPolicy)).
				To(BeTrueBecause(cmp.Diff(*notebookOAuthNetworkPolicy, expectedNotebookOAuthNetworkPolicy)))
		})

		It("Should reconcile the Network policies when modified", func() {
			By("By simulating a manual NetworkPolicy modification")
			patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"policyTypes":["Egress"]}}`))
			Expect(cli.Patch(ctx, notebookNetworkPolicy, patch)).Should(Succeed())
			time.Sleep(interval)

			By("By checking that the controller has restored the network policy spec")
			Eventually(func() (string, error) {
				key := types.NamespacedName{Name: Name + "-ctrl-np", Namespace: Namespace}
				err := cli.Get(ctx, key, notebookNetworkPolicy)
				if err != nil {
					return "", err
				}
				return string(notebookNetworkPolicy.Spec.PolicyTypes[0]), nil
			}, duration, interval).Should(Equal("Ingress"))
			Expect(CompareNotebookNetworkPolicies(*notebookNetworkPolicy, expectedNotebookNetworkPolicy)).Should(
				BeTrueBecause(cmp.Diff(*notebookNetworkPolicy, expectedNotebookNetworkPolicy)))
		})

		It("Should recreate the Network Policy when deleted", func() {
			By("By deleting the notebook OAuth Network Policy")
			Expect(cli.Delete(ctx, notebookOAuthNetworkPolicy)).Should(Succeed())
			time.Sleep(interval)

			By("By checking that the controller has recreated the OAuth Network policy")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name + "-oauth-np", Namespace: Namespace}
				return cli.Get(ctx, key, notebookOAuthNetworkPolicy)
			}, duration, interval).Should(Succeed())
			Expect(CompareNotebookNetworkPolicies(*notebookOAuthNetworkPolicy, expectedNotebookOAuthNetworkPolicy)).Should(
				BeTrueBecause(cmp.Diff(*notebookOAuthNetworkPolicy, expectedNotebookOAuthNetworkPolicy)))
		})

		It("Should delete the Network Policies", func() {
			expectedOwnerReference := metav1.OwnerReference{
				APIVersion:         "kubeflow.org/v1",
				Kind:               "Notebook",
				Name:               Name,
				UID:                notebook.GetObjectMeta().GetUID(),
				Controller:         pointer.BoolPtr(true),
				BlockOwnerDeletion: pointer.BoolPtr(true),
			}

			By("By checking that the Notebook owns the Notebook Network Policy object")
			Expect(notebookNetworkPolicy.GetObjectMeta().GetOwnerReferences()).To(ContainElement(expectedOwnerReference))

			By("By checking that the Notebook owns the Notebook OAuth Network Policy object")
			Expect(notebookOAuthNetworkPolicy.GetObjectMeta().GetOwnerReferences()).To(ContainElement(expectedOwnerReference))

			By("By deleting the recently created Notebook")
			Expect(cli.Delete(ctx, notebook)).Should(Succeed())

			By("By checking that the Notebook is deleted")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, notebook)
			}, duration, interval).Should(HaveOccurred())
		})

	})

	When("Creating a Notebook with OAuth", func() {
		const (
			Name      = "test-notebook-oauth"
			Namespace = "default"
		)

		notebook := createNotebook(Name, Namespace)
		notebook.SetLabels(map[string]string{
			"app.kubernetes.io/instance": Name,
		})
		notebook.SetAnnotations(map[string]string{
			"notebooks.opendatahub.io/inject-oauth":     "true",
			"notebooks.opendatahub.io/foo":              "bar",
			"notebooks.opendatahub.io/oauth-logout-url": "https://example.notebook-url/notebook/" + Namespace + "/" + Name,
		})
		notebook.Spec = nbv1.NotebookSpec{
			Template: nbv1.NotebookTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  Name,
						Image: "registry.redhat.io/ubi8/ubi:latest",
					}},
					Volumes: []corev1.Volume{
						{
							Name: "notebook-data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: Name + "-data",
								},
							},
						},
					},
				},
			},
		}

		expectedNotebook := nbv1.Notebook{
			ObjectMeta: metav1.ObjectMeta{
				Name:      Name,
				Namespace: Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/instance": Name,
				},
				Annotations: map[string]string{
					"notebooks.opendatahub.io/inject-oauth":     "true",
					"notebooks.opendatahub.io/foo":              "bar",
					"notebooks.opendatahub.io/oauth-logout-url": "https://example.notebook-url/notebook/" + Namespace + "/" + Name,
					"kubeflow-resource-stopped":                 "odh-notebook-controller-lock",
				},
			},
			Spec: nbv1.NotebookSpec{
				Template: nbv1.NotebookTemplateSpec{
					Spec: corev1.PodSpec{
						ServiceAccountName: Name,
						Containers: []corev1.Container{
							{
								Name:  Name,
								Image: "registry.redhat.io/ubi8/ubi:latest",
							},
							createOAuthContainer(Name, Namespace),
						},
						Volumes: []corev1.Volume{
							{
								Name: "notebook-data",
								VolumeSource: corev1.VolumeSource{
									PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
										ClaimName: Name + "-data",
									},
								},
							},
							{
								Name: "oauth-config",
								VolumeSource: corev1.VolumeSource{
									Secret: &corev1.SecretVolumeSource{
										SecretName:  Name + "-oauth-config",
										DefaultMode: pointer.Int32Ptr(420),
									},
								},
							},
							{
								Name: "tls-certificates",
								VolumeSource: corev1.VolumeSource{
									Secret: &corev1.SecretVolumeSource{
										SecretName:  Name + "-tls",
										DefaultMode: pointer.Int32Ptr(420),
									},
								},
							},
						},
					},
				},
			},
		}

		It("Should inject the OAuth proxy as a sidecar container", func() {
			ctx := context.Background()

			By("By creating a new Notebook")
			Expect(cli.Create(ctx, notebook)).Should(Succeed())
			time.Sleep(interval)

			By("By checking that the webhook has injected the sidecar container")
			Expect(CompareNotebooks(*notebook, expectedNotebook)).Should(BeTrueBecause(cmp.Diff(*notebook, expectedNotebook)))
		})

		It("Should remove the reconciliation lock annotation", func() {
			By("By checking that the annotation lock annotation is not present")
			delete(expectedNotebook.Annotations, culler.STOP_ANNOTATION)
			Eventually(func() bool {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				err := cli.Get(ctx, key, notebook)
				if err != nil {
					return false
				}
				return CompareNotebooks(*notebook, expectedNotebook)
			}, duration, interval).Should(BeTrueBecause(cmp.Diff(*notebook, expectedNotebook)))
		})

		It("Should reconcile the Notebook when modified", func() {
			By("By simulating a manual Notebook modification")
			notebook.Spec.Template.Spec.ServiceAccountName = "foo"
			notebook.Spec.Template.Spec.Containers[1].Image = "bar"
			notebook.Spec.Template.Spec.Volumes[1].VolumeSource = corev1.VolumeSource{}
			Expect(cli.Update(ctx, notebook)).Should(Succeed())
			time.Sleep(interval)

			By("By checking that the webhook has restored the Notebook spec")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, notebook)
			}, duration, interval).Should(Succeed())
			Expect(CompareNotebooks(*notebook, expectedNotebook)).Should(BeTrueBecause(cmp.Diff(*notebook, expectedNotebook)))
		})

		serviceAccount := &corev1.ServiceAccount{}
		expectedServiceAccount := createOAuthServiceAccount(Name, Namespace)

		It("Should create a Service Account for the notebook", func() {
			By("By checking that the controller has created the Service Account")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, serviceAccount)
			}, duration, interval).Should(Succeed())
			Expect(CompareNotebookServiceAccounts(*serviceAccount, expectedServiceAccount)).Should(
				BeTrueBecause(cmp.Diff(*serviceAccount, expectedServiceAccount)))
		})

		It("Should recreate the Service Account when deleted", func() {
			By("By deleting the notebook Service Account")
			Expect(cli.Delete(ctx, serviceAccount)).Should(Succeed())
			time.Sleep(interval)

			By("By checking that the controller has recreated the Service Account")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, serviceAccount)
			}, duration, interval).Should(Succeed())
			Expect(CompareNotebookServiceAccounts(*serviceAccount, expectedServiceAccount)).Should(
				BeTrueBecause(cmp.Diff(*serviceAccount, expectedServiceAccount)))
		})

		service := &corev1.Service{}
		expectedService := corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      Name + "-tls",
				Namespace: Namespace,
				Labels: map[string]string{
					"notebook-name": Name,
				},
				Annotations: map[string]string{
					"service.beta.openshift.io/serving-cert-secret-name": Name + "-tls",
				},
			},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{{
					Name:       OAuthServicePortName,
					Port:       OAuthServicePort,
					TargetPort: intstr.FromString(OAuthServicePortName),
					Protocol:   corev1.ProtocolTCP,
				}},
			},
		}

		It("Should create a Service to expose the OAuth proxy", func() {
			By("By checking that the controller has created the Service")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name + "-tls", Namespace: Namespace}
				return cli.Get(ctx, key, service)
			}, duration, interval).Should(Succeed())
			Expect(CompareNotebookServices(*service, expectedService)).Should(BeTrueBecause(cmp.Diff(*service, expectedService)))
		})

		It("Should recreate the Service when deleted", func() {
			By("By deleting the notebook Service")
			Expect(cli.Delete(ctx, service)).Should(Succeed())
			time.Sleep(interval)

			By("By checking that the controller has recreated the Service")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name + "-tls", Namespace: Namespace}
				return cli.Get(ctx, key, service)
			}, duration, interval).Should(Succeed())
			Expect(CompareNotebookServices(*service, expectedService)).Should(BeTrueBecause(cmp.Diff(*service, expectedService)))
		})

		secret := &corev1.Secret{}

		It("Should create a Secret with the OAuth proxy configuration", func() {
			By("By checking that the controller has created the Secret")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name + "-oauth-config", Namespace: Namespace}
				return cli.Get(ctx, key, secret)
			}, duration, interval).Should(Succeed())

			By("By checking that the cookie secret format is correct")
			Expect(len(secret.Data["cookie_secret"])).Should(Equal(32))
		})

		It("Should recreate the Secret when deleted", func() {
			By("By deleting the notebook Secret")
			Expect(cli.Delete(ctx, secret)).Should(Succeed())
			time.Sleep(interval)

			By("By checking that the controller has recreated the Secret")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name + "-oauth-config", Namespace: Namespace}
				return cli.Get(ctx, key, secret)
			}, duration, interval).Should(Succeed())
		})

		route := &routev1.Route{}
		expectedRoute := routev1.Route{
			ObjectMeta: metav1.ObjectMeta{
				Name:      Name,
				Namespace: Namespace,
				Labels: map[string]string{
					"notebook-name": Name,
				},
			},
			Spec: routev1.RouteSpec{
				To: routev1.RouteTargetReference{
					Kind:   "Service",
					Name:   Name + "-tls",
					Weight: pointer.Int32Ptr(100),
				},
				Port: &routev1.RoutePort{
					TargetPort: intstr.FromString(OAuthServicePortName),
				},
				TLS: &routev1.TLSConfig{
					Termination:                   routev1.TLSTerminationReencrypt,
					InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyRedirect,
				},
				WildcardPolicy: routev1.WildcardPolicyNone,
			},
			Status: routev1.RouteStatus{
				Ingress: []routev1.RouteIngress{},
			},
		}

		It("Should create a Route to expose the traffic externally", func() {
			By("By checking that the controller has created the Route")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, route)
			}, duration, interval).Should(Succeed())
			Expect(CompareNotebookRoutes(*route, expectedRoute)).Should(BeTrueBecause(cmp.Diff(*route, expectedRoute)))
		})

		It("Should recreate the Route when deleted", func() {
			By("By deleting the notebook Route")
			Expect(cli.Delete(ctx, route)).Should(Succeed())
			time.Sleep(interval)

			By("By checking that the controller has recreated the Route")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, route)
			}, duration, interval).Should(Succeed())
			Expect(CompareNotebookRoutes(*route, expectedRoute)).Should(BeTrueBecause(cmp.Diff(*route, expectedRoute)))
		})

		It("Should reconcile the Route when modified", func() {
			By("By simulating a manual Route modification")
			patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"to":{"name":"foo"}}}`))
			Expect(cli.Patch(ctx, route, patch)).Should(Succeed())
			time.Sleep(interval)

			By("By checking that the controller has restored the Route spec")
			Eventually(func() (string, error) {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				err := cli.Get(ctx, key, route)
				if err != nil {
					return "", err
				}
				return route.Spec.To.Name, nil
			}, duration, interval).Should(Equal(Name + "-tls"))
			Expect(CompareNotebookRoutes(*route, expectedRoute)).Should(BeTrueBecause(cmp.Diff(*route, expectedRoute)))
		})

		It("Should delete the OAuth proxy objects", func() {
			// Testenv cluster does not implement Kubernetes GC:
			// https://book.kubebuilder.io/reference/envtest.html#testing-considerations
			// To test that the deletion lifecycle works, test the ownership
			// instead of asserting on existence.
			expectedOwnerReference := metav1.OwnerReference{
				APIVersion:         "kubeflow.org/v1",
				Kind:               "Notebook",
				Name:               Name,
				UID:                notebook.GetObjectMeta().GetUID(),
				Controller:         pointer.BoolPtr(true),
				BlockOwnerDeletion: pointer.BoolPtr(true),
			}

			By("By checking that the Notebook owns the Service Account object")
			Expect(serviceAccount.GetObjectMeta().GetOwnerReferences()).To(ContainElement(expectedOwnerReference))

			By("By checking that the Notebook owns the Service object")
			Expect(service.GetObjectMeta().GetOwnerReferences()).To(ContainElement(expectedOwnerReference))

			By("By checking that the Notebook owns the Secret object")
			Expect(secret.GetObjectMeta().GetOwnerReferences()).To(ContainElement(expectedOwnerReference))

			By("By checking that the Notebook owns the Route object")
			Expect(route.GetObjectMeta().GetOwnerReferences()).To(ContainElement(expectedOwnerReference))

			By("By deleting the recently created Notebook")
			Expect(cli.Delete(ctx, notebook)).Should(Succeed())
			time.Sleep(interval)

			By("By checking that the Notebook is deleted")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, notebook)
			}, duration, interval).Should(HaveOccurred())
		})
	})

	When("Creating notebook as part of Service Mesh", func() {

		const (
			name      = "test-notebook-mesh"
			namespace = "mesh-ns"
		)
		testNamespaces = append(testNamespaces, namespace)

		notebookOAuthNetworkPolicy := createOAuthNetworkPolicy(name, namespace, corev1.ProtocolTCP, NotebookOAuthPort)

		It("Should not add OAuth sidecar", func() {
			notebook := createNotebook(name, namespace)
			notebook.SetAnnotations(map[string]string{AnnotationServiceMesh: "true"})
			ctx := context.Background()
			Expect(cli.Create(ctx, notebook)).Should(Succeed())

			actualNotebook := &nbv1.Notebook{}
			Eventually(func() error {
				key := types.NamespacedName{Name: name, Namespace: namespace}
				return cli.Get(ctx, key, actualNotebook)
			}, duration, interval).Should(Succeed())

			Expect(actualNotebook.Spec.Template.Spec.Containers).To(Not(ContainElement(createOAuthContainer(name, namespace))))
		})

		It("Should not define OAuth network policy", func() {
			policies := &netv1.NetworkPolicyList{}
			Eventually(func() error {
				return cli.List(context.Background(), policies, client.InNamespace(namespace))
			}, duration, interval).Should(Succeed())

			Expect(policies.Items).To(Not(ContainElement(notebookOAuthNetworkPolicy)))
		})

		It("Should not create routes", func() {
			routes := &routev1.RouteList{}
			Eventually(func() error {
				return cli.List(context.Background(), routes, client.InNamespace(namespace))
			}, duration, interval).Should(Succeed())

			Expect(routes.Items).To(BeEmpty())
		})

		It("Should not create OAuth Service Account", func() {
			oauthServiceAccount := createOAuthServiceAccount(name, namespace)

			serviceAccounts := &corev1.ServiceAccountList{}
			Eventually(func() error {
				return cli.List(context.Background(), serviceAccounts, client.InNamespace(namespace))
			}, duration, interval).Should(Succeed())

			Expect(serviceAccounts.Items).ToNot(ContainElement(oauthServiceAccount))
		})

		It("Should not create OAuth secret", func() {
			secrets := &corev1.SecretList{}
			Eventually(func() error {
				return cli.List(context.Background(), secrets, client.InNamespace(namespace))
			}, duration, interval).Should(Succeed())

			Expect(secrets.Items).To(BeEmpty())
		})

	})

})

func createNotebook(name, namespace string) *nbv1.Notebook {
	return &nbv1.Notebook{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: nbv1.NotebookSpec{
			Template: nbv1.NotebookTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  name,
					Image: "registry.redhat.io/ubi8/ubi:latest",
				}}}},
		},
	}
}

func createOAuthServiceAccount(name, namespace string) corev1.ServiceAccount {
	return corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"notebook-name": name,
			},
			Annotations: map[string]string{
				"serviceaccounts.openshift.io/oauth-redirectreference.first": "" +
					`{"kind":"OAuthRedirectReference","apiVersion":"v1","reference":{"kind":"Route","name":"` + name + `"}}`,
			},
		},
	}
}

func createOAuthContainer(name, namespace string) corev1.Container {
	return corev1.Container{
		Name:            "oauth-proxy",
		Image:           OAuthProxyImage,
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
			"--openshift-service-account=" + name,
			"--cookie-secret-file=/etc/oauth/config/cookie_secret",
			"--cookie-expire=24h0m0s",
			"--tls-cert=/etc/tls/private/tls.crt",
			"--tls-key=/etc/tls/private/tls.key",
			"--upstream=http://localhost:8888",
			"--upstream-ca=/var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
			"--email-domain=*",
			"--skip-provider-button",
			`--openshift-sar={"verb":"get","resource":"notebooks","resourceAPIGroup":"kubeflow.org",` +
				`"resourceName":"` + name + `","namespace":"$(NAMESPACE)"}`,
			"--logout-url=https://example.notebook-url/notebook/" + namespace + "/" + name,
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
}

func createOAuthNetworkPolicy(name, namespace string, npProtocol corev1.Protocol, port int32) netv1.NetworkPolicy {
	return netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-oauth-np",
			Namespace: namespace,
		},
		Spec: netv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"notebook-name": name,
				},
			},
			Ingress: []netv1.NetworkPolicyIngressRule{
				{
					Ports: []netv1.NetworkPolicyPort{
						{
							Protocol: &npProtocol,
							Port: &intstr.IntOrString{
								IntVal: port,
							},
						},
					},
				},
			},
			PolicyTypes: []netv1.PolicyType{
				netv1.PolicyTypeIngress,
			},
		},
	}
}

// createOAuthConfigmap creates a ConfigMap
// this function can be used to create any kinda of ConfigMap
func createOAuthConfigmap(name, namespace string, label map[string]string, configMapData map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    label,
		},
		Data: configMapData,
	}
}

// checkCertConfigMap checks the content of a config map defined by the name and namespace
// It triest to parse the given certFileName and checks that all certificates can be parsed there and that the number of the certificates matches what we expect.
func checkCertConfigMap(ctx context.Context, namespace string, configMapName string, certFileName string, expNumberCerts int) {
	configMap := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: namespace, Name: configMapName}
	Expect(cli.Get(ctx, key, configMap)).Should(Succeed())

	// Attempt to decode PEM encoded certificates so we are sure all are readable as expected
	certData := configMap.Data[certFileName]
	certDataByte := []byte(certData)
	certificatesFound := 0
	for len(certDataByte) > 0 {
		block, remainder := pem.Decode(certDataByte)
		certDataByte = remainder

		if block == nil {
			break
		}

		if block.Type == "CERTIFICATE" {
			// Attempt to parse the certificate
			_, err := x509.ParseCertificate(block.Bytes)
			Expect(err).ShouldNot(HaveOccurred())
			certificatesFound++
		}
	}
	Expect(certificatesFound).Should(Equal(expNumberCerts), "Number of parsed certificates don't match expected one:\n"+certData)
}
