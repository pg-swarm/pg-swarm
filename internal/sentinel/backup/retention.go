package backup

import (
	"context"
	"sort"
	"strings"
	"time"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/pg-swarm/pg-swarm/internal/sentinel/backup/storage"
	"github.com/rs/zerolog"
)

// applyRetention enforces the retention policy by deleting old backups and WAL
// segments from storage. Called after each successful backup.
func applyRetention(ctx context.Context, backend storage.Backend, cfg *pgswarmv1.BackupConfig, logger zerolog.Logger) {
	logger.Debug().Msg("applyRetention: starting")
	ret := cfg.GetRetention()
	if ret == nil {
		logger.Debug().Msg("applyRetention: no retention policy configured, skipping")
		return
	}

	basePath := cfg.GetBasePath()
	logger.Trace().
		Str("base_path", basePath).
		Int32("base_count", ret.BaseBackupCount).
		Int32("incr_count", ret.IncrementalBackupCount).
		Int32("logical_count", ret.LogicalBackupCount).
		Int32("wal_days", ret.WalRetentionDays).
		Msg("applyRetention: retention policy details")

	if ret.BaseBackupCount > 0 {
		logger.Debug().Int32("keep", ret.BaseBackupCount).Msg("applyRetention: pruning base backups")
		pruneByCount(ctx, backend, basePath+"/base/", int(ret.BaseBackupCount), logger)
	}
	if ret.IncrementalBackupCount > 0 {
		logger.Debug().Int32("keep", ret.IncrementalBackupCount).Msg("applyRetention: pruning incremental backups")
		pruneByCount(ctx, backend, basePath+"/incremental/", int(ret.IncrementalBackupCount), logger)
	}
	if ret.LogicalBackupCount > 0 {
		logger.Debug().Int32("keep", ret.LogicalBackupCount).Msg("applyRetention: pruning logical backups")
		pruneByCount(ctx, backend, basePath+"/logical/", int(ret.LogicalBackupCount), logger)
	}
	if ret.WalRetentionDays > 0 {
		logger.Debug().Int32("days", ret.WalRetentionDays).Msg("applyRetention: pruning WAL segments")
		pruneWAL(ctx, backend, basePath+"/wal/", int(ret.WalRetentionDays), logger)
	}
	logger.Debug().Msg("applyRetention: done")
}

// pruneByCount lists objects under prefix, groups them by backup directory,
// sorts by name (which contains timestamps), and deletes the oldest beyond keep count.
func pruneByCount(ctx context.Context, backend storage.Backend, prefix string, keep int, logger zerolog.Logger) {
	logger.Trace().Str("prefix", prefix).Int("keep", keep).Msg("pruneByCount: listing objects")
	objects, err := backend.List(ctx, prefix)
	if err != nil {
		logger.Warn().Err(err).Str("prefix", prefix).Msg("retention: failed to list objects")
		return
	}
	logger.Debug().Str("prefix", prefix).Int("object_count", len(objects)).Msg("pruneByCount: objects found")

	if len(objects) <= keep {
		logger.Trace().Str("prefix", prefix).Int("objects", len(objects)).Int("keep", keep).Msg("pruneByCount: within retention limit, nothing to prune")
		return
	}

	// Sort by key (timestamps sort lexically) — newest last
	sort.Slice(objects, func(i, j int) bool {
		return objects[i].Key < objects[j].Key
	})

	// Group by backup set (each backup may have data + manifest.json)
	groups := groupByBackupDir(objects)
	logger.Debug().Str("prefix", prefix).Int("group_count", len(groups)).Msg("pruneByCount: backup groups identified")
	if len(groups) <= keep {
		logger.Trace().Str("prefix", prefix).Int("groups", len(groups)).Int("keep", keep).Msg("pruneByCount: groups within retention limit")
		return
	}

	// Delete oldest groups
	toDelete := groups[:len(groups)-keep]
	logger.Debug().Str("prefix", prefix).Int("deleting_groups", len(toDelete)).Msg("pruneByCount: deleting oldest groups")
	for _, g := range toDelete {
		logger.Trace().Str("group_dir", g.dir).Int("key_count", len(g.keys)).Msg("pruneByCount: deleting group")
		for _, key := range g.keys {
			if err := backend.Delete(ctx, key); err != nil {
				logger.Warn().Err(err).Str("key", key).Msg("retention: failed to delete object")
			} else {
				logger.Info().Str("key", key).Msg("retention: deleted old backup object")
			}
		}
	}

	logger.Info().Str("prefix", prefix).Int("deleted_groups", len(toDelete)).Int("kept", keep).Msg("retention: pruning complete")
}

type backupGroup struct {
	dir  string
	keys []string
}

// groupByBackupDir groups storage objects by their parent directory.
func groupByBackupDir(objects []storage.ObjectInfo) []backupGroup {
	m := make(map[string]*backupGroup)
	var order []string

	for _, obj := range objects {
		dir := obj.Key
		if idx := strings.LastIndex(obj.Key, "/"); idx > 0 {
			dir = obj.Key[:idx]
		}
		if g, ok := m[dir]; ok {
			g.keys = append(g.keys, obj.Key)
		} else {
			m[dir] = &backupGroup{dir: dir, keys: []string{obj.Key}}
			order = append(order, dir)
		}
	}

	result := make([]backupGroup, 0, len(order))
	for _, dir := range order {
		result = append(result, *m[dir])
	}
	return result
}

// pruneWAL deletes WAL segments older than the retention window.
func pruneWAL(ctx context.Context, backend storage.Backend, prefix string, retentionDays int, logger zerolog.Logger) {
	logger.Trace().Str("prefix", prefix).Int("retention_days", retentionDays).Msg("pruneWAL: listing WAL segments")
	objects, err := backend.List(ctx, prefix)
	if err != nil {
		logger.Warn().Err(err).Str("prefix", prefix).Msg("retention: failed to list WAL segments")
		return
	}
	logger.Debug().Str("prefix", prefix).Int("wal_count", len(objects)).Msg("pruneWAL: WAL segments found")

	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	logger.Trace().Time("cutoff", cutoff).Msg("pruneWAL: retention cutoff time")
	deleted := 0
	for _, obj := range objects {
		if obj.LastModified.IsZero() {
			logger.Trace().Str("key", obj.Key).Msg("pruneWAL: skipping — no LastModified")
			continue
		}
		if obj.LastModified.Before(cutoff) {
			logger.Trace().Str("key", obj.Key).Time("modified", obj.LastModified).Msg("pruneWAL: deleting expired WAL segment")
			if err := backend.Delete(ctx, obj.Key); err != nil {
				logger.Warn().Err(err).Str("key", obj.Key).Msg("retention: failed to delete WAL segment")
			} else {
				deleted++
			}
		}
	}

	if deleted > 0 {
		logger.Info().Str("prefix", prefix).Int("deleted", deleted).Int("retention_days", retentionDays).Msg("retention: WAL pruning complete")
	} else {
		logger.Debug().Str("prefix", prefix).Int("retention_days", retentionDays).Msg("pruneWAL: no WAL segments to prune")
	}
}
