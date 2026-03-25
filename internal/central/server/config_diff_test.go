package server

import (
	"testing"

	"github.com/pg-swarm/pg-swarm/internal/shared/models"
)

func TestClassifyChanges_NoChange(t *testing.T) {
	spec := &models.ClusterSpec{
		Replicas: 3,
		Postgres: models.PostgresSpec{Version: "17", Image: "postgres:17"},
		Storage:  models.StorageSpec{Size: "10Gi"},
		Resources: models.ResourceSpec{
			CPURequest: "500m", CPULimit: "2", MemoryRequest: "1Gi", MemoryLimit: "2Gi",
		},
		PgParams: map[string]string{"work_mem": "64MB"},
	}
	diff := classifyChanges(spec, spec, nil)
	if diff.ApplyStrategy() != "no_change" {
		t.Errorf("expected no_change, got %s", diff.ApplyStrategy())
	}
}

func TestClassifyChanges_ReloadPgParams(t *testing.T) {
	old := &models.ClusterSpec{
		PgParams: map[string]string{"work_mem": "64MB", "statement_timeout": "30s"},
	}
	new := &models.ClusterSpec{
		PgParams: map[string]string{"work_mem": "128MB", "statement_timeout": "30s"},
	}
	diff := classifyChanges(old, new, nil)
	if diff.ApplyStrategy() != "reload" {
		t.Errorf("expected reload, got %s", diff.ApplyStrategy())
	}
	if len(diff.ReloadChanges) != 1 {
		t.Fatalf("expected 1 reload change, got %d", len(diff.ReloadChanges))
	}
	if diff.ReloadChanges[0].Path != "pg_params.work_mem" {
		t.Errorf("expected pg_params.work_mem, got %s", diff.ReloadChanges[0].Path)
	}
}

func TestClassifyChanges_SharedBuffersSequential(t *testing.T) {
	// shared_buffers is postmaster-context, classified as sequential in the DB.
	pc := ParamClassifications{"shared_buffers": "sequential", "wal_level": "full_restart"}
	old := &models.ClusterSpec{
		PgParams: map[string]string{"shared_buffers": "256MB"},
	}
	new := &models.ClusterSpec{
		PgParams: map[string]string{"shared_buffers": "512MB"},
	}
	diff := classifyChanges(old, new, pc)
	if diff.ApplyStrategy() != "sequential_restart" {
		t.Errorf("expected sequential_restart for shared_buffers, got %s", diff.ApplyStrategy())
	}
}

func TestClassifyChanges_WalLevelFullRestart(t *testing.T) {
	old := &models.ClusterSpec{
		PgParams: map[string]string{"wal_level": "replica"},
	}
	new := &models.ClusterSpec{
		PgParams: map[string]string{"wal_level": "logical"},
	}
	diff := classifyChanges(old, new, nil)
	if diff.ApplyStrategy() != "full_restart" {
		t.Errorf("expected full_restart, got %s", diff.ApplyStrategy())
	}
	if len(diff.FullRestartChanges) != 1 {
		t.Fatalf("expected 1 full restart change, got %d", len(diff.FullRestartChanges))
	}
}

func TestClassifyChanges_ImmutableStorage(t *testing.T) {
	old := &models.ClusterSpec{
		Storage: models.StorageSpec{Size: "10Gi", StorageClass: "fast"},
	}
	new := &models.ClusterSpec{
		Storage: models.StorageSpec{Size: "20Gi", StorageClass: "fast"},
	}
	diff := classifyChanges(old, new, nil)
	if diff.ApplyStrategy() != "rejected" {
		t.Errorf("expected rejected, got %s", diff.ApplyStrategy())
	}
	if len(diff.ImmutableErrors) != 1 {
		t.Fatalf("expected 1 immutable error, got %d", len(diff.ImmutableErrors))
	}
	if diff.ImmutableErrors[0].Path != "storage.size" {
		t.Errorf("expected storage.size, got %s", diff.ImmutableErrors[0].Path)
	}
}

func TestClassifyChanges_WalStorageImmutable(t *testing.T) {
	old := &models.ClusterSpec{
		WalStorage: &models.StorageSpec{Size: "5Gi"},
	}
	new := &models.ClusterSpec{
		WalStorage: &models.StorageSpec{Size: "10Gi"},
	}
	diff := classifyChanges(old, new, nil)
	if diff.ApplyStrategy() != "rejected" {
		t.Errorf("expected rejected, got %s", diff.ApplyStrategy())
	}
}

func TestClassifyChanges_ScaleUp(t *testing.T) {
	old := &models.ClusterSpec{Replicas: 3}
	new := &models.ClusterSpec{Replicas: 5}
	diff := classifyChanges(old, new, nil)
	if diff.ApplyStrategy() != "scale_only" {
		t.Errorf("expected scale_only, got %s", diff.ApplyStrategy())
	}
	if diff.ScaleUp == nil || *diff.ScaleUp != 5 {
		t.Error("expected ScaleUp=5")
	}
}

func TestClassifyChanges_ScaleDown(t *testing.T) {
	old := &models.ClusterSpec{Replicas: 5}
	new := &models.ClusterSpec{Replicas: 3}
	diff := classifyChanges(old, new, nil)
	if diff.ApplyStrategy() != "scale_only" {
		t.Errorf("expected scale_only for scale-down, got %s", diff.ApplyStrategy())
	}
	if diff.ScaleDown == nil || *diff.ScaleDown != 3 {
		t.Error("expected ScaleDown=3")
	}
}

func TestClassifyChanges_ResourceChange(t *testing.T) {
	old := &models.ClusterSpec{
		Resources: models.ResourceSpec{CPURequest: "500m", CPULimit: "2", MemoryRequest: "1Gi", MemoryLimit: "2Gi"},
	}
	new := &models.ClusterSpec{
		Resources: models.ResourceSpec{CPURequest: "1", CPULimit: "2", MemoryRequest: "2Gi", MemoryLimit: "4Gi"},
	}
	diff := classifyChanges(old, new, nil)
	if diff.ApplyStrategy() != "sequential_restart" {
		t.Errorf("expected sequential_restart, got %s", diff.ApplyStrategy())
	}
	if len(diff.SequentialChanges) != 3 {
		t.Errorf("expected 3 sequential changes (cpu_request, memory_request, memory_limit), got %d", len(diff.SequentialChanges))
	}
}

func TestClassifyChanges_PostgresVersionFullRestart(t *testing.T) {
	old := &models.ClusterSpec{Postgres: models.PostgresSpec{Version: "16"}}
	new := &models.ClusterSpec{Postgres: models.PostgresSpec{Version: "17"}}
	diff := classifyChanges(old, new, nil)
	if diff.ApplyStrategy() != "full_restart" {
		t.Errorf("expected full_restart, got %s", diff.ApplyStrategy())
	}
}

func TestClassifyChanges_HbaRulesReload(t *testing.T) {
	old := &models.ClusterSpec{HbaRules: []string{"host all all 0.0.0.0/0 md5"}}
	new := &models.ClusterSpec{HbaRules: []string{"host all all 0.0.0.0/0 scram-sha-256"}}
	diff := classifyChanges(old, new, nil)
	if diff.ApplyStrategy() != "reload" {
		t.Errorf("expected reload for HBA rules, got %s", diff.ApplyStrategy())
	}
}

func TestClassifyChanges_FailoverToggle(t *testing.T) {
	old := &models.ClusterSpec{Failover: &models.FailoverSpec{Enabled: false}}
	new := &models.ClusterSpec{Failover: &models.FailoverSpec{Enabled: true}}
	diff := classifyChanges(old, new, nil)
	if diff.ApplyStrategy() != "sequential_restart" {
		t.Errorf("expected sequential_restart, got %s", diff.ApplyStrategy())
	}
}

func TestClassifyChanges_MixedReloadAndFullRestart(t *testing.T) {
	old := &models.ClusterSpec{
		PgParams: map[string]string{"work_mem": "64MB", "wal_level": "replica"},
	}
	new := &models.ClusterSpec{
		PgParams: map[string]string{"work_mem": "128MB", "wal_level": "logical"},
	}
	diff := classifyChanges(old, new, nil)
	if diff.ApplyStrategy() != "full_restart" {
		t.Errorf("expected full_restart for mixed changes, got %s", diff.ApplyStrategy())
	}
	if len(diff.ReloadChanges) != 1 || len(diff.FullRestartChanges) != 1 {
		t.Errorf("expected 1 reload + 1 full restart, got %d + %d",
			len(diff.ReloadChanges), len(diff.FullRestartChanges))
	}
}

func TestClassifyChanges_DeletionProtection(t *testing.T) {
	old := &models.ClusterSpec{DeletionProtection: false}
	new := &models.ClusterSpec{DeletionProtection: true}
	diff := classifyChanges(old, new, nil)
	if diff.ApplyStrategy() != "sequential_restart" {
		t.Errorf("expected sequential_restart, got %s", diff.ApplyStrategy())
	}
}

func TestClassifyChanges_ArchiveCommandReload(t *testing.T) {
	old := &models.ClusterSpec{
		Archive: &models.ArchiveSpec{Mode: "custom", ArchiveCommand: "old_cmd"},
	}
	new := &models.ClusterSpec{
		Archive: &models.ArchiveSpec{Mode: "custom", ArchiveCommand: "new_cmd"},
	}
	diff := classifyChanges(old, new, nil)
	if diff.ApplyStrategy() != "reload" {
		t.Errorf("expected reload for archive_command change, got %s", diff.ApplyStrategy())
	}
}

func TestClassifyChanges_ArchiveModeSequential(t *testing.T) {
	old := &models.ClusterSpec{
		Archive: &models.ArchiveSpec{Mode: ""},
	}
	new := &models.ClusterSpec{
		Archive: &models.ArchiveSpec{Mode: "custom"},
	}
	diff := classifyChanges(old, new, nil)
	if diff.ApplyStrategy() != "sequential_restart" {
		t.Errorf("expected sequential_restart for archive_mode change, got %s", diff.ApplyStrategy())
	}
}

func TestConfigDiff_Summary(t *testing.T) {
	diff := &ConfigDiff{
		SequentialChanges: []ParamChange{
			{Path: "pg_params.work_mem", OldValue: "64MB", NewValue: "128MB"},
		},
	}
	s := diff.Summary()
	if s == "" || s == "no changes" {
		t.Errorf("expected non-empty summary, got %q", s)
	}
}

func TestConfigDiff_SummaryNoChanges(t *testing.T) {
	diff := &ConfigDiff{}
	if diff.Summary() != "no changes" {
		t.Errorf("expected 'no changes', got %q", diff.Summary())
	}
}
