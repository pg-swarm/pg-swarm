package server

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pg-swarm/pg-swarm/internal/shared/models"
)

// ParamClassifications maps parameter names to their update mode.
// Values: "reload", "sequential", "full_restart".
// Parameters not in the map default to "reload".
type ParamClassifications map[string]string

// DefaultParamClassifications is the fallback used when the database is not
// available (e.g., in unit tests). In production, loaded from pg_param_classifications table.
var DefaultParamClassifications = ParamClassifications{
	"wal_level": "full_restart",
}

// Mode returns the update mode for a parameter. Defaults to "reload" if not found.
func (pc ParamClassifications) Mode(name string) string {
	if pc == nil {
		return "reload"
	}
	if mode, ok := pc[name]; ok {
		return mode
	}
	return "reload"
}

// ParamChange describes a single configuration field that differs between two specs.
type ParamChange struct {
	Path     string `json:"path"`
	OldValue string `json:"old_value"`
	NewValue string `json:"new_value"`
}

// ConfigDiff holds the classified differences between two ClusterSpec values.
type ConfigDiff struct {
	ReloadChanges      []ParamChange `json:"reload_changes,omitempty"`
	SequentialChanges  []ParamChange `json:"sequential_changes,omitempty"`
	FullRestartChanges []ParamChange `json:"full_restart_changes,omitempty"`
	ImmutableErrors    []ParamChange `json:"immutable_errors,omitempty"`
	ScaleUp            *int32        `json:"scale_up,omitempty"`
	ScaleDown          *int32        `json:"scale_down,omitempty"`
}

// ApplyStrategy returns the highest-impact update mode required for this diff.
// Priority: rejected > full_restart > sequential_restart > reload > scale_only > no_change
func (d *ConfigDiff) ApplyStrategy() string {
	if len(d.ImmutableErrors) > 0 {
		return "rejected"
	}
	if len(d.FullRestartChanges) > 0 {
		return "full_restart"
	}
	if len(d.SequentialChanges) > 0 {
		return "sequential_restart"
	}
	if len(d.ReloadChanges) > 0 {
		return "reload"
	}
	if d.ScaleUp != nil || d.ScaleDown != nil {
		return "scale_only"
	}
	return "no_change"
}

// Summary returns a human-readable description of the changes.
func (d *ConfigDiff) Summary() string {
	var parts []string
	for _, c := range d.ReloadChanges {
		parts = append(parts, fmt.Sprintf("%s: %s → %s", c.Path, c.OldValue, c.NewValue))
	}
	for _, c := range d.SequentialChanges {
		parts = append(parts, fmt.Sprintf("%s: %s → %s (restart)", c.Path, c.OldValue, c.NewValue))
	}
	for _, c := range d.FullRestartChanges {
		parts = append(parts, fmt.Sprintf("%s: %s → %s (full restart)", c.Path, c.OldValue, c.NewValue))
	}
	if d.ScaleUp != nil {
		parts = append(parts, fmt.Sprintf("scale up to %d replicas", *d.ScaleUp))
	}
	if d.ScaleDown != nil {
		parts = append(parts, fmt.Sprintf("scale down to %d replicas", *d.ScaleDown))
	}
	if len(parts) == 0 {
		return "no changes"
	}
	return strings.Join(parts, "; ")
}

// classifyChanges compares old and new ClusterSpec and classifies each difference.
// classifications maps param names to their update mode ("reload", "sequential", "full_restart").
// If nil, DefaultParamClassifications is used.
func classifyChanges(old, new *models.ClusterSpec, classifications ParamClassifications) *ConfigDiff {
	if classifications == nil {
		classifications = DefaultParamClassifications
	}
	diff := &ConfigDiff{}

	// pg_params
	diffPgParams(diff, old.PgParams, new.PgParams, classifications)

	// hba_rules (reloaded by pg_reload_conf)
	if !stringSliceEqual(old.HbaRules, new.HbaRules) {
		diff.ReloadChanges = append(diff.ReloadChanges, ParamChange{
			Path:     "hba_rules",
			OldValue: strings.Join(old.HbaRules, "; "),
			NewValue: strings.Join(new.HbaRules, "; "),
		})
	}

	// postgres version/image
	if old.Postgres.Version != new.Postgres.Version {
		diff.FullRestartChanges = append(diff.FullRestartChanges, ParamChange{
			Path: "postgres.version", OldValue: old.Postgres.Version, NewValue: new.Postgres.Version,
		})
	}
	if old.Postgres.Image != new.Postgres.Image {
		diff.FullRestartChanges = append(diff.FullRestartChanges, ParamChange{
			Path: "postgres.image", OldValue: old.Postgres.Image, NewValue: new.Postgres.Image,
		})
	}
	if old.Postgres.Variant != new.Postgres.Variant {
		diff.FullRestartChanges = append(diff.FullRestartChanges, ParamChange{
			Path: "postgres.variant", OldValue: old.Postgres.Variant, NewValue: new.Postgres.Variant,
		})
	}

	// storage (immutable)
	diffStorageImmutable(diff, "storage", old.Storage, new.Storage)
	diffWalStorageImmutable(diff, old.WalStorage, new.WalStorage)

	// resources (sequential)
	diffResources(diff, old.Resources, new.Resources)

	// archive (sequential — except archive_mode off→on handled via pg_params)
	diffArchive(diff, old.Archive, new.Archive)

	// sentinel (sequential)
	diffSentinel(diff, old.Sentinel, new.Sentinel)

	// deletion_protection (no restart, PVC-level)
	if old.DeletionProtection != new.DeletionProtection {
		diff.SequentialChanges = append(diff.SequentialChanges, ParamChange{
			Path:     "deletion_protection",
			OldValue: fmt.Sprintf("%v", old.DeletionProtection),
			NewValue: fmt.Sprintf("%v", new.DeletionProtection),
		})
	}

	// backup (reload — config is pushed via event stream, no pod restart needed)
	diffBackup(diff, old.Backup, new.Backup)

	// replicas
	if old.Replicas != new.Replicas {
		if new.Replicas > old.Replicas {
			diff.ScaleUp = &new.Replicas
		} else {
			diff.ScaleDown = &new.Replicas
		}
	}

	return diff
}

// diffPgParams classifies each changed pg_params key using the three-mode classification.
func diffPgParams(diff *ConfigDiff, old, new map[string]string, classifications ParamClassifications) {
	allKeys := mergeMapKeys(old, new)
	for _, key := range allKeys {
		oldVal := old[key]
		newVal := new[key]
		if oldVal == newVal {
			continue
		}
		change := ParamChange{
			Path:     "pg_params." + key,
			OldValue: oldVal,
			NewValue: newVal,
		}
		switch classifications.Mode(key) {
		case "full_restart":
			diff.FullRestartChanges = append(diff.FullRestartChanges, change)
		case "sequential":
			diff.SequentialChanges = append(diff.SequentialChanges, change)
		default: // "reload" or unknown
			diff.ReloadChanges = append(diff.ReloadChanges, change)
		}
	}
}

func diffStorageImmutable(diff *ConfigDiff, prefix string, old, new models.StorageSpec) {
	if old.Size != new.Size {
		diff.ImmutableErrors = append(diff.ImmutableErrors, ParamChange{
			Path: prefix + ".size", OldValue: old.Size, NewValue: new.Size,
		})
	}
	if old.StorageClass != new.StorageClass {
		diff.ImmutableErrors = append(diff.ImmutableErrors, ParamChange{
			Path: prefix + ".storage_class", OldValue: old.StorageClass, NewValue: new.StorageClass,
		})
	}
}

func diffWalStorageImmutable(diff *ConfigDiff, old, new *models.StorageSpec) {
	if old == nil && new == nil {
		return
	}
	if old == nil {
		old = &models.StorageSpec{}
	}
	if new == nil {
		new = &models.StorageSpec{}
	}
	diffStorageImmutable(diff, "wal_storage", *old, *new)
}

func diffResources(diff *ConfigDiff, old, new models.ResourceSpec) {
	check := func(field, oldVal, newVal string) {
		if oldVal != newVal {
			diff.SequentialChanges = append(diff.SequentialChanges, ParamChange{
				Path: "resources." + field, OldValue: oldVal, NewValue: newVal,
			})
		}
	}
	check("cpu_request", old.CPURequest, new.CPURequest)
	check("cpu_limit", old.CPULimit, new.CPULimit)
	check("memory_request", old.MemoryRequest, new.MemoryRequest)
	check("memory_limit", old.MemoryLimit, new.MemoryLimit)
}

func diffArchive(diff *ConfigDiff, old, new *models.ArchiveSpec) {
	if old == nil && new == nil {
		return
	}
	o := archiveOrEmpty(old)
	n := archiveOrEmpty(new)
	if o.Mode != n.Mode {
		// archive_mode is postmaster context — needs sequential restart
		diff.SequentialChanges = append(diff.SequentialChanges, ParamChange{
			Path: "archive.mode", OldValue: o.Mode, NewValue: n.Mode,
		})
	}
	if o.ArchiveCommand != n.ArchiveCommand {
		diff.ReloadChanges = append(diff.ReloadChanges, ParamChange{
			Path: "archive.archive_command", OldValue: o.ArchiveCommand, NewValue: n.ArchiveCommand,
		})
	}
	if o.RestoreCommand != n.RestoreCommand {
		diff.ReloadChanges = append(diff.ReloadChanges, ParamChange{
			Path: "archive.restore_command", OldValue: o.RestoreCommand, NewValue: n.RestoreCommand,
		})
	}
	if o.ArchiveTimeoutSeconds != n.ArchiveTimeoutSeconds {
		diff.ReloadChanges = append(diff.ReloadChanges, ParamChange{
			Path:     "archive.archive_timeout_seconds",
			OldValue: fmt.Sprintf("%d", o.ArchiveTimeoutSeconds),
			NewValue: fmt.Sprintf("%d", n.ArchiveTimeoutSeconds),
		})
	}
}

func diffSentinel(diff *ConfigDiff, old, new *models.SentinelSpec) {
	if old == nil && new == nil {
		return
	}
	o := sentinelOrEmpty(old)
	n := sentinelOrEmpty(new)
	if o.Enabled != n.Enabled {
		diff.SequentialChanges = append(diff.SequentialChanges, ParamChange{
			Path:     "failover.enabled",
			OldValue: fmt.Sprintf("%v", o.Enabled),
			NewValue: fmt.Sprintf("%v", n.Enabled),
		})
	}
	if o.SidecarImage != n.SidecarImage {
		diff.SequentialChanges = append(diff.SequentialChanges, ParamChange{
			Path: "failover.sidecar_image", OldValue: o.SidecarImage, NewValue: n.SidecarImage,
		})
	}
	if o.HealthCheckIntervalSeconds != n.HealthCheckIntervalSeconds {
		diff.SequentialChanges = append(diff.SequentialChanges, ParamChange{
			Path:     "failover.health_check_interval_seconds",
			OldValue: fmt.Sprintf("%d", o.HealthCheckIntervalSeconds),
			NewValue: fmt.Sprintf("%d", n.HealthCheckIntervalSeconds),
		})
	}
}

func diffBackup(diff *ConfigDiff, old, new *models.BackupSpec) {
	if old == nil && new == nil {
		return
	}
	o := backupOrEmpty(old)
	n := backupOrEmpty(new)

	// store_id
	oldStore := ""
	newStore := ""
	if o.StoreID != nil {
		oldStore = o.StoreID.String()
	}
	if n.StoreID != nil {
		newStore = n.StoreID.String()
	}
	if oldStore != newStore {
		diff.ReloadChanges = append(diff.ReloadChanges, ParamChange{
			Path: "backup.store_id", OldValue: oldStore, NewValue: newStore,
		})
	}

	// physical
	diffPhysicalBackup(diff, o.Physical, n.Physical)

	// logical
	diffLogicalBackup(diff, o.Logical, n.Logical)

	// retention
	diffBackupRetention(diff, o.Retention, n.Retention)
}

func diffPhysicalBackup(diff *ConfigDiff, old, new *models.PhysicalBackupConfig) {
	if old == nil && new == nil {
		return
	}
	o := physicalOrEmpty(old)
	n := physicalOrEmpty(new)

	if o.Enabled != n.Enabled {
		diff.ReloadChanges = append(diff.ReloadChanges, ParamChange{
			Path: "backup.physical.enabled", OldValue: fmt.Sprintf("%v", o.Enabled), NewValue: fmt.Sprintf("%v", n.Enabled),
		})
	}
	if o.BaseSchedule != n.BaseSchedule {
		diff.ReloadChanges = append(diff.ReloadChanges, ParamChange{
			Path: "backup.physical.base_schedule", OldValue: o.BaseSchedule, NewValue: n.BaseSchedule,
		})
	}
	if o.IncrementalSchedule != n.IncrementalSchedule {
		diff.ReloadChanges = append(diff.ReloadChanges, ParamChange{
			Path: "backup.physical.incremental_schedule", OldValue: o.IncrementalSchedule, NewValue: n.IncrementalSchedule,
		})
	}
	if o.WalArchiveEnabled != n.WalArchiveEnabled {
		diff.ReloadChanges = append(diff.ReloadChanges, ParamChange{
			Path: "backup.physical.wal_archive_enabled", OldValue: fmt.Sprintf("%v", o.WalArchiveEnabled), NewValue: fmt.Sprintf("%v", n.WalArchiveEnabled),
		})
	}
	if o.ArchiveTimeoutSeconds != n.ArchiveTimeoutSeconds {
		diff.ReloadChanges = append(diff.ReloadChanges, ParamChange{
			Path:     "backup.physical.archive_timeout_seconds",
			OldValue: fmt.Sprintf("%d", o.ArchiveTimeoutSeconds),
			NewValue: fmt.Sprintf("%d", n.ArchiveTimeoutSeconds),
		})
	}
}

func diffLogicalBackup(diff *ConfigDiff, old, new *models.LogicalBackupConfig) {
	if old == nil && new == nil {
		return
	}
	o := logicalOrEmpty(old)
	n := logicalOrEmpty(new)

	if o.Enabled != n.Enabled {
		diff.ReloadChanges = append(diff.ReloadChanges, ParamChange{
			Path: "backup.logical.enabled", OldValue: fmt.Sprintf("%v", o.Enabled), NewValue: fmt.Sprintf("%v", n.Enabled),
		})
	}
	if o.Schedule != n.Schedule {
		diff.ReloadChanges = append(diff.ReloadChanges, ParamChange{
			Path: "backup.logical.schedule", OldValue: o.Schedule, NewValue: n.Schedule,
		})
	}
	if !stringSliceEqual(o.Databases, n.Databases) {
		diff.ReloadChanges = append(diff.ReloadChanges, ParamChange{
			Path:     "backup.logical.databases",
			OldValue: strings.Join(o.Databases, ", "),
			NewValue: strings.Join(n.Databases, ", "),
		})
	}
	if o.Format != n.Format {
		diff.ReloadChanges = append(diff.ReloadChanges, ParamChange{
			Path: "backup.logical.format", OldValue: o.Format, NewValue: n.Format,
		})
	}
}

func diffBackupRetention(diff *ConfigDiff, old, new *models.BackupRetention) {
	if old == nil && new == nil {
		return
	}
	o := retentionOrEmpty(old)
	n := retentionOrEmpty(new)

	check := func(field string, oldVal, newVal int) {
		if oldVal != newVal {
			diff.ReloadChanges = append(diff.ReloadChanges, ParamChange{
				Path: "backup.retention." + field, OldValue: fmt.Sprintf("%d", oldVal), NewValue: fmt.Sprintf("%d", newVal),
			})
		}
	}
	check("base_backup_count", o.BaseBackupCount, n.BaseBackupCount)
	check("incremental_backup_count", o.IncrementalBackupCount, n.IncrementalBackupCount)
	check("logical_backup_count", o.LogicalBackupCount, n.LogicalBackupCount)
	check("wal_retention_days", o.WalRetentionDays, n.WalRetentionDays)
}

// --- helpers ---

func archiveOrEmpty(a *models.ArchiveSpec) models.ArchiveSpec {
	if a == nil {
		return models.ArchiveSpec{}
	}
	return *a
}

func sentinelOrEmpty(f *models.SentinelSpec) models.SentinelSpec {
	if f == nil {
		return models.SentinelSpec{}
	}
	return *f
}

func backupOrEmpty(b *models.BackupSpec) models.BackupSpec {
	if b == nil {
		return models.BackupSpec{}
	}
	return *b
}

func physicalOrEmpty(p *models.PhysicalBackupConfig) models.PhysicalBackupConfig {
	if p == nil {
		return models.PhysicalBackupConfig{}
	}
	return *p
}

func logicalOrEmpty(l *models.LogicalBackupConfig) models.LogicalBackupConfig {
	if l == nil {
		return models.LogicalBackupConfig{}
	}
	return *l
}

func retentionOrEmpty(r *models.BackupRetention) models.BackupRetention {
	if r == nil {
		return models.BackupRetention{}
	}
	return *r
}

func mergeMapKeys(a, b map[string]string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
