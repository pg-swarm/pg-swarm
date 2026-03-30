package backup

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/pg-swarm/pg-swarm/internal/sentinel/backup/storage"
)

// Result holds the outcome of a backup operation.
type Result struct {
	BackupType  string
	BackupPath  string
	SizeBytes   int64
	PGVersion   string
	WALStartLSN string
	WALEndLSN   string
	Databases   []string
	StartedAt   time.Time
	CompletedAt time.Time
	Error       error
}

// PodRef identifies the pod to exec commands into.
type PodRef struct {
	PodName   string
	Namespace string
}

// ExecFunc runs a command in the postgres container with no output expected.
type ExecFunc func(ctx context.Context, k8sClient kubernetes.Interface, restConfig *rest.Config, podName, namespace, cmd string) error

// ExecOutputFunc runs a command in the postgres container and returns stdout.
type ExecOutputFunc func(ctx context.Context, k8sClient kubernetes.Interface, restConfig *rest.Config, podName, namespace, cmd string) (string, error)

// Executor runs pg_basebackup / pg_dump via K8s exec in the postgres
// container and uploads results to remote storage.
type Executor struct {
	pod        PodRef
	k8sClient  kubernetes.Interface
	restConfig *rest.Config
	logger     zerolog.Logger
	execFn     ExecFunc
	execOutFn  ExecOutputFunc
}

// NewExecutor creates a new Executor.
func NewExecutor(pod PodRef, k8sClient kubernetes.Interface, restConfig *rest.Config, logger zerolog.Logger, execFn ExecFunc, execOutFn ExecOutputFunc) *Executor {
	logger.Debug().
		Str("pod", pod.PodName).
		Str("namespace", pod.Namespace).
		Msg("NewExecutor: creating executor")
	return &Executor{
		pod:        pod,
		k8sClient:  k8sClient,
		restConfig: restConfig,
		logger:     logger,
		execFn:     execFn,
		execOutFn:  execOutFn,
	}
}

func (e *Executor) execInPod(ctx context.Context, cmd string) error {
	e.logger.Trace().Str("pod", e.pod.PodName).Str("cmd", cmd).Msg("execInPod: executing")
	err := e.execFn(ctx, e.k8sClient, e.restConfig, e.pod.PodName, e.pod.Namespace, cmd)
	if err != nil {
		e.logger.Debug().Err(err).Str("cmd", cmd).Msg("execInPod: failed")
	} else {
		e.logger.Trace().Str("cmd", cmd).Msg("execInPod: succeeded")
	}
	return err
}

func (e *Executor) execInPodOutput(ctx context.Context, cmd string) (string, error) {
	e.logger.Trace().Str("pod", e.pod.PodName).Str("cmd", cmd).Msg("execInPodOutput: executing")
	out, err := e.execOutFn(ctx, e.k8sClient, e.restConfig, e.pod.PodName, e.pod.Namespace, cmd)
	if err != nil {
		e.logger.Debug().Err(err).Str("cmd", cmd).Msg("execInPodOutput: failed")
	} else {
		e.logger.Trace().Str("cmd", cmd).Int("output_len", len(out)).Msg("execInPodOutput: succeeded")
	}
	return out, err
}

// ExecuteBaseBackup runs pg_basebackup on the postgres container, tars and
// uploads the result to remote storage.
func (e *Executor) ExecuteBaseBackup(ctx context.Context, cfg *pgswarmv1.BackupConfig) *Result {
	e.logger.Debug().Str("base_path", cfg.GetBasePath()).Msg("ExecuteBaseBackup: starting")
	result := &Result{
		BackupType: "base",
		StartedAt:  time.Now(),
	}

	e.logger.Trace().Msg("ExecuteBaseBackup: creating storage backend")
	backend, err := storage.New(ctx, cfg.GetDestination(), e.logger)
	if err != nil {
		result.Error = fmt.Errorf("create storage backend: %w", err)
		e.logger.Debug().Err(err).Msg("ExecuteBaseBackup: storage backend creation failed")
		return result
	}
	defer backend.Close()

	ts := result.StartedAt.UTC().Format("20060102T150405")
	backupDir := fmt.Sprintf("/tmp/pg-backup-%s", ts)
	tarFile := backupDir + ".tar.gz"
	e.logger.Trace().Str("timestamp", ts).Str("backup_dir", backupDir).Msg("ExecuteBaseBackup: computed paths")

	// Get PG version
	e.logger.Trace().Msg("ExecuteBaseBackup: querying PG version")
	pgVersion, _ := e.execInPodOutput(ctx, "postgres --version | head -1")
	result.PGVersion = strings.TrimSpace(pgVersion)
	e.logger.Debug().Str("pg_version", result.PGVersion).Msg("ExecuteBaseBackup: PG version obtained")

	// Get start LSN
	e.logger.Trace().Msg("ExecuteBaseBackup: querying start WAL LSN")
	startLSN, _ := e.execInPodOutput(ctx, "psql -U postgres -tAc \"SELECT pg_current_wal_lsn()\" 2>/dev/null || echo ''")
	result.WALStartLSN = strings.TrimSpace(startLSN)
	e.logger.Debug().Str("wal_start_lsn", result.WALStartLSN).Msg("ExecuteBaseBackup: start LSN captured")

	// Run pg_basebackup
	e.logger.Debug().Str("backup_dir", backupDir).Msg("ExecuteBaseBackup: running pg_basebackup")
	basebackupCmd := fmt.Sprintf(
		"pg_basebackup --checkpoint=fast --wal-method=none -D %s -Ft -z -U postgres -h localhost",
		backupDir,
	)
	if err := e.execInPod(ctx, basebackupCmd); err != nil {
		result.Error = fmt.Errorf("pg_basebackup: %w", err)
		e.logger.Debug().Err(err).Msg("ExecuteBaseBackup: pg_basebackup failed, cleaning up")
		// Cleanup
		e.execInPod(ctx, fmt.Sprintf("rm -rf %s %s", backupDir, tarFile))
		return result
	}
	e.logger.Debug().Msg("ExecuteBaseBackup: pg_basebackup completed")

	// The -Ft flag already outputs tar format files. Package them into a single tar.gz.
	e.logger.Trace().Msg("ExecuteBaseBackup: packaging tar archive")
	packCmd := fmt.Sprintf("cd %s && tar czf %s . && rm -rf %s", backupDir, tarFile, backupDir)
	if err := e.execInPod(ctx, packCmd); err != nil {
		result.Error = fmt.Errorf("tar: %w", err)
		e.logger.Debug().Err(err).Msg("ExecuteBaseBackup: tar packaging failed")
		return result
	}

	// Get file size
	sizeStr, _ := e.execInPodOutput(ctx, fmt.Sprintf("stat -c %%s %s", tarFile))
	fmt.Sscanf(strings.TrimSpace(sizeStr), "%d", &result.SizeBytes)
	e.logger.Debug().Int64("size_bytes", result.SizeBytes).Msg("ExecuteBaseBackup: backup size determined")

	// Get end LSN
	endLSN, _ := e.execInPodOutput(ctx, "psql -U postgres -tAc \"SELECT pg_current_wal_lsn()\" 2>/dev/null || echo ''")
	result.WALEndLSN = strings.TrimSpace(endLSN)
	e.logger.Debug().Str("wal_end_lsn", result.WALEndLSN).Msg("ExecuteBaseBackup: end LSN captured")

	// Upload via storage backend.
	key := fmt.Sprintf("%s/base/%s/base.tar.gz", cfg.GetBasePath(), ts)
	e.logger.Debug().Str("key", key).Msg("ExecuteBaseBackup: uploading backup archive")
	if err := e.uploadFromPod(ctx, backend, key, tarFile); err != nil {
		result.Error = fmt.Errorf("upload: %w", err)
		e.logger.Debug().Err(err).Msg("ExecuteBaseBackup: upload failed")
		return result
	}
	e.logger.Debug().Str("key", key).Msg("ExecuteBaseBackup: backup archive uploaded")

	// Upload a metadata manifest.
	manifest := fmt.Sprintf(`{"backup_type":"base","timestamp":"%s","pg_version":"%s","wal_start_lsn":"%s","wal_end_lsn":"%s","size_bytes":%d}`,
		ts, result.PGVersion, result.WALStartLSN, result.WALEndLSN, result.SizeBytes)
	manifestKey := fmt.Sprintf("%s/base/%s/manifest.json", cfg.GetBasePath(), ts)
	e.logger.Trace().Str("manifest_key", manifestKey).Msg("ExecuteBaseBackup: uploading metadata manifest")
	if err := backend.Upload(ctx, manifestKey, strings.NewReader(manifest)); err != nil {
		e.logger.Warn().Err(err).Msg("failed to upload manifest (backup data is safe)")
	}

	// Upload pg_basebackup's own backup_manifest so incremental backups can
	// chain off this base backup via --incremental=<manifest>.
	pgManifestKey := fmt.Sprintf("%s/base/%s/backup_manifest", cfg.GetBasePath(), ts)
	e.logger.Trace().Str("pg_manifest_key", pgManifestKey).Msg("ExecuteBaseBackup: uploading pg backup_manifest")
	if err := e.uploadFromPod(ctx, backend, pgManifestKey, backupDir+"/backup_manifest"); err != nil {
		e.logger.Warn().Err(err).Msg("failed to upload pg backup_manifest (incremental chaining unavailable for this base)")
	}

	// Cleanup local
	e.logger.Trace().Msg("ExecuteBaseBackup: cleaning up local files")
	e.execInPod(ctx, fmt.Sprintf("rm -f %s", tarFile))

	result.BackupPath = key
	result.CompletedAt = time.Now()
	e.logger.Debug().
		Str("path", result.BackupPath).
		Int64("size_bytes", result.SizeBytes).
		Dur("duration", result.CompletedAt.Sub(result.StartedAt)).
		Msg("ExecuteBaseBackup: completed successfully")
	return result
}

// ExecuteIncrementalBackup runs pg_basebackup --incremental using the most
// recent base backup's pg_basebackup manifest. Falls back to a full base
// backup when no prior manifest exists or when PG < 17.
func (e *Executor) ExecuteIncrementalBackup(ctx context.Context, cfg *pgswarmv1.BackupConfig) *Result {
	e.logger.Debug().Str("base_path", cfg.GetBasePath()).Msg("ExecuteIncrementalBackup: starting")
	result := &Result{
		BackupType: "incremental",
		StartedAt:  time.Now(),
	}

	// Check PG version >= 17 for incremental backup support
	e.logger.Trace().Msg("ExecuteIncrementalBackup: checking PG version for incremental support")
	versionStr, _ := e.execInPodOutput(ctx, "psql -U postgres -tAc \"SHOW server_version_num\" 2>/dev/null || echo '0'")
	var versionNum int
	fmt.Sscanf(strings.TrimSpace(versionStr), "%d", &versionNum)
	e.logger.Debug().Int("version_num", versionNum).Msg("ExecuteIncrementalBackup: PG version number")

	if versionNum < 170000 {
		e.logger.Info().Int("version_num", versionNum).Msg("PG < 17, falling back to base backup")
		r := e.ExecuteBaseBackup(ctx, cfg)
		r.BackupType = "incremental"
		return r
	}

	// Open the storage backend to locate the prior base backup manifest.
	e.logger.Trace().Msg("ExecuteIncrementalBackup: creating storage backend")
	backend, err := storage.New(ctx, cfg.GetDestination(), e.logger)
	if err != nil {
		result.Error = fmt.Errorf("create storage backend: %w", err)
		e.logger.Debug().Err(err).Msg("ExecuteIncrementalBackup: storage backend creation failed")
		return result
	}
	defer backend.Close()

	// List all objects under the base/ prefix and find the latest manifest.json.
	// Keys look like: <base_path>/base/<timestamp>/manifest.json
	listPrefix := cfg.GetBasePath() + "/base/"
	e.logger.Trace().Str("prefix", listPrefix).Msg("ExecuteIncrementalBackup: listing prior base backups")
	objects, err := backend.List(ctx, listPrefix)
	if err != nil || len(objects) == 0 {
		e.logger.Info().Err(err).Int("count", len(objects)).Msg("no prior base backup found, running full base backup")
		r := e.ExecuteBaseBackup(ctx, cfg)
		r.BackupType = "incremental"
		return r
	}
	e.logger.Debug().Int("object_count", len(objects)).Msg("ExecuteIncrementalBackup: found objects in base/ prefix")

	// Find the most recent manifest.json (keys sort lexically; pick the last one).
	var latestManifestKey string
	for _, obj := range objects {
		if strings.HasSuffix(obj.Key, "/backup_manifest") || strings.HasSuffix(obj.Key, "/manifest.json") {
			// Prefer pg_basebackup's own backup_manifest over our metadata manifest.json
			if strings.HasSuffix(obj.Key, "/backup_manifest") {
				if obj.Key > latestManifestKey {
					latestManifestKey = obj.Key
				}
			} else if latestManifestKey == "" || (!strings.Contains(latestManifestKey, "/backup_manifest") && obj.Key > latestManifestKey) {
				latestManifestKey = obj.Key
			}
		}
	}

	if latestManifestKey == "" {
		e.logger.Info().Msg("no pg_basebackup manifest found in prior base backups, running full base backup")
		r := e.ExecuteBaseBackup(ctx, cfg)
		r.BackupType = "incremental"
		return r
	}

	e.logger.Info().Str("manifest_key", latestManifestKey).Msg("found prior backup manifest, running incremental backup")

	// Download the manifest file and write it to the postgres pod.
	e.logger.Trace().Str("manifest_key", latestManifestKey).Msg("ExecuteIncrementalBackup: downloading prior manifest")
	manifestReader, err := backend.Download(ctx, latestManifestKey)
	if err != nil {
		result.Error = fmt.Errorf("download prior manifest: %w", err)
		e.logger.Debug().Err(err).Msg("ExecuteIncrementalBackup: failed to download prior manifest")
		return result
	}
	defer manifestReader.Close()

	var manifestBuf bytes.Buffer
	if _, err := manifestBuf.ReadFrom(manifestReader); err != nil {
		result.Error = fmt.Errorf("read prior manifest: %w", err)
		return result
	}
	e.logger.Debug().Int("manifest_size", manifestBuf.Len()).Msg("ExecuteIncrementalBackup: prior manifest downloaded")

	// Write manifest to a temp file in the pod via base64-encoded exec.
	manifestPath := "/tmp/pg-prior-manifest"
	encoded := base64.StdEncoding.EncodeToString(manifestBuf.Bytes())
	e.logger.Trace().Msg("ExecuteIncrementalBackup: writing manifest to pod")
	writeScript := fmt.Sprintf("echo '%s' | base64 -d > %s", encoded, manifestPath)
	if err := e.execInPod(ctx, writeScript); err != nil {
		result.Error = fmt.Errorf("write manifest to pod: %w", err)
		return result
	}

	// Get PG version and LSNs.
	e.logger.Trace().Msg("ExecuteIncrementalBackup: querying PG version and LSNs")
	pgVersion, _ := e.execInPodOutput(ctx, "postgres --version | head -1")
	result.PGVersion = strings.TrimSpace(pgVersion)

	startLSN, _ := e.execInPodOutput(ctx, "psql -U postgres -tAc \"SELECT pg_current_wal_lsn()\" 2>/dev/null || echo ''")
	result.WALStartLSN = strings.TrimSpace(startLSN)
	e.logger.Debug().Str("pg_version", result.PGVersion).Str("wal_start_lsn", result.WALStartLSN).Msg("ExecuteIncrementalBackup: version and start LSN captured")

	ts := result.StartedAt.UTC().Format("20060102T150405")
	backupDir := fmt.Sprintf("/tmp/pg-incr-%s", ts)
	tarFile := backupDir + ".tar.gz"

	// Run pg_basebackup with --incremental pointing at the prior manifest.
	e.logger.Debug().Str("manifest_path", manifestPath).Str("backup_dir", backupDir).Msg("ExecuteIncrementalBackup: running pg_basebackup --incremental")
	incrCmd := fmt.Sprintf(
		"pg_basebackup --checkpoint=fast --wal-method=none --incremental=%s -D %s -Ft -z -U postgres -h localhost",
		manifestPath, backupDir,
	)
	if err := e.execInPod(ctx, incrCmd); err != nil {
		result.Error = fmt.Errorf("pg_basebackup --incremental: %w", err)
		e.logger.Debug().Err(err).Msg("ExecuteIncrementalBackup: pg_basebackup --incremental failed, cleaning up")
		e.execInPod(ctx, fmt.Sprintf("rm -rf %s %s %s", backupDir, tarFile, manifestPath))
		return result
	}
	e.logger.Debug().Msg("ExecuteIncrementalBackup: pg_basebackup --incremental completed")

	// Pack into a single archive.
	e.logger.Trace().Msg("ExecuteIncrementalBackup: packaging tar archive")
	packCmd := fmt.Sprintf("cd %s && tar czf %s . && rm -rf %s", backupDir, tarFile, backupDir)
	if err := e.execInPod(ctx, packCmd); err != nil {
		result.Error = fmt.Errorf("tar incremental backup: %w", err)
		return result
	}

	sizeStr, _ := e.execInPodOutput(ctx, fmt.Sprintf("stat -c %%s %s", tarFile))
	fmt.Sscanf(strings.TrimSpace(sizeStr), "%d", &result.SizeBytes)

	endLSN, _ := e.execInPodOutput(ctx, "psql -U postgres -tAc \"SELECT pg_current_wal_lsn()\" 2>/dev/null || echo ''")
	result.WALEndLSN = strings.TrimSpace(endLSN)
	e.logger.Debug().Int64("size_bytes", result.SizeBytes).Str("wal_end_lsn", result.WALEndLSN).Msg("ExecuteIncrementalBackup: size and end LSN captured")

	key := fmt.Sprintf("%s/incremental/%s/incremental.tar.gz", cfg.GetBasePath(), ts)
	e.logger.Debug().Str("key", key).Msg("ExecuteIncrementalBackup: uploading incremental archive")
	if err := e.uploadFromPod(ctx, backend, key, tarFile); err != nil {
		result.Error = fmt.Errorf("upload incremental: %w", err)
		e.logger.Debug().Err(err).Msg("ExecuteIncrementalBackup: upload failed")
		return result
	}

	// Upload our metadata manifest and also save pg_basebackup's own manifest
	// so future incrementals can chain off this backup.
	manifest := fmt.Sprintf(`{"backup_type":"incremental","timestamp":"%s","pg_version":"%s","wal_start_lsn":"%s","wal_end_lsn":"%s","size_bytes":%d,"based_on":"%s"}`,
		ts, result.PGVersion, result.WALStartLSN, result.WALEndLSN, result.SizeBytes, latestManifestKey)
	manifestKey := fmt.Sprintf("%s/incremental/%s/manifest.json", cfg.GetBasePath(), ts)
	e.logger.Trace().Str("manifest_key", manifestKey).Msg("ExecuteIncrementalBackup: uploading metadata manifest")
	if err := backend.Upload(ctx, manifestKey, strings.NewReader(manifest)); err != nil {
		e.logger.Warn().Err(err).Msg("failed to upload incremental manifest")
	}

	// Upload pg_basebackup's backup_manifest from this incremental so it can
	// serve as the base for the next incremental in the chain.
	pgManifestKey := fmt.Sprintf("%s/incremental/%s/backup_manifest", cfg.GetBasePath(), ts)
	e.logger.Trace().Str("pg_manifest_key", pgManifestKey).Msg("ExecuteIncrementalBackup: uploading pg backup_manifest")
	if err := e.uploadFromPod(ctx, backend, pgManifestKey, backupDir+"/backup_manifest"); err != nil {
		e.logger.Warn().Err(err).Msg("failed to upload pg backup_manifest (incremental chaining may break)")
	}

	// Cleanup
	e.logger.Trace().Msg("ExecuteIncrementalBackup: cleaning up local files")
	e.execInPod(ctx, fmt.Sprintf("rm -f %s %s", tarFile, manifestPath))

	result.BackupPath = key
	result.CompletedAt = time.Now()
	e.logger.Debug().
		Str("path", result.BackupPath).
		Int64("size_bytes", result.SizeBytes).
		Dur("duration", result.CompletedAt.Sub(result.StartedAt)).
		Msg("ExecuteIncrementalBackup: completed successfully")
	return result
}

// ExecuteLogicalBackup runs pg_dump for each configured database.
func (e *Executor) ExecuteLogicalBackup(ctx context.Context, cfg *pgswarmv1.BackupConfig) *Result {
	e.logger.Debug().Str("base_path", cfg.GetBasePath()).Msg("ExecuteLogicalBackup: starting")
	result := &Result{
		BackupType: "logical",
		StartedAt:  time.Now(),
	}

	logicalCfg := cfg.GetLogical()
	if logicalCfg == nil {
		e.logger.Debug().Msg("ExecuteLogicalBackup: no logical backup configuration")
		result.Error = fmt.Errorf("no logical backup configuration")
		return result
	}

	e.logger.Trace().Msg("ExecuteLogicalBackup: creating storage backend")
	backend, err := storage.New(ctx, cfg.GetDestination(), e.logger)
	if err != nil {
		result.Error = fmt.Errorf("create storage backend: %w", err)
		e.logger.Debug().Err(err).Msg("ExecuteLogicalBackup: storage backend creation failed")
		return result
	}
	defer backend.Close()

	// Get PG version
	e.logger.Trace().Msg("ExecuteLogicalBackup: querying PG version")
	pgVersion, _ := e.execInPodOutput(ctx, "postgres --version | head -1")
	result.PGVersion = strings.TrimSpace(pgVersion)
	e.logger.Debug().Str("pg_version", result.PGVersion).Msg("ExecuteLogicalBackup: PG version obtained")

	// Determine databases to back up
	databases := logicalCfg.GetDatabases()
	if len(databases) == 0 {
		// Discover all user databases
		e.logger.Debug().Msg("ExecuteLogicalBackup: no databases configured, discovering user databases")
		dbList, err := e.execInPodOutput(ctx, "psql -U postgres -tAc \"SELECT datname FROM pg_database WHERE datistemplate = false AND datname != 'postgres'\"")
		if err != nil {
			result.Error = fmt.Errorf("list databases: %w", err)
			return result
		}
		for _, db := range strings.Split(strings.TrimSpace(dbList), "\n") {
			db = strings.TrimSpace(db)
			if db != "" {
				databases = append(databases, db)
			}
		}
		e.logger.Debug().Strs("discovered_databases", databases).Msg("ExecuteLogicalBackup: databases discovered")
	}
	if len(databases) == 0 {
		e.logger.Debug().Msg("ExecuteLogicalBackup: no databases to back up")
		result.Error = fmt.Errorf("no databases to back up")
		return result
	}

	ts := result.StartedAt.UTC().Format("20060102T150405")
	format := logicalCfg.GetFormat()
	if format == "" {
		format = "custom"
	}
	e.logger.Debug().Int("db_count", len(databases)).Str("format", format).Str("timestamp", ts).Msg("ExecuteLogicalBackup: starting pg_dump loop")

	var totalSize int64
	for i, db := range databases {
		ext := dumpExtension(format)
		dumpFile := fmt.Sprintf("/tmp/pg-dump-%s-%s%s", db, ts, ext)

		e.logger.Debug().Str("database", db).Int("index", i+1).Int("total", len(databases)).Msg("ExecuteLogicalBackup: dumping database")
		dumpCmd := fmt.Sprintf("pg_dump -U postgres -h localhost -F%s -f %s %s",
			dumpFormatFlag(format), dumpFile, db)

		if err := e.execInPod(ctx, dumpCmd); err != nil {
			result.Error = fmt.Errorf("pg_dump %s: %w", db, err)
			e.logger.Debug().Err(err).Str("database", db).Msg("ExecuteLogicalBackup: pg_dump failed")
			return result
		}

		// Get size
		sizeStr, _ := e.execInPodOutput(ctx, fmt.Sprintf("stat -c %%s %s", dumpFile))
		var sz int64
		fmt.Sscanf(strings.TrimSpace(sizeStr), "%d", &sz)
		totalSize += sz
		e.logger.Trace().Str("database", db).Int64("size_bytes", sz).Msg("ExecuteLogicalBackup: dump size")

		// Upload
		key := fmt.Sprintf("%s/logical/%s/%s%s", cfg.GetBasePath(), ts, db, ext)
		e.logger.Debug().Str("database", db).Str("key", key).Msg("ExecuteLogicalBackup: uploading dump")
		if err := e.uploadFromPod(ctx, backend, key, dumpFile); err != nil {
			result.Error = fmt.Errorf("upload %s: %w", db, err)
			e.logger.Debug().Err(err).Str("database", db).Msg("ExecuteLogicalBackup: upload failed")
			return result
		}

		// Cleanup
		e.execInPod(ctx, fmt.Sprintf("rm -f %s", dumpFile))
		e.logger.Trace().Str("database", db).Msg("ExecuteLogicalBackup: dump file cleaned up")
	}

	result.Databases = databases
	result.SizeBytes = totalSize
	result.BackupPath = fmt.Sprintf("%s/logical/%s/", cfg.GetBasePath(), ts)
	result.CompletedAt = time.Now()
	e.logger.Debug().
		Str("path", result.BackupPath).
		Int64("total_size_bytes", totalSize).
		Int("db_count", len(databases)).
		Dur("duration", result.CompletedAt.Sub(result.StartedAt)).
		Msg("ExecuteLogicalBackup: completed successfully")
	return result
}

// ExecuteLogicalRestore downloads a dump and restores it via pg_restore/psql.
func (e *Executor) ExecuteLogicalRestore(ctx context.Context, cmd *pgswarmv1.RestoreCommand, cfg *pgswarmv1.BackupConfig) error {
	e.logger.Debug().
		Str("restore_id", cmd.GetRestoreId()).
		Str("backup_path", cmd.GetBackupPath()).
		Str("target_db", cmd.GetTargetDatabase()).
		Msg("ExecuteLogicalRestore: starting")

	e.logger.Trace().Msg("ExecuteLogicalRestore: creating storage backend")
	backend, err := storage.New(ctx, cfg.GetDestination(), e.logger)
	if err != nil {
		e.logger.Debug().Err(err).Msg("ExecuteLogicalRestore: storage backend creation failed")
		return fmt.Errorf("create storage backend: %w", err)
	}
	defer backend.Close()

	// Download the backup file to the pod
	localPath := fmt.Sprintf("/tmp/restore-%s", cmd.GetRestoreId())
	e.logger.Debug().Str("backup_path", cmd.GetBackupPath()).Str("local_path", localPath).Msg("ExecuteLogicalRestore: downloading backup")
	if err := e.downloadToPod(ctx, backend, cmd.GetBackupPath(), localPath); err != nil {
		e.logger.Debug().Err(err).Msg("ExecuteLogicalRestore: download failed")
		return fmt.Errorf("download backup: %w", err)
	}
	defer e.execInPod(ctx, fmt.Sprintf("rm -f %s", localPath))

	targetDB := cmd.GetTargetDatabase()

	// Determine format by file extension
	if strings.HasSuffix(cmd.GetBackupPath(), ".sql") {
		// Plain SQL — use psql
		e.logger.Debug().Str("target_db", targetDB).Msg("ExecuteLogicalRestore: restoring via psql (plain SQL)")
		restoreCmd := fmt.Sprintf("psql -U postgres -h localhost -d %s -f %s", targetDB, localPath)
		err := e.execInPod(ctx, restoreCmd)
		if err != nil {
			e.logger.Debug().Err(err).Msg("ExecuteLogicalRestore: psql restore failed")
		} else {
			e.logger.Debug().Msg("ExecuteLogicalRestore: psql restore completed")
		}
		return err
	}

	// Custom/directory format — use pg_restore
	e.logger.Debug().Str("target_db", targetDB).Msg("ExecuteLogicalRestore: restoring via pg_restore (custom format)")
	restoreCmd := fmt.Sprintf("pg_restore -U postgres -h localhost -d %s --clean --if-exists %s",
		targetDB, localPath)
	err = e.execInPod(ctx, restoreCmd)
	if err != nil {
		e.logger.Debug().Err(err).Msg("ExecuteLogicalRestore: pg_restore failed")
	} else {
		e.logger.Debug().Msg("ExecuteLogicalRestore: pg_restore completed")
	}
	return err
}

// ExecutePITRRestore performs a point-in-time recovery.
func (e *Executor) ExecutePITRRestore(ctx context.Context, cmd *pgswarmv1.RestoreCommand, cfg *pgswarmv1.BackupConfig) error {
	e.logger.Debug().
		Str("restore_id", cmd.GetRestoreId()).
		Str("backup_path", cmd.GetBackupPath()).
		Str("restore_mode", cmd.GetRestoreMode()).
		Msg("ExecutePITRRestore: starting")

	e.logger.Trace().Msg("ExecutePITRRestore: creating storage backend")
	backend, err := storage.New(ctx, cfg.GetDestination(), e.logger)
	if err != nil {
		e.logger.Debug().Err(err).Msg("ExecutePITRRestore: storage backend creation failed")
		return fmt.Errorf("create storage backend: %w", err)
	}
	defer backend.Close()

	targetTime := cmd.GetTargetTime().AsTime().UTC().Format("2006-01-02 15:04:05+00")
	e.logger.Debug().Str("target_time", targetTime).Msg("ExecutePITRRestore: target time computed")

	// 1. Stop PostgreSQL
	e.logger.Info().Str("target_time", targetTime).Msg("stopping PG for PITR restore")
	e.logger.Debug().Msg("ExecutePITRRestore: step 1 — stopping PostgreSQL")
	if err := e.execInPod(ctx, "pg_ctl stop -m fast -D /var/lib/postgresql/data/pgdata"); err != nil {
		e.logger.Debug().Err(err).Msg("ExecutePITRRestore: failed to stop PostgreSQL")
		return fmt.Errorf("stop pg: %w", err)
	}
	e.logger.Debug().Msg("ExecutePITRRestore: PostgreSQL stopped")

	// 2. Download and extract base backup to PGDATA
	e.logger.Debug().Str("backup_path", cmd.GetBackupPath()).Msg("ExecutePITRRestore: step 2 — downloading base backup")
	if err := e.downloadToPod(ctx, backend, cmd.GetBackupPath(), "/tmp/pitr-base.tar.gz"); err != nil {
		e.logger.Debug().Err(err).Msg("ExecutePITRRestore: failed to download base backup")
		return fmt.Errorf("download base backup: %w", err)
	}
	e.logger.Debug().Msg("ExecutePITRRestore: extracting base backup to PGDATA")
	extractScript := `set -e
PGDATA="/var/lib/postgresql/data/pgdata"
rm -rf "$PGDATA"/*
cd "$PGDATA"
tar xzf /tmp/pitr-base.tar.gz
rm -f /tmp/pitr-base.tar.gz`
	if err := e.execInPod(ctx, extractScript); err != nil {
		e.logger.Debug().Err(err).Msg("ExecutePITRRestore: failed to extract base backup")
		return fmt.Errorf("extract base backup: %w", err)
	}
	e.logger.Debug().Msg("ExecutePITRRestore: base backup extracted")

	// 3. Create recovery.signal and configure recovery
	e.logger.Debug().Str("target_time", targetTime).Msg("ExecutePITRRestore: step 3 — configuring recovery parameters")
	recoveryScript := fmt.Sprintf(`set -e
PGDATA="/var/lib/postgresql/data/pgdata"
touch "$PGDATA/recovery.signal"
sed -i '/^recovery_target_time/d' "$PGDATA/postgresql.auto.conf" 2>/dev/null || true
sed -i '/^restore_command/d' "$PGDATA/postgresql.auto.conf" 2>/dev/null || true
echo "recovery_target_time = '%s'" >> "$PGDATA/postgresql.auto.conf"
echo "recovery_target_action = 'promote'" >> "$PGDATA/postgresql.auto.conf"`,
		targetTime)
	if err := e.execInPod(ctx, recoveryScript); err != nil {
		e.logger.Debug().Err(err).Msg("ExecutePITRRestore: failed to configure recovery")
		return fmt.Errorf("configure recovery: %w", err)
	}

	// 4. PG will be restarted by the pod wrapper script automatically
	e.logger.Info().Str("target_time", targetTime).Msg("PITR recovery configured, PG will restart via wrapper")
	e.logger.Debug().Msg("ExecutePITRRestore: completed successfully")
	return nil
}

// uploadFromPod reads a file from the postgres container via exec and uploads it to storage.
func (e *Executor) uploadFromPod(ctx context.Context, backend storage.Backend, key, podPath string) error {
	e.logger.Trace().Str("key", key).Str("pod_path", podPath).Msg("uploadFromPod: reading file from pod")
	output, err := e.execInPodOutput(ctx, fmt.Sprintf("cat %s", podPath))
	if err != nil {
		e.logger.Debug().Err(err).Str("pod_path", podPath).Msg("uploadFromPod: failed to read file")
		return fmt.Errorf("read file %s: %w", podPath, err)
	}
	e.logger.Trace().Str("key", key).Int("data_len", len(output)).Msg("uploadFromPod: uploading to storage")
	err = backend.Upload(ctx, key, bytes.NewReader([]byte(output)))
	if err != nil {
		e.logger.Debug().Err(err).Str("key", key).Msg("uploadFromPod: upload failed")
	} else {
		e.logger.Trace().Str("key", key).Msg("uploadFromPod: upload succeeded")
	}
	return err
}

// downloadToPod downloads a file from storage and writes it into the pod.
func (e *Executor) downloadToPod(ctx context.Context, backend storage.Backend, key, podPath string) error {
	e.logger.Trace().Str("key", key).Str("pod_path", podPath).Msg("downloadToPod: downloading from storage")
	r, err := backend.Download(ctx, key)
	if err != nil {
		e.logger.Debug().Err(err).Str("key", key).Msg("downloadToPod: download failed")
		return fmt.Errorf("download %s: %w", key, err)
	}
	defer r.Close()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		e.logger.Debug().Err(err).Str("key", key).Msg("downloadToPod: read failed")
		return fmt.Errorf("read %s: %w", key, err)
	}
	e.logger.Debug().Str("key", key).Int("data_len", buf.Len()).Msg("downloadToPod: data downloaded")

	// Write file to the shared volume from the sentinel container directly.
	// The /var/lib/postgresql/data volume is mounted in both containers.
	// For paths under /tmp (not shared), use exec to write via base64.
	if err := os.MkdirAll(filepath.Dir(podPath), 0755); err != nil {
		// Fallback: write via exec if direct write fails (path not on shared volume)
		e.logger.Trace().Str("pod_path", podPath).Msg("downloadToPod: direct write failed, falling back to exec")
		encoded := base64.StdEncoding.EncodeToString(buf.Bytes())
		script := fmt.Sprintf("echo '%s' | base64 -d > %s", encoded, podPath)
		return e.execInPod(ctx, script)
	}
	e.logger.Trace().Str("pod_path", podPath).Msg("downloadToPod: writing file directly to shared volume")
	return os.WriteFile(podPath, buf.Bytes(), 0644)
}

func dumpFormatFlag(format string) string {
	switch format {
	case "custom":
		return "c"
	case "plain":
		return "p"
	case "directory":
		return "d"
	default:
		return "c"
	}
}

func dumpExtension(format string) string {
	switch format {
	case "custom":
		return ".dump"
	case "plain":
		return ".sql"
	case "directory":
		return ".dir"
	default:
		return ".dump"
	}
}
