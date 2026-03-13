//go:build integration

package operator

import (
	"context"
	"fmt"
	"testing"
	"time"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// testClient builds a real K8s client from the default kubeconfig (minikube).
func testClient(t *testing.T) kubernetes.Interface {
	t.Helper()
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, nil).ClientConfig()
	if err != nil {
		t.Fatalf("failed to load kubeconfig: %v", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("failed to create K8s client: %v", err)
	}
	// Quick connectivity check
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.CoreV1().Namespaces().Get(ctx, "default", metav1.GetOptions{}); err != nil {
		t.Fatalf("cannot reach K8s cluster (is minikube running?): %v", err)
	}
	return client
}

// testNamespace returns a unique namespace name scoped to a test.
func testNamespace(t *testing.T) string {
	t.Helper()
	// Use test name, sanitized for K8s (lowercase, alphanumeric + dashes)
	name := fmt.Sprintf("pgswarm-test-%d", time.Now().UnixNano()%100000)
	return name
}

// cleanupNamespace deletes a namespace and all its resources.
func cleanupNamespace(t *testing.T, client kubernetes.Interface, ns string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	propagation := metav1.DeletePropagationForeground
	err := client.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Logf("warning: failed to delete namespace %s: %v", ns, err)
	}
}

// waitForStatefulSet waits until the StatefulSet exists (not necessarily ready).
func waitForStatefulSet(ctx context.Context, client kubernetes.Interface, ns, name string) (*appsv1.StatefulSet, error) {
	for {
		sts, err := client.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			return sts, nil
		}
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timed out waiting for statefulset %s/%s", ns, name)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// integrationCfg returns a minimal ClusterConfig suitable for integration tests.
// Uses small resources to work on minikube.
func integrationCfg(clusterName, namespace string) *pgswarmv1.ClusterConfig {
	return &pgswarmv1.ClusterConfig{
		ClusterName:   clusterName,
		Namespace:     namespace,
		Replicas:      1,
		ConfigVersion: 1,
		Postgres: &pgswarmv1.PostgresSpec{
			Version: "17",
			Image:   "postgres:17-alpine",
		},
		Storage: &pgswarmv1.StorageSpec{
			Size: "256Mi",
		},
		Resources: &pgswarmv1.ResourceSpec{
			CpuRequest:    "50m",
			CpuLimit:      "200m",
			MemoryRequest: "64Mi",
			MemoryLimit:   "256Mi",
		},
		PgParams: map[string]string{
			"shared_buffers": "32MB",
		},
	}
}

// resourceExists checks if a resource exists without returning the object.
func resourceExists(ctx context.Context, client kubernetes.Interface, ns, kind, name string) (bool, error) {
	var err error
	switch kind {
	case "configmap":
		_, err = client.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
	case "secret":
		_, err = client.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	case "service":
		_, err = client.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	case "statefulset":
		_, err = client.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
	case "serviceaccount":
		_, err = client.CoreV1().ServiceAccounts(ns).Get(ctx, name, metav1.GetOptions{})
	case "role":
		_, err = client.RbacV1().Roles(ns).Get(ctx, name, metav1.GetOptions{})
	case "rolebinding":
		_, err = client.RbacV1().RoleBindings(ns).Get(ctx, name, metav1.GetOptions{})
	case "lease":
		_, err = client.CoordinationV1().Leases(ns).Get(ctx, name, metav1.GetOptions{})
	default:
		return false, fmt.Errorf("unknown kind %q", kind)
	}
	if err == nil {
		return true, nil
	}
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return false, err
}

// mustExist asserts that a resource exists in the cluster.
func mustExist(t *testing.T, ctx context.Context, client kubernetes.Interface, ns, kind, name string) {
	t.Helper()
	exists, err := resourceExists(ctx, client, ns, kind, name)
	if err != nil {
		t.Fatalf("failed to check %s/%s: %v", kind, name, err)
	}
	if !exists {
		t.Errorf("expected %s/%s to exist in namespace %s", kind, name, ns)
	}
}

// mustNotExist asserts that a resource does not exist in the cluster.
func mustNotExist(t *testing.T, ctx context.Context, client kubernetes.Interface, ns, kind, name string) {
	t.Helper()
	exists, err := resourceExists(ctx, client, ns, kind, name)
	if err != nil {
		t.Fatalf("failed to check %s/%s: %v", kind, name, err)
	}
	if exists {
		t.Errorf("expected %s/%s to NOT exist in namespace %s", kind, name, ns)
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestIntegration_HandleConfig_CreatesAllResources verifies that HandleConfig
// creates a namespace, secret, configmap, services, and statefulset on a real cluster.
func TestIntegration_HandleConfig_CreatesAllResources(t *testing.T) {
	client := testClient(t)
	ns := testNamespace(t)
	defer cleanupNamespace(t, client, ns)

	op := New(client, "minikube", ns)
	cfg := integrationCfg("test-pg", ns)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := op.HandleConfig(cfg); err != nil {
		t.Fatalf("HandleConfig failed: %v", err)
	}

	// Verify all expected resources exist
	mustExist(t, ctx, client, ns, "secret", "test-pg-secret")
	mustExist(t, ctx, client, ns, "configmap", "test-pg-config")
	mustExist(t, ctx, client, ns, "configmap", "pg-swarm-minikube-test-pg") // config-store
	mustExist(t, ctx, client, ns, "service", "test-pg-headless")
	mustExist(t, ctx, client, ns, "service", "test-pg-rw")
	mustExist(t, ctx, client, ns, "service", "test-pg-ro")
	mustExist(t, ctx, client, ns, "statefulset", "test-pg")

	// Verify namespace was created with correct labels
	nsObj, err := client.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get namespace: %v", err)
	}
	if nsObj.Labels[LabelManagedBy] != ManagedByValue {
		t.Errorf("namespace missing %s=%s label", LabelManagedBy, ManagedByValue)
	}
}

// TestIntegration_HandleConfig_StatefulSetSpec verifies the created StatefulSet
// has the correct spec (replicas, image, volumes, probes).
func TestIntegration_HandleConfig_StatefulSetSpec(t *testing.T) {
	client := testClient(t)
	ns := testNamespace(t)
	defer cleanupNamespace(t, client, ns)

	op := New(client, "minikube", ns)
	cfg := integrationCfg("spec-pg", ns)
	cfg.Replicas = 2

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := op.HandleConfig(cfg); err != nil {
		t.Fatalf("HandleConfig failed: %v", err)
	}

	sts, err := client.AppsV1().StatefulSets(ns).Get(ctx, "spec-pg", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get statefulset: %v", err)
	}

	// Replicas
	if *sts.Spec.Replicas != int32(2) {
		t.Errorf("replicas = %d, want 2", *sts.Spec.Replicas)
	}

	// Service name
	if sts.Spec.ServiceName != "spec-pg-headless" {
		t.Errorf("serviceName = %q, want spec-pg-headless", sts.Spec.ServiceName)
	}

	// Container image
	main := sts.Spec.Template.Spec.Containers[0]
	if main.Image != "postgres:17-alpine" {
		t.Errorf("image = %q, want postgres:17-alpine", main.Image)
	}

	// Liveness probe
	if main.LivenessProbe == nil {
		t.Error("missing liveness probe")
	}

	// Readiness probe
	if main.ReadinessProbe == nil {
		t.Error("missing readiness probe")
	}

	// Volume claim templates
	if len(sts.Spec.VolumeClaimTemplates) != 1 {
		t.Errorf("VCT count = %d, want 1", len(sts.Spec.VolumeClaimTemplates))
	}
	dataVCT := sts.Spec.VolumeClaimTemplates[0]
	if dataVCT.Name != "data" {
		t.Errorf("VCT name = %q, want data", dataVCT.Name)
	}
	size := dataVCT.Spec.Resources.Requests[corev1.ResourceStorage]
	if size.String() != "256Mi" {
		t.Errorf("VCT size = %s, want 256Mi", size.String())
	}

	// Labels on StatefulSet
	if sts.Labels[LabelCluster] != "spec-pg" {
		t.Errorf("statefulset missing cluster label")
	}
	if sts.Labels[LabelManagedBy] != ManagedByValue {
		t.Errorf("statefulset missing managed-by label")
	}
}

// TestIntegration_HandleConfig_ConfigMapContent verifies the ConfigMap contains
// correct postgresql.conf and pg_hba.conf.
func TestIntegration_HandleConfig_ConfigMapContent(t *testing.T) {
	client := testClient(t)
	ns := testNamespace(t)
	defer cleanupNamespace(t, client, ns)

	op := New(client, "minikube", ns)
	cfg := integrationCfg("cm-pg", ns)
	cfg.HbaRules = []string{"host all testuser 10.0.0.0/8 md5"}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := op.HandleConfig(cfg); err != nil {
		t.Fatalf("HandleConfig failed: %v", err)
	}

	cm, err := client.CoreV1().ConfigMaps(ns).Get(ctx, "cm-pg-config", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get configmap: %v", err)
	}

	pgConf := cm.Data["postgresql.conf"]
	hbaConf := cm.Data["pg_hba.conf"]

	if pgConf == "" {
		t.Fatal("postgresql.conf is empty")
	}
	if hbaConf == "" {
		t.Fatal("pg_hba.conf is empty")
	}

	// Mandatory HA params
	for _, param := range []string{"wal_level = replica", "hot_standby = on", "max_wal_senders = 10"} {
		if !contains(pgConf, param) {
			t.Errorf("postgresql.conf missing mandatory param %q", param)
		}
	}

	// User param
	if !contains(pgConf, "shared_buffers = 32MB") {
		t.Error("postgresql.conf missing user param shared_buffers = 32MB")
	}

	// HBA rules
	if !contains(hbaConf, "host replication repl_user 0.0.0.0/0 md5") {
		t.Error("pg_hba.conf missing mandatory replication rule")
	}
	if !contains(hbaConf, "host all testuser 10.0.0.0/8 md5") {
		t.Error("pg_hba.conf missing user-defined rule")
	}
}

// TestIntegration_HandleConfig_ServiceSelectors verifies that RW, RO, and headless
// services have correct selectors on a real cluster.
func TestIntegration_HandleConfig_ServiceSelectors(t *testing.T) {
	client := testClient(t)
	ns := testNamespace(t)
	defer cleanupNamespace(t, client, ns)

	op := New(client, "minikube", ns)
	cfg := integrationCfg("svc-pg", ns)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := op.HandleConfig(cfg); err != nil {
		t.Fatalf("HandleConfig failed: %v", err)
	}

	// Headless service
	hl, err := client.CoreV1().Services(ns).Get(ctx, "svc-pg-headless", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get headless svc: %v", err)
	}
	if hl.Spec.ClusterIP != corev1.ClusterIPNone {
		t.Errorf("headless ClusterIP = %q, want None", hl.Spec.ClusterIP)
	}
	if hl.Spec.Selector[LabelCluster] != "svc-pg" {
		t.Errorf("headless service missing cluster selector")
	}

	// RW service
	rw, err := client.CoreV1().Services(ns).Get(ctx, "svc-pg-rw", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get rw svc: %v", err)
	}
	if rw.Spec.Selector[LabelRole] != RolePrimary {
		t.Errorf("rw service role selector = %q, want %s", rw.Spec.Selector[LabelRole], RolePrimary)
	}

	// RO service
	ro, err := client.CoreV1().Services(ns).Get(ctx, "svc-pg-ro", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ro svc: %v", err)
	}
	if ro.Spec.Selector[LabelRole] != RoleReplica {
		t.Errorf("ro service role selector = %q, want %s", ro.Spec.Selector[LabelRole], RoleReplica)
	}
}

// TestIntegration_HandleConfig_SecretPreserved verifies that the secret is created
// once and not overwritten on subsequent HandleConfig calls.
func TestIntegration_HandleConfig_SecretPreserved(t *testing.T) {
	client := testClient(t)
	ns := testNamespace(t)
	defer cleanupNamespace(t, client, ns)

	op := New(client, "minikube", ns)
	cfg := integrationCfg("secret-pg", ns)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// First apply
	if err := op.HandleConfig(cfg); err != nil {
		t.Fatalf("HandleConfig v1 failed: %v", err)
	}

	// Read the secret
	secret1, err := client.CoreV1().Secrets(ns).Get(ctx, "secret-pg-secret", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	password1 := string(secret1.Data["superuser-password"])

	// Second apply with bumped version
	cfg.ConfigVersion = 2
	if err := op.HandleConfig(cfg); err != nil {
		t.Fatalf("HandleConfig v2 failed: %v", err)
	}

	// Secret should retain original password
	secret2, err := client.CoreV1().Secrets(ns).Get(ctx, "secret-pg-secret", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret after v2: %v", err)
	}
	password2 := string(secret2.Data["superuser-password"])

	if password1 != password2 {
		t.Errorf("secret was overwritten: password changed from %q to %q", password1, password2)
	}
}

// TestIntegration_HandleConfig_Idempotent verifies that calling HandleConfig
// with the same config version is a no-op.
func TestIntegration_HandleConfig_Idempotent(t *testing.T) {
	client := testClient(t)
	ns := testNamespace(t)
	defer cleanupNamespace(t, client, ns)

	op := New(client, "minikube", ns)
	cfg := integrationCfg("idem-pg", ns)

	// First call
	if err := op.HandleConfig(cfg); err != nil {
		t.Fatalf("HandleConfig first call failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Record the StatefulSet resourceVersion
	sts1, err := client.AppsV1().StatefulSets(ns).Get(ctx, "idem-pg", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get sts: %v", err)
	}
	rv1 := sts1.ResourceVersion

	// Second call with same version — should skip
	if err := op.HandleConfig(cfg); err != nil {
		t.Fatalf("HandleConfig second call failed: %v", err)
	}

	// StatefulSet should not be updated (same resourceVersion)
	sts2, err := client.AppsV1().StatefulSets(ns).Get(ctx, "idem-pg", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get sts after second call: %v", err)
	}
	if sts2.ResourceVersion != rv1 {
		t.Errorf("statefulset was updated on duplicate version (rv %s → %s)", rv1, sts2.ResourceVersion)
	}
}

// TestIntegration_HandleConfig_UpdateBumpsResources verifies that HandleConfig
// with a new version updates the StatefulSet pod template.
func TestIntegration_HandleConfig_UpdateBumpsResources(t *testing.T) {
	client := testClient(t)
	ns := testNamespace(t)
	defer cleanupNamespace(t, client, ns)

	op := New(client, "minikube", ns)
	cfg := integrationCfg("upd-pg", ns)

	if err := op.HandleConfig(cfg); err != nil {
		t.Fatalf("HandleConfig v1 failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Bump version and change replicas + memory
	cfg.ConfigVersion = 2
	cfg.Replicas = 3
	cfg.Resources.MemoryLimit = "512Mi"

	if err := op.HandleConfig(cfg); err != nil {
		t.Fatalf("HandleConfig v2 failed: %v", err)
	}

	sts, err := client.AppsV1().StatefulSets(ns).Get(ctx, "upd-pg", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get sts: %v", err)
	}

	if *sts.Spec.Replicas != int32(3) {
		t.Errorf("replicas = %d, want 3", *sts.Spec.Replicas)
	}

	memLimit := sts.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
	if memLimit.String() != "512Mi" {
		t.Errorf("memory limit = %s, want 512Mi", memLimit.String())
	}
}

// TestIntegration_HandleConfig_ConfigMapUpdated verifies that postgresql.conf
// is updated when config version is bumped.
func TestIntegration_HandleConfig_ConfigMapUpdated(t *testing.T) {
	client := testClient(t)
	ns := testNamespace(t)
	defer cleanupNamespace(t, client, ns)

	op := New(client, "minikube", ns)
	cfg := integrationCfg("cmu-pg", ns)

	if err := op.HandleConfig(cfg); err != nil {
		t.Fatalf("HandleConfig v1 failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cm1, err := client.CoreV1().ConfigMaps(ns).Get(ctx, "cmu-pg-config", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap v1: %v", err)
	}
	if contains(cm1.Data["postgresql.conf"], "work_mem") {
		t.Fatal("v1 should not have work_mem")
	}

	// v2: add work_mem
	cfg.ConfigVersion = 2
	cfg.PgParams["work_mem"] = "8MB"

	if err := op.HandleConfig(cfg); err != nil {
		t.Fatalf("HandleConfig v2 failed: %v", err)
	}

	cm2, err := client.CoreV1().ConfigMaps(ns).Get(ctx, "cmu-pg-config", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap v2: %v", err)
	}
	if !contains(cm2.Data["postgresql.conf"], "work_mem = 8MB") {
		t.Error("v2 configmap missing work_mem = 8MB")
	}
}

// TestIntegration_HandleDelete_RemovesAllResources verifies that HandleDelete
// cleans up all resources created by HandleConfig.
func TestIntegration_HandleDelete_RemovesAllResources(t *testing.T) {
	client := testClient(t)
	ns := testNamespace(t)
	defer cleanupNamespace(t, client, ns)

	op := New(client, "minikube", ns)
	cfg := integrationCfg("del-pg", ns)

	if err := op.HandleConfig(cfg); err != nil {
		t.Fatalf("HandleConfig failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Verify resources exist first
	mustExist(t, ctx, client, ns, "statefulset", "del-pg")
	mustExist(t, ctx, client, ns, "secret", "del-pg-secret")

	// Delete
	delMsg := &pgswarmv1.DeleteCluster{
		ClusterName: "del-pg",
		Namespace:   ns,
	}
	if err := op.HandleDelete(delMsg); err != nil {
		t.Fatalf("HandleDelete failed: %v", err)
	}

	// Wait briefly for deletion to propagate
	time.Sleep(2 * time.Second)

	mustNotExist(t, ctx, client, ns, "statefulset", "del-pg")
	mustNotExist(t, ctx, client, ns, "service", "del-pg-headless")
	mustNotExist(t, ctx, client, ns, "service", "del-pg-rw")
	mustNotExist(t, ctx, client, ns, "service", "del-pg-ro")
	mustNotExist(t, ctx, client, ns, "configmap", "del-pg-config")
	mustNotExist(t, ctx, client, ns, "secret", "del-pg-secret")
	mustNotExist(t, ctx, client, ns, "configmap", "pg-swarm-minikube-del-pg")
}

// TestIntegration_HandleConfig_WithFailoverRBAC verifies that enabling failover
// creates the ServiceAccount, Role, and RoleBinding.
func TestIntegration_HandleConfig_WithFailoverRBAC(t *testing.T) {
	client := testClient(t)
	ns := testNamespace(t)
	defer cleanupNamespace(t, client, ns)

	op := New(client, "minikube", ns)
	cfg := integrationCfg("fo-pg", ns)
	cfg.Failover = &pgswarmv1.FailoverSpec{
		Enabled:                    true,
		HealthCheckIntervalSeconds: 5,
		SidecarImage:               "postgres:17-alpine", // just needs a valid image
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := op.HandleConfig(cfg); err != nil {
		t.Fatalf("HandleConfig failed: %v", err)
	}

	// Verify RBAC resources
	mustExist(t, ctx, client, ns, "serviceaccount", "fo-pg-failover")
	mustExist(t, ctx, client, ns, "role", "fo-pg-failover")
	mustExist(t, ctx, client, ns, "rolebinding", "fo-pg-failover")

	// Verify StatefulSet has 2 containers
	sts, err := client.AppsV1().StatefulSets(ns).Get(ctx, "fo-pg", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get sts: %v", err)
	}
	if len(sts.Spec.Template.Spec.Containers) != 2 {
		t.Errorf("container count = %d, want 2 (postgres + failover sidecar)", len(sts.Spec.Template.Spec.Containers))
	}
	if sts.Spec.Template.Spec.ServiceAccountName != "fo-pg-failover" {
		t.Errorf("serviceAccountName = %q, want fo-pg-failover", sts.Spec.Template.Spec.ServiceAccountName)
	}
}

// TestIntegration_HandleDelete_WithFailoverRBAC verifies that HandleDelete
// cleans up failover RBAC resources.
func TestIntegration_HandleDelete_WithFailoverRBAC(t *testing.T) {
	client := testClient(t)
	ns := testNamespace(t)
	defer cleanupNamespace(t, client, ns)

	op := New(client, "minikube", ns)
	cfg := integrationCfg("fo-del-pg", ns)
	cfg.Failover = &pgswarmv1.FailoverSpec{
		Enabled:      true,
		SidecarImage: "postgres:17-alpine",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := op.HandleConfig(cfg); err != nil {
		t.Fatalf("HandleConfig failed: %v", err)
	}

	mustExist(t, ctx, client, ns, "serviceaccount", "fo-del-pg-failover")

	delMsg := &pgswarmv1.DeleteCluster{
		ClusterName: "fo-del-pg",
		Namespace:   ns,
	}
	if err := op.HandleDelete(delMsg); err != nil {
		t.Fatalf("HandleDelete failed: %v", err)
	}

	time.Sleep(2 * time.Second)

	mustNotExist(t, ctx, client, ns, "serviceaccount", "fo-del-pg-failover")
	mustNotExist(t, ctx, client, ns, "role", "fo-del-pg-failover")
	mustNotExist(t, ctx, client, ns, "rolebinding", "fo-del-pg-failover")
}

// TestIntegration_HandleConfig_ConfigStoreRedactsPasswords verifies the
// config-store ConfigMap contains redacted passwords.
func TestIntegration_HandleConfig_ConfigStoreRedactsPasswords(t *testing.T) {
	client := testClient(t)
	ns := testNamespace(t)
	defer cleanupNamespace(t, client, ns)

	op := New(client, "minikube", ns)
	cfg := integrationCfg("store-pg", ns)
	cfg.Databases = []*pgswarmv1.DatabaseSpec{
		{Name: "mydb", User: "myuser", Password: "supersecret123"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := op.HandleConfig(cfg); err != nil {
		t.Fatalf("HandleConfig failed: %v", err)
	}

	cmName := "pg-swarm-minikube-store-pg"
	cm, err := client.CoreV1().ConfigMaps(ns).Get(ctx, cmName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get config-store: %v", err)
	}

	jsonData := cm.Data["config.json"]
	if jsonData == "" {
		t.Fatal("config.json is empty")
	}
	if contains(jsonData, "supersecret123") {
		t.Error("config-store should NOT contain plaintext password")
	}
	if !contains(jsonData, "***") {
		t.Error("config-store should contain redacted password marker '***'")
	}
	if !contains(jsonData, "mydb") {
		t.Error("config-store should contain database name")
	}
}

// TestIntegration_HandleConfig_DatabaseSecret verifies that database user
// passwords are stored in the K8s secret.
func TestIntegration_HandleConfig_DatabaseSecret(t *testing.T) {
	client := testClient(t)
	ns := testNamespace(t)
	defer cleanupNamespace(t, client, ns)

	op := New(client, "minikube", ns)
	cfg := integrationCfg("dbsec-pg", ns)
	cfg.Databases = []*pgswarmv1.DatabaseSpec{
		{Name: "app", User: "appuser", Password: "pass1"},
		{Name: "analytics", User: "analyst", Password: "pass2"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := op.HandleConfig(cfg); err != nil {
		t.Fatalf("HandleConfig failed: %v", err)
	}

	secret, err := client.CoreV1().Secrets(ns).Get(ctx, "dbsec-pg-secret", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}

	// Superuser and replication passwords should exist
	if len(secret.Data["superuser-password"]) == 0 {
		t.Error("missing superuser-password")
	}
	if len(secret.Data["replication-password"]) == 0 {
		t.Error("missing replication-password")
	}

	// Per-user passwords
	if string(secret.Data["password-appuser"]) != "pass1" {
		t.Errorf("password-appuser = %q, want pass1", string(secret.Data["password-appuser"]))
	}
	if string(secret.Data["password-analyst"]) != "pass2" {
		t.Errorf("password-analyst = %q, want pass2", string(secret.Data["password-analyst"]))
	}
}

// TestIntegration_HandleConfig_DefaultNamespace verifies that an empty namespace
// in the config is resolved to the operator's default.
func TestIntegration_HandleConfig_DefaultNamespace(t *testing.T) {
	client := testClient(t)
	ns := testNamespace(t)
	defer cleanupNamespace(t, client, ns)

	op := New(client, "minikube", ns) // default namespace = ns
	cfg := integrationCfg("defns-pg", "") // empty namespace

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := op.HandleConfig(cfg); err != nil {
		t.Fatalf("HandleConfig failed: %v", err)
	}

	// Should be in the operator's default namespace
	mustExist(t, ctx, client, ns, "statefulset", "defns-pg")
	mustExist(t, ctx, client, ns, "service", "defns-pg-rw")
}

// TestIntegration_HandleConfig_ServiceUpdate verifies that services can be
// updated without ClusterIP conflicts.
func TestIntegration_HandleConfig_ServiceUpdate(t *testing.T) {
	client := testClient(t)
	ns := testNamespace(t)
	defer cleanupNamespace(t, client, ns)

	op := New(client, "minikube", ns)
	cfg := integrationCfg("svcup-pg", ns)

	if err := op.HandleConfig(cfg); err != nil {
		t.Fatalf("HandleConfig v1 failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Get the ClusterIP assigned to the RW service
	rw1, err := client.CoreV1().Services(ns).Get(ctx, "svcup-pg-rw", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get rw svc v1: %v", err)
	}
	clusterIP := rw1.Spec.ClusterIP

	// v2: this should preserve ClusterIP
	cfg.ConfigVersion = 2
	cfg.PgParams["work_mem"] = "16MB"

	if err := op.HandleConfig(cfg); err != nil {
		t.Fatalf("HandleConfig v2 failed: %v", err)
	}

	rw2, err := client.CoreV1().Services(ns).Get(ctx, "svcup-pg-rw", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get rw svc v2: %v", err)
	}
	if rw2.Spec.ClusterIP != clusterIP {
		t.Errorf("ClusterIP changed from %s to %s (should be preserved)", clusterIP, rw2.Spec.ClusterIP)
	}
}

// TestIntegration_HandleConfig_MultipleClusters verifies that two independent
// clusters can coexist in the same namespace.
func TestIntegration_HandleConfig_MultipleClusters(t *testing.T) {
	client := testClient(t)
	ns := testNamespace(t)
	defer cleanupNamespace(t, client, ns)

	op := New(client, "minikube", ns)

	cfg1 := integrationCfg("alpha-pg", ns)
	cfg2 := integrationCfg("beta-pg", ns)

	if err := op.HandleConfig(cfg1); err != nil {
		t.Fatalf("HandleConfig alpha failed: %v", err)
	}
	if err := op.HandleConfig(cfg2); err != nil {
		t.Fatalf("HandleConfig beta failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Both should exist
	mustExist(t, ctx, client, ns, "statefulset", "alpha-pg")
	mustExist(t, ctx, client, ns, "statefulset", "beta-pg")
	mustExist(t, ctx, client, ns, "service", "alpha-pg-rw")
	mustExist(t, ctx, client, ns, "service", "beta-pg-rw")

	// Delete one — the other should survive
	if err := op.HandleDelete(&pgswarmv1.DeleteCluster{ClusterName: "alpha-pg", Namespace: ns}); err != nil {
		t.Fatalf("HandleDelete alpha failed: %v", err)
	}
	time.Sleep(2 * time.Second)

	mustNotExist(t, ctx, client, ns, "statefulset", "alpha-pg")
	mustExist(t, ctx, client, ns, "statefulset", "beta-pg")
}

// ---------------------------------------------------------------------------
// contains helper (avoids importing strings in the test build tag file)
// ---------------------------------------------------------------------------

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
