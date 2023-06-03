package e2e

import (
	"context"
	"flag"
	"fmt"
	netv1 "k8s.io/api/networking/v1"
	"os"
	"testing"
	"time"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	routev1 "github.com/openshift/api/route/v1"
	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	k8sclient "k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlruntime "sigs.k8s.io/controller-runtime/pkg/client/config"
)

var (
	notebookTestNamespace string
	skipDeletion          bool
	deploymentMode        DeploymentMode
	scheme                = runtime.NewScheme()
)

// Holds information specific to individual tests
type testContext struct {
	// Rest config
	cfg *rest.Config
	// client for k8s resources
	kubeClient *k8sclient.Clientset
	// custom client for managing cutom resources
	customClient client.Client
	// namespace for running the tests
	testNamespace string
	// time required to create a resource
	resourceCreationTimeout time.Duration
	// time interval to check for resource creation
	resourceRetryInterval time.Duration
	// test Notebook for e2e
	testNotebooks []notebookContext
	// context for accessing resources
	ctx context.Context
}

// DeploymentMode indicates what infra scenarios should be verified by the test
// with default being OAuthProxy scenario.
type DeploymentMode int

const (
	OAuthProxy DeploymentMode = iota
	ServiceMesh
)

var modes = [...]string{"oauth", "service-mesh"}

// Implementing flag.Value funcs, so we can use DeploymentMode as a CLI flag.
func (d *DeploymentMode) String() string {
	return modes[*d]
}

func (d *DeploymentMode) Set(s string) error {
	for i := range modes {
		if modes[i] == s {
			*d = DeploymentMode(i)
			return nil
		}
	}

	return errors.Errorf("Unknown deployment mode %s. Try any of these %v", s, modes)
}

// notebookContext holds information about test notebook
// Any notebook that needs to be added to the e2e test suite should be defined in
// the notebookContext struct.
type notebookContext struct {
	// metadata for Notebook object
	nbObjectMeta *metav1.ObjectMeta
	// metadata for Notebook Spec
	nbSpec         *nbv1.NotebookSpec
	deploymentMode DeploymentMode
}

func NewTestContext() (*testContext, error) {

	// GetConfig(): If KUBECONFIG env variable is set, it is used to create
	// the client, else the inClusterConfig() is used.
	// Lastly if none of them are set, it uses  $HOME/.kube/config to create the client.
	config, err := ctrlruntime.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("error creating the config object %v", err)
	}

	kc, err := k8sclient.NewForConfig(config)
	if err != nil {
		return nil, errors.Wrap(err, "failed to initialize Kubernetes client")
	}

	// custom client to manages resources like Notebook, Route etc
	custClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, errors.Wrap(err, "failed to initialize custom client")
	}

	// Setup all test Notebooks
	testNotebooksContextList := []notebookContext{setupThothMinimalOAuthNotebook(), setupThothMinimalServiceMeshNotebook()}

	return &testContext{
		cfg:                     config,
		kubeClient:              kc,
		customClient:            custClient,
		testNamespace:           notebookTestNamespace,
		resourceCreationTimeout: time.Minute * 1,
		resourceRetryInterval:   time.Second * 10,
		ctx:                     context.TODO(),
		testNotebooks:           testNotebooksContextList,
	}, nil
}

// TestE2ENotebookController sets up the testing suite for KFNBC.
func TestE2ENotebookController(t *testing.T) {

	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(nbv1.AddToScheme(scheme))
	utilruntime.Must(routev1.Install(scheme))
	utilruntime.Must(netv1.AddToScheme(scheme))

	// individual test suites after the operator is running
	if !t.Run("validate controllers", testNotebookControllerValidation) {
		return
	}
	// Run create and delete tests for all the test notebooks
	t.Run("create", creationTestSuite)
	if !skipDeletion {
		t.Run("delete", deletionTestSuite)
	}
}

func TestMain(m *testing.M) {
	flag.StringVar(&notebookTestNamespace, "nb-namespace",
		"e2e-notebook-controller", "Custom namespace where the notebook controllers are deployed")
	flag.BoolVar(&skipDeletion, "skip-deletion", false, "skip deletion of the controllers")
	flag.Var(&deploymentMode, "deploymentMode", "sets deployment mode")
	flag.Parse()

	os.Exit(m.Run())
}
