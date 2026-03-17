package operator

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
)

// walRestoreCommand is the POSIX-compatible restore_command shell script that uses
// shared emptyDir volumes to fetch WAL segments from the backup sidecar.
// It checks for a pre-fetched file, then signals the sidecar via a .request file
// and polls for up to 30s. Works with any postgres image (no curl/wget needed).
const walRestoreCommand = `test -f /wal-restore/%f && cp /wal-restore/%f %p && exit 0; echo %f > /wal-restore/.request; for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25 26 27 28 29 30; do sleep 1; test -f /wal-restore/%f && cp /wal-restore/%f %p && rm -f /wal-restore/%f && exit 0; test -f /wal-restore/.error && rm -f /wal-restore/.error && exit 1; done; exit 1`

// mandatoryPgParams are HA-required PostgreSQL parameters that are always set.
// User pg_params can override these.
var mandatoryPgParams = map[string]string{
	"listen_addresses":          "'*'",
	"wal_level":                 "replica",
	"max_wal_senders":           "10",
	"max_replication_slots":     "10",
	"hot_standby":               "on",
	"wal_log_hints":             "on",
	"max_slot_wal_keep_size":    "-1",
	"recovery_target_timeline":  "'latest'",
	"wal_keep_size":             "'512MB'",
	"shared_preload_libraries":  "'pg_stat_statements'",
	"pg_stat_statements.track":  "all",
}

// mandatoryHbaRules are required pg_hba.conf entries for HA operation.
var mandatoryHbaRules = []string{
	"local all all trust",
	"host all all 0.0.0.0/0 md5",
	"host replication repl_user 0.0.0.0/0 md5",
	"host replication backup_user 0.0.0.0/0 md5",
	"host replication postgres 0.0.0.0/0 md5",
}

// buildConfigMap creates the ConfigMap containing postgresql.conf and pg_hba.conf.
func buildConfigMap(cfg *pgswarmv1.ClusterConfig) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName(cfg.ClusterName, "config"),
			Namespace: cfg.Namespace,
			Labels:    clusterLabels(cfg.ClusterName, cfg.ProfileName, cfg.LabelSelector),
		},
		Data: map[string]string{
			"postgresql.conf": buildPostgresConf(cfg.PgParams, cfg.Archive, cfg.Backups),
			"pg_hba.conf":    buildHbaConf(cfg.HbaRules),
		},
	}
}

// buildPostgresConf generates the postgresql.conf content by merging mandatory HA params with user overrides.
func buildPostgresConf(userParams map[string]string, archive *pgswarmv1.ArchiveSpec, backups []*pgswarmv1.BackupConfig) string {
	merged := make(map[string]string, len(mandatoryPgParams)+len(userParams)+4)
	for k, v := range mandatoryPgParams {
		merged[k] = v
	}

	// Archive settings (before user params so user can override)
	if archive != nil && archive.Mode != "" {
		merged["archive_mode"] = "on"
		timeout := archive.ArchiveTimeoutSeconds
		if timeout <= 0 {
			timeout = 60
		}
		merged["archive_timeout"] = fmt.Sprintf("%d", timeout)

		switch archive.Mode {
		case "custom":
			merged["archive_command"] = fmt.Sprintf("'%s'", archive.ArchiveCommand)
		default:
			// Sidecar handles WAL archiving via shared emptyDir volumes
			merged["archive_command"] = "'cp %p /wal-staging/%f'"
			merged["restore_command"] = "'" + walRestoreCommand + "'"
		}
	} else if len(backups) > 0 {
		// Backup profiles configured — sidecar handles WAL archiving via shared emptyDir volumes
		merged["archive_mode"] = "on"
		timeout := int32(60)
		for _, b := range backups {
			if b.Physical != nil && b.Physical.ArchiveTimeoutSeconds > 0 {
				timeout = b.Physical.ArchiveTimeoutSeconds
				break
			}
		}
		merged["archive_timeout"] = fmt.Sprintf("%d", timeout)
		merged["archive_command"] = "'cp %p /wal-staging/%f'"
		merged["restore_command"] = "'" + walRestoreCommand + "'"
	} else {
		merged["archive_mode"] = "off"
	}

	// Incremental backups (PG 17+) require summarize_wal
	for _, b := range backups {
		if b.Physical != nil && b.Physical.IncrementalSchedule != "" {
			merged["summarize_wal"] = "on"
			break
		}
	}

	// User params override everything (escape hatch)
	for k, v := range userParams {
		merged[k] = v
	}

	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sb, "%s = %s\n", k, merged[k])
	}
	return sb.String()
}

// buildHbaConf generates the pg_hba.conf content with mandatory HA rules followed by user rules.
func buildHbaConf(userRules []string) string {
	var sb strings.Builder
	sb.WriteString("# TYPE  DATABASE  USER  ADDRESS  METHOD\n")
	for _, rule := range mandatoryHbaRules {
		sb.WriteString(rule)
		sb.WriteByte('\n')
	}
	for _, rule := range userRules {
		sb.WriteString(rule)
		sb.WriteByte('\n')
	}
	return sb.String()
}
