//go:build e2e

// End-to-end chaos test suite for pg-swarm.
//
// This suite is fully self-contained: it deploys central + satellite into
// minikube, runs chaos tests, and tears everything down on exit.
//
// Prerequisites:
//   - minikube running with images loaded (make minikube-build-all)
//
// Run:
//   make test-e2e
//   # or: go test -tags e2e -timeout 30m -v ./test/e2e/
//
// Environment variables:
//   E2E_KEEP_RESOURCES=true  — skip teardown on exit (for debugging)
//   E2E_PG_PASSWORD          — superuser password for psql (from cluster secret)

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	systemNS    = "pgswarm-system"
	clusterNS   = "e2e-test"
	clusterName = "e2e-chaos"
	profileName = "dev"
	httpPort    = "38080"
	grpcPort    = "39090"
	pgPort      = "35432"
	testDBName  = "e2e_testdb"
	testDBUser  = "e2e_user"
	testDBPass  = "e2e_secret_123"

	centralKustomize   = "../../deploy/k8s/central/overlays/minikube"
	satelliteKustomize = "../../deploy/k8s/satellite/overlays/minikube"
)

// E2ESuite is the top-level test suite. Tests run in declaration order.
type E2ESuite struct {
	suite.Suite
	api         *CentralClient
	k8s         *K8sHelper
	k8sClient   kubernetes.Interface
	pg          *PGClient
	cancelPF    context.CancelFunc
	satelliteID string
	clusterID   string
	deployed    bool
	rowsBefore  int

	// lastBackupPath is set by Test_50 and used by Test_52 to restore.
	lastBackupPath string
}

// SetupTest runs before every individual test and inserts a 60-second delay
// so the cluster has time to stabilise between tests.
func (s *E2ESuite) SetupTest() {
	name := s.T().Name()
	// Skip the delay for the very first test and for setup-phase tests.
	if name == "TestE2E/Test_01_WaitForSatelliteRegistration" {
		return
	}
	s.T().Log("inter-test stabilisation delay: 60s")
	time.Sleep(60 * time.Second)
}

func TestE2E(t *testing.T) {
	s := new(E2ESuite)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n\n--- INTERRUPTED ---")
		if os.Getenv("E2E_KEEP_RESOURCES") == "true" {
			fmt.Println("E2E_KEEP_RESOURCES=true — leaving resources in cluster.")
			fmt.Printf("  kubectl delete -k %s\n", satelliteKustomize)
			fmt.Printf("  kubectl delete -k %s\n", centralKustomize)
			fmt.Printf("  kubectl delete namespace %s\n", clusterNS)
		} else {
			fmt.Println("Cleaning up deployed resources...")
			s.cleanup()
		}
		os.Exit(1)
	}()

	suite.Run(t, s)
}

// ---------- Setup & Teardown ----------

func (s *E2ESuite) SetupSuite() {
	home, _ := os.UserHomeDir()
	kubeconfig := home + "/.kube/config"
	if v := os.Getenv("KUBECONFIG"); v != "" {
		kubeconfig = v
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	s.Require().NoError(err, "cannot load kubeconfig")

	client, err := kubernetes.NewForConfig(cfg)
	s.Require().NoError(err, "cannot create k8s client")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{Limit: 1})
	s.Require().NoError(err, "cluster unreachable — is minikube running?")

	s.k8sClient = client
	s.k8s = &K8sHelper{Client: client, Namespace: clusterNS}

	// Deploy stack
	s.T().Log("deploying central (postgres + central server)...")
	out, err := s.k8s.Kubectl("apply", "-k", centralKustomize)
	s.Require().NoError(err, "failed to deploy central:\n%s", out)
	s.T().Log("deploying satellite...")
	out, err = s.k8s.Kubectl("apply", "-k", satelliteKustomize)
	s.Require().NoError(err, "failed to deploy satellite:\n%s", out)
	s.deployed = true

	s.T().Log("waiting for central pod to be ready...")
	s.Require().NoError(s.waitForDeployment(client, systemNS, "pg-swarm-central", 3*time.Minute))
	s.T().Log("waiting for satellite pod to be ready...")
	s.Require().NoError(s.waitForDeployment(client, systemNS, "pg-swarm-satellite", 2*time.Minute))

	// Create test namespace
	_, err = client.CoreV1().Namespaces().Get(context.Background(), clusterNS, metav1.GetOptions{})
	if err != nil {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: clusterNS}}
		_, err = client.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
		s.Require().NoError(err, "failed to create namespace %s", clusterNS)
		s.T().Logf("created namespace: %s", clusterNS)
	}

	// Port-forward to central
	pfCtx, pfCancel := context.WithCancel(context.Background())
	s.cancelPF = pfCancel
	s.Require().NoError(StartPortForward(pfCtx, client, systemNS, httpPort, grpcPort))

	s.api = NewCentralClient("http://localhost:" + httpPort)
	s.Require().NoError(WaitFor(30*time.Second, "central API ready", s.api.IsReady))
	s.T().Log("setup complete — central API is reachable")
}

func (s *E2ESuite) TearDownSuite() {
	if os.Getenv("E2E_KEEP_RESOURCES") == "true" {
		fmt.Println("E2E_KEEP_RESOURCES=true — skipping teardown")
		if s.cancelPF != nil {
			s.cancelPF()
		}
		return
	}
	s.cleanup()
}

func (s *E2ESuite) cleanup() {
	if s.pg != nil {
		s.pg.Close()
		s.pg = nil
	}
	if s.cancelPF != nil {
		s.cancelPF()
		s.cancelPF = nil
	}
	if s.api != nil && s.clusterID != "" {
		fmt.Printf("cleaning up: deleting cluster %s...\n", clusterName)
		_ = s.api.DeleteCluster(s.clusterID)
		_ = WaitFor(60*time.Second, "cluster deletion", func() bool {
			clusters, err := s.api.ListClusters()
			if err != nil {
				return true
			}
			for _, c := range clusters {
				if c.ID == s.clusterID {
					return false
				}
			}
			return true
		})
	}
	if s.k8sClient != nil {
		fmt.Printf("cleaning up: deleting namespace %s...\n", clusterNS)
		propagation := metav1.DeletePropagationForeground
		_ = s.k8sClient.CoreV1().Namespaces().Delete(
			context.Background(), clusterNS,
			metav1.DeleteOptions{PropagationPolicy: &propagation},
		)
	}
	if s.deployed {
		fmt.Println("cleaning up: removing satellite...")
		s.k8s.Kubectl("delete", "-k", satelliteKustomize, "--ignore-not-found")
		fmt.Println("cleaning up: removing central...")
		s.k8s.Kubectl("delete", "-k", centralKustomize, "--ignore-not-found")
		s.deployed = false
	}
	fmt.Println("cleanup complete")
}

// ---------- Shared helpers (used by test files) ----------

func (s *E2ESuite) waitForDeployment(client kubernetes.Interface, ns, name string, timeout time.Duration) error {
	return WaitFor(timeout, fmt.Sprintf("deployment %s/%s ready", ns, name), func() bool {
		dep, err := client.AppsV1().Deployments(ns).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return dep.Status.ReadyReplicas >= 1
	})
}

func (s *E2ESuite) getHealth() *ClusterHealth {
	all, err := s.api.ListHealth()
	if err != nil {
		return nil
	}
	for _, h := range all {
		if h.ClusterName == clusterName {
			return &h
		}
	}
	return nil
}

func (s *E2ESuite) logHealth() {
	if h := s.getHealth(); h != nil {
		s.T().Logf("health: %s", HealthSummary(*h))
	}
}

func (s *E2ESuite) logEvents(limit int) {
	events, err := s.api.ListEvents(limit)
	if err != nil || len(events) == 0 {
		return
	}
	s.T().Logf("recent events:\n%s", FormatEvents(events, limit))
}

func (s *E2ESuite) assertNoPrimaryDuplicates() {
	count, primaries := s.k8s.CountPrimaries(clusterName)
	s.Assert().Equal(1, count, "expected exactly 1 primary, got %d: %v", count, primaries)
}

// monitorLabels polls pod labels at 1s intervals until stop is closed or
// maxDuration elapses. Returns the history for split-brain assertions.
func (s *E2ESuite) monitorLabels(stop <-chan struct{}, maxDuration time.Duration) *PodLabelHistory {
	history := &PodLabelHistory{}
	deadline := time.Now().Add(maxDuration)
	for time.Now().Before(deadline) {
		select {
		case <-stop:
			return history
		default:
		}
		pods, err := s.k8s.GetClusterPods(clusterName)
		if err == nil {
			history.Observe(pods)
		}
		time.Sleep(1 * time.Second)
	}
	return history
}

// startLabelMonitor starts monitoring in a goroutine and returns a function
// to stop it and get the results.
func (s *E2ESuite) startLabelMonitor(maxDuration time.Duration) (stop func() *PodLabelHistory) {
	stopCh := make(chan struct{})
	done := make(chan *PodLabelHistory, 1)
	go func() {
		done <- s.monitorLabels(stopCh, maxDuration)
	}()
	return func() *PodLabelHistory {
		close(stopCh)
		return <-done
	}
}
