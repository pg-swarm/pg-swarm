package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
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

// ExecStreamFunc runs a command in the postgres container and streams its stdout
// to the provided io.Writer. Used for streaming backup data without buffering.
type ExecStreamFunc func(ctx context.Context, k8sClient kubernetes.Interface, restConfig *rest.Config, podName, namespace, cmd string, stdout io.Writer) error

// Executor runs pg_basebackup / pg_dump via K8s exec in the postgres
// container and uploads results to remote storage.
type Executor struct {
	pod         PodRef
	k8sClient   kubernetes.Interface
	restConfig  *rest.Config
	logger      zerolog.Logger
	execFn      ExecFunc
	execOutFn   ExecOutputFunc
	execStreamFn ExecStreamFunc
}

// NewExecutor creates a new Executor.
func NewExecutor(pod PodRef, k8sClient kubernetes.Interface, restConfig *rest.Config, logger zerolog.Logger, execFn ExecFunc, execOutFn ExecOutputFunc, execStreamFn ExecStreamFunc) *Executor {
	logger.Debug().
		Str("pod", pod.PodName).
		Str("namespace", pod.Namespace).
		Msg("NewExecutor: creating executor")
	return &Executor{
		pod:          pod,
		k8sClient:    k8sClient,
		restConfig:   restConfig,
		logger:       logger,
		execFn:       execFn,
		execOutFn:    execOutFn,
		execStreamFn: execStreamFn,
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

	// Run pg_basebackup into a temp directory in the pod.
	// This produces base.tar.gz and backup_manifest as separate files.
	tmpDir := fmt.Sprintf("/tmp/pg-basebackup-%s", ts)
	e.logger.Debug().Str("tmp_dir", tmpDir).Msg("ExecuteBaseBackup: running pg_basebackup to temp dir")
	backupCmd := fmt.Sprintf(
		"pg_basebackup -U postgres -D %s -Ft -z --checkpoint=fast --wal-method=none --no-password",
		tmpDir,
	)
	if err := e.execInPod(ctx, backupCmd); err != nil {
		result.Error = fmt.Errorf("run pg_basebackup: %w", err)
		e.logger.Debug().Err(err).Msg("ExecuteBaseBackup: pg_basebackup failed")
		return result
	}
	e.logger.Debug().Str("tmp_dir", tmpDir).Msg("ExecuteBaseBackup: pg_basebackup completed")

	// Stream base.tar.gz from pod temp dir to storage (already gzip-compressed by -z).
	tarKey := fmt.Sprintf("%s/base/%s/base.tar.gz", cfg.GetBasePath(), ts)
	e.logger.Debug().Str("key", tarKey).Msg("ExecuteBaseBackup: streaming base.tar.gz to storage")
	if err := e.streamFromPod(ctx, backend, tarKey, "cat "+tmpDir+"/base.tar.gz", nil); err != nil {
		result.Error = fmt.Errorf("upload base tar: %w", err)
		e.logger.Debug().Err(err).Msg("ExecuteBaseBackup: tar upload failed")
		e.execInPod(ctx, "rm -rf "+tmpDir)
		return result
	}
	e.logger.Debug().Str("key", tarKey).Msg("ExecuteBaseBackup: base.tar.gz uploaded successfully")

	// Get end LSN
	e.logger.Trace().Msg("ExecuteBaseBackup: querying end WAL LSN")
	endLSN, _ := e.execInPodOutput(ctx, "psql -U postgres -tAc \"SELECT pg_current_wal_lsn()\" 2>/dev/null || echo ''")
	result.WALEndLSN = strings.TrimSpace(endLSN)
	e.logger.Debug().Str("wal_end_lsn", result.WALEndLSN).Msg("ExecuteBaseBackup: end LSN captured")

	// Read backup_manifest from pod (small file, ~10-100 KB), gzip it, upload as standalone object.
	// This manifest is required for incremental backups via --incremental=<path>.
	e.logger.Trace().Str("tmp_dir", tmpDir).Msg("ExecuteBaseBackup: reading backup_manifest")
	manifestRaw, err := e.execInPodOutput(ctx, "cat "+tmpDir+"/backup_manifest")
	if err != nil {
		e.logger.Warn().Err(err).Msg("could not read backup_manifest (incremental backups will not be available)")
	} else {
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		if _, err := gw.Write([]byte(manifestRaw)); err != nil {
			e.logger.Warn().Err(err).Msg("could not gzip backup_manifest")
		} else if err := gw.Close(); err != nil {
			e.logger.Warn().Err(err).Msg("could not close gzip writer")
		} else {
			manifestKey := fmt.Sprintf("%s/base/%s/backup_manifest.gz", cfg.GetBasePath(), ts)
			e.logger.Trace().Str("manifest_key", manifestKey).Msg("ExecuteBaseBackup: uploading backup_manifest.gz")
			if err := backend.Upload(ctx, manifestKey, bytes.NewReader(buf.Bytes())); err != nil {
				e.logger.Warn().Err(err).Msg("failed to upload backup_manifest.gz (incremental backups will not be available)")
			} else {
				e.logger.Debug().Str("manifest_key", manifestKey).Msg("ExecuteBaseBackup: backup_manifest.gz uploaded successfully")
			}
		}
	}

	// Cleanup temp directory in pod.
	e.logger.Trace().Str("tmp_dir", tmpDir).Msg("ExecuteBaseBackup: cleaning up temp directory")
	if err := e.execInPod(ctx, "rm -rf "+tmpDir); err != nil {
		e.logger.Warn().Err(err).Msg("could not clean up temp directory (will be cleaned on next backup)")
	}

	// SizeBytes tracking is deferred pending a counting io.Reader wrapper.
	result.SizeBytes = 0

	result.BackupPath = tarKey
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

	// List all objects under the base/ prefix and find the latest backup_manifest.gz.
	// Keys look like: <base_path>/base/<timestamp>/backup_manifest.gz
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

	// Find the most recent backup_manifest.gz (keys sort lexically; pick the last one).
	var latestManifestKey string
	for _, obj := range objects {
		if strings.HasSuffix(obj.Key, "/backup_manifest.gz") {
			if obj.Key > latestManifestKey {
				latestManifestKey = obj.Key
			}
		}
	}

	if latestManifestKey == "" {
		e.logger.Info().Msg("no backup_manifest.gz found in prior base backups, running full base backup")
		r := e.ExecuteBaseBackup(ctx, cfg)
		r.BackupType = "incremental"
		return r
	}

	e.logger.Info().Str("manifest_key", latestManifestKey).Msg("found prior backup manifest, running incremental backup")

	// Download the gzip-compressed manifest file and decompress it.
	e.logger.Trace().Str("manifest_key", latestManifestKey).Msg("ExecuteIncrementalBackup: downloading prior backup_manifest.gz")
	manifestGzReader, err := backend.Download(ctx, latestManifestKey)
	if err != nil {
		result.Error = fmt.Errorf("download prior manifest: %w", err)
		e.logger.Debug().Err(err).Msg("ExecuteIncrementalBackup: failed to download prior manifest")
		return result
	}
	defer manifestGzReader.Close()

	// Decompress the gzip-compressed manifest.
	gr, err := gzip.NewReader(manifestGzReader)
	if err != nil {
		result.Error = fmt.Errorf("decompress prior manifest: %w", err)
		e.logger.Debug().Err(err).Msg("ExecuteIncrementalBackup: failed to decompress manifest")
		return result
	}
	defer gr.Close()

	var manifestBuf bytes.Buffer
	if _, err := manifestBuf.ReadFrom(gr); err != nil {
		result.Error = fmt.Errorf("read decompressed prior manifest: %w", err)
		return result
	}
	e.logger.Debug().Int("manifest_size", manifestBuf.Len()).Msg("ExecuteIncrementalBackup: prior manifest downloaded and decompressed")

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

	// Run pg_basebackup --incremental into a temp directory in the pod.
	// This produces incremental.tar.gz and backup_manifest as separate files.
	tmpDir := fmt.Sprintf("/tmp/pg-incr-%s", ts)
	e.logger.Debug().Str("tmp_dir", tmpDir).Str("manifest_path", manifestPath).Msg("ExecuteIncrementalBackup: running pg_basebackup --incremental to temp dir")
	incrCmd := fmt.Sprintf(
		"pg_basebackup -U postgres -D %s -Ft -z --checkpoint=fast --wal-method=none --no-password --incremental=%s",
		tmpDir, manifestPath,
	)
	if err := e.execInPod(ctx, incrCmd); err != nil {
		result.Error = fmt.Errorf("run pg_basebackup --incremental: %w", err)
		e.logger.Debug().Err(err).Msg("ExecuteIncrementalBackup: pg_basebackup failed")
		e.execInPod(ctx, fmt.Sprintf("rm -rf %s %s", tmpDir, manifestPath))
		return result
	}
	e.logger.Debug().Str("tmp_dir", tmpDir).Msg("ExecuteIncrementalBackup: pg_basebackup --incremental completed")

	// Stream incremental.tar.gz from pod temp dir to storage (already gzip-compressed by -z).
	tarKey := fmt.Sprintf("%s/incremental/%s/incremental.tar.gz", cfg.GetBasePath(), ts)
	e.logger.Debug().Str("key", tarKey).Msg("ExecuteIncrementalBackup: streaming incremental.tar.gz to storage")
	if err := e.streamFromPod(ctx, backend, tarKey, "cat "+tmpDir+"/base.tar.gz", nil); err != nil {
		result.Error = fmt.Errorf("upload incremental tar: %w", err)
		e.logger.Debug().Err(err).Msg("ExecuteIncrementalBackup: tar upload failed")
		e.execInPod(ctx, fmt.Sprintf("rm -rf %s %s", tmpDir, manifestPath))
		return result
	}
	e.logger.Debug().Str("key", tarKey).Msg("ExecuteIncrementalBackup: incremental.tar.gz uploaded successfully")

	endLSN, _ := e.execInPodOutput(ctx, "psql -U postgres -tAc \"SELECT pg_current_wal_lsn()\" 2>/dev/null || echo ''")
	result.WALEndLSN = strings.TrimSpace(endLSN)
	e.logger.Debug().Str("wal_end_lsn", result.WALEndLSN).Msg("ExecuteIncrementalBackup: end LSN captured")

	// Read backup_manifest from pod, gzip it, upload as standalone object.
	e.logger.Trace().Str("tmp_dir", tmpDir).Msg("ExecuteIncrementalBackup: reading backup_manifest")
	manifestRaw, err := e.execInPodOutput(ctx, "cat "+tmpDir+"/backup_manifest")
	if err != nil {
		e.logger.Warn().Err(err).Msg("could not read backup_manifest (future incrementals from this backup will not be available)")
	} else {
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		if _, err := gw.Write([]byte(manifestRaw)); err != nil {
			e.logger.Warn().Err(err).Msg("could not gzip backup_manifest")
		} else if err := gw.Close(); err != nil {
			e.logger.Warn().Err(err).Msg("could not close gzip writer")
		} else {
			manifestKey := fmt.Sprintf("%s/incremental/%s/backup_manifest.gz", cfg.GetBasePath(), ts)
			e.logger.Trace().Str("manifest_key", manifestKey).Msg("ExecuteIncrementalBackup: uploading backup_manifest.gz")
			if err := backend.Upload(ctx, manifestKey, bytes.NewReader(buf.Bytes())); err != nil {
				e.logger.Warn().Err(err).Msg("failed to upload backup_manifest.gz (future incrementals from this backup will not be available)")
			} else {
				e.logger.Debug().Str("manifest_key", manifestKey).Msg("ExecuteIncrementalBackup: backup_manifest.gz uploaded successfully")
			}
		}
	}

	// SizeBytes tracking is deferred pending a counting io.Reader wrapper.
	result.SizeBytes = 0

	// Cleanup temp directory and prior manifest file.
	e.logger.Trace().Str("tmp_dir", tmpDir).Str("manifest_path", manifestPath).Msg("ExecuteIncrementalBackup: cleaning up temporary files")
	e.execInPod(ctx, fmt.Sprintf("rm -rf %s %s", tmpDir, manifestPath))

	result.BackupPath = tarKey
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
	// Stream logical backups as plain SQL with gzip compression
	e.logger.Debug().Int("db_count", len(databases)).Str("timestamp", ts).Msg("ExecuteLogicalBackup: starting pg_dump stream loop")

	for i, db := range databases {
		e.logger.Debug().Str("database", db).Int("index", i+1).Int("total", len(databases)).Msg("ExecuteLogicalBackup: streaming database dump")

		// pg_dump -Fp outputs plain SQL to stdout
		dumpCmd := fmt.Sprintf("pg_dump -U postgres -Fp %s", db)
		key := fmt.Sprintf("%s/logical/%s/%s.sql.gz", cfg.GetBasePath(), ts, db)

		// Wrap the stream with gzip compression
		gzipWrap := func(w io.Writer) (io.Writer, func() error) {
			gw := gzip.NewWriter(w)
			return gw, gw.Close
		}

		e.logger.Debug().Str("database", db).Str("key", key).Msg("ExecuteLogicalBackup: streaming dump to storage")
		if err := e.streamFromPod(ctx, backend, key, dumpCmd, gzipWrap); err != nil {
			result.Error = fmt.Errorf("stream dump %s: %w", db, err)
			e.logger.Debug().Err(err).Str("database", db).Msg("ExecuteLogicalBackup: stream failed")
			return result
		}
	}

	result.Databases = databases
	// SizeBytes tracking requires wrapping the PipeReader with a counting reader; deferred for now.
	result.SizeBytes = 0
	result.BackupPath = fmt.Sprintf("%s/logical/%s/", cfg.GetBasePath(), ts)
	result.CompletedAt = time.Now()
	e.logger.Debug().
		Str("path", result.BackupPath).
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
		restoreCmd := fmt.Sprintf("psql -U postgres -d %s -f %s", targetDB, localPath)
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
	restoreCmd := fmt.Sprintf("pg_restore -U postgres -d %s --clean --if-exists %s",
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

// streamFromPod runs a pod command and pipes its stdout through an optional transform
// (e.g., gzip.Writer) directly to storage. The producer (pod exec) runs in a goroutine
// to allow concurrent upload from the pipe. Errors from both sides are captured and returned.
//
// The wrap parameter allows inserting an optional transformation layer (e.g., gzip compression).
// If wrap is nil, the stream goes directly to storage.
// If wrap is not nil, wrap returns the wrapped writer and a cleanup function (e.g., gw.Close).
func (e *Executor) streamFromPod(
	ctx context.Context,
	backend storage.Backend,
	key, cmd string,
	wrap func(io.Writer) (io.Writer, func() error),
) error {
	pr, pw := io.Pipe()

	var writerForCmd io.Writer = pw
	var closeExtra func() error

	if wrap != nil {
		writerForCmd, closeExtra = wrap(pw)
	}

	errCh := make(chan error, 1)
	go func() {
		err := e.execStreamFn(
			ctx,
			e.k8sClient,
			e.restConfig,
			e.pod.PodName,
			e.pod.Namespace,
			cmd,
			writerForCmd,
		)
		// Flush gzip footer (or other cleanup) before closing the pipe
		if closeExtra != nil {
			if flushErr := closeExtra(); flushErr != nil && err == nil {
				err = flushErr
			}
		}
		if err != nil {
			pw.CloseWithError(err)
		} else {
			pw.Close()
		}
		errCh <- err
	}()

	uploadErr := backend.Upload(ctx, key, pr)
	// Prevent goroutine leak: if upload fails, close the pipe reader so the
	// producer goroutine unblocks and can exit cleanly.
	if uploadErr != nil {
		pr.CloseWithError(uploadErr)
	}
	execErr := <-errCh

	if execErr != nil {
		return fmt.Errorf("pod exec stream: %w", execErr)
	}
	if uploadErr != nil {
		return fmt.Errorf("storage upload: %w", uploadErr)
	}
	return nil
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

