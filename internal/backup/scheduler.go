package backup

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"
)

// Scheduler runs backup jobs on cron schedules. Active only on the replica sidecar.
type Scheduler struct {
	sidecar *Sidecar
	stop    chan struct{}
}

// NewScheduler creates a new backup scheduler.
func NewScheduler(s *Sidecar) *Scheduler {
	return &Scheduler{
		sidecar: s,
		stop:    make(chan struct{}),
	}
}

// Run starts the scheduler. It runs backup jobs based on configured schedules.
// For simplicity, this uses a ticker-based approach with cron expression parsing
// rather than pulling in a cron library dependency.
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

	baseTicker := sc.newTicker(sc.sidecar.cfg.BaseSchedule)
	incrTicker := sc.newTicker(sc.sidecar.cfg.IncrSchedule)
	logicTicker := sc.newTicker(sc.sidecar.cfg.LogicSchedule)

	defer func() {
		if baseTicker != nil {
			baseTicker.Stop()
		}
		if incrTicker != nil {
			incrTicker.Stop()
		}
		if logicTicker != nil {
			logicTicker.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-sc.stop:
			return
		case <-tickerChan(baseTicker):
			go sc.runWithRecovery(ctx, "base", sc.sidecar.RunBaseBackup)
		case <-tickerChan(incrTicker):
			go sc.runWithRecovery(ctx, "incremental", sc.sidecar.RunIncrementalBackup)
		case <-tickerChan(logicTicker):
			go sc.runWithRecovery(ctx, "logical", sc.sidecar.RunLogicalBackup)
		}
	}
}

// Stop signals the scheduler to shut down.
func (sc *Scheduler) Stop() {
	select {
	case sc.stop <- struct{}{}:
	default:
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

// newTicker creates a ticker from a cron expression. For simplicity, we
// parse common cron intervals into durations. Full cron parsing would
// require a library like robfig/cron.
func (sc *Scheduler) newTicker(cronExpr string) *time.Ticker {
	d := parseCronInterval(cronExpr)
	if d == 0 {
		return nil
	}
	return time.NewTicker(d)
}

// parseCronInterval converts common cron expressions to durations.
// Supports: "0 * * * *" (hourly), "0 */N * * *" (every N hours),
// "0 0 * * *" (daily), "0 0 * * 0" (weekly), and minute intervals.
func parseCronInterval(expr string) time.Duration {
	if expr == "" {
		return 0
	}
	// Simple heuristic-based parsing for common patterns
	switch {
	case expr == "0 * * * *":
		return time.Hour
	case expr == "0 0 * * *" || expr == "0 2 * * *" || expr == "0 3 * * *":
		return 24 * time.Hour
	case expr == "0 0 * * 0" || expr == "0 2 * * 0":
		return 7 * 24 * time.Hour
	case len(expr) > 4 && expr[:4] == "*/5 ":
		return 5 * time.Minute
	case len(expr) > 5 && expr[:5] == "*/15 ":
		return 15 * time.Minute
	case len(expr) > 5 && expr[:5] == "*/30 ":
		return 30 * time.Minute
	case len(expr) > 6 && expr[:6] == "0 */2 ":
		return 2 * time.Hour
	case len(expr) > 6 && expr[:6] == "0 */4 ":
		return 4 * time.Hour
	case len(expr) > 6 && expr[:6] == "0 */6 ":
		return 6 * time.Hour
	case len(expr) > 7 && expr[:7] == "0 */12 ":
		return 12 * time.Hour
	default:
		// Default to daily if we can't parse
		return 24 * time.Hour
	}
}

// tickerChan returns the channel from a ticker, or a nil channel if ticker is nil.
func tickerChan(t *time.Ticker) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}
