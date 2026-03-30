//go:build e2e

// Phase 6: Backup and restore tests.
//
// These tests require a backup store to be configured via environment variables:
//
//	E2E_BACKUP_STORE_TYPE  — "sftp", "gcs", or "s3" (default: skip)
//
// SFTP (easiest for in-cluster testing):
//
//	E2E_SFTP_HOST, E2E_SFTP_PORT, E2E_SFTP_USER, E2E_SFTP_BASE_PATH
//	E2E_SFTP_PASSWORD or E2E_SFTP_PRIVATE_KEY
//
// GCS:
//
//	E2E_GCS_BUCKET, E2E_GCS_PATH_PREFIX, E2E_GCS_SERVICE_ACCOUNT_JSON
//
// S3 / MinIO:
//
//	E2E_S3_BUCKET, E2E_S3_REGION, E2E_S3_PATH_PREFIX
//	E2E_S3_ACCESS_KEY_ID, E2E_S3_SECRET_ACCESS_KEY
//	E2E_S3_ENDPOINT (optional, e.g. http://minio:9000)
//	E2E_S3_FORCE_PATH_STYLE=true (for MinIO)

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"
)

// backupStoreEnv returns the configured store type or "" if not set.
func backupStoreEnv() string {
	return os.Getenv("E2E_BACKUP_STORE_TYPE")
}

// requireBackupEnv skips the test if backup store env vars are not set.
func (s *E2ESuite) requireBackupEnv() string {
	t := backupStoreEnv()
	if t == "" {
		s.T().Skip("E2E_BACKUP_STORE_TYPE not set — skipping backup tests")
	}
	return t
}

// buildStorePayload constructs the request body for creating a backup store.
func (s *E2ESuite) buildStorePayload(storeType string) map[string]interface{} {
	switch storeType {
	case "sftp":
		port, _ := strconv.Atoi(os.Getenv("E2E_SFTP_PORT"))
		if port == 0 {
			port = 22
		}
		return map[string]interface{}{
			"name":       "e2e-sftp-store",
			"store_type": "sftp",
			"config": map[string]interface{}{
				"host":      os.Getenv("E2E_SFTP_HOST"),
				"port":      port,
				"user":      os.Getenv("E2E_SFTP_USER"),
				"base_path": os.Getenv("E2E_SFTP_BASE_PATH"),
			},
			"credentials": map[string]interface{}{
				"password":    os.Getenv("E2E_SFTP_PASSWORD"),
				"private_key": os.Getenv("E2E_SFTP_PRIVATE_KEY"),
			},
		}
	case "gcs":
		return map[string]interface{}{
			"name":       "e2e-gcs-store",
			"store_type": "gcs",
			"config": map[string]interface{}{
				"bucket":      os.Getenv("E2E_GCS_BUCKET"),
				"path_prefix": os.Getenv("E2E_GCS_PATH_PREFIX"),
			},
			"credentials": map[string]interface{}{
				"service_account_json": os.Getenv("E2E_GCS_SERVICE_ACCOUNT_JSON"),
			},
		}
	case "s3":
		forcePathStyle, _ := strconv.ParseBool(os.Getenv("E2E_S3_FORCE_PATH_STYLE"))
		return map[string]interface{}{
			"name":       "e2e-s3-store",
			"store_type": "s3",
			"config": map[string]interface{}{
				"bucket":           os.Getenv("E2E_S3_BUCKET"),
				"region":           os.Getenv("E2E_S3_REGION"),
				"path_prefix":      os.Getenv("E2E_S3_PATH_PREFIX"),
				"endpoint":         os.Getenv("E2E_S3_ENDPOINT"),
				"force_path_style": forcePathStyle,
			},
			"credentials": map[string]interface{}{
				"access_key_id":     os.Getenv("E2E_S3_ACCESS_KEY_ID"),
				"secret_access_key": os.Getenv("E2E_S3_SECRET_ACCESS_KEY"),
			},
		}
	default:
		s.FailNow("unknown E2E_BACKUP_STORE_TYPE: " + storeType)
		return nil
	}
}

// Test_50_CreateBackupStoreAndConfigureCluster creates a backup store via the
// admin API, then updates the cluster config to enable logical backups using it.
func (s *E2ESuite) Test_50_CreateBackupStoreAndConfigureCluster() {
	storeType := s.requireBackupEnv()
	s.Require().NotEmpty(s.clusterID, "cluster ID not set — setup tests must pass first")

	// Create the backup store
	payload := s.buildStorePayload(storeType)
	store, err := s.api.CreateBackupStore(payload)
	s.Require().NoError(err, "failed to create backup store")
	s.Require().NotEmpty(store.ID)
	s.T().Logf("backup store created: id=%s type=%s", store.ID, store.StoreType)

	// Fetch current cluster config
	cluster, err := s.api.GetCluster(s.clusterID)
	s.Require().NoError(err)

	// Unmarshal the existing config and add a backup spec
	var config map[string]interface{}
	s.Require().NoError(json.Unmarshal(cluster.Config, &config))
	config["backup"] = map[string]interface{}{
		"store_id": store.ID,
		"logical": map[string]interface{}{
			"enabled":   true,
			"schedule":  "0 3 * * *", // 3am daily (cron-triggered; we'll manually trigger in tests)
			"databases": []string{testDBName},
			"format":    "custom",
		},
		"retention": map[string]interface{}{
			"logical_backup_count": 5,
		},
	}

	err = s.api.UpdateCluster(s.clusterID, map[string]interface{}{"config": config})
	s.Require().NoError(err, "failed to update cluster config with backup spec")
	s.T().Log("cluster config updated with backup spec")
}

// Test_51_TriggerLogicalBackupAndWait triggers an on-demand logical backup and
// waits up to 5 minutes for a backup.status event confirming completion.
func (s *E2ESuite) Test_51_TriggerLogicalBackupAndWait() {
	s.requireBackupEnv()
	s.Require().NotEmpty(s.clusterID, "cluster ID not set")

	// Ensure cluster is healthy before triggering
	err := s.k8s.WatchPodsReady(clusterName, 1, 2*time.Minute)
	s.Require().NoError(err, "cluster not healthy before backup trigger")

	resp, err := s.api.TriggerBackup(s.clusterID, "logical")
	s.Require().NoError(err, "failed to trigger logical backup")
	s.T().Logf("backup triggered: operation_id=%s", resp.OperationID)

	// Poll backup inventory until we see a completed entry.
	// The backup.status event is stored as a BackupInventory record by the event handler.
	var completedBackup *BackupInventoryItem
	err = WaitFor(5*time.Minute, "logical backup completed", func() bool {
		items, err := s.api.ListBackups(s.clusterID, 10)
		if err != nil {
			s.T().Logf("  polling backups: %v", err)
			return false
		}
		for i := range items {
			if items[i].BackupType == "logical" && items[i].Status == "completed" {
				completedBackup = &items[i]
				return true
			}
		}
		// Log current state
		if len(items) > 0 {
			s.T().Logf("  polling backups: latest=%s status=%s", items[0].BackupType, items[0].Status)
		} else {
			s.T().Log("  polling backups: no entries yet")
		}
		return false
	})
	s.Require().NoError(err, "logical backup did not complete within timeout")
	s.Require().NotNil(completedBackup)

	s.Assert().Equal("logical", completedBackup.BackupType)
	s.Assert().Equal("completed", completedBackup.Status)
	s.Assert().NotEmpty(completedBackup.BackupPath, "backup path should not be empty")
	s.Assert().Greater(completedBackup.SizeBytes, int64(0), "backup size should be > 0")

	s.lastBackupPath = completedBackup.BackupPath
	s.T().Logf("logical backup completed: path=%s size=%d bytes pg_version=%s",
		completedBackup.BackupPath, completedBackup.SizeBytes, completedBackup.PGVersion)
}

// Test_52_TriggerLogicalRestoreAndVerify restores the backup taken in Test_51
// into a scratch database on the same cluster and verifies the row count.
func (s *E2ESuite) Test_52_TriggerLogicalRestoreAndVerify() {
	s.requireBackupEnv()
	s.Require().NotEmpty(s.clusterID, "cluster ID not set")
	if s.lastBackupPath == "" {
		s.T().Skip("no backup path available — Test_51 must pass first")
	}

	// Create a scratch database for the restore target
	restoreDB := "e2e_restore_target"
	_, err := s.api.CreateClusterDatabase(s.clusterID, map[string]interface{}{
		"db_name":  restoreDB,
		"db_user":  testDBUser,
		"password": testDBPass,
	})
	s.Require().NoError(err, "failed to create restore target database")
	s.T().Logf("restore target database %s created", restoreDB)

	// Wait for the database to be ready
	err = WaitFor(2*time.Minute, "restore target db created", func() bool {
		dbs, err := s.api.ListClusterDatabases(s.clusterID)
		if err != nil {
			return false
		}
		for _, db := range dbs {
			if db.DBName == restoreDB && db.Status == "created" {
				return true
			}
		}
		return false
	})
	s.Require().NoError(err, "restore target database not ready")

	// Trigger restore into the scratch DB
	resp, err := s.api.TriggerRestore(s.clusterID, map[string]interface{}{
		"restore_type":    "logical",
		"backup_path":     s.lastBackupPath,
		"target_database": restoreDB,
	})
	s.Require().NoError(err, "failed to trigger restore")
	s.T().Logf("restore triggered: restore_id=%s", resp.RestoreID)

	// Wait for restore to complete — poll the restore operations list
	err = WaitFor(5*time.Minute, "logical restore completed", func() bool {
		items, err := s.api.ListRestoreOperations(s.clusterID)
		if err != nil {
			return false
		}
		for _, item := range items {
			if item.ID == resp.RestoreID {
				s.T().Logf("  restore status: %s", item.Status)
				return item.Status == "completed"
			}
		}
		return false
	})
	s.Require().NoError(err, "restore did not complete within timeout")
	s.T().Log("restore completed")

	// Verify the data exists in the restore target
	pg, err := NewPGClient(s.k8s, clusterName, pgPort)
	s.Require().NoError(err)
	defer pg.Close()

	count, err := pg.ExecDB(restoreDB, "SELECT count(*) FROM e2e_test")
	s.Require().NoError(err, "failed to query restored data")
	s.Assert().Equal(fmt.Sprintf("%d", s.rowsBefore), count,
		"restored row count mismatch: expected %d got %s", s.rowsBefore, count)
	s.T().Logf("restore verified: %s rows in %s.e2e_test", count, restoreDB)
}

// Test_53_TriggerBaseBackupOnReplica triggers a physical base backup on the
// replica pod and waits for it to complete.
func (s *E2ESuite) Test_53_TriggerBaseBackupOnReplica() {
	s.requireBackupEnv()
	s.Require().NotEmpty(s.clusterID, "cluster ID not set")

	// Ensure a replica exists
	_, err := s.k8s.GetReplicaPod(clusterName)
	s.Require().NoError(err, "no replica pod available — need at least 2 pods for base backup test")

	// Update cluster config to enable physical backups
	cluster, err := s.api.GetCluster(s.clusterID)
	s.Require().NoError(err)

	var config map[string]interface{}
	s.Require().NoError(json.Unmarshal(cluster.Config, &config))

	backup, _ := config["backup"].(map[string]interface{})
	if backup == nil {
		s.T().Skip("no backup config in cluster — Test_50 must pass first")
	}
	backup["physical"] = map[string]interface{}{
		"enabled":       true,
		"base_schedule": "0 2 * * *", // 2am daily (we'll trigger manually)
	}
	config["backup"] = backup

	err = s.api.UpdateCluster(s.clusterID, map[string]interface{}{"config": config})
	s.Require().NoError(err, "failed to update cluster config with physical backup spec")
	s.T().Log("physical backup enabled in cluster config")

	// Give the satellite time to push the config to the sidecar
	time.Sleep(10 * time.Second)

	// Trigger base backup — the satellite's backup handler selects a replica
	resp, err := s.api.TriggerBackup(s.clusterID, "base")
	s.Require().NoError(err, "failed to trigger base backup")
	s.T().Logf("base backup triggered: operation_id=%s", resp.OperationID)

	// Wait for a completed base backup entry
	err = WaitFor(10*time.Minute, "base backup completed", func() bool {
		items, err := s.api.ListBackups(s.clusterID, 20)
		if err != nil {
			return false
		}
		for _, item := range items {
			if item.BackupType == "base" && item.Status == "completed" {
				s.T().Logf("  base backup found: path=%s size=%d bytes", item.BackupPath, item.SizeBytes)
				return true
			}
		}
		if len(items) > 0 {
			s.T().Logf("  polling: latest=%s status=%s", items[0].BackupType, items[0].Status)
		}
		return false
	})
	s.Require().NoError(err, "base backup did not complete within timeout")
	s.T().Log("base backup completed successfully")
}
