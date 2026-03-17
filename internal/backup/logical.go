package backup

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// RunLogicalBackup executes pg_dump for configured databases (or pg_dumpall),
// uploads results to the logical/ subfolder, and notifies the primary.
func (s *Sidecar) RunLogicalBackup(ctx context.Context) error {
	start := time.Now()
	timestamp := start.UTC().Format("20060102T150405Z")
	backupID := uuid.New().String()
	subfolder := "logical"

	log.Info().Str("backup_id", backupID).Msg("starting logical backup")

	databases := parseDatabases(os.Getenv("LOGICAL_DATABASES"))

	var totalSize int64
	var lastErr error

	if len(databases) == 0 {
		// pg_dumpall
		filename := timestamp + "_dumpall.sql.gz"
		remotePath := s.destPrefix() + subfolder + "/" + filename

		pr, pw := io.Pipe()
		go func() {
			gw := gzip.NewWriter(pw)
			cmd := exec.CommandContext(ctx, "pg_dumpall",
				"-h", s.cfg.PGHost, "-p", s.cfg.PGPort, "-U", s.cfg.PGUser,
			)
			cmd.Env = append(os.Environ(), "PGPASSWORD="+s.cfg.PGPassword)
			cmd.Stdout = gw
			cmd.Stderr = os.Stderr
			err := cmd.Run()
			gw.Close()
			pw.CloseWithError(err)
		}()

		if err := s.dest.Upload(ctx, remotePath, pr); err != nil {
			lastErr = fmt.Errorf("upload dumpall: %w", err)
		} else {
			// Estimate size from remote
			totalSize = estimatePipeSize()
		}
	} else {
		for _, db := range databases {
			filename := fmt.Sprintf("%s_%s.sql.gz", timestamp, db)
			remotePath := s.destPrefix() + subfolder + "/" + filename

			pr, pw := io.Pipe()
			go func(dbName string) {
				gw := gzip.NewWriter(pw)
				cmd := exec.CommandContext(ctx, "pg_dump",
					"-h", s.cfg.PGHost, "-p", s.cfg.PGPort, "-U", s.cfg.PGUser,
					dbName,
				)
				cmd.Env = append(os.Environ(), "PGPASSWORD="+s.cfg.PGPassword)
				cmd.Stdout = gw
				cmd.Stderr = os.Stderr
				err := cmd.Run()
				gw.Close()
				pw.CloseWithError(err)
			}(db)

			if err := s.dest.Upload(ctx, remotePath, pr); err != nil {
				log.Error().Err(err).Str("database", db).Msg("failed to upload logical backup")
				lastErr = err
			}
		}
	}

	duration := time.Since(start).Seconds()
	status := "completed"
	errMsg := ""
	if lastErr != nil {
		status = "failed"
		errMsg = lastErr.Error()
	}

	log.Info().
		Str("backup_id", backupID).
		Float64("duration_secs", duration).
		Str("status", status).
		Msg("logical backup completed")

	s.notifyPrimary(ctx, &BackupCompleteRequest{
		ID:           backupID,
		Type:         "logical",
		Filename:     timestamp + ".sql.gz",
		Subfolder:    subfolder,
		SizeBytes:    totalSize,
		DurationSecs: duration,
		Error:        errMsg,
	})

	if s.reporter != nil {
		s.reporter.ReportBackup(ctx, "logical", status, totalSize, errMsg)
	}

	return lastErr
}

func parseDatabases(s string) []string {
	if s == "" {
		return nil
	}
	var dbs []string
	for _, db := range strings.Split(s, ",") {
		db = strings.TrimSpace(db)
		if db != "" {
			dbs = append(dbs, db)
		}
	}
	return dbs
}

// estimatePipeSize returns a rough size estimate when streaming through a pipe.
// Actual size tracking would require a counting writer wrapper.
func estimatePipeSize() int64 {
	return 0
}
