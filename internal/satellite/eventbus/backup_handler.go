package eventbus

import (
	"context"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/pg-swarm/pg-swarm/internal/satellite/sidecar"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"google.golang.org/protobuf/encoding/protojson"
)

// BackupHandler routes backup-related events from central to the appropriate
// sidecar streams. It follows the same pattern as LogRuleHandler.
type BackupHandler struct {
	streams   *sidecar.SidecarStreamManager
	bus       *EventBus
	connector streamSender
	logger    zerolog.Logger
}

// streamSender abstracts the satellite stream connector for forwarding events.
type streamSender interface {
	ForwardEvent(evt *pgswarmv1.Event)
}

// NewBackupHandler creates a handler that routes backup events to sidecars.
func NewBackupHandler(sm *sidecar.SidecarStreamManager, bus *EventBus, connector streamSender) *BackupHandler {
	return &BackupHandler{
		streams:   sm,
		bus:       bus,
		connector: connector,
		logger:    log.With().Str("component", "backup-handler").Logger(),
	}
}

// Register subscribes this handler to backup-related events on the EventBus.
func (h *BackupHandler) Register() {
	h.bus.Subscribe("backup.config_update", "backup", h.handleConfigUpdate)
	h.bus.Subscribe("backup.trigger", "backup", h.handleTrigger)
	h.bus.Subscribe("restore.requested", "backup", h.handleRestore)
	h.bus.Subscribe("sidecar.set_log_level", "backup", h.handleSetLogLevel)
}

// handleConfigUpdate pushes a BackupConfig to all connected sidecars for the cluster.
func (h *BackupHandler) handleConfigUpdate(ctx context.Context, evt *pgswarmv1.Event) error {
	clusterName := evt.GetClusterName()
	namespace := evt.GetNamespace()
	configJSON := evt.Data["config"]

	if configJSON == "" {
		h.logger.Warn().Str("cluster", clusterName).Msg("backup.config_update missing config data")
		return nil
	}

	streams := h.streams.ListByCluster(namespace, clusterName)
	if len(streams) == 0 {
		h.logger.Warn().Str("cluster", clusterName).Str("namespace", namespace).
			Msg("no connected sidecars for cluster, cannot push backup config")
		return nil
	}

	h.logger.Info().Str("cluster", clusterName).Int("sidecars", len(streams)).
		Msg("pushing backup config to sidecars")

	for _, s := range streams {
		go func(s *sidecar.SidecarStream) {
			result, err := h.streams.SendEventCommandAndWait(ctx, s.Namespace, s.PodName, "command.backup_config", map[string]string{
				"config": configJSON,
			})
			if err != nil {
				h.logger.Error().Err(err).Str("pod", s.PodName).Msg("failed to push backup config")
				return
			}
			if !result.Success {
				h.logger.Warn().Str("pod", s.PodName).Str("error", result.Error).Msg("sidecar rejected backup config")
			} else {
				h.logger.Info().Str("pod", s.PodName).Msg("backup config applied on sidecar")
			}
		}(s)
	}

	return nil
}

// handleTrigger sends a backup trigger to a replica sidecar for the cluster.
func (h *BackupHandler) handleTrigger(ctx context.Context, evt *pgswarmv1.Event) error {
	clusterName := evt.GetClusterName()
	namespace := evt.GetNamespace()
	backupType := evt.Data["backup_type"]
	if backupType == "" {
		backupType = "base"
	}

	// Prefer replica pods for physical backups; logical can run anywhere.
	// PreferReplica returns replicas first when role is known, all pods otherwise.
	streams := h.streams.PreferReplica(namespace, clusterName)
	if len(streams) == 0 {
		h.logger.Warn().Str("cluster", clusterName).Msg("no connected sidecars for backup trigger")
		resultEvt := NewEvent("backup.trigger.failed", clusterName, namespace, "satellite")
		WithData(resultEvt, "error", "no connected sidecars")
		WithSeverity(resultEvt, "error")
		if evt.GetOperationId() != "" {
			WithOperationID(resultEvt, evt.GetOperationId())
		}
		_ = h.bus.Publish(ctx, resultEvt)
		return nil
	}

	// Pick the best candidate (replica preferred, role unknown pods are fine too).
	target := streams[0]
	h.logger.Info().
		Str("pod", target.PodName).
		Bool("is_primary", target.IsPrimary.Load()).
		Msg("selected sidecar for backup trigger")

	h.logger.Info().Str("cluster", clusterName).Str("pod", target.PodName).
		Str("type", backupType).Msg("triggering backup on sidecar")

	go func() {
		result, err := h.streams.SendEventCommandAndWait(ctx, target.Namespace, target.PodName, "command.backup_trigger", map[string]string{
			"backup_type": backupType,
		})

		resultEvt := NewPodEvent("backup.trigger.completed", clusterName, namespace, target.PodName, "satellite")
		if evt.GetOperationId() != "" {
			WithOperationID(resultEvt, evt.GetOperationId())
		}
		WithData(resultEvt, "backup_type", backupType)

		if err != nil {
			h.logger.Error().Err(err).Str("pod", target.PodName).Msg("backup trigger dispatch failed")
			WithData(resultEvt, "success", "false")
			WithData(resultEvt, "error", err.Error())
			WithSeverity(resultEvt, "error")
			resultEvt.Type = "backup.trigger.failed"
		} else if !result.Success {
			h.logger.Warn().Str("pod", target.PodName).Str("error", result.Error).Msg("backup trigger failed on sidecar")
			WithData(resultEvt, "success", "false")
			WithData(resultEvt, "error", result.Error)
			WithSeverity(resultEvt, "warning")
			resultEvt.Type = "backup.trigger.failed"
		} else {
			h.logger.Info().Str("pod", target.PodName).Str("type", backupType).Msg("backup triggered successfully")
			WithData(resultEvt, "success", "true")
		}

		_ = h.bus.Publish(ctx, resultEvt)
	}()

	return nil
}

// handleSetLogLevel fans out a log-level change command to all sidecars in the cluster.
func (h *BackupHandler) handleSetLogLevel(ctx context.Context, evt *pgswarmv1.Event) error {
	clusterName := evt.GetClusterName()
	namespace := evt.GetNamespace()
	level := evt.Data["level"]

	if level == "" {
		h.logger.Warn().Str("cluster", clusterName).Msg("sidecar.set_log_level missing level data")
		return nil
	}

	streams := h.streams.ListByCluster(namespace, clusterName)
	if len(streams) == 0 {
		h.logger.Warn().Str("cluster", clusterName).Str("namespace", namespace).
			Msg("no connected sidecars for log level change")
		return nil
	}

	h.logger.Info().Str("cluster", clusterName).Str("level", level).Int("sidecars", len(streams)).
		Msg("pushing log level change to sidecars")

	for _, s := range streams {
		go func(s *sidecar.SidecarStream) {
			result, err := h.streams.SendEventCommandAndWait(ctx, s.Namespace, s.PodName, "command.set_log_level", map[string]string{
				"level": level,
			})
			if err != nil {
				h.logger.Error().Err(err).Str("pod", s.PodName).Msg("failed to set log level on sidecar")
				return
			}
			if !result.Success {
				h.logger.Warn().Str("pod", s.PodName).Str("error", result.Error).Msg("sidecar rejected log level change")
			} else {
				h.logger.Info().Str("pod", s.PodName).Str("level", level).Msg("log level changed on sidecar")
			}
		}(s)
	}

	return nil
}

// handleRestore sends a restore command to the specified pod or a sidecar in the cluster.
func (h *BackupHandler) handleRestore(ctx context.Context, evt *pgswarmv1.Event) error {
	clusterName := evt.GetClusterName()
	namespace := evt.GetNamespace()
	restoreCmdJSON := evt.Data["restore_command"]

	if restoreCmdJSON == "" {
		h.logger.Warn().Str("cluster", clusterName).Msg("restore.requested missing restore_command data")
		return nil
	}

	// Parse to get target pod if specified
	var cmd pgswarmv1.RestoreCommand
	if err := protojson.Unmarshal([]byte(restoreCmdJSON), &cmd); err != nil {
		h.logger.Error().Err(err).Msg("failed to parse restore command")
		return nil
	}

	// Pick a sidecar from the cluster
	streams := h.streams.ListByCluster(namespace, clusterName)
	if len(streams) == 0 {
		h.logger.Warn().Str("cluster", clusterName).Msg("no connected sidecars for restore")
		resultEvt := NewEvent("restore.failed", clusterName, namespace, "satellite")
		WithData(resultEvt, "error", "no connected sidecars")
		WithSeverity(resultEvt, "error")
		if evt.GetOperationId() != "" {
			WithOperationID(resultEvt, evt.GetOperationId())
		}
		_ = h.bus.Publish(ctx, resultEvt)
		return nil
	}
	targetPod := streams[0].PodName

	h.logger.Info().Str("cluster", clusterName).Str("pod", targetPod).
		Str("type", cmd.GetRestoreType()).Msg("dispatching restore to sidecar")

	go func() {
		result, err := h.streams.SendEventCommandAndWait(ctx, namespace, targetPod, "command.restore", map[string]string{
			"restore_command": restoreCmdJSON,
		})

		resultEvt := NewPodEvent("restore.completed", clusterName, namespace, targetPod, "satellite")
		if evt.GetOperationId() != "" {
			WithOperationID(resultEvt, evt.GetOperationId())
		}

		if err != nil {
			h.logger.Error().Err(err).Str("pod", targetPod).Msg("restore dispatch failed")
			WithData(resultEvt, "success", "false")
			WithData(resultEvt, "error", err.Error())
			WithSeverity(resultEvt, "error")
			resultEvt.Type = "restore.failed"
		} else if !result.Success {
			h.logger.Warn().Str("pod", targetPod).Str("error", result.Error).Msg("restore failed on sidecar")
			WithData(resultEvt, "success", "false")
			WithData(resultEvt, "error", result.Error)
			WithSeverity(resultEvt, "warning")
			resultEvt.Type = "restore.failed"
		} else {
			h.logger.Info().Str("pod", targetPod).Msg("restore completed successfully")
			WithData(resultEvt, "success", "true")
		}

		_ = h.bus.Publish(ctx, resultEvt)
	}()

	return nil
}
