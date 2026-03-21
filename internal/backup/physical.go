package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// RunBaseBackup executes a full pg_basebackup against the local PostgreSQL,
// uploads the result, and notifies the primary sidecar.
func (s *Sidecar) RunBaseBackup(ctx context.Context) error {
	if allowed, reason := s.isClusterStatusRunning(ctx); !allowed {
		return fmt.Errorf("backup blocked: cluster not RUNNING: %s", reason)
	}
	start := time.Now()
	timestamp := start.UTC().Format("20060102T150405Z")
	backupID := uuid.New().String()
	filename := timestamp + ".tar.gz"
	subfolder := "base"
	tmpDir, err := os.MkdirTemp("", "basebackup-")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	log.Info().Str("backup_id", backupID).Msg("starting base backup")

	// Run pg_basebackup
	cmd := exec.CommandContext(ctx, "pg_basebackup",
		"-h", s.cfg.PGHost, "-p", s.cfg.PGPort, "-U", s.cfg.PGUser,
		"-D", tmpDir, "-Ft", "-z", "-Xs", "-P",
	)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+s.cfg.PGPassword)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		s.notifyPrimary(ctx, &BackupCompleteRequest{
			ID: backupID, Type: "base", Filename: filename, Subfolder: subfolder,
			Error: fmt.Sprintf("pg_basebackup failed: %v", err),
		})
		return fmt.Errorf("pg_basebackup: %w", err)
	}

	// Calculate size and upload
	var totalSize int64
	entries, _ := os.ReadDir(tmpDir)
	for _, e := range entries {
		info, _ := e.Info()
		if info != nil {
			totalSize += info.Size()
		}
	}

	// Upload base.tar.gz
	baseTar := filepath.Join(tmpDir, "base.tar.gz")
	remotePath := s.destPrefix() + subfolder + "/" + filename
	if err := uploadFile(ctx, s.dest, baseTar, remotePath); err != nil {
		// Try uploading each file individually if base.tar.gz doesn't exist
		for _, e := range entries {
			entryPath := filepath.Join(tmpDir, e.Name())
			entryRemote := s.destPrefix() + subfolder + "/" + timestamp + "_" + e.Name()
			uploadFile(ctx, s.dest, entryPath, entryRemote)
		}
	}

	// Upload backup_manifest if present
	manifestPath := filepath.Join(tmpDir, "backup_manifest")
	if _, err := os.Stat(manifestPath); err == nil {
		manifestRemote := s.destPrefix() + subfolder + "/" + timestamp + "_manifest.gz"
		uploadGzipped(ctx, s.dest, manifestPath, manifestRemote)
	}

	duration := time.Since(start).Seconds()
	log.Info().
		Str("backup_id", backupID).
		Float64("duration_secs", duration).
		Int64("size_bytes", totalSize).
		Msg("base backup completed")

	// Notify primary
	s.notifyPrimary(ctx, &BackupCompleteRequest{
		ID:           backupID,
		Type:         "base",
		Filename:     filename,
		Subfolder:    subfolder,
		SizeBytes:    totalSize,
		DurationSecs: duration,
	})

	// Report status with health context
	if s.reporter != nil {
		hs := s.checkHealth(ctx)
		s.reporter.ReportBackupWithHealth(ctx, "base", "completed", totalSize, "", &hs)
	}

	return nil
}

// RunIncrementalBackup executes an incremental pg_basebackup. Falls back to
// a full base backup if the manifest is missing or WAL gap detected.
func (s *Sidecar) RunIncrementalBackup(ctx context.Context) error {
	if allowed, reason := s.isClusterStatusRunning(ctx); !allowed {
		return fmt.Errorf("backup blocked: cluster not RUNNING: %s", reason)
	}
	start := time.Now()
	timestamp := start.UTC().Format("20060102T150405Z")
	backupID := uuid.New().String()
	filename := timestamp + ".tar.gz"
	subfolder := "incremental"
	tmpDir, err := os.MkdirTemp("", "incrbackup-")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	log.Info().Str("backup_id", backupID).Msg("starting incremental backup")

	// Download latest manifest from destination
	manifestFile := filepath.Join(tmpDir, "prev_manifest")
	hasManifest := s.downloadLatestManifest(ctx, manifestFile)

	if !hasManifest {
		log.Info().Msg("no manifest found — falling back to base backup")
		return s.RunBaseBackup(ctx)
	}

	// Try incremental backup
	backupDir := filepath.Join(tmpDir, "data")
	cmd := exec.CommandContext(ctx, "pg_basebackup",
		"-h", s.cfg.PGHost, "-p", s.cfg.PGPort, "-U", s.cfg.PGUser,
		"-D", backupDir, "--incremental="+manifestFile, "-Ft", "-z", "-Xs", "-P",
	)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+s.cfg.PGPassword)

	var stderr bytes.Buffer
	cmd.Stdout = os.Stdout
	cmd.Stderr = &stderr
	err = cmd.Run()

	if err != nil {
		errMsg := stderr.String()
		if strings.Contains(errMsg, "manifest requires WAL") {
			log.Warn().Msg("incremental backup failed on standby (WAL gap) — falling back to base")
			return s.RunBaseBackup(ctx)
		}
		s.notifyPrimary(ctx, &BackupCompleteRequest{
			ID: backupID, Type: "incremental", Filename: filename, Subfolder: subfolder,
			Error: fmt.Sprintf("pg_basebackup --incremental failed: %v: %s", err, errMsg),
		})
		return fmt.Errorf("pg_basebackup --incremental: %w", err)
	}

	// Upload
	var totalSize int64
	entries, _ := os.ReadDir(backupDir)
	for _, e := range entries {
		info, _ := e.Info()
		if info != nil {
			totalSize += info.Size()
		}
		entryPath := filepath.Join(backupDir, e.Name())
		entryRemote := s.destPrefix() + subfolder + "/" + timestamp + "_" + e.Name()
		uploadFile(ctx, s.dest, entryPath, entryRemote)
	}

	// Upload manifest
	manifestPath := filepath.Join(backupDir, "backup_manifest")
	if _, err := os.Stat(manifestPath); err == nil {
		manifestRemote := s.destPrefix() + subfolder + "/" + timestamp + "_manifest.gz"
		uploadGzipped(ctx, s.dest, manifestPath, manifestRemote)
	}

	duration := time.Since(start).Seconds()
	log.Info().
		Str("backup_id", backupID).
		Float64("duration_secs", duration).
		Int64("size_bytes", totalSize).
		Msg("incremental backup completed")

	s.notifyPrimary(ctx, &BackupCompleteRequest{
		ID:           backupID,
		Type:         "incremental",
		Filename:     filename,
		Subfolder:    subfolder,
		SizeBytes:    totalSize,
		DurationSecs: duration,
	})

	if s.reporter != nil {
		hs := s.checkHealth(ctx)
		s.reporter.ReportBackupWithHealth(ctx, "incremental", "completed", totalSize, "", &hs)
	}
	return nil
}

// downloadLatestManifest tries to find and download the latest backup manifest
// from the destination (either from base/ or incremental/ subfolder).
func (s *Sidecar) downloadLatestManifest(ctx context.Context, outPath string) bool {
	// Try incremental manifests first, then base manifests
	for _, subfolder := range []string{"incremental", "base"} {
		prefix := s.destPrefix() + subfolder + "/"
		keys, err := s.dest.List(ctx, prefix)
		if err != nil {
			continue
		}
		// Find the latest manifest file
		var latestManifest string
		for _, key := range keys {
			if strings.HasSuffix(key, "_manifest.gz") {
				if key > latestManifest {
					latestManifest = key
				}
			}
		}
		if latestManifest != "" {
			if err := downloadAndDecompress(ctx, s.dest, latestManifest, outPath); err == nil {
				return true
			}
		}
	}
	return false
}

// uploadGzipped compresses a local file and uploads it.
func uploadGzipped(ctx context.Context, dest interface {
	Upload(ctx context.Context, remotePath string, r io.Reader) error
}, localPath, remotePath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	pr, pw := io.Pipe()
	go func() {
		gw := gzip.NewWriter(pw)
		io.Copy(gw, f)
		gw.Close()
		pw.Close()
	}()

	return dest.Upload(ctx, remotePath, pr)
}

// downloadAndDecompress downloads a gzipped file and decompresses it.
// Writes to a temp file first, then renames atomically so that concurrent
// readers (e.g. restore_command polling with test -f) never see a partial file.
func downloadAndDecompress(ctx context.Context, dest interface {
	Download(ctx context.Context, remotePath string, w io.Writer) error
}, remotePath, localPath string) error {
	pr, pw := io.Pipe()
	go func() {
		err := dest.Download(ctx, remotePath, pw)
		pw.CloseWithError(err)
	}()

	gr, err := gzip.NewReader(pr)
	if err != nil {
		return err
	}
	defer gr.Close()

	tmpPath := localPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	if _, err := io.Copy(f, gr); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}
	f.Close()

	return os.Rename(tmpPath, localPath)
}

// notifyPrimary sends a backup completion notification to the primary sidecar.
func (s *Sidecar) notifyPrimary(ctx context.Context, req *BackupCompleteRequest) {
	if s.notifier != nil {
		if err := s.notifier.NotifyBackupComplete(ctx, req); err != nil {
			log.Warn().Err(err).Str("backup_id", req.ID).Msg("failed to notify primary (will retry)")
		}
	} else if s.meta != nil {
		// Single-replica mode: record directly
		a := &APIServer{sidecar: s}
		// Simulate the notification locally
		status := "completed"
		if req.Error != "" {
			status = "failed"
		}
		if req.Type == "base" && req.Error == "" {
			s.meta.SealActiveSet()
			s.meta.CreateBackupSet(req.PGVersion, req.WALStartLSN)
		}
		activeSetID, _ := s.meta.ActiveSetID()
		if activeSetID != "" {
			s.meta.InsertBackup(&BackupRecord{
				ID:          req.ID,
				SetID:       activeSetID,
				Type:        req.Type,
				Filename:    req.Filename,
				Subfolder:   req.Subfolder,
				SizeBytes:   req.SizeBytes,
				WALStartLSN: req.WALStartLSN,
				WALEndLSN:   req.WALEndLSN,
				Status:      status,
				Error:       req.Error,
				DatabaseName: req.DatabaseName,
			})
		}
		// Upload updated metadata
		remoteMeta := s.destPrefix() + "backups.db"
		uploadFile(ctx, s.dest, s.meta.Path(), remoteMeta)
		_ = a // suppress unused
	}
}
