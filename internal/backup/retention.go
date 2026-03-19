package backup

import (
	"context"
	"strings"

	"github.com/rs/zerolog/log"
)

// RetentionWorker deletes expired backup sets and their associated files.
type RetentionWorker struct {
	sidecar       *Sidecar
	retentionSets int
	retentionDays int
}

// NewRetentionWorker creates a new retention worker.
func NewRetentionWorker(s *Sidecar, sets, days int) *RetentionWorker {
	if sets <= 0 {
		sets = 3
	}
	if days <= 0 {
		days = 30
	}
	return &RetentionWorker{
		sidecar:       s,
		retentionSets: sets,
		retentionDays: days,
	}
}

// RunOnce executes one retention pass: deletes old backup sets and their files.
func (rw *RetentionWorker) RunOnce(ctx context.Context) {
	if rw.sidecar.meta == nil {
		return
	}

	sets, err := rw.sidecar.meta.ListSets()
	if err != nil {
		log.Error().Err(err).Msg("retention: failed to list backup sets")
		return
	}

	if len(sets) <= rw.retentionSets {
		log.Debug().Int("sets", len(sets)).Int("retain", rw.retentionSets).Msg("retention: nothing to delete")
		return
	}

	// Delete oldest sets beyond the retention count
	toDelete := sets[rw.retentionSets:]
	for _, set := range toDelete {
		log.Info().Str("set_id", set.ID).Str("started_at", set.StartedAt).Msg("retention: deleting expired backup set")

		// Delete files for all backups in this set
		backups, err := rw.sidecar.meta.BackupsForSet(set.ID)
		if err != nil {
			log.Error().Err(err).Str("set_id", set.ID).Msg("retention: failed to list backups for set")
			continue
		}
		for _, b := range backups {
			remotePath := rw.sidecar.destPrefix() + b.Subfolder + "/" + b.Filename
			if err := rw.sidecar.dest.Delete(ctx, remotePath); err != nil {
				log.Warn().Err(err).Str("file", remotePath).Msg("retention: failed to delete backup file")
			}
			// Also delete manifest if it exists
			if b.Type == "base" || b.Type == "incremental" {
				manifestName := strings.TrimSuffix(b.Filename, ".tar.gz") + "_manifest.gz"
				manifestPath := rw.sidecar.destPrefix() + b.Subfolder + "/" + manifestName
				rw.sidecar.dest.Delete(ctx, manifestPath)
			}
		}

		// Delete WAL segments for this set
		walSegs, err := rw.sidecar.meta.WALSegmentsForSet(set.ID)
		if err != nil {
			log.Error().Err(err).Str("set_id", set.ID).Msg("retention: failed to list WAL segments for set")
			continue
		}
		for _, seg := range walSegs {
			remotePath := rw.sidecar.destPrefix() + "wal/" + seg.Name + ".gz"
			if err := rw.sidecar.dest.Delete(ctx, remotePath); err != nil {
				log.Warn().Err(err).Str("wal", seg.Name).Msg("retention: failed to delete WAL segment")
			}
		}

		// Delete the set from metadata (cascades to backups, wal_segments, backup_stats)
		if err := rw.sidecar.meta.DeleteSet(set.ID); err != nil {
			log.Error().Err(err).Str("set_id", set.ID).Msg("retention: failed to delete backup set from metadata")
		}
	}

	// Vacuum to reclaim space
	if err := rw.sidecar.meta.Vacuum(); err != nil {
		log.Warn().Err(err).Msg("retention: vacuum failed")
	}

	// Upload updated backups.db
	remoteMeta := rw.sidecar.destPrefix() + "backups.db"
	if err := uploadFile(ctx, rw.sidecar.dest, rw.sidecar.meta.Path(), remoteMeta); err != nil {
		log.Warn().Err(err).Msg("retention: failed to upload backups.db")
	}

	log.Info().Int("deleted_sets", len(toDelete)).Msg("retention pass completed")
}
