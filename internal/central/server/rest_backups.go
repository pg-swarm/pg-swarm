package server

import (
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/pg-swarm/pg-swarm/internal/satellite/eventbus"
	"github.com/pg-swarm/pg-swarm/internal/shared/models"
	"github.com/rs/zerolog/log"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// listBackupInventory returns the backup history for a cluster.
func (s *RESTServer) listBackupInventory(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid cluster id"})
	}

	cfg, err := s.store.GetClusterConfig(c.Context(), id)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "cluster not found"})
	}
	if cfg.SatelliteID == nil {
		return c.JSON([]interface{}{})
	}

	limit := c.QueryInt("limit", 50)
	items, err := s.store.ListBackupInventory(c.Context(), *cfg.SatelliteID, cfg.Name, limit)
	if err != nil {
		log.Error().Err(err).Msg("failed to list backup inventory")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list backups"})
	}
	return c.JSON(items)
}

// triggerBackup sends a backup trigger to the satellite for a cluster.
func (s *RESTServer) triggerBackup(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid cluster id"})
	}

	var body struct {
		BackupType string `json:"backup_type"`
	}
	if err := c.BodyParser(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if body.BackupType == "" {
		body.BackupType = "base"
	}

	cfg, err := s.store.GetClusterConfig(c.Context(), id)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "cluster not found"})
	}
	if cfg.SatelliteID == nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": "cluster has no satellite"})
	}

	// Build and embed the BackupConfig so the sidecar can apply it immediately
	// even if it hasn't received a config_update yet (e.g. after a reconnect).
	spec, _ := cfg.ParseSpec()
	operationID := uuid.New().String()
	evt := eventbus.NewEvent("backup.trigger", cfg.Name, cfg.Namespace, "central")
	eventbus.WithOperationID(evt, operationID)
	eventbus.WithData(evt, "backup_type", body.BackupType)

	if spec.Backup != nil && spec.Backup.StoreID != nil {
		if bc, err := buildBackupConfig(s.store, s.encryptor, spec.Backup, cfg); err == nil {
			if bcJSON, err := protojson.Marshal(bc); err == nil {
				eventbus.WithData(evt, "backup_config", string(bcJSON))
			} else {
				log.Warn().Err(err).Str("cluster", cfg.Name).Msg("failed to marshal backup config for trigger")
			}
		} else {
			log.Warn().Err(err).Str("cluster", cfg.Name).Msg("failed to build backup config for trigger")
		}
	} else {
		log.Debug().Str("cluster", cfg.Name).
			Bool("has_backup_spec", spec.Backup != nil).
			Msg("trigger: not embedding backup_config (no backup spec or store_id)")
	}

	if err := s.streams.PushEvent(*cfg.SatelliteID, evt); err != nil {
		log.Error().Err(err).Str("cluster", cfg.Name).Msg("failed to send backup trigger")
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}

	log.Info().Str("cluster", cfg.Name).Str("type", body.BackupType).Str("operation_id", operationID).Msg("backup trigger sent")
	return c.JSON(fiber.Map{"status": "backup triggered", "backup_type": body.BackupType, "operation_id": operationID})
}

// listRestoreOperations returns the restore history for a cluster.
func (s *RESTServer) listRestoreOperations(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid cluster id"})
	}

	cfg, err := s.store.GetClusterConfig(c.Context(), id)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "cluster not found"})
	}
	if cfg.SatelliteID == nil {
		return c.JSON([]interface{}{})
	}

	limit := c.QueryInt("limit", 50)
	items, err := s.store.ListRestoreOperations(c.Context(), *cfg.SatelliteID, cfg.Name, limit)
	if err != nil {
		log.Error().Err(err).Msg("failed to list restore operations")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list restores"})
	}
	return c.JSON(items)
}

// triggerRestore sends a restore command to the satellite for a cluster.
func (s *RESTServer) triggerRestore(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid cluster id"})
	}

	var body struct {
		RestoreType    string `json:"restore_type"`    // "logical" or "pitr"
		RestoreMode    string `json:"restore_mode"`    // "in_place" or "new_cluster" (PITR)
		BackupID       string `json:"backup_id"`       // source backup UUID (logical)
		BackupPath     string `json:"backup_path"`     // storage path
		TargetDatabase string `json:"target_database"` // logical restore target
		TargetTime     string `json:"target_time"`     // PITR target (RFC3339)
	}
	if err := c.BodyParser(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if body.RestoreType == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "restore_type is required"})
	}

	cfg, err := s.store.GetClusterConfig(c.Context(), id)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "cluster not found"})
	}
	if cfg.SatelliteID == nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": "cluster has no satellite"})
	}

	// Create restore operation record
	restoreOp := &models.RestoreOperation{
		ID:             uuid.New(),
		SatelliteID:    *cfg.SatelliteID,
		ClusterName:    cfg.Name,
		RestoreType:    body.RestoreType,
		RestoreMode:    body.RestoreMode,
		TargetDatabase: body.TargetDatabase,
		Status:         "pending",
	}
	if body.BackupID != "" {
		if bid, err := uuid.Parse(body.BackupID); err == nil {
			restoreOp.BackupID = &bid
		}
	}

	if err := s.store.CreateRestoreOperation(c.Context(), restoreOp); err != nil {
		log.Error().Err(err).Msg("failed to create restore operation")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create restore operation"})
	}

	// Parse target_time from request body if present
	var targetTimestamp *timestamppb.Timestamp
	if body.TargetTime != "" {
		if t, err := time.Parse(time.RFC3339, body.TargetTime); err == nil {
			targetTimestamp = timestamppb.New(t)
		}
	}

	// Build proto RestoreCommand
	cmd := &pgswarmv1.RestoreCommand{
		ClusterName:    cfg.Name,
		Namespace:      cfg.Namespace,
		RestoreId:      restoreOp.ID.String(),
		RestoreType:    body.RestoreType,
		RestoreMode:    body.RestoreMode,
		BackupId:       body.BackupID,
		BackupPath:     body.BackupPath,
		TargetDatabase: body.TargetDatabase,
		TargetTime:     targetTimestamp,
	}

	cmdJSON, err := protojson.Marshal(cmd)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to serialize restore command"})
	}

	operationID := uuid.New().String()
	evt := eventbus.NewEvent("restore.requested", cfg.Name, cfg.Namespace, "central")
	eventbus.WithOperationID(evt, operationID)
	eventbus.WithData(evt, "restore_command", string(cmdJSON))

	// Embed backup_config so the sidecar can apply storage credentials immediately,
	// even if it hasn't received a config_update since its last reconnect.
	spec, _ := cfg.ParseSpec()
	if spec.Backup != nil && spec.Backup.StoreID != nil {
		if bc, err := buildBackupConfig(s.store, s.encryptor, spec.Backup, cfg); err == nil {
			if bcJSON, err := protojson.Marshal(bc); err == nil {
				eventbus.WithData(evt, "backup_config", string(bcJSON))
			}
		}
	}

	if err := s.streams.PushEvent(*cfg.SatelliteID, evt); err != nil {
		log.Error().Err(err).Str("cluster", cfg.Name).Msg("failed to send restore command")
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}

	log.Info().Str("cluster", cfg.Name).Str("type", body.RestoreType).Str("restore_id", restoreOp.ID.String()).Msg("restore command sent")
	return c.JSON(fiber.Map{
		"status":       "restore initiated",
		"restore_id":   restoreOp.ID.String(),
		"operation_id": operationID,
	})
}
