package backup

import (
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/pg-swarm/pg-swarm/internal/sentinel/backup/storage"
)

const (
	walStagingDir   = "/wal-staging"
	walPollInterval = 1 * time.Second
)

// WALArchiver watches the WAL staging directory on the primary pod,
// compresses each completed WAL segment with gzip, uploads it to
// remote storage, and deletes the local copy on success.
type WALArchiver struct {
	backupCfg *pgswarmv1.BackupConfig
	logger    zerolog.Logger
}

// NewWALArchiver creates a new WAL archiver.
func NewWALArchiver(cfg *pgswarmv1.BackupConfig, logger zerolog.Logger) *WALArchiver {
	l := logger.With().Str("component", "wal-archiver").Logger()
	l.Debug().
		Str("base_path", cfg.GetBasePath()).
		Str("store_id", cfg.GetStoreId()).
		Msg("NewWALArchiver: creating WAL archiver")
	return &WALArchiver{
		backupCfg: cfg,
		logger:    l,
	}
}

// Run polls the WAL staging directory and uploads new segments.
// It blocks until ctx is cancelled.
func (w *WALArchiver) Run(ctx context.Context) {
	w.logger.Info().Str("dir", walStagingDir).Msg("WAL archiver started")

	w.logger.Trace().Msg("Run: creating storage backend")
	backend, err := storage.New(ctx, w.backupCfg.GetDestination(), w.logger)
	if err != nil {
		w.logger.Error().Err(err).Msg("failed to create storage backend for WAL archiver")
		return
	}
	defer backend.Close()
	w.logger.Debug().Msg("Run: storage backend created, entering poll loop")

	ticker := time.NewTicker(walPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.logger.Info().Msg("WAL archiver stopped")
			return
		case <-ticker.C:
			w.archivePending(ctx, backend)
		}
	}
}

// archivePending scans the staging directory and uploads any WAL files found.
func (w *WALArchiver) archivePending(ctx context.Context, backend storage.Backend) {
	entries, err := os.ReadDir(walStagingDir)
	if err != nil {
		if !os.IsNotExist(err) {
			w.logger.Warn().Err(err).Msg("failed to read WAL staging directory")
		}
		return
	}

	candidates := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		// Skip partial files (PG writes to temp name, then renames)
		if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".tmp") {
			w.logger.Trace().Str("file", name).Msg("archivePending: skipping partial/temp file")
			continue
		}

		candidates++
		w.logger.Trace().Str("file", name).Msg("archivePending: archiving WAL segment")
		if err := w.archiveFile(ctx, backend, name); err != nil {
			w.logger.Error().Err(err).Str("file", name).Msg("failed to archive WAL segment")
		}
	}
	if candidates > 0 {
		w.logger.Debug().Int("count", candidates).Msg("archivePending: processed WAL segments")
	}
}

// archiveFile compresses and uploads a single WAL file, then deletes the local copy.
func (w *WALArchiver) archiveFile(ctx context.Context, backend storage.Backend, name string) error {
	localPath := filepath.Join(walStagingDir, name)
	w.logger.Trace().Str("file", name).Str("local_path", localPath).Msg("archiveFile: opening WAL segment")

	f, err := os.Open(localPath)
	if err != nil {
		w.logger.Debug().Err(err).Str("file", name).Msg("archiveFile: failed to open WAL file")
		return err
	}
	defer f.Close()

	// Pipe gzip-compressed data directly to the storage backend.
	pr, pw := io.Pipe()
	gw := gzip.NewWriter(pw)

	go func() {
		_, copyErr := io.Copy(gw, f)
		if copyErr != nil {
			w.logger.Debug().Err(copyErr).Str("file", name).Msg("archiveFile: gzip copy failed")
			gw.Close()
			pw.CloseWithError(copyErr)
			return
		}
		gw.Close()
		pw.Close()
	}()

	key := w.walKey(name)
	w.logger.Trace().Str("file", name).Str("key", key).Msg("archiveFile: uploading compressed WAL segment")
	if err := backend.Upload(ctx, key, pr); err != nil {
		w.logger.Debug().Err(err).Str("key", key).Msg("archiveFile: upload failed")
		return err
	}

	// Upload succeeded — remove local file.
	if err := os.Remove(localPath); err != nil {
		w.logger.Warn().Err(err).Str("file", name).Msg("failed to remove archived WAL file")
	} else {
		w.logger.Trace().Str("file", name).Msg("archiveFile: local file removed")
	}

	w.logger.Debug().Str("file", name).Str("key", key).Msg("WAL segment archived")
	return nil
}

// walKey builds the storage key for a WAL segment:
// <base_path>/wal/<filename>.gz
func (w *WALArchiver) walKey(name string) string {
	key := w.backupCfg.GetBasePath() + "/wal/" + name + ".gz"
	w.logger.Trace().Str("name", name).Str("key", key).Msg("walKey: computed storage key")
	return key
}
