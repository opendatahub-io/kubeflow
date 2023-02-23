package e2e

import (
	"fmt"
	"log"
	"testing"

	admregv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func creationTestSuite(t *testing.T) {
	testCtx, err := NewTestContext()
	require.NoError(t, err)
	t.Run(testCtx.webhookDeployment.ObjectMeta.Name, func(t *testing.T) {
		t.Run("Creation of webhook deployment", func(t *testing.T) {
			err = testCtx.testWebhookCreation()
			require.NoError(t, err, "error creating deployment ")
		})
		t.Run("Creation of webhook service", func(t *testing.T) {
			err = testCtx.testServiceCreation()
			require.NoError(t, err, "error creating service ")
		})
		t.Run("Creation of mutating webhook config", func(t *testing.T) {
			err = testCtx.testWebhookConfigCreation()
			require.NoError(t, err, "error creating mutating webhook config ")
		})
	})
}

func (testCtx *testContext) testWebhookCreation() error {
	testDeployment := testCtx.webhookDeployment
	deploymentLookup := types.NamespacedName{Name: testDeployment.ObjectMeta.Name, Namespace: testDeployment.ObjectMeta.Namespace}
	createdDeployment := appsv1.Deployment{}

	err := testCtx.customClient.Get(testCtx.ctx, deploymentLookup, &createdDeployment)

	if err != nil {
		if errors.IsNotFound(err) {
			createErr := testCtx.customClient.Create(testCtx.ctx, &testDeployment)
			if createErr != nil {
				return fmt.Errorf("Error creating Deployment %v: %v", testDeployment.ObjectMeta.Name, createErr)
			}
		} else {
			return fmt.Errorf("Error getting Deployment %v: %v", testDeployment.ObjectMeta.Name, err)
		}
	}

	return nil
}

func (testCtx *testContext) testServiceCreation() error {
	testService := testCtx.setupService()
	serviceLookup := types.NamespacedName{Name: testService.ObjectMeta.Name, Namespace: testService.ObjectMeta.Namespace}
	createdService := corev1.Service{}

	err := testCtx.customClient.Get(testCtx.ctx, serviceLookup, &createdService)

	if err != nil {
		if errors.IsNotFound(err) {
			createErr := testCtx.customClient.Create(testCtx.ctx, &testService)
			if createErr != nil {
				return fmt.Errorf("Error creating Service %v: %v", testService.ObjectMeta.Name, createErr)
			}
		} else {
			return fmt.Errorf("Error getting Service %v: %v", testService.ObjectMeta.Name, err)
		}
	}

	return nil
}

func (testCtx *testContext) testWebhookConfigCreation() error {
	testConfig := testCtx.setupMutatingWebhook()
	configLookup := types.NamespacedName{Name: testConfig.ObjectMeta.Name, Namespace: testConfig.ObjectMeta.Namespace}
	createdConfig := admregv1.MutatingWebhookConfiguration{}

	err := testCtx.customClient.Get(testCtx.ctx, configLookup, &createdConfig)

	if err != nil {
		if errors.IsNotFound(err) {
			createErr := testCtx.customClient.Create(testCtx.ctx, &testConfig)
			if createErr != nil {
				return fmt.Errorf("Error creating mutating webhook config %v: %v", testConfig.ObjectMeta.Name, createErr)
			}
		} else {
			return fmt.Errorf("Error getting mutating webhook config %v: %v", testConfig.ObjectMeta.Name, err)
		}
	}

	return nil
}

func validationTestSuite(t *testing.T) {
	testCtx, err := NewTestContext()
	require.NoError(t, err)
	t.Run("Validate Webhook", func(t *testing.T) {
		err = testCtx.validateWebhookDeployment()
		require.NoError(t, err, "error validating Webhook Deployment")
	})
}

func (testCtx *testContext) validateWebhookDeployment() error {
	testDeployment := testCtx.webhookDeployment
	deploymentName := testDeployment.ObjectMeta.Name
	deploymentLookup := types.NamespacedName{Name: deploymentName, Namespace: testDeployment.ObjectMeta.Namespace}
	createdDeployment := &appsv1.Deployment{}

	err := wait.Poll(testCtx.resourceRetryInterval, testCtx.resourceCreationTimeout, func() (done bool, err error) {
		errDep := testCtx.customClient.Get(testCtx.ctx, deploymentLookup, createdDeployment)
		if errDep != nil {
			log.Printf("Failed to get %s Deployment", deploymentName)
			return false, errDep
		} else {
			if createdDeployment.Status.Replicas == createdDeployment.Status.AvailableReplicas {
				return true, nil
			} else {
				return false, nil
			}
		}
	})
	if err != nil {
		return fmt.Errorf("error validating webhook deployment: %v", err)
	}
	return nil
}

func deletionTestSuite(t *testing.T) {
	testCtx, err := NewTestContext()
	require.NoError(t, err)
	t.Run(testCtx.webhookDeployment.ObjectMeta.Name, func(t *testing.T) {
		t.Run("Webhook Deployment Deletion", func(t *testing.T) {
			err = testCtx.testWebhookDeploymentDeletion()
			require.NoError(t, err, "error deleting webhook deployment")
		})
		t.Run("Webhook Service Deletion", func(t *testing.T) {
			err = testCtx.testServiceDeletion()
			require.NoError(t, err, "error deleting webhook service")
		})
		// t.Run("Dependent Resource Deletion", func(t *testing.T) {
		// 	err = testCtx.testNotebookResourcesDeletion(nbContext.nbObjectMeta)
		// 	require.NoError(t, err, "error deleting dependent resources ")
		// })
	})
}

func (testCtx *testContext) testWebhookDeploymentDeletion() error {
	testDeployment := testCtx.webhookDeployment
	deploymentLookup := types.NamespacedName{Name: testDeployment.ObjectMeta.Name, Namespace: testDeployment.ObjectMeta.Namespace}
	createdDeployment := &appsv1.Deployment{}

	err := testCtx.customClient.Get(testCtx.ctx, deploymentLookup, createdDeployment)
	if err == nil {
		webhookErr := testCtx.customClient.Delete(testCtx.ctx, createdDeployment, &client.DeleteOptions{})
		if webhookErr != nil {
			return fmt.Errorf("error deleting test webhook deployment %s: %v", testDeployment.ObjectMeta.Name, webhookErr)
		}
	} else if !errors.IsNotFound(err) {
		if err != nil {
			return fmt.Errorf("error getting test webhook deployment: %v", err)
		}
	}
	return nil
}
func (testCtx *testContext) testServiceDeletion() error {
	testService := testCtx.setupService()
	serviceLookup := types.NamespacedName{Name: testService.ObjectMeta.Name, Namespace: testService.ObjectMeta.Namespace}
	createdService := &corev1.Service{}

	err := testCtx.customClient.Get(testCtx.ctx, serviceLookup, createdService)
	if err == nil {
		delerr := testCtx.customClient.Delete(testCtx.ctx, createdService, &client.DeleteOptions{})
		if delerr != nil {
			return fmt.Errorf("error deleting test webhook service %s: %v", testService.ObjectMeta.Name, delerr)
		}
	} else if !errors.IsNotFound(err) {
		if err != nil {
			return fmt.Errorf("error getting test webhook service: %v", err)
		}
	}
	return nil
}

func (testCtx *testContext) setupService() corev1.Service {
	annotations := map[string]string{
		"service.beta.openshift.io/serving-cert-secret-name": "odh-notebook-controller-webhook-cert",
	}
	selector := map[string]string{
		"apps":                          "odh-notebook-controller",
		"app.kubernetes.io/part-of":     "odh-notebook-controller",
		"component.opendatahub.io/name": "odh-notebook-controller-webhook",
		"kustomize.component":           "odh-notebook-controller-webhook",
		"opendatahub.io/component":      "true",
	}

	return corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        "webhook-service-e2e",
			Annotations: annotations,
			Namespace:   testCtx.testNamespace,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name: "webhook",
					Port: int32(443),
					TargetPort: intstr.IntOrString{
						IntVal: int32(443),
						StrVal: "webhook",
						Type:   intstr.Type(1),
					},
				},
			},
			Selector: selector,
		},
	}
}

func (testCtx *testContext) setupMutatingWebhook() admregv1.MutatingWebhookConfiguration {
	failPolicy := admregv1.FailurePolicyType("Fail")
	sideEffects := admregv1.SideEffectClass("None")
	path := "/mutate-notebook-v1"
	return admregv1.MutatingWebhookConfiguration{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admissionregistration.k8s.io/v1",
			Kind:       "MutatingWebhookConfiguration",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mutating-webhook-configuration",
			Namespace: testCtx.testNamespace,
		},
		Webhooks: []admregv1.MutatingWebhook{
			{
				Name:                    "notebooks.opendatahub.io",
				FailurePolicy:           &failPolicy,
				SideEffects:             &sideEffects,
				AdmissionReviewVersions: []string{"v1"},
				ClientConfig: admregv1.WebhookClientConfig{
					Service: &admregv1.ServiceReference{
						Name:      "webhook-service-e2e",
						Namespace: testCtx.testNamespace,
						Path:      &path,
					},
				},
				Rules: []admregv1.RuleWithOperations{
					{
						Operations: []admregv1.OperationType{
							admregv1.OperationType("CREATE"),
							admregv1.OperationType("UPDATE"),
						},
						Rule: admregv1.Rule{
							APIGroups:   []string{"kubeflow.org"},
							APIVersions: []string{"v1"},
							Resources:   []string{"notebooks"},
						},
					},
				},
			},
		},
	}
}

// apiVersion: admissionregistration.k8s.io/v1
// kind: MutatingWebhookConfiguration
// metadata:
//   creationTimestamp: null
//   name: mutating-webhook-configuration
// webhooks:
// - admissionReviewVersions:
//   - v1
//   clientConfig:
//     service:
//       name: webhook-service
//       namespace: opendatahub
//       path: /mutate-notebook-v1
//   failurePolicy: Fail
//   name: notebooks.opendatahub.io
//   rules:
//   - apiGroups:
//     - kubeflow.org
//     apiVersions:
//     - v1
//     operations:
//     - CREATE
//     - UPDATE
//     resources:
//     - notebooks
//   sideEffects: None
