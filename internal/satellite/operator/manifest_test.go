package operator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// writeYAML marshals obj to YAML and writes it to testdata/<dir>/<filename>.
func writeYAML(t *testing.T, dir, filename string, obj interface{}) {
	t.Helper()
	out, err := yaml.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal %s: %v", filename, err)
	}

	path := filepath.Join("testdata", dir)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}

	full := filepath.Join(path, filename)
	if err := os.WriteFile(full, out, 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
	t.Logf("wrote %s (%d bytes)", full, len(out))
}

// writeAll writes the full manifest set for a config to testdata/<dir>/.
func writeAll(t *testing.T, dir string, cfg *pgswarmv1.ClusterConfig) {
	t.Helper()

	// Namespace (only for non-default namespaces)
	if cfg.Namespace != "" && cfg.Namespace != "default" {
		writeYAML(t, dir, "namespace.yaml", buildNamespace(cfg.Namespace))
	}

	secret := buildSecret(cfg)
	writeYAML(t, dir, "secret.yaml", secret)
	writeYAML(t, dir, "configmap.yaml", buildConfigMap(cfg))
	writeYAML(t, dir, "service-headless.yaml", buildHeadlessService(cfg))
	writeYAML(t, dir, "service-rw.yaml", buildRWService(cfg))
	writeYAML(t, dir, "service-ro.yaml", buildROService(cfg))
	writeYAML(t, dir, "statefulset.yaml", buildStatefulSet(cfg, secret.Name, "ghcr.io/pg-swarm/pg-swarm-failover:latest"))

	// Config-store ConfigMap (requires operator instance for naming)
	op := New(nil, "minikube", "pg-clusters", "")
	writeYAML(t, dir, "configmap-store.yaml", op.buildConfigStore(cfg))
}

// hasVolumeMount returns true if the container has a VolumeMount with the given name.
func hasVolumeMount(c corev1.Container, name string) bool {
	for _, vm := range c.VolumeMounts {
		if vm.Name == name {
			return true
		}
	}
	return false
}

// hasEnvFrom returns true if the container has an EnvFrom with the given secret name.
func hasEnvFrom(c corev1.Container, secretName string) bool {
	for _, ef := range c.EnvFrom {
		if ef.SecretRef != nil && ef.SecretRef.Name == secretName {
			return true
		}
	}
	return false
}

// hasEnvVar returns true if the container has an env var with the given name.
func hasEnvVar(c corev1.Container, name string) bool {
	for _, ev := range c.Env {
		if ev.Name == name {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func baseCfg() *pgswarmv1.ClusterConfig {
	return &pgswarmv1.ClusterConfig{
		ClusterName:   "my-pg",
		Namespace:     "default",
		Replicas:      3,
		ConfigVersion: 1,
		Postgres: &pgswarmv1.PostgresSpec{
			Version: "17",
			Image:   "postgres:17-alpine",
		},
		Storage: &pgswarmv1.StorageSpec{
			Size: "10Gi",
		},
		Resources: &pgswarmv1.ResourceSpec{
			CpuRequest:    "250m",
			CpuLimit:      "1",
			MemoryRequest: "512Mi",
			MemoryLimit:   "1Gi",
		},
		PgParams: map[string]string{
			"shared_buffers": "256MB",
			"work_mem":       "16MB",
		},
		HbaRules: []string{
			"host all app_user 10.0.0.0/8 md5",
		},
	}
}

func pvcArchiveCfg() *pgswarmv1.ClusterConfig {
	cfg := baseCfg()
	cfg.ClusterName = "my-pg-wal"
	cfg.Archive = &pgswarmv1.ArchiveSpec{
		Mode:                  "pvc",
		ArchiveTimeoutSeconds: 120,
		ArchiveStorage: &pgswarmv1.ArchiveStorageSpec{
			Size:         "50Gi",
			StorageClass: "fast-ssd",
		},
	}
	return cfg
}

func customArchiveCfg() *pgswarmv1.ClusterConfig {
	cfg := baseCfg()
	cfg.ClusterName = "my-pg-s3"
	cfg.Archive = &pgswarmv1.ArchiveSpec{
		Mode:                  "custom",
		ArchiveCommand:        "aws s3 cp %p s3://my-bucket/wal/%f",
		RestoreCommand:        "aws s3 cp s3://my-bucket/wal/%f %p",
		ArchiveTimeoutSeconds: 300,
		CredentialsSecret: &pgswarmv1.SecretRef{
			Name: "aws-credentials",
		},
	}
	return cfg
}

// ---------------------------------------------------------------------------
// Tests: manifest YAML generation + structural assertions
// ---------------------------------------------------------------------------

func TestManifests_NoArchive(t *testing.T) {
	cfg := baseCfg()
	writeAll(t, "no-archive", cfg)

	// ConfigMap: archive_mode = off, no archive_command
	cm := buildConfigMap(cfg)
	pgConf := cm.Data["postgresql.conf"]
	if !strings.Contains(pgConf, "archive_mode = off") {
		t.Error("expected archive_mode = off in postgresql.conf")
	}
	if strings.Contains(pgConf, "archive_command") {
		t.Error("unexpected archive_command when archive is disabled")
	}

	// StatefulSet: only 1 VCT (data), no wal-archive mount
	secret := buildSecret(cfg)
	sts := buildStatefulSet(cfg, secret.Name, "ghcr.io/pg-swarm/pg-swarm-failover:latest")
	if n := len(sts.Spec.VolumeClaimTemplates); n != 1 {
		t.Errorf("expected 1 VCT, got %d", n)
	}
	if sts.Spec.VolumeClaimTemplates[0].Name != "data" {
		t.Errorf("expected VCT name 'data', got %q", sts.Spec.VolumeClaimTemplates[0].Name)
	}
	main := sts.Spec.Template.Spec.Containers[0]
	if hasVolumeMount(main, "wal-archive") {
		t.Error("main container should not have wal-archive mount without archive config")
	}
	init := sts.Spec.Template.Spec.InitContainers[0]
	if hasVolumeMount(init, "wal-archive") {
		t.Error("init container should not have wal-archive mount without archive config")
	}

	// Services: correct names and selectors
	rw := buildRWService(cfg)
	if rw.Spec.Selector[LabelRole] != RolePrimary {
		t.Errorf("RW service should select role=%s, got %q", RolePrimary, rw.Spec.Selector[LabelRole])
	}
	ro := buildROService(cfg)
	if ro.Spec.Selector[LabelRole] != RoleReplica {
		t.Errorf("RO service should select role=%s, got %q", RoleReplica, ro.Spec.Selector[LabelRole])
	}
	hl := buildHeadlessService(cfg)
	if hl.Spec.ClusterIP != corev1.ClusterIPNone {
		t.Errorf("headless service should have ClusterIP=None, got %q", hl.Spec.ClusterIP)
	}
}

func TestManifests_PVCArchive(t *testing.T) {
	cfg := pvcArchiveCfg()
	writeAll(t, "pvc-archive", cfg)

	// ConfigMap: archive_mode = on, sidecar-based archive_command, timeout
	cm := buildConfigMap(cfg)
	pgConf := cm.Data["postgresql.conf"]
	if !strings.Contains(pgConf, "archive_mode = on") {
		t.Error("expected archive_mode = on")
	}
	// PVC archive mode uses file-based WAL staging via shared emptyDir
	if !strings.Contains(pgConf, "cp %p /wal-staging/%f") {
		t.Error("expected file-based archive_command in postgresql.conf")
	}
	if !strings.Contains(pgConf, "/wal-restore/.request") {
		t.Error("expected file-based restore_command in postgresql.conf")
	}
	if !strings.Contains(pgConf, "archive_timeout = 120") {
		t.Error("expected archive_timeout = 120")
	}

	// StatefulSet: only 1 VCT (data) — wal-archive VCT removed in sidecar architecture
	secret := buildSecret(cfg)
	sts := buildStatefulSet(cfg, secret.Name, "ghcr.io/pg-swarm/pg-swarm-failover:latest")
	if n := len(sts.Spec.VolumeClaimTemplates); n != 1 {
		t.Fatalf("expected 1 VCT (data only, wal-archive removed), got %d", n)
	}

	// No wal-archive mounts on containers (sidecar handles WAL)
	init := sts.Spec.Template.Spec.InitContainers[0]
	if hasVolumeMount(init, "wal-archive") {
		t.Error("init container should not have wal-archive mount (sidecar handles WAL)")
	}
	main := sts.Spec.Template.Spec.Containers[0]
	if hasVolumeMount(main, "wal-archive") {
		t.Error("main container should not have wal-archive mount (sidecar handles WAL)")
	}

	// No EnvFrom credentials (PVC mode doesn't need them)
	if hasEnvFrom(main, "aws-credentials") {
		t.Error("PVC mode should not have credential EnvFrom")
	}
}

func TestManifests_CustomArchive(t *testing.T) {
	cfg := customArchiveCfg()
	writeAll(t, "custom-archive", cfg)

	// ConfigMap: custom archive_command, timeout 300
	cm := buildConfigMap(cfg)
	pgConf := cm.Data["postgresql.conf"]
	if !strings.Contains(pgConf, "archive_mode = on") {
		t.Error("expected archive_mode = on")
	}
	if !strings.Contains(pgConf, "aws s3 cp %p s3://my-bucket/wal/%f") {
		t.Error("expected custom archive_command in postgresql.conf")
	}
	if !strings.Contains(pgConf, "archive_timeout = 300") {
		t.Error("expected archive_timeout = 300")
	}

	// StatefulSet: only 1 VCT (no wal-archive PVC for custom mode)
	secret := buildSecret(cfg)
	sts := buildStatefulSet(cfg, secret.Name, "ghcr.io/pg-swarm/pg-swarm-failover:latest")
	if n := len(sts.Spec.VolumeClaimTemplates); n != 1 {
		t.Errorf("custom mode should have 1 VCT (data only), got %d", n)
	}

	// Main container: no wal-archive mount, but has credentials EnvFrom
	main := sts.Spec.Template.Spec.Containers[0]
	if hasVolumeMount(main, "wal-archive") {
		t.Error("custom mode should not mount wal-archive volume")
	}
	if !hasEnvFrom(main, "aws-credentials") {
		t.Error("custom mode with credentials should have EnvFrom for aws-credentials")
	}

	// Init script should have restore_command for replicas, no mkdir
	init := sts.Spec.Template.Spec.InitContainers[0]
	initScript := init.Command[2]
	if strings.Contains(initScript, "mkdir -p /wal-archive") {
		t.Error("custom mode should not have mkdir /wal-archive")
	}
	if !strings.Contains(initScript, "restore_command = 'aws s3 cp s3://my-bucket/wal/%f %p'") {
		t.Error("init script missing custom restore_command for replicas")
	}
}

func TestManifests_CustomNamespace(t *testing.T) {
	cfg := baseCfg()
	cfg.ClusterName = "edge-pg"
	cfg.Namespace = "tenant-alpha"
	writeAll(t, "custom-namespace", cfg)

	// Namespace manifest should be generated for non-default namespace
	ns := buildNamespace(cfg.Namespace)
	if ns.Name != "tenant-alpha" {
		t.Errorf("namespace name = %q, want tenant-alpha", ns.Name)
	}
	if ns.Labels[LabelManagedBy] != ManagedByValue {
		t.Errorf("namespace missing %s label", LabelManagedBy)
	}

	// All resources should be in tenant-alpha namespace
	secret := buildSecret(cfg)
	cm := buildConfigMap(cfg)
	sts := buildStatefulSet(cfg, secret.Name, "ghcr.io/pg-swarm/pg-swarm-failover:latest")
	hl := buildHeadlessService(cfg)
	rw := buildRWService(cfg)
	ro := buildROService(cfg)

	for _, obj := range []struct {
		name string
		ns   string
	}{
		{"secret", secret.Namespace},
		{"configmap", cm.Namespace},
		{"statefulset", sts.Namespace},
		{"headless-svc", hl.Namespace},
		{"rw-svc", rw.Namespace},
		{"ro-svc", ro.Namespace},
	} {
		if obj.ns != "tenant-alpha" {
			t.Errorf("%s namespace = %q, want tenant-alpha", obj.name, obj.ns)
		}
	}

	// Init script DNS should reference the custom namespace
	init := sts.Spec.Template.Spec.InitContainers[0]
	initScript := init.Command[2]
	if !strings.Contains(initScript, "edge-pg-rw.tenant-alpha.svc.cluster.local") {
		t.Error("init script PRIMARY_HOST should use tenant-alpha namespace via RW service")
	}
}

func TestManifests_DefaultNamespaceResolution(t *testing.T) {
	// Config with empty namespace — operator should fill in the default
	cfg := baseCfg()
	cfg.ClusterName = "orphan-pg"
	cfg.Namespace = "" // empty

	op := New(nil, "minikube", "pg-clusters", "")
	op.resolveNamespace(cfg)

	if cfg.Namespace != "pg-clusters" {
		t.Fatalf("resolveNamespace should set empty namespace to default, got %q", cfg.Namespace)
	}

	writeAll(t, "default-namespace", cfg)

	sts := buildStatefulSet(cfg, resourceName(cfg.ClusterName, "secret"), "ghcr.io/pg-swarm/pg-swarm-failover:latest")
	if sts.Namespace != "pg-clusters" {
		t.Errorf("statefulset namespace = %q, want pg-clusters", sts.Namespace)
	}
}

func TestManifests_DefaultNamespaceFallback(t *testing.T) {
	// Operator with empty defaultNamespace falls back to "default"
	op := New(nil, "minikube", "", "")
	cfg := &pgswarmv1.ClusterConfig{
		ClusterName: "test",
		Namespace:   "",
	}
	op.resolveNamespace(cfg)
	if cfg.Namespace != "default" {
		t.Errorf("expected fallback namespace 'default', got %q", cfg.Namespace)
	}
}

func TestManifests_NamespacePreserved(t *testing.T) {
	// Config with explicit namespace should not be overridden
	op := New(nil, "minikube", "pg-clusters", "")
	cfg := &pgswarmv1.ClusterConfig{
		ClusterName: "test",
		Namespace:   "custom-ns",
	}
	op.resolveNamespace(cfg)
	if cfg.Namespace != "custom-ns" {
		t.Errorf("resolveNamespace should not override explicit namespace, got %q", cfg.Namespace)
	}
}

func TestManifests_MinimalConfig(t *testing.T) {
	// Minimal config: no resources, no pg_params, no hba_rules, no archive
	cfg := &pgswarmv1.ClusterConfig{
		ClusterName:   "minimal",
		Namespace:     "default",
		Replicas:      1,
		ConfigVersion: 1,
		Postgres: &pgswarmv1.PostgresSpec{
			Version: "16",
			Image:   "postgres:16",
		},
		Storage: &pgswarmv1.StorageSpec{
			Size: "1Gi",
		},
	}
	writeAll(t, "minimal", cfg)

	// ConfigMap should still have mandatory HA params
	cm := buildConfigMap(cfg)
	pgConf := cm.Data["postgresql.conf"]
	for _, param := range []string{"wal_level = replica", "hot_standby = on", "max_wal_senders = 10"} {
		if !strings.Contains(pgConf, param) {
			t.Errorf("minimal config missing mandatory param %q", param)
		}
	}

	// StatefulSet: no resource limits set
	secret := buildSecret(cfg)
	sts := buildStatefulSet(cfg, secret.Name, "ghcr.io/pg-swarm/pg-swarm-failover:latest")
	main := sts.Spec.Template.Spec.Containers[0]
	if main.Resources.Requests != nil || main.Resources.Limits != nil {
		t.Error("minimal config should not set resource requests/limits")
	}
	if *sts.Spec.Replicas != int32(1) {
		t.Errorf("expected 1 replica, got %d", *sts.Spec.Replicas)
	}
}

func TestManifests_StorageClass(t *testing.T) {
	cfg := baseCfg()
	cfg.ClusterName = "sc-pg"
	cfg.Storage.StorageClass = "gp3"
	writeAll(t, "storage-class", cfg)

	secret := buildSecret(cfg)
	sts := buildStatefulSet(cfg, secret.Name, "ghcr.io/pg-swarm/pg-swarm-failover:latest")
	vct := sts.Spec.VolumeClaimTemplates[0]
	if vct.Spec.StorageClassName == nil || *vct.Spec.StorageClassName != "gp3" {
		t.Error("expected storageClassName = gp3 on data VCT")
	}
}

func TestManifests_CustomArchiveNoCredentials(t *testing.T) {
	// Custom archive mode without credentials secret
	cfg := baseCfg()
	cfg.ClusterName = "pg-nocreds"
	cfg.Archive = &pgswarmv1.ArchiveSpec{
		Mode:           "custom",
		ArchiveCommand: "pgbackrest --stanza=main archive-push %p",
		RestoreCommand: "pgbackrest --stanza=main archive-get %f %p",
	}
	writeAll(t, "custom-no-creds", cfg)

	secret := buildSecret(cfg)
	sts := buildStatefulSet(cfg, secret.Name, "ghcr.io/pg-swarm/pg-swarm-failover:latest")
	main := sts.Spec.Template.Spec.Containers[0]
	if len(main.EnvFrom) != 0 {
		t.Error("custom mode without credentials should have no EnvFrom")
	}

	// archive_command should still be set
	cm := buildConfigMap(cfg)
	pgConf := cm.Data["postgresql.conf"]
	if !strings.Contains(pgConf, "pgbackrest --stanza=main archive-push %p") {
		t.Error("expected pgbackrest archive_command in postgresql.conf")
	}
}

func TestManifests_ArchiveDefaultTimeout(t *testing.T) {
	// Archive with zero timeout should default to 60
	cfg := baseCfg()
	cfg.ClusterName = "pg-timeout"
	cfg.Archive = &pgswarmv1.ArchiveSpec{
		Mode: "pvc",
		ArchiveStorage: &pgswarmv1.ArchiveStorageSpec{
			Size: "5Gi",
		},
		// ArchiveTimeoutSeconds intentionally 0
	}

	cm := buildConfigMap(cfg)
	pgConf := cm.Data["postgresql.conf"]
	if !strings.Contains(pgConf, "archive_timeout = 60") {
		t.Error("zero archive_timeout should default to 60")
	}
}

func TestManifests_Labels(t *testing.T) {
	cfg := baseCfg()
	secret := buildSecret(cfg)
	sts := buildStatefulSet(cfg, secret.Name, "ghcr.io/pg-swarm/pg-swarm-failover:latest")

	// All resources should carry standard labels
	for _, labels := range []map[string]string{
		secret.Labels,
		buildConfigMap(cfg).Labels,
		buildHeadlessService(cfg).Labels,
		buildRWService(cfg).Labels,
		buildROService(cfg).Labels,
		sts.Labels,
	} {
		if labels[LabelManagedBy] != ManagedByValue {
			t.Errorf("missing label %s=%s", LabelManagedBy, ManagedByValue)
		}
		if labels[LabelAppName] != AppNameValue {
			t.Errorf("missing label %s=%s", LabelAppName, AppNameValue)
		}
		if labels[LabelCluster] != cfg.ClusterName {
			t.Errorf("missing label %s=%s", LabelCluster, cfg.ClusterName)
		}
	}
}

func databaseCfg() *pgswarmv1.ClusterConfig {
	cfg := baseCfg()
	cfg.ClusterName = "app-pg"
	cfg.Databases = []*pgswarmv1.DatabaseSpec{
		{Name: "myapp", User: "app_user", Password: "s3cret1"},
		{Name: "analytics", User: "analyst", Password: "s3cret2"},
	}
	return cfg
}

func TestManifests_WithDatabases(t *testing.T) {
	cfg := databaseCfg()
	writeAll(t, "with-databases", cfg)

	// Secret should contain per-user passwords
	secret := buildSecret(cfg)
	if secret.StringData["password-app_user"] != "s3cret1" {
		t.Errorf("expected password-app_user=s3cret1, got %q", secret.StringData["password-app_user"])
	}
	if secret.StringData["password-analyst"] != "s3cret2" {
		t.Errorf("expected password-analyst=s3cret2, got %q", secret.StringData["password-analyst"])
	}

	// Init container should have DB_PASSWORD env vars from secret
	sts := buildStatefulSet(cfg, secret.Name, "ghcr.io/pg-swarm/pg-swarm-failover:latest")
	init := sts.Spec.Template.Spec.InitContainers[0]
	if !hasEnvVar(init, "DB_PASSWORD_APP_USER") {
		t.Error("init container missing DB_PASSWORD_APP_USER env var")
	}
	if !hasEnvVar(init, "DB_PASSWORD_ANALYST") {
		t.Error("init container missing DB_PASSWORD_ANALYST env var")
	}

	// Init script should contain CREATE ROLE and CREATE DATABASE
	initScript := init.Command[2]
	if !strings.Contains(initScript, "rolname='app_user'") {
		t.Error("init script missing CREATE ROLE for app_user")
	}
	if !strings.Contains(initScript, "CREATE DATABASE myapp OWNER app_user") {
		t.Error("init script missing CREATE DATABASE myapp")
	}
	if !strings.Contains(initScript, "rolname='analyst'") {
		t.Error("init script missing CREATE ROLE for analyst")
	}
	if !strings.Contains(initScript, "CREATE DATABASE analytics OWNER analyst") {
		t.Error("init script missing CREATE DATABASE analytics")
	}

	// Config-store ConfigMap should exist with correct name and redacted passwords
	op := New(nil, "minikube", "pg-clusters", "")
	cfgStore := op.buildConfigStore(cfg)
	if cfgStore.Name != "pg-swarm-minikube-app-pg" {
		t.Errorf("config-store name = %q, want pg-swarm-minikube-app-pg", cfgStore.Name)
	}
	jsonData := cfgStore.Data["config.json"]
	if strings.Contains(jsonData, "s3cret1") || strings.Contains(jsonData, "s3cret2") {
		t.Error("config-store should redact database passwords")
	}
	if !strings.Contains(jsonData, "***") {
		t.Error("config-store should contain redacted password marker '***'")
	}
}

func TestManifests_ConfigStoreName(t *testing.T) {
	op := New(nil, "prod-cluster", "default", "")
	name := op.configStoreName("orders-db")
	if name != "pg-swarm-prod-cluster-orders-db" {
		t.Errorf("configStoreName = %q, want pg-swarm-prod-cluster-orders-db", name)
	}
}

func failoverCfg() *pgswarmv1.ClusterConfig {
	cfg := baseCfg()
	cfg.ClusterName = "ha-pg"
	cfg.Failover = &pgswarmv1.FailoverSpec{
		Enabled:                    true,
		HealthCheckIntervalSeconds: 10,
		SidecarImage:               "my-registry/failover:v1",
	}
	return cfg
}

func TestManifests_WithFailover(t *testing.T) {
	cfg := failoverCfg()
	writeAll(t, "with-failover", cfg)

	secret := buildSecret(cfg)
	sts := buildStatefulSet(cfg, secret.Name, "ghcr.io/pg-swarm/pg-swarm-failover:latest")

	// Should have 2 containers: postgres + failover sidecar
	containers := sts.Spec.Template.Spec.Containers
	if len(containers) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(containers))
	}

	sidecar := containers[1]
	if sidecar.Name != "failover" {
		t.Errorf("sidecar name = %q, want failover", sidecar.Name)
	}
	if sidecar.Image != "my-registry/failover:v1" {
		t.Errorf("sidecar image = %q, want my-registry/failover:v1", sidecar.Image)
	}

	// Sidecar should have required env vars
	for _, name := range []string{"POD_NAME", "POD_NAMESPACE", "CLUSTER_NAME", "HEALTH_CHECK_INTERVAL", "POSTGRES_PASSWORD"} {
		if !hasEnvVar(sidecar, name) {
			t.Errorf("sidecar missing env var %s", name)
		}
	}

	// Check HEALTH_CHECK_INTERVAL value
	for _, ev := range sidecar.Env {
		if ev.Name == "HEALTH_CHECK_INTERVAL" && ev.Value != "10" {
			t.Errorf("HEALTH_CHECK_INTERVAL = %q, want 10", ev.Value)
		}
	}

	// ServiceAccountName should be set
	if sts.Spec.Template.Spec.ServiceAccountName != "ha-pg-failover" {
		t.Errorf("serviceAccountName = %q, want ha-pg-failover", sts.Spec.Template.Spec.ServiceAccountName)
	}

	// RBAC resources
	sa := buildFailoverServiceAccount(cfg)
	if sa.Name != "ha-pg-failover" {
		t.Errorf("SA name = %q, want ha-pg-failover", sa.Name)
	}
	if sa.Namespace != cfg.Namespace {
		t.Errorf("SA namespace = %q, want %s", sa.Namespace, cfg.Namespace)
	}

	role := buildFailoverRole(cfg)
	if len(role.Rules) != 3 {
		t.Fatalf("expected 3 RBAC rules, got %d", len(role.Rules))
	}

	rb := buildFailoverRoleBinding(cfg)
	if rb.RoleRef.Name != sa.Name {
		t.Errorf("rolebinding roleRef = %q, want %s", rb.RoleRef.Name, sa.Name)
	}

	// Write RBAC manifests for inspection
	writeYAML(t, "with-failover", "serviceaccount.yaml", sa)
	writeYAML(t, "with-failover", "role.yaml", role)
	writeYAML(t, "with-failover", "rolebinding.yaml", rb)
}

func TestManifests_FailoverDefaultImage(t *testing.T) {
	cfg := baseCfg()
	cfg.ClusterName = "default-fo"
	cfg.Failover = &pgswarmv1.FailoverSpec{
		Enabled: true,
		// No image or interval specified
	}

	secret := buildSecret(cfg)
	sts := buildStatefulSet(cfg, secret.Name, "ghcr.io/pg-swarm/pg-swarm-failover:latest")
	sidecar := sts.Spec.Template.Spec.Containers[1]

	if sidecar.Image != "ghcr.io/pg-swarm/pg-swarm-failover:latest" {
		t.Errorf("default sidecar image = %q, want ghcr.io/pg-swarm/pg-swarm-failover:latest", sidecar.Image)
	}
	// Default interval should be 1 (fast-path failover)
	for _, ev := range sidecar.Env {
		if ev.Name == "HEALTH_CHECK_INTERVAL" && ev.Value != "1" {
			t.Errorf("default HEALTH_CHECK_INTERVAL = %q, want 1", ev.Value)
		}
	}
}

func TestManifests_NoFailover(t *testing.T) {
	cfg := baseCfg()
	secret := buildSecret(cfg)
	sts := buildStatefulSet(cfg, secret.Name, "ghcr.io/pg-swarm/pg-swarm-failover:latest")

	// Without failover, should have only 1 container
	if len(sts.Spec.Template.Spec.Containers) != 1 {
		t.Errorf("expected 1 container without failover, got %d", len(sts.Spec.Template.Spec.Containers))
	}

	// No serviceAccountName set
	if sts.Spec.Template.Spec.ServiceAccountName != "" {
		t.Errorf("serviceAccountName should be empty without failover, got %q", sts.Spec.Template.Spec.ServiceAccountName)
	}
}

func walStorageCfg() *pgswarmv1.ClusterConfig {
	cfg := baseCfg()
	cfg.ClusterName = "wal-pg"
	cfg.WalStorage = &pgswarmv1.StorageSpec{
		Size:         "5Gi",
		StorageClass: "fast-ssd",
	}
	return cfg
}

func TestManifests_WithWalStorage(t *testing.T) {
	cfg := walStorageCfg()
	writeAll(t, "with-wal-storage", cfg)

	secret := buildSecret(cfg)
	sts := buildStatefulSet(cfg, secret.Name, "ghcr.io/pg-swarm/pg-swarm-failover:latest")

	// Should have 2 VCTs: data + wal
	if n := len(sts.Spec.VolumeClaimTemplates); n != 2 {
		t.Fatalf("expected 2 VCTs (data + wal), got %d", n)
	}

	walVCT := sts.Spec.VolumeClaimTemplates[1]
	if walVCT.Name != "wal" {
		t.Errorf("expected second VCT name 'wal', got %q", walVCT.Name)
	}
	walSize := walVCT.Spec.Resources.Requests[corev1.ResourceStorage]
	if walSize.String() != "5Gi" {
		t.Errorf("expected wal size 5Gi, got %s", walSize.String())
	}
	if walVCT.Spec.StorageClassName == nil || *walVCT.Spec.StorageClassName != "fast-ssd" {
		t.Error("expected wal storageClassName = fast-ssd")
	}

	// Both init and main containers must mount /var/lib/postgresql/wal
	init := sts.Spec.Template.Spec.InitContainers[0]
	if !hasVolumeMount(init, "wal") {
		t.Error("init container missing wal mount")
	}
	main := sts.Spec.Template.Spec.Containers[0]
	if !hasVolumeMount(main, "wal") {
		t.Error("main container missing wal mount")
	}

	// Init script should contain symlink logic
	initScript := init.Command[2]
	if !strings.Contains(initScript, "ln -s /var/lib/postgresql/wal") {
		t.Error("init script missing WAL symlink logic")
	}
	if !strings.Contains(initScript, `mv "$PGDATA/pg_wal"/*`) {
		t.Error("init script missing pg_wal move logic")
	}
}

func TestManifests_WithoutWalStorage(t *testing.T) {
	cfg := baseCfg()
	secret := buildSecret(cfg)
	sts := buildStatefulSet(cfg, secret.Name, "ghcr.io/pg-swarm/pg-swarm-failover:latest")

	// Should have only 1 VCT (data)
	if n := len(sts.Spec.VolumeClaimTemplates); n != 1 {
		t.Errorf("expected 1 VCT without wal_storage, got %d", n)
	}

	// No wal mount on containers
	init := sts.Spec.Template.Spec.InitContainers[0]
	if hasVolumeMount(init, "wal") {
		t.Error("init container should not have wal mount without wal_storage")
	}
	main := sts.Spec.Template.Spec.Containers[0]
	if hasVolumeMount(main, "wal") {
		t.Error("main container should not have wal mount without wal_storage")
	}

	// Init script should not contain symlink logic
	initScript := init.Command[2]
	if strings.Contains(initScript, "ln -s /var/lib/postgresql/wal") {
		t.Error("init script should not have WAL symlink logic without wal_storage")
	}
}

func TestManifests_SecretPasswords(t *testing.T) {
	cfg := baseCfg()
	secret := buildSecret(cfg)

	su := secret.StringData["superuser-password"]
	repl := secret.StringData["replication-password"]

	if len(su) != 24 {
		t.Errorf("superuser-password length = %d, want 24", len(su))
	}
	if len(repl) != 24 {
		t.Errorf("replication-password length = %d, want 24", len(repl))
	}
	if su == repl {
		t.Error("superuser and replication passwords should differ")
	}
}

// ---------------------------------------------------------------------------
// Backup sidecar tests
// ---------------------------------------------------------------------------

func backupCfg() *pgswarmv1.ClusterConfig {
	cfg := baseCfg()
	cfg.ClusterName = "backup-pg"
	cfg.Failover = &pgswarmv1.FailoverSpec{Enabled: true}
	cfg.Backups = []*pgswarmv1.BackupConfig{
		{
			BackupProfileId: "abcdef1234567890",
			Physical: &pgswarmv1.PhysicalBackupConfig{
				BaseSchedule:        "0 2 * * *",
				IncrementalSchedule: "0 */6 * * *",
				WalArchiveEnabled:   true,
			},
			Logical: &pgswarmv1.LogicalBackupConfig{
				Schedule:  "0 3 * * *",
				Databases: []string{"mydb"},
			},
			Destination: &pgswarmv1.BackupDestination{
				Type: "s3",
				S3: &pgswarmv1.S3Destination{
					Bucket:    "my-backups",
					Region:    "us-east-1",
					PathPrefix: "pg/",
				},
			},
			Retention: &pgswarmv1.BackupRetention{
				BaseBackupCount: 5,
				WalRetentionDays: 14,
			},
		},
	}
	return cfg
}

func TestManifests_BackupSidecarInjected(t *testing.T) {
	cfg := backupCfg()
	secret := buildSecret(cfg)
	sts := buildStatefulSet(cfg, secret.Name, "ghcr.io/pg-swarm/pg-swarm-failover:latest", "sat-123")

	// Should have 3 containers: postgres + failover + backup
	containers := sts.Spec.Template.Spec.Containers
	if len(containers) != 3 {
		t.Fatalf("expected 3 containers (postgres, failover, backup), got %d", len(containers))
	}

	backup := containers[2]
	if backup.Name != "backup" {
		t.Errorf("backup sidecar name = %q, want backup", backup.Name)
	}
	if backup.Image != "ghcr.io/pg-swarm/pg-swarm-backup-sidecar:latest" {
		t.Errorf("backup sidecar image = %q, want default sidecar image", backup.Image)
	}

	// Check port
	if len(backup.Ports) != 1 || backup.Ports[0].ContainerPort != 8442 {
		t.Error("backup sidecar should expose port 8442")
	}

	// Backup sidecar must have wal-staging and wal-restore volume mounts
	if !hasVolumeMount(backup, "wal-staging") {
		t.Error("backup sidecar missing wal-staging volume mount")
	}
	if !hasVolumeMount(backup, "wal-restore") {
		t.Error("backup sidecar missing wal-restore volume mount")
	}

	// Postgres container must also have wal-staging and wal-restore mounts
	pgContainer := containers[0]
	if !hasVolumeMount(pgContainer, "wal-staging") {
		t.Error("postgres container missing wal-staging volume mount")
	}
	if !hasVolumeMount(pgContainer, "wal-restore") {
		t.Error("postgres container missing wal-restore volume mount")
	}

	// Init container must have wal-restore mount (for recovery during init)
	initC := sts.Spec.Template.Spec.InitContainers[0]
	if !hasVolumeMount(initC, "wal-restore") {
		t.Error("init container missing wal-restore volume mount")
	}

	// Pod template should have wal-staging and wal-restore emptyDir volumes
	volumes := sts.Spec.Template.Spec.Volumes
	hasWalStaging := false
	hasWalRestore := false
	for _, v := range volumes {
		if v.Name == "wal-staging" && v.EmptyDir != nil {
			hasWalStaging = true
		}
		if v.Name == "wal-restore" && v.EmptyDir != nil {
			hasWalRestore = true
		}
	}
	if !hasWalStaging {
		t.Error("pod template missing wal-staging emptyDir volume")
	}
	if !hasWalRestore {
		t.Error("pod template missing wal-restore emptyDir volume")
	}

	// Check required env vars
	for _, name := range []string{"SATELLITE_ID", "CLUSTER_NAME", "POD_NAME", "NAMESPACE", "DEST_TYPE", "PGUSER", "PGPASSWORD", "BASE_SCHEDULE", "INCR_SCHEDULE", "LOGICAL_SCHEDULE"} {
		if !hasEnvVar(backup, name) {
			t.Errorf("backup sidecar missing env var %s", name)
		}
	}

	// Check SATELLITE_ID value
	for _, ev := range backup.Env {
		if ev.Name == "SATELLITE_ID" && ev.Value != "sat-123" {
			t.Errorf("SATELLITE_ID = %q, want sat-123", ev.Value)
		}
		if ev.Name == "PGHOST" && ev.Value != "localhost" {
			t.Errorf("PGHOST = %q, want localhost (sidecar connects locally)", ev.Value)
		}
	}
}

func TestManifests_BackupSidecarNotInjectedWithoutBackup(t *testing.T) {
	cfg := baseCfg()
	cfg.Failover = &pgswarmv1.FailoverSpec{Enabled: true}
	secret := buildSecret(cfg)
	sts := buildStatefulSet(cfg, secret.Name, "ghcr.io/pg-swarm/pg-swarm-failover:latest")

	// Should have 2 containers: postgres + failover (no backup)
	if len(sts.Spec.Template.Spec.Containers) != 2 {
		t.Errorf("expected 2 containers without backup config, got %d", len(sts.Spec.Template.Spec.Containers))
	}
}

func TestManifests_BackupArchiveCommand(t *testing.T) {
	cfg := backupCfg()
	cm := buildConfigMap(cfg)
	pgConf := cm.Data["postgresql.conf"]

	// Backup profiles should auto-configure WAL archiving via shared emptyDir
	if !strings.Contains(pgConf, "archive_mode = on") {
		t.Error("expected archive_mode = on when backups configured")
	}
	if !strings.Contains(pgConf, "cp %p /wal-staging/%f") {
		t.Error("expected file-based archive_command")
	}
	if !strings.Contains(pgConf, "/wal-restore/.request") {
		t.Error("expected file-based restore_command")
	}
	// Incremental schedule should enable summarize_wal
	if !strings.Contains(pgConf, "summarize_wal = on") {
		t.Error("expected summarize_wal = on for incremental backups")
	}
}

func TestManifests_BackupCredentialSecret(t *testing.T) {
	cfg := backupCfg()
	backup := cfg.Backups[0]
	backup.Destination.S3.AccessKeyId = "AKID"
	backup.Destination.S3.SecretAccessKey = "SECRET"

	secret := buildBackupCredentialSecret(cfg, backup)
	if secret == nil {
		t.Fatal("expected credential secret")
	}
	if secret.StringData["aws-access-key-id"] != "AKID" {
		t.Error("expected aws-access-key-id=AKID")
	}
	if secret.StringData["aws-secret-access-key"] != "SECRET" {
		t.Error("expected aws-secret-access-key=SECRET")
	}
}

func TestManifests_BackupRBAC(t *testing.T) {
	cfg := backupCfg()

	sa := buildBackupServiceAccount(cfg)
	if sa.Name != "backup-pg-backup" {
		t.Errorf("SA name = %q, want backup-pg-backup", sa.Name)
	}

	role := buildBackupRole(cfg)
	if len(role.Rules) != 2 {
		t.Errorf("expected 2 RBAC rules (leases + configmaps), got %d", len(role.Rules))
	}

	rb := buildBackupRoleBinding(cfg)
	if rb.RoleRef.Name != sa.Name {
		t.Errorf("rolebinding roleRef = %q, want %s", rb.RoleRef.Name, sa.Name)
	}
}

func TestManifests_BackupEnvVarsS3(t *testing.T) {
	cfg := backupCfg()
	backup := cfg.Backups[0]
	secretName := "backup-pg-secret"

	env := backupSidecarEnvVars(cfg, backup, secretName, "sat-456")

	envMap := make(map[string]string)
	for _, e := range env {
		if e.Value != "" {
			envMap[e.Name] = e.Value
		}
	}

	if envMap["SATELLITE_ID"] != "sat-456" {
		t.Error("missing or wrong SATELLITE_ID")
	}
	if envMap["S3_BUCKET"] != "my-backups" {
		t.Error("missing S3_BUCKET")
	}
	if envMap["S3_REGION"] != "us-east-1" {
		t.Error("missing S3_REGION")
	}
	if envMap["BASE_SCHEDULE"] != "0 2 * * *" {
		t.Error("missing BASE_SCHEDULE")
	}
	if envMap["INCR_SCHEDULE"] != "0 */6 * * *" {
		t.Error("missing INCR_SCHEDULE")
	}
	if envMap["LOGICAL_SCHEDULE"] != "0 3 * * *" {
		t.Error("missing LOGICAL_SCHEDULE")
	}
	if envMap["RETENTION_SETS"] != "5" {
		t.Errorf("RETENTION_SETS = %q, want 5", envMap["RETENTION_SETS"])
	}
	if envMap["RETENTION_DAYS"] != "14" {
		t.Errorf("RETENTION_DAYS = %q, want 14", envMap["RETENTION_DAYS"])
	}
}
