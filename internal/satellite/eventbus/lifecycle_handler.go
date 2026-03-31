package eventbus

import (
	"context"
	"fmt"
	"strconv"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/pg-swarm/pg-swarm/internal/satellite/operator"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"google.golang.org/protobuf/encoding/protojson"
)

// LifecycleHandler processes cluster lifecycle events (create, update, delete,
// pause, unpause) by delegating to the existing Operator. It emits result
// events (config.applied, config.rejected, cluster.deleted, etc.) back
// through the EventBus.
type LifecycleHandler struct {
	operator *operator.Operator
	bus      *EventBus
	logger   zerolog.Logger
}

// NewLifecycleHandler creates a LifecycleHandler that bridges events to the
// existing operator reconciliation logic.
func NewLifecycleHandler(op *operator.Operator, bus *EventBus) *LifecycleHandler {
	return &LifecycleHandler{
		operator: op,
		bus:      bus,
		logger:   log.With().Str("component", "lifecycle-handler").Logger(),
	}
}

// Register subscribes to all cluster lifecycle event patterns on the bus.
func (h *LifecycleHandler) Register() {
	h.bus.Subscribe("cluster.create", "lifecycle", h.handleCreateOrUpdate)
	h.bus.Subscribe("cluster.update", "lifecycle", h.handleCreateOrUpdate)
	h.bus.Subscribe("cluster.delete", "lifecycle", h.handleDelete)
	h.bus.Subscribe("cluster.pause", "lifecycle", h.handlePause)
	h.bus.Subscribe("cluster.unpause", "lifecycle", h.handleUnpause)
}

func (h *LifecycleHandler) handleCreateOrUpdate(ctx context.Context, evt *pgswarmv1.Event) error {
	cfg := evt.GetClusterConfig()
	if cfg == nil {
		h.logger.Warn().
			Str("event_type", evt.GetType()).
			Str("cluster", evt.GetClusterName()).
			Msg("event missing ClusterConfig payload, ignoring")
		return nil
	}

	h.logger.Info().
		Str("event_type", evt.GetType()).
		Str("cluster", cfg.ClusterName).
		Int64("version", cfg.ConfigVersion).
		Msg("processing cluster lifecycle event")

	err := h.operator.HandleConfig(cfg)

	// Emit result event
	resultEvt := NewEvent("config.applied", cfg.ClusterName, cfg.Namespace, "satellite")
	WithData(resultEvt, "config_version", strconv.FormatInt(cfg.ConfigVersion, 10))

	if err != nil {
		resultEvt.Type = "config.rejected"
		WithSeverity(resultEvt, "error")
		WithData(resultEvt, "error", err.Error())
		h.logger.Error().Err(err).
			Str("cluster", cfg.ClusterName).
			Int64("version", cfg.ConfigVersion).
			Msg("cluster config handling failed")
	} else {
		h.logger.Info().
			Str("cluster", cfg.ClusterName).
			Int64("version", cfg.ConfigVersion).
			Msg("cluster config applied successfully")

		// Push backup config to sidecars if present
		if cfg.Backups != nil {
			h.EmitBackupConfigUpdate(ctx, cfg)
		}
	}

	// Publish result event (will be forwarded to central by the bus)
	return h.bus.Publish(ctx, resultEvt)
}

func (h *LifecycleHandler) handleDelete(ctx context.Context, evt *pgswarmv1.Event) error {
	del := evt.GetDeleteCluster()
	if del == nil {
		// Fall back to event fields if no typed payload
		del = &pgswarmv1.DeleteCluster{
			ClusterName: evt.GetClusterName(),
			Namespace:   evt.GetNamespace(),
		}
	}

	if del.ClusterName == "" {
		h.logger.Warn().Msg("cluster.delete event missing cluster_name, ignoring")
		return nil
	}

	h.logger.Info().
		Str("cluster", del.ClusterName).
		Str("namespace", del.Namespace).
		Msg("processing cluster delete event")

	err := h.operator.HandleDelete(del)

	resultEvt := NewEvent("cluster.deleted", del.ClusterName, del.Namespace, "satellite")
	if err != nil {
		resultEvt.Type = "cluster.delete_failed"
		WithSeverity(resultEvt, "error")
		WithData(resultEvt, "error", err.Error())
		h.logger.Error().Err(err).
			Str("cluster", del.ClusterName).
			Msg("cluster deletion failed")
	} else {
		h.logger.Info().
			Str("cluster", del.ClusterName).
			Msg("cluster deleted successfully")
	}

	return h.bus.Publish(ctx, resultEvt)
}

func (h *LifecycleHandler) handlePause(ctx context.Context, evt *pgswarmv1.Event) error {
	cfg := evt.GetClusterConfig()
	if cfg == nil {
		return fmt.Errorf("cluster.pause event missing ClusterConfig payload")
	}

	cfg.Paused = true
	h.logger.Info().
		Str("cluster", cfg.ClusterName).
		Msg("processing cluster pause event")

	err := h.operator.HandleConfig(cfg)

	resultEvt := NewEvent("cluster.paused", cfg.ClusterName, cfg.Namespace, "satellite")
	if err != nil {
		resultEvt.Type = "cluster.pause_failed"
		WithSeverity(resultEvt, "error")
		WithData(resultEvt, "error", err.Error())
	}
	return h.bus.Publish(ctx, resultEvt)
}

// EmitBackupConfigUpdate serializes the BackupConfig and publishes a
// backup.config_update event so the BackupHandler pushes it to sidecars.
// Exported so the agent can call it when a sidecar connects after the
// initial config push.
func (h *LifecycleHandler) EmitBackupConfigUpdate(ctx context.Context, cfg *pgswarmv1.ClusterConfig) {
	jsonBytes, err := protojson.Marshal(cfg.Backups)
	if err != nil {
		h.logger.Error().Err(err).Str("cluster", cfg.ClusterName).Msg("failed to marshal backup config")
		return
	}
	evt := NewEvent("backup.config_update", cfg.ClusterName, cfg.Namespace, "satellite")
	WithData(evt, "config", string(jsonBytes))
	_ = h.bus.Publish(ctx, evt)
	h.logger.Info().Str("cluster", cfg.ClusterName).Msg("emitted backup.config_update event")
}

func (h *LifecycleHandler) handleUnpause(ctx context.Context, evt *pgswarmv1.Event) error {
	cfg := evt.GetClusterConfig()
	if cfg == nil {
		return fmt.Errorf("cluster.unpause event missing ClusterConfig payload")
	}

	cfg.Paused = false
	h.logger.Info().
		Str("cluster", cfg.ClusterName).
		Msg("processing cluster unpause event")

	err := h.operator.HandleConfig(cfg)

	resultEvt := NewEvent("cluster.unpaused", cfg.ClusterName, cfg.Namespace, "satellite")
	if err != nil {
		resultEvt.Type = "cluster.unpause_failed"
		WithSeverity(resultEvt, "error")
		WithData(resultEvt, "error", err.Error())
	}
	return h.bus.Publish(ctx, resultEvt)
}
