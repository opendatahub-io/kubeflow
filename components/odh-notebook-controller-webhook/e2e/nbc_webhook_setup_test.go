package e2e

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlruntime "sigs.k8s.io/controller-runtime/pkg/client/config"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

var (
	webhookTestNamespace string
	skipDeletion         bool
	scheme               = runtime.NewScheme()
)

type testContext struct {
	webhookDeployment appsv1.Deployment
	cfg               *rest.Config
	kubeClient        *k8sclient.Clientset
	customClient      client.Client
	testNamespace     string
	// time rquired to create a resource
	resourceCreationTimeout time.Duration
	// time interval to check for resource creation
	resourceRetryInterval time.Duration
	ctx                   context.Context
}

func NewTestContext() (*testContext, error) {

	// GetConfig(): If KUBECONFIG env variable is set, it is used to create
	// the client, else the inClusterConfig() is used.
	// Lastly if none of the them are set, it uses  $HOME/.kube/config to create the client.
	config, err := ctrlruntime.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("error creating the config object %v", err)
	}

	kc, err := k8sclient.NewForConfig(config)
	if err != nil {
		return nil, errors.Wrap(err, "failed to initialize Kubernetes client")
	}

	// custom client to manages resources like KfDef, Route etc
	custClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, errors.Wrap(err, "failed to initialize custom client")
	}

	return &testContext{
		webhookDeployment:       setupDeployment(webhookTestNamespace),
		cfg:                     config,
		kubeClient:              kc,
		customClient:            custClient,
		testNamespace:           webhookTestNamespace,
		resourceCreationTimeout: time.Minute * 1,
		resourceRetryInterval:   time.Second * 10,
		ctx:                     context.TODO(),
	}, nil
}

func TestE2EWebhook(t *testing.T) {

	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(appsv1.AddToScheme(scheme))

	t.Run("create", creationTestSuite)
	if !t.Run("validate webhook", validationTestSuite) {
		return
	}
	if !skipDeletion {
		t.Run("delete", deletionTestSuite)
	}
}

func TestMain(m *testing.M) {
	// call flag.Parse() here if TestMain uses flags
	flag.StringVar(&webhookTestNamespace, "namespace",
		"opendatahub", "Custom namespace where the webhook should be deployed")
	flag.Parse()
	os.Exit(m.Run())
}

func setupDeployment(testNamespace string) appsv1.Deployment {

	// Helper variables
	labels := map[string]string{
		"apps":                          "odh-notebook-controller",
		"app.kubernetes.io/part-of":     "odh-notebook-controller",
		"component.opendatahub.io/name": "odh-notebook-controller-webhook",
		"kustomize.component":           "odh-notebook-controller-webhook",
		"opendatahub.io/component":      "true",
	}
	replicas := int32(1)
	runAsNonRoot := true
	defaultMode := int32(420)

	return appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Deployment",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "webhook-e2e",
			Namespace: testNamespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "webhook",
							SecurityContext: &corev1.SecurityContext{
								RunAsNonRoot: &runAsNonRoot,
							},
							Image: "quay.io/mroman_redhat/odh-notebook-controller-webhook:dev-3", // TODO:Change to "quay.io/opendatahub/odh-notebook-controller-webhook:latest"
							Ports: []corev1.ContainerPort{
								{
									Name:          "webhook",
									ContainerPort: 8443,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "webhook-cert",
									MountPath: "/tmp/k8s-webhook-server/serving-certs",
									ReadOnly:  true,
								},
							},
							Command: []string{"/webhook"},
							Args:    []string{"--oauth-proxy-image", "registry.redhat.io/openshift4/ose-oauth-proxy:v4.10"},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "webhook-cert",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName:  "odh-notebook-controller-webhook-cert",
									DefaultMode: &defaultMode,
								},
							},
						},
					},
				},
			},
		},
	}
}
