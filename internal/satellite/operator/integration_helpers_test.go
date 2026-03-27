//go:build integration

package operator

import (
	"context"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
)

var testClient kubernetes.Interface

func TestMain(m *testing.M) {
	home, _ := os.UserHomeDir()
	kubeconfig := home + "/.kube/config"
	if v := os.Getenv("KUBECONFIG"); v != "" {
		kubeconfig = v
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		log.Printf("skipping integration tests: %v", err)
		os.Exit(0)
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Printf("skipping integration tests: %v", err)
		os.Exit(0)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		log.Printf("skipping integration tests: cluster unreachable: %v", err)
		os.Exit(0)
	}

	testClient = client
	os.Exit(m.Run())
}

// createTestNamespace creates a unique namespace and registers cleanup.
func createTestNamespace(t *testing.T) string {
	t.Helper()
	// Use test name hash for short unique suffix
	ns := fmt.Sprintf("pgswarm-test-%x", time.Now().UnixNano()&0xffffffff)
	if len(ns) > 63 {
		ns = ns[:63]
	}

	_, err := testClient.CoreV1().Namespaces().Create(
		context.Background(),
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		},
		metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatalf("create namespace %s: %v", ns, err)
	}
	t.Logf("created namespace %s", ns)

	t.Cleanup(func() { cleanupNamespace(t, ns) })
	return ns
}

// cleanupNamespace deletes a namespace with foreground propagation and waits for it to be gone.
func cleanupNamespace(t *testing.T, ns string) {
	t.Helper()
	propagation := metav1.DeletePropagationForeground
	err := testClient.CoreV1().Namespaces().Delete(
		context.Background(),
		ns,
		metav1.DeleteOptions{PropagationPolicy: &propagation},
	)
	if apierrors.IsNotFound(err) {
		return
	}
	if err != nil {
		t.Logf("warning: delete namespace %s: %v", ns, err)
		return
	}

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		_, err := testClient.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			t.Logf("namespace %s deleted", ns)
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Logf("warning: namespace %s still exists after 60s", ns)
}

// newTestOperator creates an Operator for integration tests.
func newTestOperator(t *testing.T, ns string) *Operator {
	t.Helper()
	return New(testClient, "integ-test", ns, "ghcr.io/pg-swarm/pg-swarm-sentinel:latest")
}

// integrationCfg returns a minimal ClusterConfig for testing.
func integrationCfg(clusterName, ns string) *pgswarmv1.ClusterConfig {
	return &pgswarmv1.ClusterConfig{
		ClusterName:   clusterName,
		Namespace:     ns,
		Replicas:      1,
		ConfigVersion: 1,
		Postgres: &pgswarmv1.PostgresSpec{
			Image: "postgres:17-alpine",
		},
		Storage: &pgswarmv1.StorageSpec{
			Size: "256Mi",
		},
	}
}

// waitForStatefulSetReady polls until the STS has at least 1 ready replica.
func waitForStatefulSetReady(t *testing.T, ns, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sts, err := testClient.AppsV1().StatefulSets(ns).Get(context.Background(), name, metav1.GetOptions{})
		if err == nil && sts.Status.ReadyReplicas >= 1 {
			t.Logf("StatefulSet %s/%s ready (replicas=%d)", ns, name, sts.Status.ReadyReplicas)
			return
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("StatefulSet %s/%s not ready after %v", ns, name, timeout)
}

// assertResourceExists verifies a resource exists in the namespace.
func assertResourceExists(t *testing.T, ns, kind, name string) {
	t.Helper()
	ctx := context.Background()
	var err error
	switch kind {
	case "StatefulSet":
		_, err = testClient.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
	case "Service":
		_, err = testClient.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	case "ConfigMap":
		_, err = testClient.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
	case "Secret":
		_, err = testClient.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	case "CronJob":
		_, err = testClient.BatchV1().CronJobs(ns).Get(ctx, name, metav1.GetOptions{})
	case "Job":
		_, err = testClient.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
	case "ServiceAccount":
		_, err = testClient.CoreV1().ServiceAccounts(ns).Get(ctx, name, metav1.GetOptions{})
	case "Role":
		_, err = testClient.RbacV1().Roles(ns).Get(ctx, name, metav1.GetOptions{})
	case "RoleBinding":
		_, err = testClient.RbacV1().RoleBindings(ns).Get(ctx, name, metav1.GetOptions{})
	case "PersistentVolumeClaim":
		_, err = testClient.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
	default:
		t.Fatalf("assertResourceExists: unsupported kind %q", kind)
	}
	if err != nil {
		t.Errorf("expected %s/%s to exist in %s: %v", kind, name, ns, err)
	}
}

// assertResourceNotExists verifies a resource does NOT exist in the namespace.
func assertResourceNotExists(t *testing.T, ns, kind, name string) {
	t.Helper()
	ctx := context.Background()
	var err error
	switch kind {
	case "StatefulSet":
		_, err = testClient.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
	case "Service":
		_, err = testClient.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	case "ConfigMap":
		_, err = testClient.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
	case "Secret":
		_, err = testClient.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	case "CronJob":
		_, err = testClient.BatchV1().CronJobs(ns).Get(ctx, name, metav1.GetOptions{})
	case "Job":
		_, err = testClient.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
	case "PersistentVolumeClaim":
		_, err = testClient.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
	default:
		t.Fatalf("assertResourceNotExists: unsupported kind %q", kind)
	}
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected %s/%s to NOT exist in %s (err=%v)", kind, name, ns, err)
	}
}

// pollUntilNotFound polls until a resource is gone or timeout.
func pollUntilNotFound(t *testing.T, ns, kind, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx := context.Background()
		var err error
		switch kind {
		case "StatefulSet":
			_, err = testClient.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		case "Service":
			_, err = testClient.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
		case "ConfigMap":
			_, err = testClient.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
		case "Secret":
			_, err = testClient.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		}
		if apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Errorf("%s/%s still exists after %v", kind, name, timeout)
}
