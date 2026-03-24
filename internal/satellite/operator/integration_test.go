//go:build integration

package operator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
)

func TestIntegration_ClusterCreation(t *testing.T) {
	ns := createTestNamespace(t)
	op := newTestOperator(t, ns)
	name := "integ-create"

	cfg := integrationCfg(name, ns)
	if err := op.HandleConfig(cfg); err != nil {
		t.Fatalf("HandleConfig: %v", err)
	}

	// StatefulSet
	sts, err := testClient.AppsV1().StatefulSets(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("StatefulSet not found: %v", err)
	}
	if *sts.Spec.Replicas != 1 {
		t.Errorf("expected 1 replica, got %d", *sts.Spec.Replicas)
	}
	if sts.Spec.ServiceName != resourceName(name, "headless") {
		t.Errorf("serviceName = %q, want %q", sts.Spec.ServiceName, resourceName(name, "headless"))
	}
	pgContainer := sts.Spec.Template.Spec.Containers[0]
	if pgContainer.Image != "postgres:17-alpine" {
		t.Errorf("image = %q, want postgres:17-alpine", pgContainer.Image)
	}

	// Services
	assertResourceExists(t, ns, "Service", resourceName(name, "headless"))
	headless, _ := testClient.CoreV1().Services(ns).Get(context.Background(), resourceName(name, "headless"), metav1.GetOptions{})
	if headless.Spec.ClusterIP != "None" {
		t.Errorf("headless service ClusterIP = %q, want None", headless.Spec.ClusterIP)
	}
	assertResourceExists(t, ns, "Service", resourceName(name, "rw"))
	assertResourceExists(t, ns, "Service", resourceName(name, "ro"))

	// ConfigMap
	cm, err := testClient.CoreV1().ConfigMaps(ns).Get(context.Background(), resourceName(name, "config"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("ConfigMap not found: %v", err)
	}
	if pgConf, ok := cm.Data["postgresql.conf"]; !ok || !strings.Contains(pgConf, "wal_level = replica") {
		t.Errorf("postgresql.conf missing or doesn't contain wal_level = replica")
	}

	// Config-store ConfigMap
	storeName := op.configStoreName(name)
	storeCM, err := testClient.CoreV1().ConfigMaps(ns).Get(context.Background(), storeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("config-store ConfigMap not found: %v", err)
	}
	var stored map[string]interface{}
	if err := json.Unmarshal([]byte(storeCM.Data["config.json"]), &stored); err != nil {
		t.Errorf("config-store ConfigMap has invalid JSON: %v", err)
	}

	// Secret
	secret, err := testClient.CoreV1().Secrets(ns).Get(context.Background(), resourceName(name, "secret"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Secret not found: %v", err)
	}
	for _, key := range []string{"superuser-password", "replication-password"} {
		if _, ok := secret.Data[key]; !ok {
			t.Errorf("secret missing key %q", key)
		}
	}
}

func TestIntegration_ClusterReady(t *testing.T) {
	ns := createTestNamespace(t)
	op := newTestOperator(t, ns)
	name := "integ-ready"

	cfg := integrationCfg(name, ns)
	if err := op.HandleConfig(cfg); err != nil {
		t.Fatalf("HandleConfig: %v", err)
	}

	waitForStatefulSetReady(t, ns, name, 5*time.Minute)

	// Verify pod is Running with correct labels
	pods, err := testClient.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{
		LabelSelector: LabelCluster + "=" + name,
	})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) == 0 {
		t.Fatal("no pods found")
	}
	pod := pods.Items[0]
	if pod.Status.Phase != "Running" {
		t.Errorf("pod phase = %q, want Running", pod.Status.Phase)
	}
	if pod.Name != name+"-0" {
		t.Errorf("pod name = %q, want %s-0", pod.Name, name)
	}

	// Validate bug fix: isStatefulSetReady should return true
	ctx := context.Background()
	if !isStatefulSetReady(ctx, testClient, ns, name) {
		t.Error("isStatefulSetReady returned false for a ready StatefulSet (bug fix validation)")
	}
}

func TestIntegration_ClusterDeletion(t *testing.T) {
	ns := createTestNamespace(t)
	op := newTestOperator(t, ns)
	name := "integ-delete"

	cfg := integrationCfg(name, ns)
	if err := op.HandleConfig(cfg); err != nil {
		t.Fatalf("HandleConfig: %v", err)
	}
	assertResourceExists(t, ns, "StatefulSet", name)

	del := &pgswarmv1.DeleteCluster{
		ClusterName: name,
		Namespace:   ns,
	}
	if err := op.HandleDelete(del); err != nil {
		t.Fatalf("HandleDelete: %v", err)
	}

	// Poll for resources to be gone
	pollUntilNotFound(t, ns, "StatefulSet", name, 30*time.Second)
	assertResourceNotExists(t, ns, "Service", resourceName(name, "headless"))
	assertResourceNotExists(t, ns, "Service", resourceName(name, "rw"))
	assertResourceNotExists(t, ns, "Service", resourceName(name, "ro"))
	assertResourceNotExists(t, ns, "ConfigMap", resourceName(name, "config"))
	assertResourceNotExists(t, ns, "Secret", resourceName(name, "secret"))
}

func TestIntegration_IdempotentReconcile(t *testing.T) {
	ns := createTestNamespace(t)
	op := newTestOperator(t, ns)
	name := "integ-idempotent"

	cfg := integrationCfg(name, ns)
	if err := op.HandleConfig(cfg); err != nil {
		t.Fatalf("HandleConfig (1st): %v", err)
	}

	// Get STS resourceVersion after first reconcile
	sts1, err := testClient.AppsV1().StatefulSets(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get STS: %v", err)
	}
	rv1 := sts1.ResourceVersion

	// Second HandleConfig with same configVersion should be a no-op
	if err := op.HandleConfig(cfg); err != nil {
		t.Fatalf("HandleConfig (2nd): %v", err)
	}

	sts2, err := testClient.AppsV1().StatefulSets(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get STS after 2nd call: %v", err)
	}
	if sts2.ResourceVersion != rv1 {
		t.Errorf("STS resourceVersion changed: %s → %s (expected no-op)", rv1, sts2.ResourceVersion)
	}
}
