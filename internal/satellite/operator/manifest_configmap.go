package operator

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
)

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
			"postgresql.conf": buildPostgresConf(cfg.PgParams, cfg.Archive),
			"pg_hba.conf":    buildHbaConf(cfg.HbaRules),
		},
	}
}

// buildPostgresConf generates the postgresql.conf content by merging mandatory HA params with user overrides.
func buildPostgresConf(userParams map[string]string, archive *pgswarmv1.ArchiveSpec) string {
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
		case "pvc":
			merged["archive_command"] = "'test ! -f /wal-archive/%f && cp %p /wal-archive/%f'"
		case "custom":
			merged["archive_command"] = fmt.Sprintf("'%s'", archive.ArchiveCommand)
		}
	} else {
		merged["archive_mode"] = "off"
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
