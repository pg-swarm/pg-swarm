package operator

import (
	"encoding/json"
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
	"listen_addresses":         "'*'",
	"wal_level":                "replica",
	"max_wal_senders":          "10",
	"max_replication_slots":    "10",
	"hot_standby":              "on",
	"wal_log_hints":            "on",
	"max_slot_wal_keep_size":   "-1",
	"recovery_target_timeline": "'latest'",
	"wal_keep_size":            "'512MB'",
	"shared_preload_libraries": "'pg_stat_statements'",
	"pg_stat_statements.track": "all",
	"password_encryption":      "'scram-sha-256'",
}

// defaultPgParams are sensible production defaults applied to every cluster.
// Profile pg_params override these. mandatoryPgParams override everything.
var defaultPgParams = map[string]string{
	// Connections
	"max_connections": "100",
	// Memory
	"shared_buffers":       "256MB",
	"effective_cache_size": "4GB",
	"work_mem":             "4MB",
	"maintenance_work_mem": "128MB",
	"huge_pages":           "try",
	// WAL
	"wal_buffers":                  "16MB",
	"min_wal_size":                 "1GB",
	"max_wal_size":                 "4GB",
	"checkpoint_timeout":           "15min",
	"checkpoint_completion_target": "0.9",
	// Query Planner
	"random_page_cost":          "1.1",
	"seq_page_cost":             "1.0",
	"effective_io_concurrency":  "200",
	"default_statistics_target": "100",
	"jit":                       "on",
	// Replication
	"track_commit_timestamp": "on",
	"synchronous_commit":     "on",
	"wal_receiver_timeout":   "60s",
	"wal_sender_timeout":     "60s",
	// Logging
	"log_min_duration_statement":  "200",
	"log_statement":               "none",
	"log_line_prefix":             "'%m [%p] %q[user=%u,db=%d] '",
	"log_checkpoints":             "on",
	"log_connections":             "off",
	"log_disconnections":          "off",
	"log_lock_waits":              "off",
	"log_temp_files":              "-1",
	"log_autovacuum_min_duration": "-1",
	// Autovacuum
	"autovacuum":                      "on",
	"autovacuum_max_workers":          "3",
	"autovacuum_naptime":              "1min",
	"autovacuum_vacuum_threshold":     "50",
	"autovacuum_vacuum_scale_factor":  "0.2",
	"autovacuum_analyze_threshold":    "50",
	"autovacuum_analyze_scale_factor": "0.1",
	// Client Defaults
	"timezone":                            "'UTC'",
	"statement_timeout":                   "0",
	"idle_in_transaction_session_timeout": "0",
	"lock_timeout":                        "0",
	"default_text_search_config":          "'pg_catalog.english'",
}

// mandatoryHbaRules are required pg_hba.conf entries for HA operation.
var mandatoryHbaRules = []string{
	"local all all trust",
	"host all all 0.0.0.0/0 scram-sha-256",
	"host replication repl_user 0.0.0.0/0 scram-sha-256",
	"host replication postgres 0.0.0.0/0 scram-sha-256",
}

// recoveryRulesConfigMapName returns the ConfigMap name for recovery rules.
func recoveryRulesConfigMapName(clusterName string) string {
	return resourceName(clusterName, "recovery-rules")
}

// buildRecoveryRulesConfigMap creates the ConfigMap containing recovery rules JSON
// for the sentinel sidecar to watch and apply.
func buildRecoveryRulesConfigMap(cfg *pgswarmv1.ClusterConfig) *corev1.ConfigMap {
	rulesJSON := "[]"
	if len(cfg.RecoveryRules) > 0 {
		type rule struct {
			Name            string `json:"name"`
			Pattern         string `json:"pattern"`
			Severity        string `json:"severity"`
			Action          string `json:"action"`
			ExecCommand     string `json:"exec_command,omitempty"`
			CooldownSeconds int32  `json:"cooldown_seconds"`
			Enabled         bool   `json:"enabled"`
			Category        string `json:"category"`
		}
		var rules []rule
		for _, r := range cfg.RecoveryRules {
			rules = append(rules, rule{
				Name:            r.Name,
				Pattern:         r.Pattern,
				Severity:        r.Severity,
				Action:          r.Action,
				ExecCommand:     r.ExecCommand,
				CooldownSeconds: r.CooldownSeconds,
				Enabled:         r.Enabled,
				Category:        r.Category,
			})
		}
		if data, err := json.Marshal(rules); err == nil {
			rulesJSON = string(data)
		}
	}
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      recoveryRulesConfigMapName(cfg.ClusterName),
			Namespace: cfg.Namespace,
			Labels:    clusterLabels(cfg.ClusterName, cfg.ProfileName, cfg.LabelSelector),
		},
		Data: map[string]string{
			"rules.json": rulesJSON,
		},
	}
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
			"pg_hba.conf":     buildHbaConf(cfg),
		},
	}
}

// buildPostgresConf generates the postgresql.conf content by layering:
// 1. Production defaults (lowest priority)
// 2. Archive settings
// 3. User/profile pg_params (override defaults)
// 4. Mandatory HA params (highest priority, cannot be overridden)
func buildPostgresConf(userParams map[string]string, archive *pgswarmv1.ArchiveSpec) string {
	merged := make(map[string]string, len(defaultPgParams)+len(mandatoryPgParams)+len(userParams)+4)

	// 1. Production defaults (lowest priority)
	for k, v := range defaultPgParams {
		merged[k] = v
	}

	// 2. Archive settings
	if archive != nil && archive.Mode == "custom" {
		merged["archive_mode"] = "on"
		timeout := archive.ArchiveTimeoutSeconds
		if timeout <= 0 {
			timeout = 60
		}
		merged["archive_timeout"] = fmt.Sprintf("%d", timeout)
		merged["archive_command"] = fmt.Sprintf("'%s'", archive.ArchiveCommand)
	} else {
		merged["archive_mode"] = "off"
	}

	// 3. User/profile params override defaults
	for k, v := range userParams {
		merged[k] = v
	}

	// 4. Mandatory HA params (highest priority — cannot be overridden)
	for k, v := range mandatoryPgParams {
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

// buildHbaConf generates the pg_hba.conf content with mandatory HA rules,
// profile-level user rules, and cluster-level database access rules.
func buildHbaConf(cfg *pgswarmv1.ClusterConfig) string {
	var sb strings.Builder
	sb.WriteString("# TYPE  DATABASE  USER  ADDRESS  METHOD\n")

	// 1. Mandatory rules (replication, local access)
	for _, rule := range mandatoryHbaRules {
		sb.WriteString(rule)
		sb.WriteByte('\n')
	}

	// 2. Profile-level user rules
	for _, rule := range cfg.HbaRules {
		sb.WriteString(rule)
		sb.WriteByte('\n')
	}

	// 3. Cluster-level database access rules (auto-generated from cluster_databases)
	for _, cdb := range cfg.ClusterDatabases {
		cidrs := cdb.AllowedCidrs
		if len(cidrs) == 0 {
			cidrs = []string{"0.0.0.0/0"}
		}
		for _, cidr := range cidrs {
			sb.WriteString(fmt.Sprintf("host %s %s %s scram-sha-256", cdb.DbName, cdb.DbUser, cidr))
			sb.WriteByte('\n')
		}
	}

	return sb.String()
}
