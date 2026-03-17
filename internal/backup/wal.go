package backup

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

const (
	walStagingDir = "/wal-staging"
	walRestoreDir = "/wal-restore"
	walRequestFile = walRestoreDir + "/.request"
	walErrorFile   = walRestoreDir + "/.error"
)

// WatchWALStaging polls /wal-staging/ for new WAL files deposited by
// PostgreSQL's archive_command (cp %p /wal-staging/%f), compresses them,
// uploads to the destination, records metadata, then deletes the local copy.
func (s *Sidecar) WatchWALStaging(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			entries, err := os.ReadDir(walStagingDir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
					continue
				}
				name := e.Name()
				localPath := walStagingDir + "/" + name
				remotePath := s.destPrefix() + "wal/" + name + ".gz"

				if err := uploadGzipped(ctx, s.dest, localPath, remotePath); err != nil {
					log.Error().Err(err).Str("wal", name).Msg("WAL upload failed")
					continue
				}

				if s.meta != nil {
					activeSetID, _ := s.meta.ActiveSetID()
					if activeSetID != "" {
						s.meta.InsertWALSegment(&WALSegment{
							Name:     name,
							SetID:    activeSetID,
							Timeline: walTimeline(name),
						})
					}
				}

				os.Remove(localPath)
				log.Debug().Str("wal", name).Msg("WAL archived via staging")
			}
		}
	}
}

// WatchWALRestore polls /wal-restore/.request for WAL fetch requests written
// by PostgreSQL's restore_command. When a request is found, the sidecar
// downloads the WAL segment from the destination, decompresses it, and places
// it at /wal-restore/<name> for the restore_command to pick up.
func (s *Sidecar) WatchWALRestore(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			data, err := os.ReadFile(walRequestFile)
			if err != nil {
				continue // no request pending
			}
			walName := strings.TrimSpace(string(data))
			if walName == "" {
				continue
			}

			remotePath := s.destPrefix() + "wal/" + walName + ".gz"
			localPath := walRestoreDir + "/" + walName

			if err := downloadAndDecompress(ctx, s.dest, remotePath, localPath); err != nil {
				log.Warn().Err(err).Str("wal", walName).Msg("WAL fetch failed")
				os.WriteFile(walErrorFile, []byte(walName), 0644)
			}

			os.Remove(walRequestFile)
		}
	}
}

// uploadGzipped and downloadAndDecompress are defined in physical.go.
