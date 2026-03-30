package backup

import (
	"context"

	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
)

// Scheduler manages cron-based backup schedules.
type Scheduler struct {
	mgr    *Manager
	cron   *cron.Cron
	logger zerolog.Logger
}

// NewScheduler creates a new scheduler.
func NewScheduler(mgr *Manager, logger zerolog.Logger) *Scheduler {
	l := logger.With().Str("component", "backup-scheduler").Logger()
	l.Debug().Msg("NewScheduler: creating scheduler")
	return &Scheduler{
		mgr:    mgr,
		cron:   cron.New(),
		logger: l,
	}
}

// Reload (re)parses cron expressions from the backup config and registers
// scheduled jobs. Existing jobs are removed first.
func (s *Scheduler) Reload(physical *pgswarmv1.PhysicalBackupConfig, logical *pgswarmv1.LogicalBackupConfig) {
	s.logger.Debug().
		Bool("has_physical", physical != nil).
		Bool("has_logical", logical != nil).
		Msg("Reload: reloading backup schedules")

	// Stop existing cron and create a fresh one
	s.logger.Trace().Msg("Reload: stopping existing cron jobs")
	s.cron.Stop()
	s.cron = cron.New()

	if physical != nil {
		if physical.BaseSchedule != "" {
			s.logger.Trace().Str("schedule", physical.BaseSchedule).Msg("Reload: registering base backup schedule")
			if _, err := s.cron.AddFunc(physical.BaseSchedule, func() {
				s.triggerIfReplica("base")
			}); err != nil {
				s.logger.Error().Err(err).Str("schedule", physical.BaseSchedule).Msg("invalid base backup cron expression")
			} else {
				s.logger.Info().Str("schedule", physical.BaseSchedule).Msg("base backup scheduled")
			}
		}

		if physical.IncrementalSchedule != "" {
			s.logger.Trace().Str("schedule", physical.IncrementalSchedule).Msg("Reload: registering incremental backup schedule")
			if _, err := s.cron.AddFunc(physical.IncrementalSchedule, func() {
				s.triggerIfReplica("incremental")
			}); err != nil {
				s.logger.Error().Err(err).Str("schedule", physical.IncrementalSchedule).Msg("invalid incremental backup cron expression")
			} else {
				s.logger.Info().Str("schedule", physical.IncrementalSchedule).Msg("incremental backup scheduled")
			}
		}
	}

	if logical != nil && logical.Schedule != "" {
		s.logger.Trace().Str("schedule", logical.Schedule).Msg("Reload: registering logical backup schedule")
		if _, err := s.cron.AddFunc(logical.Schedule, func() {
			s.triggerIfReplica("logical")
		}); err != nil {
			s.logger.Error().Err(err).Str("schedule", logical.Schedule).Msg("invalid logical backup cron expression")
		} else {
			s.logger.Info().Str("schedule", logical.Schedule).Msg("logical backup scheduled")
		}
	}

	s.logger.Trace().Msg("Reload: starting cron scheduler")
	s.cron.Start()
	s.logger.Debug().Msg("Reload: done")
}

// triggerIfReplica fires a backup only if this pod is a replica.
// Base, incremental, and logical backups run on replicas only.
func (s *Scheduler) triggerIfReplica(backupType string) {
	s.logger.Trace().Str("type", backupType).Msg("triggerIfReplica: cron job fired")
	if s.mgr.isPrimary.Load() {
		s.logger.Debug().Str("type", backupType).Msg("skipping scheduled backup on primary pod")
		return
	}

	s.logger.Debug().Str("type", backupType).Msg("triggerIfReplica: pod is replica, triggering backup")
	if err := s.mgr.TriggerBackup(context.Background(), backupType); err != nil {
		s.logger.Warn().Err(err).Str("type", backupType).Msg("scheduled backup trigger failed")
	} else {
		s.logger.Debug().Str("type", backupType).Msg("triggerIfReplica: backup triggered successfully")
	}
}

// Stop halts all scheduled jobs.
func (s *Scheduler) Stop() {
	s.logger.Debug().Msg("Stop: halting all scheduled jobs")
	s.cron.Stop()
	s.logger.Trace().Msg("Stop: done")
}
