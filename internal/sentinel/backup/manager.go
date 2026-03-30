// Package backup coordinates all backup operations on the sentinel sidecar.
// It owns the cron scheduler, WAL archiver, and backup executor. All backup
// work runs in child goroutines so the Monitor's tick loop is never blocked.
package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/pg-swarm/pg-swarm/internal/sentinel/backup/storage"
)

// EventEmitter is satisfied by *SidecarConnector. Using an interface here
// breaks the import cycle between the backup and sentinel packages.
type EventEmitter interface {
	EmitEvent(evt *pgswarmv1.Event)
}

// PodConfig holds the identity fields the manager needs from the sentinel config.
type PodConfig struct {
	PodName     string
	Namespace   string
	ClusterName string
}

// Manager coordinates all backup operations on the sentinel sidecar.
type Manager struct {
	cfg        PodConfig
	emitter    EventEmitter
	k8sClient  kubernetes.Interface
	restConfig *rest.Config
	logger     zerolog.Logger
	execFn     ExecFunc
	execOutFn  ExecOutputFunc

	// backupCfg holds the current BackupConfig pushed from satellite.
	backupCfg atomic.Pointer[pgswarmv1.BackupConfig]

	// mu guards running to prevent concurrent backups.
	mu      sync.Mutex
	running string // "" or "base"/"incremental"/"logical"/"restore"

	// isPrimary is set by the monitor's tick loop.
	isPrimary atomic.Bool

	scheduler   *Scheduler
	walArchiver *WALArchiver
	walCancel   context.CancelFunc // cancel the current WAL archiver goroutine
}

// NewManager creates a new backup manager.
func NewManager(cfg PodConfig, emitter EventEmitter, k8sClient kubernetes.Interface, restConfig *rest.Config, execFn ExecFunc, execOutFn ExecOutputFunc) *Manager {
	l := log.With().Str("component", "backup-manager").Logger()
	l.Debug().
		Str("pod", cfg.PodName).
		Str("namespace", cfg.Namespace).
		Str("cluster", cfg.ClusterName).
		Msg("creating backup manager")
	return &Manager{
		cfg:        cfg,
		emitter:    emitter,
		k8sClient:  k8sClient,
		restConfig: restConfig,
		logger:     l,
		execFn:     execFn,
		execOutFn:  execOutFn,
	}
}

// Run starts the backup manager. It blocks until ctx is cancelled.
func (bm *Manager) Run(ctx context.Context) {
	bm.logger.Info().Msg("backup manager started")
	bm.logger.Trace().Msg("Run: waiting for context cancellation")
	<-ctx.Done()
	bm.logger.Debug().Msg("Run: context cancelled, shutting down")
	bm.stopScheduler()
	bm.stopWALArchiver()
	bm.logger.Info().Msg("backup manager stopped")
}

// UpdateConfig applies a new BackupConfig received from the satellite stream.
// It reloads the scheduler and adjusts the WAL archiver.
func (bm *Manager) UpdateConfig(cfg *pgswarmv1.BackupConfig) {
	bm.logger.Debug().
		Str("store_id", cfg.GetStoreId()).
		Str("base_path", cfg.GetBasePath()).
		Bool("has_physical", cfg.GetPhysical() != nil).
		Bool("has_logical", cfg.GetLogical() != nil).
		Bool("has_retention", cfg.GetRetention() != nil).
		Msg("UpdateConfig: received new backup config")

	bm.backupCfg.Store(cfg)
	bm.logger.Info().
		Str("store_id", cfg.GetStoreId()).
		Str("base_path", cfg.GetBasePath()).
		Msg("backup config updated")

	// Reload scheduler with new schedules
	bm.logger.Trace().Msg("UpdateConfig: reloading scheduler")
	bm.reloadScheduler(cfg)

	// Restart WAL archiver if role and config warrant it
	isPrimary := bm.isPrimary.Load()
	walEnabled := cfg.GetPhysical() != nil && cfg.GetPhysical().WalArchiveEnabled
	bm.logger.Trace().Bool("is_primary", isPrimary).Bool("wal_archive_enabled", walEnabled).Msg("UpdateConfig: evaluating WAL archiver")
	if isPrimary && walEnabled {
		bm.startWALArchiver()
	} else {
		bm.stopWALArchiver()
	}
}

// SetRole is called from Monitor.tick() after determining the PG role.
// It starts or stops the WAL archiver based on whether this pod is primary.
func (bm *Manager) SetRole(isPrimary bool) {
	bm.logger.Debug().Bool("is_primary", isPrimary).Msg("SetRole: called")
	prev := bm.isPrimary.Swap(isPrimary)
	if prev == isPrimary {
		bm.logger.Trace().Bool("role", isPrimary).Msg("SetRole: no role change, returning")
		return // no change
	}

	bm.logger.Debug().Bool("prev_primary", prev).Bool("new_primary", isPrimary).Msg("SetRole: role changed")

	cfg := bm.backupCfg.Load()
	if cfg == nil {
		bm.logger.Debug().Msg("SetRole: no backup config loaded, skipping WAL archiver adjustment")
		return
	}

	if isPrimary && cfg.GetPhysical() != nil && cfg.GetPhysical().WalArchiveEnabled {
		bm.logger.Info().Msg("pod became primary, starting WAL archiver")
		bm.startWALArchiver()
	} else if !isPrimary {
		bm.logger.Info().Msg("pod became replica, stopping WAL archiver")
		bm.stopWALArchiver()
	}
}

// TriggerBackup starts an on-demand backup. Returns an error if a backup is
// already running or if this pod's role is incompatible with the backup type.
// The actual backup runs in a goroutine.
func (bm *Manager) TriggerBackup(ctx context.Context, backupType string) error {
	bm.logger.Debug().Str("type", backupType).Msg("TriggerBackup: called")

	cfg := bm.backupCfg.Load()
	if cfg == nil {
		bm.logger.Debug().Msg("TriggerBackup: no backup config available")
		return fmt.Errorf("no backup config available")
	}

	// Physical backups (base, incremental) must not run on the primary — they
	// use pg_basebackup which is I/O-intensive and should run on a replica.
	// Logical backups (pg_dump) can run on any instance.
	isPrimary := bm.isPrimary.Load()
	bm.logger.Trace().Bool("is_primary", isPrimary).Str("type", backupType).Msg("TriggerBackup: checking role compatibility")
	if isPrimary && (backupType == "base" || backupType == "incremental") {
		bm.logger.Debug().Str("type", backupType).Msg("TriggerBackup: rejected — physical backup on primary")
		return fmt.Errorf("physical backup type %q must run on a replica, not the primary", backupType)
	}

	bm.mu.Lock()
	if bm.running != "" {
		current := bm.running
		bm.mu.Unlock()
		bm.logger.Debug().Str("type", backupType).Str("current", current).Msg("TriggerBackup: rejected — already running")
		return fmt.Errorf("backup already running: %s", current)
	}
	bm.running = backupType
	bm.mu.Unlock()

	go func() {
		defer func() {
			bm.mu.Lock()
			bm.running = ""
			bm.mu.Unlock()
			bm.logger.Trace().Str("type", backupType).Msg("TriggerBackup: goroutine finished, running state cleared")
		}()

		bm.logger.Info().Str("type", backupType).Msg("starting backup")
		startedAt := time.Now()

		bm.logger.Trace().
			Str("pod", bm.cfg.PodName).
			Str("namespace", bm.cfg.Namespace).
			Msg("TriggerBackup: creating executor")
		executor := NewExecutor(
			PodRef{PodName: bm.cfg.PodName, Namespace: bm.cfg.Namespace},
			bm.k8sClient,
			bm.restConfig,
			bm.logger,
			bm.execFn,
			bm.execOutFn,
		)

		var result *Result
		switch backupType {
		case "base":
			bm.logger.Debug().Msg("TriggerBackup: dispatching base backup")
			result = executor.ExecuteBaseBackup(ctx, cfg)
		case "incremental":
			bm.logger.Debug().Msg("TriggerBackup: dispatching incremental backup")
			result = executor.ExecuteIncrementalBackup(ctx, cfg)
		case "logical":
			bm.logger.Debug().Msg("TriggerBackup: dispatching logical backup")
			result = executor.ExecuteLogicalBackup(ctx, cfg)
		default:
			bm.logger.Debug().Str("type", backupType).Msg("TriggerBackup: unknown backup type")
			result = &Result{
				BackupType: backupType,
				StartedAt:  startedAt,
				Error:      fmt.Errorf("unknown backup type: %s", backupType),
			}
		}

		if result.Error != nil {
			bm.logger.Error().
				Err(result.Error).
				Str("type", backupType).
				Str("pod", bm.cfg.PodName).
				Msg("backup failed")
		} else {
			bm.logger.Info().
				Str("type", backupType).
				Str("path", result.BackupPath).
				Int64("size_bytes", result.SizeBytes).
				Dur("duration", result.CompletedAt.Sub(result.StartedAt)).
				Msg("backup completed successfully")
		}

		bm.logger.Trace().Msg("TriggerBackup: reporting backup status")
		bm.reportBackupStatus(result)

		// Apply retention after successful backup
		if result.Error == nil && cfg.GetRetention() != nil {
			bm.logger.Debug().Msg("TriggerBackup: applying retention policy")
			bm.runRetention(ctx, cfg)
		}
	}()

	bm.logger.Debug().Str("type", backupType).Msg("TriggerBackup: backup goroutine launched")
	return nil
}

// TriggerRestore starts an on-demand restore. Returns an error if a backup
// or restore is already running.
func (bm *Manager) TriggerRestore(ctx context.Context, cmd *pgswarmv1.RestoreCommand) error {
	bm.logger.Debug().
		Str("type", cmd.GetRestoreType()).
		Str("restore_id", cmd.GetRestoreId()).
		Str("backup_path", cmd.GetBackupPath()).
		Str("target_db", cmd.GetTargetDatabase()).
		Msg("TriggerRestore: called")

	cfg := bm.backupCfg.Load()
	if cfg == nil {
		bm.logger.Debug().Msg("TriggerRestore: no backup config available")
		return fmt.Errorf("no backup config available")
	}

	bm.mu.Lock()
	if bm.running != "" {
		current := bm.running
		bm.mu.Unlock()
		bm.logger.Debug().Str("current", current).Msg("TriggerRestore: rejected — operation already running")
		return fmt.Errorf("operation already running: %s", current)
	}
	bm.running = "restore"
	bm.mu.Unlock()

	go func() {
		defer func() {
			bm.mu.Lock()
			bm.running = ""
			bm.mu.Unlock()
			bm.logger.Trace().Str("restore_id", cmd.GetRestoreId()).Msg("TriggerRestore: goroutine finished, running state cleared")
		}()

		bm.logger.Info().
			Str("type", cmd.GetRestoreType()).
			Str("restore_id", cmd.GetRestoreId()).
			Msg("starting restore")

		bm.logger.Trace().
			Str("pod", bm.cfg.PodName).
			Str("namespace", bm.cfg.Namespace).
			Msg("TriggerRestore: creating executor")
		executor := NewExecutor(
			PodRef{PodName: bm.cfg.PodName, Namespace: bm.cfg.Namespace},
			bm.k8sClient,
			bm.restConfig,
			bm.logger,
			bm.execFn,
			bm.execOutFn,
		)

		var errMsg string
		var err error
		switch cmd.GetRestoreType() {
		case "logical":
			bm.logger.Debug().Str("restore_id", cmd.GetRestoreId()).Msg("TriggerRestore: dispatching logical restore")
			err = executor.ExecuteLogicalRestore(ctx, cmd, cfg)
		case "pitr":
			bm.logger.Debug().Str("restore_id", cmd.GetRestoreId()).Msg("TriggerRestore: dispatching PITR restore")
			err = executor.ExecutePITRRestore(ctx, cmd, cfg)
		default:
			bm.logger.Debug().Str("type", cmd.GetRestoreType()).Msg("TriggerRestore: unknown restore type")
			err = fmt.Errorf("unknown restore type: %s", cmd.GetRestoreType())
		}
		if err != nil {
			errMsg = err.Error()
			bm.logger.Error().Err(err).Str("restore_id", cmd.GetRestoreId()).Msg("TriggerRestore: restore execution failed")
		} else {
			bm.logger.Debug().Str("restore_id", cmd.GetRestoreId()).Msg("TriggerRestore: restore execution succeeded")
		}

		bm.logger.Trace().Str("restore_id", cmd.GetRestoreId()).Msg("TriggerRestore: reporting restore status")
		bm.reportRestoreStatus(cmd, errMsg)
	}()

	bm.logger.Debug().Str("restore_id", cmd.GetRestoreId()).Msg("TriggerRestore: restore goroutine launched")
	return nil
}

// IsRunning returns whether a backup or restore is in progress and its type.
func (bm *Manager) IsRunning() (bool, string) {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	bm.logger.Trace().Bool("running", bm.running != "").Str("type", bm.running).Msg("IsRunning: checked")
	return bm.running != "", bm.running
}

func (bm *Manager) reloadScheduler(cfg *pgswarmv1.BackupConfig) {
	bm.logger.Debug().Msg("reloadScheduler: stopping old scheduler and creating new one")
	bm.stopScheduler()
	bm.scheduler = NewScheduler(bm, bm.logger)
	bm.scheduler.Reload(cfg.GetPhysical(), cfg.GetLogical())
	bm.logger.Debug().Msg("reloadScheduler: done")
}

func (bm *Manager) stopScheduler() {
	if bm.scheduler != nil {
		bm.logger.Trace().Msg("stopScheduler: stopping active scheduler")
		bm.scheduler.Stop()
		bm.scheduler = nil
	} else {
		bm.logger.Trace().Msg("stopScheduler: no active scheduler")
	}
}

func (bm *Manager) startWALArchiver() {
	bm.logger.Debug().Msg("startWALArchiver: called")
	bm.stopWALArchiver()
	cfg := bm.backupCfg.Load()
	if cfg == nil || cfg.GetDestination() == nil {
		bm.logger.Debug().Msg("startWALArchiver: no config or destination, skipping")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	bm.walCancel = cancel
	bm.walArchiver = NewWALArchiver(cfg, bm.logger)
	bm.logger.Debug().Msg("startWALArchiver: WAL archiver goroutine launched")
	go bm.walArchiver.Run(ctx)
}

func (bm *Manager) stopWALArchiver() {
	if bm.walCancel != nil {
		bm.logger.Trace().Msg("stopWALArchiver: cancelling active WAL archiver")
		bm.walCancel()
		bm.walCancel = nil
	} else {
		bm.logger.Trace().Msg("stopWALArchiver: no active WAL archiver")
	}
	bm.walArchiver = nil
}

// runRetention creates a storage backend and applies the retention policy.
func (bm *Manager) runRetention(ctx context.Context, cfg *pgswarmv1.BackupConfig) {
	bm.logger.Debug().Msg("runRetention: creating storage backend")
	backend, err := storage.New(ctx, cfg.GetDestination(), bm.logger)
	if err != nil {
		bm.logger.Warn().Err(err).Msg("retention: failed to create storage backend")
		return
	}
	defer backend.Close()
	bm.logger.Trace().Msg("runRetention: applying retention policy")
	applyRetention(ctx, backend, cfg, bm.logger)
	bm.logger.Debug().Msg("runRetention: done")
}

// reportBackupStatus sends a BackupStatusReport event to the satellite.
func (bm *Manager) reportBackupStatus(result *Result) {
	bm.logger.Debug().
		Str("type", result.BackupType).
		Str("path", result.BackupPath).
		Bool("has_error", result.Error != nil).
		Msg("reportBackupStatus: called")

	if bm.emitter == nil {
		bm.logger.Debug().Msg("reportBackupStatus: no emitter configured, skipping")
		return
	}

	status := "completed"
	errMsg := ""
	if result.Error != nil {
		status = "failed"
		errMsg = result.Error.Error()
		bm.logger.Error().Err(result.Error).Str("type", result.BackupType).Msg("backup failed")
	} else {
		bm.logger.Info().Str("type", result.BackupType).Str("path", result.BackupPath).
			Int64("size", result.SizeBytes).Msg("backup completed")
	}

	data := map[string]string{
		"backup_type":   result.BackupType,
		"status":        status,
		"backup_path":   result.BackupPath,
		"size_bytes":    fmt.Sprintf("%d", result.SizeBytes),
		"pg_version":    result.PGVersion,
		"wal_start_lsn": result.WALStartLSN,
		"wal_end_lsn":   result.WALEndLSN,
		"error_message": errMsg,
	}
	if result.Databases != nil {
		if b, err := json.Marshal(result.Databases); err == nil {
			data["databases"] = string(b)
		}
	}

	evt := &pgswarmv1.Event{
		Id:          fmt.Sprintf("backup-%s-%d", result.BackupType, result.StartedAt.UnixMilli()),
		Type:        "backup.status",
		ClusterName: bm.cfg.ClusterName,
		Namespace:   bm.cfg.Namespace,
		PodName:     bm.cfg.PodName,
		Severity:    "info",
		Source:      "sidecar",
		Timestamp:   timestamppb.Now(),
		Data:        data,
	}
	if result.Error != nil {
		evt.Severity = "error"
	}

	bm.logger.Trace().Str("event_id", evt.Id).Str("type", evt.Type).Msg("reportBackupStatus: emitting event")
	bm.emitter.EmitEvent(evt)
	bm.logger.Debug().Str("event_id", evt.Id).Msg("reportBackupStatus: event emitted")
}

// reportRestoreStatus sends a RestoreStatusReport event to the satellite.
func (bm *Manager) reportRestoreStatus(cmd *pgswarmv1.RestoreCommand, errMsg string) {
	bm.logger.Debug().
		Str("restore_id", cmd.GetRestoreId()).
		Bool("has_error", errMsg != "").
		Msg("reportRestoreStatus: called")

	if bm.emitter == nil {
		bm.logger.Debug().Msg("reportRestoreStatus: no emitter configured, skipping")
		return
	}

	status := "completed"
	severity := "info"
	if errMsg != "" {
		status = "failed"
		severity = "error"
		bm.logger.Error().Str("error", errMsg).Str("restore_id", cmd.GetRestoreId()).Msg("restore failed")
	} else {
		bm.logger.Info().Str("restore_id", cmd.GetRestoreId()).Msg("restore completed")
	}

	evt := &pgswarmv1.Event{
		Id:          fmt.Sprintf("restore-%s", cmd.GetRestoreId()),
		Type:        "restore.status",
		ClusterName: bm.cfg.ClusterName,
		Namespace:   bm.cfg.Namespace,
		PodName:     bm.cfg.PodName,
		Severity:    severity,
		Source:      "sidecar",
		Timestamp:   timestamppb.Now(),
		Data: map[string]string{
			"restore_id":    cmd.GetRestoreId(),
			"restore_type":  cmd.GetRestoreType(),
			"status":        status,
			"error_message": errMsg,
		},
	}

	bm.logger.Trace().Str("event_id", evt.Id).Str("type", evt.Type).Msg("reportRestoreStatus: emitting event")
	bm.emitter.EmitEvent(evt)
	bm.logger.Debug().Str("event_id", evt.Id).Msg("reportRestoreStatus: event emitted")
}
