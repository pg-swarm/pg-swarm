// Cluster and cluster database REST API handlers.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/pg-swarm/pg-swarm/internal/central/crypto"
	"github.com/pg-swarm/pg-swarm/internal/central/store"
	"github.com/pg-swarm/pg-swarm/internal/satellite/eventbus"
	"github.com/pg-swarm/pg-swarm/internal/shared/models"
	"github.com/rs/zerolog/log"
)

// --- Cluster Configs ---

func (s *RESTServer) listClusterConfigs(c *fiber.Ctx) error {
	configs, err := s.store.ListClusterConfigs(c.Context())
	if err != nil {
		log.Error().Err(err).Msg("failed to list cluster configs")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list cluster configs"})
	}
	return c.JSON(configs)
}

func (s *RESTServer) createClusterConfig(c *fiber.Ctx) error {
	var cfg models.ClusterConfig
	if err := c.BodyParser(&cfg); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}

	// Validate spec
	spec, err := cfg.ParseSpec()
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid config: " + err.Error()})
	}
	if err := models.ValidateArchiveSpec(spec.Archive); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	if err := validatePostgresImage(s.store, spec); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}

	cfg.ID = uuid.New()
	cfg.State = models.ClusterStateCreating

	// Initialize applied_profile_version to the latest profile version so
	// newly created clusters don't show a stale "update available" badge.
	if cfg.ProfileID != nil {
		if versions, err := s.store.ListConfigVersions(c.Context(), *cfg.ProfileID); err == nil && len(versions) > 0 {
			cfg.AppliedProfileVersion = versions[0].Version
		}
	}

	if err := s.store.CreateClusterConfig(c.Context(), &cfg); err != nil {
		log.Error().Err(err).Msg("failed to create cluster config")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create cluster config"})
	}

	log.Info().Str("config_id", cfg.ID.String()).Str("name", cfg.Name).Msg("cluster config created")

	s.pushConfigToSatellite(&cfg)

	auditLog(c, "cluster.create", "cluster", cfg.ID.String(), cfg.Name, "")
	return c.Status(fiber.StatusCreated).JSON(cfg)
}

func (s *RESTServer) getClusterConfig(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid config id"})
	}

	cfg, err := s.store.GetClusterConfig(c.Context(), id)
	if err != nil {
		log.Error().Err(err).Str("config_id", id.String()).Msg("failed to get cluster config")
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "cluster config not found"})
	}

	return c.JSON(cfg)
}

func (s *RESTServer) updateClusterConfig(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid config id"})
	}

	var cfg models.ClusterConfig
	if err := c.BodyParser(&cfg); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}

	// Validate spec
	spec, err := cfg.ParseSpec()
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid config: " + err.Error()})
	}
	if err := models.ValidateArchiveSpec(spec.Archive); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	if err := validatePostgresImage(s.store, spec); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}

	cfg.ID = id

	// Preserve server-managed fields that the client should not overwrite.
	existing, err := s.store.GetClusterConfig(c.Context(), id)
	if err != nil {
		log.Error().Err(err).Str("config_id", id.String()).Msg("failed to get existing cluster config for update")
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "cluster config not found"})
	}
	cfg.State = existing.State
	cfg.SatelliteID = existing.SatelliteID
	cfg.ProfileID = existing.ProfileID
	cfg.DeploymentRuleID = existing.DeploymentRuleID

	if err := s.store.UpdateClusterConfig(c.Context(), &cfg); err != nil {
		log.Error().Err(err).Str("config_id", id.String()).Msg("failed to update cluster config")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to update cluster config"})
	}

	log.Info().Str("config_id", id.String()).Str("name", cfg.Name).Msg("cluster config updated")

	s.pushConfigToSatellite(&cfg)

	auditLog(c, "cluster.update", "cluster", id.String(), cfg.Name, "")
	return c.JSON(cfg)
}

func (s *RESTServer) deleteClusterConfig(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid config id"})
	}

	// Fetch config before deleting so we can notify the satellite
	cfg, err := s.store.GetClusterConfig(c.Context(), id)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "cluster config not found"})
	}

	if err := s.store.DeleteClusterConfig(c.Context(), id); err != nil {
		log.Error().Err(err).Str("config_id", id.String()).Msg("failed to delete cluster config")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to delete cluster config"})
	}

	// Notify satellite to tear down resources
	if cfg.SatelliteID != nil {
		del := &pgswarmv1.DeleteCluster{ClusterName: cfg.Name, Namespace: cfg.Namespace}
		evt := eventbus.NewEvent("cluster.delete", cfg.Name, cfg.Namespace, "central")
		eventbus.WithSeverity(evt, "warning")
		evt.Payload = &pgswarmv1.Event_DeleteCluster{DeleteCluster: del}
		if err := s.streams.PushEvent(*cfg.SatelliteID, evt); err != nil {
			log.Warn().Err(err).Str("config_id", id.String()).Msg("failed to push delete event to satellite (may be offline)")
		}
	}

	log.Info().Str("config_id", id.String()).Msg("cluster config deleted")
	auditLog(c, "cluster.delete", "cluster", id.String(), cfg.Name, "")
	return c.JSON(fiber.Map{"status": "deleted"})
}

func (s *RESTServer) pauseCluster(c *fiber.Ctx) error {
	return s.setClusterPaused(c, true)
}

func (s *RESTServer) resumeCluster(c *fiber.Ctx) error {
	return s.setClusterPaused(c, false)
}

func (s *RESTServer) setClusterPaused(c *fiber.Ctx, paused bool) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid cluster id"})
	}

	cfg, err := s.store.SetClusterPaused(c.Context(), id, paused)
	if err != nil {
		action := "pause"
		if !paused {
			action = "resume"
		}
		log.Error().Err(err).Str("config_id", id.String()).Msg("failed to " + action + " cluster")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to " + action + " cluster"})
	}

	action := "cluster.pause"
	if !paused {
		action = "cluster.resume"
	}
	log.Info().Str("config_id", id.String()).Bool("paused", paused).Msg("cluster pause state changed")
	s.pushConfigToSatellite(cfg)
	auditLog(c, action, "cluster", id.String(), cfg.Name, "")
	return c.JSON(cfg)
}

func (s *RESTServer) switchoverCluster(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid cluster id"})
	}

	var body struct {
		TargetPod   string `json:"target_pod"`
		Interactive bool   `json:"interactive"`
	}
	if err := c.BodyParser(&body); err != nil || body.TargetPod == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "target_pod is required"})
	}

	cfg, err := s.store.GetClusterConfig(c.Context(), id)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "cluster not found"})
	}
	if cfg.SatelliteID == nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": "cluster has no satellite"})
	}

	operationID := uuid.New().String()
	satelliteID := cfg.SatelliteID.String()
	req := &pgswarmv1.SwitchoverRequest{
		ClusterName: cfg.Name,
		Namespace:   cfg.Namespace,
		TargetPod:   body.TargetPod,
		OperationId: operationID,
		Interactive: body.Interactive,
	}

	if s.opsTracker != nil {
		s.opsTracker.Start(operationID, cfg.Name, "", body.TargetPod, satelliteID, body.Interactive)
	}

	evt := eventbus.NewEvent("switchover.requested", cfg.Name, cfg.Namespace, "central")
	eventbus.WithOperationID(evt, operationID)
	eventbus.WithData(evt, "target_pod", body.TargetPod)
	evt.Payload = &pgswarmv1.Event_SwitchoverRequest{SwitchoverRequest: req}

	if err := s.streams.PushEvent(*cfg.SatelliteID, evt); err != nil {
		log.Error().Err(err).Str("cluster", cfg.Name).Msg("failed to send switchover event")
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}

	log.Info().Str("cluster", cfg.Name).Str("target", body.TargetPod).Str("operation_id", operationID).Bool("interactive", body.Interactive).Msg("switchover request sent")
	auditLog(c, "cluster.switchover", "cluster", id.String(), cfg.Name, "target_pod="+body.TargetPod)
	return c.JSON(fiber.Map{"status": "switchover initiated", "target_pod": body.TargetPod, "operation_id": operationID})
}

// switchoverContinue signals the satellite to execute the next switchover step.
// Called by the dashboard after the user reviews the result of the current step.
func (s *RESTServer) switchoverContinue(c *fiber.Ctx) error {
	clusterID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid cluster id"})
	}
	_ = clusterID

	var body struct {
		OperationID string `json:"operation_id"`
	}
	if err := c.BodyParser(&body); err != nil || body.OperationID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "operation_id is required"})
	}

	if s.opsTracker == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "ops tracker unavailable"})
	}
	op, ok := s.opsTracker.GetOp(body.OperationID)
	if !ok {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "operation not found"})
	}
	if !op.WaitingForUser {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "operation is not waiting for user input"})
	}

	satID, err := uuid.Parse(op.SatelliteID)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "invalid satellite id in operation"})
	}

	evt := eventbus.NewEvent("switchover.step.execute", op.ClusterName, "", "central")
	eventbus.WithOperationID(evt, body.OperationID)

	if err := s.streams.PushEvent(satID, evt); err != nil {
		log.Error().Err(err).Str("operation_id", body.OperationID).Msg("failed to send switchover continue event")
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}

	s.opsTracker.ClearWaitingForUser(body.OperationID)
	log.Info().Str("operation_id", body.OperationID).Int32("step", op.CurrentStep).Msg("switchover continue sent")
	return c.JSON(fiber.Map{"status": "proceeding"})
}

// switchoverAbort signals the satellite to abort an in-progress interactive switchover.
func (s *RESTServer) switchoverAbort(c *fiber.Ctx) error {
	clusterID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid cluster id"})
	}
	_ = clusterID

	var body struct {
		OperationID string `json:"operation_id"`
	}
	if err := c.BodyParser(&body); err != nil || body.OperationID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "operation_id is required"})
	}

	if s.opsTracker == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "ops tracker unavailable"})
	}
	op, ok := s.opsTracker.GetOp(body.OperationID)
	if !ok {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "operation not found"})
	}

	satID, err := uuid.Parse(op.SatelliteID)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "invalid satellite id in operation"})
	}

	evt := eventbus.NewEvent("switchover.abort", op.ClusterName, "", "central")
	eventbus.WithOperationID(evt, body.OperationID)

	if err := s.streams.PushEvent(satID, evt); err != nil {
		log.Error().Err(err).Str("operation_id", body.OperationID).Msg("failed to send switchover abort event")
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}

	s.opsTracker.ClearWaitingForUser(body.OperationID)
	log.Info().Str("operation_id", body.OperationID).Msg("switchover abort sent")
	return c.JSON(fiber.Map{"status": "aborting"})
}

func (s *RESTServer) clusterProfileDiff(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid cluster id"})
	}

	cfg, err := s.store.GetClusterConfig(c.Context(), id)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "cluster not found"})
	}
	if cfg.ProfileID == nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "cluster has no profile"})
	}

	profile, err := s.store.GetProfile(c.Context(), *cfg.ProfileID)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "profile not found"})
	}

	versions, err := s.store.ListConfigVersions(c.Context(), *cfg.ProfileID)
	if err != nil || len(versions) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "no profile versions found"})
	}
	latestVersion := versions[0].Version

	if cfg.AppliedProfileVersion >= latestVersion {
		return c.JSON(fiber.Map{"apply_strategy": "no_change"})
	}

	oldSpec, err := cfg.ParseSpec()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to parse cluster config"})
	}

	var newSpec models.ClusterSpec
	if err := json.Unmarshal(profile.Config, &newSpec); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to parse profile config"})
	}

	// Fill in server-side defaults so params already applied by the satellite
	// don't appear as spurious changes in the diff.
	fillDefaultPgParams(oldSpec)
	fillDefaultPgParams(&newSpec)

	paramModes := s.loadParamClassifications(c.Context())
	diff := classifyChanges(oldSpec, &newSpec, paramModes)

	return c.JSON(fiber.Map{
		"cluster_name":            cfg.Name,
		"profile_name":            profile.Name,
		"applied_profile_version": cfg.AppliedProfileVersion,
		"latest_profile_version":  latestVersion,
		"reload_changes":          diff.ReloadChanges,
		"sequential_changes":      diff.SequentialChanges,
		"full_restart_changes":    diff.FullRestartChanges,
		"immutable_errors":        diff.ImmutableErrors,
		"scale_up":                diff.ScaleUp,
		"scale_down":              diff.ScaleDown,
		"apply_strategy":          diff.ApplyStrategy(),
	})
}

// applyCluster applies the latest profile config to a single cluster.
func (s *RESTServer) applyCluster(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid cluster id"})
	}

	var body struct {
		Confirmed bool `json:"confirmed"`
	}
	if err := c.BodyParser(&body); err != nil || !body.Confirmed {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "confirmation required"})
	}

	cfg, err := s.store.GetClusterConfig(c.Context(), id)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "cluster not found"})
	}
	if cfg.ProfileID == nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "cluster has no profile"})
	}

	// Load the profile's current config
	profile, err := s.store.GetProfile(c.Context(), *cfg.ProfileID)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "profile not found"})
	}

	// Get latest profile version
	versions, err := s.store.ListConfigVersions(c.Context(), *cfg.ProfileID)
	if err != nil || len(versions) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "no profile versions found"})
	}
	latestVersion := versions[0].Version

	// Update cluster config to match profile
	cfg.Config = profile.Config
	cfg.AppliedProfileVersion = latestVersion
	cfg.State = models.ClusterStateUpdating
	if err := s.store.UpdateClusterConfig(c.Context(), cfg); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}

	// Push to satellite
	spec, _ := cfg.ParseSpec()
	log.Info().
		Str("cluster", cfg.Name).
		Int("profile_version", latestVersion).
		Int64("config_version", cfg.ConfigVersion).
		Interface("pg_params", spec.PgParams).
		Msg("pushing cluster config to satellite")
	s.pushConfigToSatellite(cfg)

	auditLog(c, "cluster.apply", "cluster", id.String(), cfg.Name, fmt.Sprintf("profile_version=%d", latestVersion))
	return c.JSON(fiber.Map{
		"status":                  "in_progress",
		"cluster":                 cfg.Name,
		"applied_profile_version": latestVersion,
	})
}

func resolveStorageTiers(rawConfig json.RawMessage, tierMappings map[string]string) (json.RawMessage, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(rawConfig, &doc); err != nil {
		return rawConfig, fmt.Errorf("unmarshal config for tier resolution: %w", err)
	}

	resolve := func(sc string) (string, error) {
		if len(sc) > 5 && sc[:5] == "tier:" {
			tier := sc[5:]
			mapped, ok := tierMappings[tier]
			if !ok {
				return "", fmt.Errorf("unmapped tier %q", tier)
			}
			return mapped, nil
		}
		return sc, nil
	}

	resolveStorage := func(key string) error {
		raw, ok := doc[key]
		if !ok || raw == nil {
			return nil
		}
		var storage map[string]json.RawMessage
		if err := json.Unmarshal(raw, &storage); err != nil {
			return nil // not a storage object, skip
		}
		scRaw, ok := storage["storage_class"]
		if !ok {
			return nil
		}
		var sc string
		if err := json.Unmarshal(scRaw, &sc); err != nil {
			return nil
		}
		resolved, err := resolve(sc)
		if err != nil {
			return err
		}
		if resolved != sc {
			storage["storage_class"], _ = json.Marshal(resolved)
			doc[key], _ = json.Marshal(storage)
		}
		return nil
	}

	var missing []string
	if err := resolveStorage("storage"); err != nil {
		missing = append(missing, err.Error())
	}
	if err := resolveStorage("wal_storage"); err != nil {
		missing = append(missing, err.Error())
	}

	if len(missing) > 0 {
		return rawConfig, fmt.Errorf("missing tier mappings: %s", strings.Join(missing, ", "))
	}

	resolved, err := json.Marshal(doc)
	if err != nil {
		return rawConfig, fmt.Errorf("marshal resolved config: %w", err)
	}
	return resolved, nil
}

func validatePostgresImage(st store.Store, spec *models.ClusterSpec) error {
	if spec.Postgres.Version == "" {
		return fmt.Errorf("postgres version is required")
	}
	image := resolvePostgresImage(st, spec)
	if image == "" {
		variant := spec.Postgres.Variant
		if variant == "" {
			variant = "alpine"
		}
		return fmt.Errorf("cannot resolve postgres image: version %q variant %q is not in the postgres versions registry and no explicit image was provided — add it via Admin > Postgres Versions or set the image field", spec.Postgres.Version, variant)
	}
	return nil
}

// resolvePostgresImage looks up the image tag for a version+variant and prepends the registry.
func resolvePostgresImage(st store.Store, spec *models.ClusterSpec) string {
	version := spec.Postgres.Version
	variant := spec.Postgres.Variant
	if variant == "" {
		variant = "alpine"
	}

	pv, err := st.GetPostgresVersionBySpec(context.Background(), version, variant)
	if err != nil {
		log.Warn().Str("version", version).Str("variant", variant).Msg("postgres version not found in DB, using image field as-is")
		return spec.Postgres.Image
	}

	image := "postgres:" + pv.ImageTag
	if spec.Postgres.Registry != "" {
		image = spec.Postgres.Registry + "/postgres:" + pv.ImageTag
	}
	return image
}

// --- Config push helper ---

func (s *RESTServer) pushConfigToSatellite(cfg *models.ClusterConfig) {
	if s.streams == nil || cfg.SatelliteID == nil {
		return
	}

	protoConfig, err := buildProtoClusterConfig(s.store, cfg, s.encryptor)
	if err != nil {
		log.Error().Err(err).
			Str("config_id", cfg.ID.String()).
			Str("name", cfg.Name).
			Msg("failed to build proto config for push")
		return
	}

	// Determine event type: create (version 1) vs update (version > 1)
	evtType := "cluster.update"
	if protoConfig.ConfigVersion <= 1 {
		evtType = "cluster.create"
	}

	evt := eventbus.NewEvent(evtType, protoConfig.ClusterName, protoConfig.Namespace, "central")
	evt.Payload = &pgswarmv1.Event_ClusterConfig{ClusterConfig: protoConfig}

	if err := s.streams.PushEvent(*cfg.SatelliteID, evt); err != nil {
		log.Error().Err(err).
			Str("satellite_id", cfg.SatelliteID.String()).
			Str("config_id", cfg.ID.String()).
			Str("event_type", evtType).
			Msg("failed to push config event to satellite")
	}
}

// buildProtoClusterConfig converts a models.ClusterConfig into the protobuf
// ClusterConfig that satellites expect. It resolves profile names, label
// selectors, and postgres images from the store.
func buildProtoClusterConfig(st store.Store, cfg *models.ClusterConfig, enc *crypto.Encryptor) (*pgswarmv1.ClusterConfig, error) {
	spec, err := cfg.ParseSpec()
	if err != nil {
		return nil, fmt.Errorf("parse cluster spec: %w", err)
	}

	// Resolve profile name and label selector for K8s labels
	var profileName string
	var labelSelector map[string]string
	if cfg.ProfileID != nil {
		if p, err := st.GetProfile(context.Background(), *cfg.ProfileID); err == nil {
			profileName = p.Name
		}
	}
	if cfg.DeploymentRuleID != nil {
		if r, err := st.GetDeploymentRule(context.Background(), *cfg.DeploymentRuleID); err == nil {
			labelSelector = r.LabelSelector
		}
	}

	// Resolve the image from the postgres_versions table
	resolvedImage := resolvePostgresImage(st, spec)

	protoConfig := &pgswarmv1.ClusterConfig{
		ClusterName:        cfg.Name,
		Namespace:          cfg.Namespace,
		Replicas:           spec.Replicas,
		ConfigVersion:      cfg.ConfigVersion,
		ProfileName:        profileName,
		LabelSelector:      labelSelector,
		Paused:             cfg.Paused,
		DeletionProtection: spec.DeletionProtection,
		Postgres: &pgswarmv1.PostgresSpec{
			Version: spec.Postgres.Version,
			Image:   resolvedImage,
		},
		Storage: &pgswarmv1.StorageSpec{
			Size:         spec.Storage.Size,
			StorageClass: spec.Storage.StorageClass,
		},
		Resources: &pgswarmv1.ResourceSpec{
			CpuRequest:    spec.Resources.CPURequest,
			CpuLimit:      spec.Resources.CPULimit,
			MemoryRequest: spec.Resources.MemoryRequest,
			MemoryLimit:   spec.Resources.MemoryLimit,
		},
		PgParams: spec.PgParams,
		HbaRules: spec.HbaRules,
	}

	if spec.WalStorage != nil {
		protoConfig.WalStorage = &pgswarmv1.StorageSpec{
			Size:         spec.WalStorage.Size,
			StorageClass: spec.WalStorage.StorageClass,
		}
	}

	if spec.Archive != nil && spec.Archive.Mode != "" {
		protoConfig.Archive = &pgswarmv1.ArchiveSpec{
			Mode:                  spec.Archive.Mode,
			ArchiveCommand:        spec.Archive.ArchiveCommand,
			RestoreCommand:        spec.Archive.RestoreCommand,
			ArchiveTimeoutSeconds: spec.Archive.ArchiveTimeoutSeconds,
		}
		if spec.Archive.CredentialsSecret != nil {
			protoConfig.Archive.CredentialsSecret = &pgswarmv1.SecretRef{
				Name: spec.Archive.CredentialsSecret.Name,
			}
		}
	}

	if spec.Sentinel != nil && spec.Sentinel.Enabled {
		protoConfig.Sentinel = &pgswarmv1.SentinelSpec{
			Enabled:                    true,
			HealthCheckIntervalSeconds: spec.Sentinel.HealthCheckIntervalSeconds,
			SidecarImage:               spec.Sentinel.SidecarImage,
		}
	}

	// Resolve backup config from profile + backup store
	if spec.Backup != nil && spec.Backup.StoreID != nil {
		if bc, err := buildBackupConfig(st, enc, spec.Backup, cfg); err == nil {
			protoConfig.Backups = bc
		} else {
			log.Warn().Err(err).Str("cluster", cfg.Name).Msg("failed to build backup config, skipping")
		}
	}

	// Resolve event handlers from profile's event_rule_set_id
	if cfg.ProfileID != nil {
		if p, err := st.GetProfile(context.Background(), *cfg.ProfileID); err == nil && p.EventRuleSetID != nil {
			if handlers, err := st.ListRuleSetHandlers(context.Background(), *p.EventRuleSetID); err == nil {
				for _, h := range handlers {
					if !h.Enabled {
						continue
					}
					// We need the full rule details — fetch them
				}
				// Get all rules to build a map
				if allRules, err := st.ListEventRules(context.Background()); err == nil {
					ruleMap := make(map[uuid.UUID]*models.EventRule, len(allRules))
					for _, r := range allRules {
						ruleMap[r.ID] = r
					}
					for _, h := range handlers {
						if !h.Enabled {
							continue
						}
						r := ruleMap[h.EventRuleID]
						if r == nil || !r.Enabled {
							continue
						}
						protoConfig.RecoveryRules = append(protoConfig.RecoveryRules, &pgswarmv1.RecoveryRule{
							Name:            r.Name,
							Pattern:         r.Pattern,
							Severity:        r.Severity,
							CooldownSeconds: int32(r.CooldownSeconds),
							Enabled:         r.Enabled,
							Category:        r.Category,
						})
					}
				}
			}
		}
	}

	// Include cluster-level databases (dynamically managed, not from profile)
	clusterDBs, err := st.ListClusterDatabases(context.Background(), cfg.ID)
	if err == nil {
		for _, cdb := range clusterDBs {
			password := ""
			if len(cdb.Password) > 0 && enc != nil {
				if decrypted, err := enc.Decrypt(cdb.Password); err == nil {
					password = string(decrypted)
				}
			}
			protoConfig.ClusterDatabases = append(protoConfig.ClusterDatabases, &pgswarmv1.ClusterDatabase{
				DbName:       cdb.DBName,
				DbUser:       cdb.DBUser,
				Password:     password,
				AllowedCidrs: cdb.AllowedCIDRs,
			})
		}
	}

	return protoConfig, nil
}

// buildBackupConfig resolves a BackupSpec into a proto BackupConfig by looking
// up the BackupStore, decrypting its credentials, and assembling the destination.
func buildBackupConfig(st store.Store, enc *crypto.Encryptor, backup *models.BackupSpec, cfg *models.ClusterConfig) (*pgswarmv1.BackupConfig, error) {
	if backup.StoreID == nil {
		return nil, fmt.Errorf("backup store_id is required")
	}

	bs, err := st.GetBackupStore(context.Background(), *backup.StoreID)
	if err != nil {
		return nil, fmt.Errorf("get backup store: %w", err)
	}

	bc := &pgswarmv1.BackupConfig{
		StoreId:  bs.ID.String(),
		BasePath: cfg.Namespace + "/" + cfg.Name,
	}

	// Physical
	if backup.Physical != nil && backup.Physical.Enabled {
		bc.Physical = &pgswarmv1.PhysicalBackupConfig{
			BaseSchedule:          backup.Physical.BaseSchedule,
			IncrementalSchedule:   backup.Physical.IncrementalSchedule,
			WalArchiveEnabled:     backup.Physical.WalArchiveEnabled,
			ArchiveTimeoutSeconds: backup.Physical.ArchiveTimeoutSeconds,
		}
	}

	// Logical
	if backup.Logical != nil && backup.Logical.Enabled {
		bc.Logical = &pgswarmv1.LogicalBackupConfig{
			Schedule:  backup.Logical.Schedule,
			Databases: backup.Logical.Databases,
			Format:    backup.Logical.Format,
		}
	}

	// Retention
	if backup.Retention != nil {
		bc.Retention = &pgswarmv1.BackupRetention{
			BaseBackupCount:        int32(backup.Retention.BaseBackupCount),
			IncrementalBackupCount: int32(backup.Retention.IncrementalBackupCount),
			WalRetentionDays:       int32(backup.Retention.WalRetentionDays),
			LogicalBackupCount:     int32(backup.Retention.LogicalBackupCount),
		}
	}

	// Destination — decrypt credentials and build proto destination
	dest := &pgswarmv1.BackupDestination{Type: bs.StoreType}

	var storeConfig json.RawMessage
	if bs.Config != nil {
		storeConfig = bs.Config
	}

	var plainCreds []byte
	if bs.Credentials != nil && enc != nil {
		plainCreds, err = enc.Decrypt(bs.Credentials)
		if err != nil {
			return nil, fmt.Errorf("decrypt backup store credentials: %w", err)
		}
	}

	switch bs.StoreType {
	case "gcs":
		var cfg models.GCSStoreConfig
		if storeConfig != nil {
			_ = json.Unmarshal(storeConfig, &cfg)
		}
		gcs := &pgswarmv1.GCSDestination{
			Bucket:     cfg.Bucket,
			PathPrefix: cfg.PathPrefix,
		}
		if plainCreds != nil {
			var creds models.GCSCredentials
			if json.Unmarshal(plainCreds, &creds) == nil {
				gcs.ServiceAccountJson = creds.ServiceAccountJSON
			}
		}
		dest.Gcs = gcs

	case "sftp":
		var cfg models.SFTPStoreConfig
		if storeConfig != nil {
			_ = json.Unmarshal(storeConfig, &cfg)
		}
		sftp := &pgswarmv1.SFTPDestination{
			Host:     cfg.Host,
			Port:     int32(cfg.Port),
			User:     cfg.User,
			BasePath: cfg.BasePath,
		}
		if plainCreds != nil {
			var creds models.SFTPCredentials
			if json.Unmarshal(plainCreds, &creds) == nil {
				sftp.Password = creds.Password
				sftp.PrivateKey = creds.PrivateKey
			}
		}
		dest.Sftp = sftp

	case "s3":
		var cfg models.S3StoreConfig
		if storeConfig != nil {
			_ = json.Unmarshal(storeConfig, &cfg)
		}
		s3dest := &pgswarmv1.S3Destination{
			Bucket:         cfg.Bucket,
			Region:         cfg.Region,
			PathPrefix:     cfg.PathPrefix,
			Endpoint:       cfg.Endpoint,
			ForcePathStyle: cfg.ForcePathStyle,
		}
		if plainCreds != nil {
			var creds models.S3Credentials
			if json.Unmarshal(plainCreds, &creds) == nil {
				s3dest.AccessKeyId = creds.AccessKeyID
				s3dest.SecretAccessKey = creds.SecretAccessKey
			}
		}
		dest.S3 = s3dest
	}

	bc.Destination = dest
	return bc, nil
}

// rePushClustersForProfile updates the config on all clusters linked to the
// given profile, bumps their config_version, and pushes to satellites.
func (s *RESTServer) rePushClustersForProfile(ctx context.Context, profileID uuid.UUID) {
	// Load the profile's current config
	profile, err := s.store.GetProfile(ctx, profileID)
	if err != nil {
		log.Error().Err(err).Msg("failed to get profile for re-push")
		return
	}

	// Find all clusters using this profile (covers both manual and rule-based)
	clusters, err := s.store.GetClusterConfigsByProfile(ctx, profileID)
	if err != nil {
		log.Error().Err(err).Msg("failed to get clusters for profile")
		return
	}
	for _, cfg := range clusters {
		// Update the cluster's config to match the profile's current config
		cfg.Config = profile.Config
		if err := s.store.UpdateClusterConfig(ctx, cfg); err != nil {
			log.Error().Err(err).Str("cluster", cfg.Name).Msg("failed to update cluster config")
			continue
		}
		s.pushConfigToSatellite(cfg)
	}
}

// --- Cluster Databases ---

func (s *RESTServer) listClusterDatabases(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid cluster id"})
	}
	dbs, err := s.store.ListClusterDatabases(c.Context(), id)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	if dbs == nil {
		dbs = []*models.ClusterDatabase{}
	}
	return c.JSON(dbs)
}

func (s *RESTServer) createClusterDatabase(c *fiber.Ctx) error {
	clusterID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid cluster id"})
	}

	var body struct {
		DBName       string   `json:"db_name"`
		DBUser       string   `json:"db_user"`
		Password     string   `json:"password"`
		AllowedCIDRs []string `json:"allowed_cidrs"`
	}
	if err := c.BodyParser(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if body.DBName == "" || body.DBUser == "" || body.Password == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "db_name, db_user, and password are required"})
	}

	// Encrypt password
	var encPassword []byte
	if s.encryptor != nil {
		encPassword, err = s.encryptor.Encrypt([]byte(body.Password))
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to encrypt password"})
		}
	}

	db := &models.ClusterDatabase{
		ClusterID:    clusterID,
		DBName:       body.DBName,
		DBUser:       body.DBUser,
		Password:     encPassword,
		AllowedCIDRs: body.AllowedCIDRs,
	}
	if err := s.store.CreateClusterDatabase(c.Context(), db); err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}

	// Push updated config to satellite
	cfg, _ := s.store.GetClusterConfig(c.Context(), clusterID)
	if cfg != nil {
		_ = s.store.UpdateClusterConfig(c.Context(), cfg) // bump config_version
		s.pushConfigToSatellite(cfg)
	}

	log.Info().Str("cluster_id", clusterID.String()).Str("db", body.DBName).Msg("cluster database created")
	auditLog(c, "cluster_database.create", "cluster_database", db.ID.String(), body.DBName, "cluster_id="+clusterID.String())
	return c.Status(fiber.StatusCreated).JSON(db)
}

func (s *RESTServer) updateClusterDatabase(c *fiber.Ctx) error {
	dbID, err := uuid.Parse(c.Params("dbid"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid database id"})
	}
	clusterID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid cluster id"})
	}

	var body struct {
		DBUser       string   `json:"db_user"`
		Password     string   `json:"password"`
		AllowedCIDRs []string `json:"allowed_cidrs"`
	}
	if err := c.BodyParser(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}

	db := &models.ClusterDatabase{
		ID:           dbID,
		AllowedCIDRs: body.AllowedCIDRs,
	}
	if body.DBUser != "" {
		db.DBUser = body.DBUser
	}
	if body.Password != "" && s.encryptor != nil {
		encPassword, err := s.encryptor.Encrypt([]byte(body.Password))
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to encrypt password"})
		}
		db.Password = encPassword
	}

	if err := s.store.UpdateClusterDatabase(c.Context(), db); err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}

	// Push updated config to satellite
	cfg, _ := s.store.GetClusterConfig(c.Context(), clusterID)
	if cfg != nil {
		_ = s.store.UpdateClusterConfig(c.Context(), cfg)
		s.pushConfigToSatellite(cfg)
	}

	log.Info().Str("db_id", dbID.String()).Msg("cluster database updated")
	auditLog(c, "cluster_database.update", "cluster_database", dbID.String(), "", "cluster_id="+clusterID.String())
	return c.JSON(db)
}

func (s *RESTServer) deleteClusterDatabase(c *fiber.Ctx) error {
	dbID, err := uuid.Parse(c.Params("dbid"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid database id"})
	}
	clusterID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid cluster id"})
	}

	if err := s.store.DeleteClusterDatabase(c.Context(), dbID); err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
	}

	// Push updated config to satellite (HBA rules will be regenerated without this DB)
	cfg, _ := s.store.GetClusterConfig(c.Context(), clusterID)
	if cfg != nil {
		_ = s.store.UpdateClusterConfig(c.Context(), cfg)
		s.pushConfigToSatellite(cfg)
	}

	log.Info().Str("db_id", dbID.String()).Msg("cluster database deleted")
	auditLog(c, "cluster_database.delete", "cluster_database", dbID.String(), "", "cluster_id="+clusterID.String())
	return c.SendStatus(fiber.StatusNoContent)
}

// setSidecarLogLevel sends a log-level change command to all sentinel sidecars in a cluster.
func (s *RESTServer) setSidecarLogLevel(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid cluster id"})
	}

	var body struct {
		Level string `json:"level"`
	}
	if err := c.BodyParser(&body); err != nil || body.Level == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "level is required (trace, debug, info, warn, error)"})
	}

	cfg, err := s.store.GetClusterConfig(c.Context(), id)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "cluster not found"})
	}
	if cfg.SatelliteID == nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": "cluster has no satellite"})
	}

	evt := eventbus.NewEvent("sidecar.set_log_level", cfg.Name, cfg.Namespace, "central")
	eventbus.WithData(evt, "level", body.Level)

	if err := s.streams.PushEvent(*cfg.SatelliteID, evt); err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}

	log.Info().Str("cluster", cfg.Name).Str("level", body.Level).Msg("sidecar log level change sent")
	auditLog(c, "sidecar.set_log_level", "cluster", id.String(), cfg.Name, "level="+body.Level)
	return c.JSON(fiber.Map{"status": "level change sent", "level": body.Level})
}
