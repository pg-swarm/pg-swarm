package backup

import (
	"context"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog/log"
)

// Scheduler runs backup jobs on cron schedules. Active only on the replica sidecar.
type Scheduler struct {
	sidecar *Sidecar
	cron    *cron.Cron
}

// NewScheduler creates a new backup scheduler.
func NewScheduler(s *Sidecar) *Scheduler {
	return &Scheduler{
		sidecar: s,
	}
}

// Run starts the scheduler. It runs backup jobs based on configured schedules.
func (sc *Scheduler) Run(ctx context.Context) {
	log.Info().
		Str("base", sc.sidecar.cfg.BaseSchedule).
		Str("incremental", sc.sidecar.cfg.IncrSchedule).
		Str("logical", sc.sidecar.cfg.LogicSchedule).
		Msg("scheduler started")

	// Run initial base backup if no existing backups
	if sc.sidecar.cfg.BaseSchedule != "" {
		go sc.runWithRecovery(ctx, "initial-base", sc.sidecar.RunBaseBackup)
	}

	sc.cron = cron.New()
	sc.addJob(ctx, sc.sidecar.cfg.BaseSchedule, "base", sc.sidecar.RunBaseBackup)
	sc.addJob(ctx, sc.sidecar.cfg.IncrSchedule, "incremental", sc.sidecar.RunIncrementalBackup)
	sc.addJob(ctx, sc.sidecar.cfg.LogicSchedule, "logical", sc.sidecar.RunLogicalBackup)
	sc.cron.Start()

	<-ctx.Done()
	sc.cron.Stop()
}

// Stop signals the scheduler to shut down.
func (sc *Scheduler) Stop() {
	if sc.cron != nil {
		sc.cron.Stop()
	}
}

// addJob registers a cron job if the expression is non-empty.
func (sc *Scheduler) addJob(ctx context.Context, cronExpr, name string, fn func(context.Context) error) {
	if cronExpr == "" {
		return
	}
	if _, err := sc.cron.AddFunc(cronExpr, func() {
		sc.runWithRecovery(ctx, name, fn)
	}); err != nil {
		log.Error().Err(err).Str("backup", name).Str("cron", cronExpr).Msg("invalid cron expression")
	}
}

// runWithRecovery runs a backup function, recovering from panics.
// It checks cluster lifecycle status and health before running; backups are
// skipped unless the cluster is RUNNING. The initial base backup retries for
// up to 10 minutes on both cluster status and health.
func (sc *Scheduler) runWithRecovery(ctx context.Context, name string, fn func(context.Context) error) {
	defer func() {
		if r := recover(); r != nil {
			log.Error().Interface("panic", r).Str("backup", name).Msg("backup panicked")
		}
	}()

	// Check cluster lifecycle status before backup
	if allowed, reason := sc.sidecar.isClusterStatusRunning(ctx); !allowed {
		if name == "initial-base" {
			if !sc.waitForClusterStatus(ctx, 10*time.Minute, 10*time.Second) {
				log.Warn().Str("backup", name).Msg("cluster status not RUNNING after 10min — skipping initial base")
				if sc.sidecar.reporter != nil {
					sc.sidecar.reporter.ReportBackup(ctx, "base", "skipped", 0, "cluster not RUNNING: "+reason)
				}
				return
			}
		} else {
			log.Warn().Str("backup", name).Str("reason", reason).Msg("cluster not RUNNING — skipping backup")
			if sc.sidecar.reporter != nil {
				sc.sidecar.reporter.ReportBackup(ctx, name, "skipped", 0, "cluster not RUNNING: "+reason)
			}
			return
		}
	}

	// Replication health gate: only applies to physical backups (base/incremental).
	// Logical backups (pg_dump) query local data and don't depend on streaming.
	if name != "logical" {
		hs := sc.sidecar.checkHealth(ctx)
		if !hs.Healthy {
			if name == "initial-base" {
				// First base backup: retry health every 30s for up to 10 minutes
				if !sc.waitForHealth(ctx, 10*time.Minute, 30*time.Second) {
					log.Warn().Str("backup", name).Msg("health check timed out for initial base — skipping")
					if sc.sidecar.reporter != nil {
						sc.sidecar.reporter.ReportBackup(ctx, "base", "skipped", 0, "health check timed out")
					}
					return
				}
			} else {
				log.Warn().Str("backup", name).Str("reason", hs.Reason).Msg("health check failed — skipping backup")
				if sc.sidecar.reporter != nil {
					sc.sidecar.reporter.ReportBackup(ctx, name, "skipped", 0, "health check failed: "+hs.Reason)
				}
				return
			}
		}
	}

	if err := fn(ctx); err != nil {
		log.Error().Err(err).Str("backup", name).Msg("backup failed")
	}
}

// waitForClusterStatus polls isClusterStatusRunning until allowed or timeout.
func (sc *Scheduler) waitForClusterStatus(ctx context.Context, timeout, interval time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if allowed, _ := sc.sidecar.isClusterStatusRunning(ctx); allowed {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(interval):
		}
	}
	return false
}

// waitForHealth retries checkHealth at the given interval until healthy or timeout.
func (sc *Scheduler) waitForHealth(ctx context.Context, timeout, interval time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		hs := sc.sidecar.checkHealth(ctx)
		if hs.Healthy {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(interval):
		}
	}
	return false
}
