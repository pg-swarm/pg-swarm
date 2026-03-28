// Profile REST API handlers.
package server

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/pg-swarm/pg-swarm/internal/satellite/eventbus"
	"github.com/pg-swarm/pg-swarm/internal/shared/models"
	"github.com/rs/zerolog/log"
)

// --- Profiles ---

func (s *RESTServer) listProfiles(c *fiber.Ctx) error {
	profiles, err := s.store.ListProfiles(c.Context())
	if err != nil {
		log.Error().Err(err).Msg("failed to list profiles")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list profiles"})
	}
	return c.JSON(profiles)
}

func (s *RESTServer) createProfile(c *fiber.Ctx) error {
	var profile models.ClusterProfile
	if err := c.BodyParser(&profile); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if profile.Name == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "name is required"})
	}

	spec, err := profile.ParseSpec()
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid config: " + err.Error()})
	}
	if err := models.ValidateArchiveSpec(spec.Archive); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	if err := models.ValidateBackupSpec(spec.Backup); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	if err := validatePostgresImage(s.store, spec); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}

	profile.ID = uuid.New()

	if err := s.store.CreateProfile(c.Context(), &profile); err != nil {
		log.Error().Err(err).Msg("failed to create profile")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create profile"})
	}

	log.Info().Str("profile_id", profile.ID.String()).Str("name", profile.Name).Msg("profile created")
	auditLog(c, "profile.create", "profile", profile.ID.String(), profile.Name, "")
	return c.Status(fiber.StatusCreated).JSON(profile)
}

func (s *RESTServer) getProfile(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid profile id"})
	}
	profile, err := s.store.GetProfile(c.Context(), id)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "profile not found"})
	}
	return c.JSON(profile)
}

func (s *RESTServer) updateProfile(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid profile id"})
	}

	var profile models.ClusterProfile
	if err := c.BodyParser(&profile); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}

	spec, err := profile.ParseSpec()
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid config: " + err.Error()})
	}
	if err := models.ValidateArchiveSpec(spec.Archive); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	if err := models.ValidateBackupSpec(spec.Backup); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	if err := validatePostgresImage(s.store, spec); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}

	// Load existing profile for diff computation
	existing, err := s.store.GetProfile(c.Context(), id)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "profile not found"})
	}
	oldSpec, _ := existing.ParseSpec()

	// Classify changes (load full-restart set from DB)
	paramModes := s.loadParamClassifications(c.Context())
	diff := classifyChanges(oldSpec, spec, paramModes)

	// Check immutable fields if active clusters exist
	clusters, _ := s.store.GetClusterConfigsByProfile(c.Context(), id)
	if len(clusters) > 0 && len(diff.ImmutableErrors) > 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error":            "immutable fields cannot be changed on profiles with active clusters",
			"immutable_errors": diff.ImmutableErrors,
		})
	}

	// Save profile (no lock check)
	profile.ID = id
	if err := s.store.UpdateProfile(c.Context(), &profile); err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}

	log.Info().Str("profile_id", id.String()).Str("name", profile.Name).Msg("profile updated")
	auditLog(c, "profile.update", "profile", id.String(), profile.Name, "")

	// Create config version record
	_ = s.store.CreateConfigVersion(c.Context(), &models.ConfigVersion{
		ProfileID:     id,
		Config:        profile.Config,
		ChangeSummary: diff.Summary(),
		ApplyStatus:   "pending",
	})

	// If active clusters exist, return change_impact for confirmation
	if len(clusters) > 0 && diff.ApplyStrategy() != "no_change" {
		clusterNames := make([]string, len(clusters))
		for i, cfg := range clusters {
			clusterNames[i] = cfg.Name
		}
		return c.JSON(fiber.Map{
			"profile": profile,
			"change_impact": fiber.Map{
				"affected_clusters":     clusterNames,
				"reload_changes":        diff.ReloadChanges,
				"sequential_changes":    diff.SequentialChanges,
				"full_restart_changes":  diff.FullRestartChanges,
				"apply_strategy":        diff.ApplyStrategy(),
				"requires_confirmation": true,
			},
		})
	}

	return c.JSON(profile)
}

// applyProfile lists clusters that have pending profile updates.
// Actual application is done per-cluster via POST /clusters/:id/apply.
func (s *RESTServer) applyProfile(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid profile id"})
	}

	// Get latest profile version
	versions, err := s.store.ListConfigVersions(c.Context(), id)
	if err != nil || len(versions) == 0 {
		return c.JSON(fiber.Map{"pending_clusters": []string{}})
	}
	latestVersion := versions[0].Version

	// Find clusters with outdated profile version
	clusters, _ := s.store.GetClusterConfigsByProfile(c.Context(), id)
	var pending []fiber.Map
	for _, cfg := range clusters {
		if cfg.AppliedProfileVersion < latestVersion {
			pending = append(pending, fiber.Map{
				"id":                      cfg.ID,
				"name":                    cfg.Name,
				"applied_profile_version": cfg.AppliedProfileVersion,
				"latest_profile_version":  latestVersion,
			})
		}
	}
	if pending == nil {
		pending = []fiber.Map{}
	}

	auditLog(c, "profile.apply", "profile", id.String(), "", fmt.Sprintf("pending_clusters=%d", len(pending)))
	return c.JSON(fiber.Map{"pending_clusters": pending})
}

func (s *RESTServer) listProfileVersions(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid profile id"})
	}
	versions, err := s.store.ListConfigVersions(c.Context(), id)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	if versions == nil {
		versions = []*models.ConfigVersion{}
	}
	return c.JSON(versions)
}

func (s *RESTServer) getProfileVersion(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid profile id"})
	}
	version, err := strconv.Atoi(c.Params("version"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid version number"})
	}
	v, err := s.store.GetConfigVersion(c.Context(), id, version)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "version not found"})
	}
	return c.JSON(v)
}

func (s *RESTServer) revertProfile(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid profile id"})
	}

	var body struct {
		TargetVersion int `json:"target_version"`
	}
	if err := c.BodyParser(&body); err != nil || body.TargetVersion < 1 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "target_version is required"})
	}

	// Load target version
	v, err := s.store.GetConfigVersion(c.Context(), id, body.TargetVersion)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "version not found"})
	}

	// Load current profile for diff
	existing, err := s.store.GetProfile(c.Context(), id)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "profile not found"})
	}

	oldSpec, _ := existing.ParseSpec()
	var newSpec models.ClusterSpec
	if err := json.Unmarshal(v.Config, &newSpec); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "invalid version config"})
	}
	diff := classifyChanges(oldSpec, &newSpec, s.loadParamClassifications(c.Context()))

	// Check immutable fields
	clusters, _ := s.store.GetClusterConfigsByProfile(c.Context(), id)
	if len(clusters) > 0 && len(diff.ImmutableErrors) > 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error":            "revert would change immutable fields on active clusters",
			"immutable_errors": diff.ImmutableErrors,
		})
	}

	// Update profile with reverted config
	existing.Config = v.Config
	if err := s.store.UpdateProfile(c.Context(), existing); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}

	// Create new version record for the revert
	_ = s.store.CreateConfigVersion(c.Context(), &models.ConfigVersion{
		ProfileID:     id,
		Config:        v.Config,
		ChangeSummary: fmt.Sprintf("Reverted to version %d", body.TargetVersion),
		ApplyStatus:   "pending",
	})

	auditLog(c, "profile.revert", "profile", id.String(), existing.Name, fmt.Sprintf("target_version=%d", body.TargetVersion))

	// Return change impact
	if len(clusters) > 0 && diff.ApplyStrategy() != "no_change" {
		clusterNames := make([]string, len(clusters))
		for i, cfg := range clusters {
			clusterNames[i] = cfg.Name
		}
		return c.JSON(fiber.Map{
			"profile": existing,
			"change_impact": fiber.Map{
				"affected_clusters":     clusterNames,
				"reload_changes":        diff.ReloadChanges,
				"sequential_changes":    diff.SequentialChanges,
				"full_restart_changes":  diff.FullRestartChanges,
				"apply_strategy":        diff.ApplyStrategy(),
				"requires_confirmation": true,
			},
		})
	}

	return c.JSON(existing)
}

func (s *RESTServer) deleteProfile(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid profile id"})
	}

	cascade := c.Query("cascade") == "true"
	ctx := c.Context()

	if cascade {
		// 1. Delete all clusters linked to this profile (and notify satellites)
		clusters, err := s.store.GetClusterConfigsByProfile(ctx, id)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list profile clusters"})
		}
		for _, cfg := range clusters {
			if cfg.SatelliteID != nil {
				del := &pgswarmv1.DeleteCluster{ClusterName: cfg.Name, Namespace: cfg.Namespace}
				evt := eventbus.NewEvent("cluster.delete", cfg.Name, cfg.Namespace, "central")
				eventbus.WithSeverity(evt, "warning")
				evt.Payload = &pgswarmv1.Event_DeleteCluster{DeleteCluster: del}
				if pushErr := s.streams.PushEvent(*cfg.SatelliteID, evt); pushErr != nil {
					log.Warn().Err(pushErr).Str("cluster", cfg.Name).Msg("cascade: failed to push delete event to satellite")
				}
			}
			if delErr := s.store.DeleteClusterConfig(ctx, cfg.ID); delErr != nil {
				log.Error().Err(delErr).Str("cluster_id", cfg.ID.String()).Msg("cascade: failed to delete cluster config")
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to delete cluster " + cfg.Name})
			}
		}

		// 2. Delete deployment rules linked to this profile
		rules, err := s.store.GetDeploymentRulesByProfile(ctx, id)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list profile deployment rules"})
		}
		for _, rule := range rules {
			if delErr := s.store.DeleteDeploymentRule(ctx, rule.ID); delErr != nil {
				log.Error().Err(delErr).Str("rule_id", rule.ID.String()).Msg("cascade: failed to delete deployment rule")
			}
		}

		// 3. Force-delete the profile (bypasses lock check)
		if err := s.store.ForceDeleteProfile(ctx, id); err != nil {
			return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
		}
		log.Info().Str("profile_id", id.String()).Int("clusters_deleted", len(clusters)).Int("rules_deleted", len(rules)).Msg("profile cascade deleted")
		auditLog(c, "profile.delete", "profile", id.String(), "", fmt.Sprintf("cascade=true clusters_deleted=%d rules_deleted=%d", len(clusters), len(rules)))
		return c.JSON(fiber.Map{"status": "deleted", "clusters_deleted": len(clusters), "rules_deleted": len(rules)})
	}

	// Non-cascade: only works on unlocked profiles with no FK dependents
	if err := s.store.DeleteProfile(ctx, id); err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}
	log.Info().Str("profile_id", id.String()).Msg("profile deleted")
	auditLog(c, "profile.delete", "profile", id.String(), "", "")
	return c.JSON(fiber.Map{"status": "deleted"})
}

func (s *RESTServer) cascadePreview(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid profile id"})
	}

	clusters, err := s.store.GetClusterConfigsByProfile(c.Context(), id)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list profile clusters"})
	}

	type preview struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Satellite string `json:"satellite"`
	}
	items := make([]preview, 0, len(clusters))
	for _, cfg := range clusters {
		satName := ""
		if cfg.SatelliteID != nil {
			sat, err := s.store.GetSatellite(c.Context(), *cfg.SatelliteID)
			if err == nil {
				satName = sat.Hostname
			}
		}
		items = append(items, preview{
			ID:        cfg.ID.String(),
			Name:      cfg.Name,
			Namespace: cfg.Namespace,
			Satellite: satName,
		})
	}
	return c.JSON(items)
}

func (s *RESTServer) cloneProfile(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid profile id"})
	}

	source, err := s.store.GetProfile(c.Context(), id)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "profile not found"})
	}

	var body struct {
		Name string `json:"name"`
	}
	if err := c.BodyParser(&body); err != nil || body.Name == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "name is required for cloned profile"})
	}

	clone := &models.ClusterProfile{
		ID:          uuid.New(),
		Name:        body.Name,
		Description: source.Description,
		Config:      source.Config,
	}

	if err := s.store.CreateProfile(c.Context(), clone); err != nil {
		log.Error().Err(err).Msg("failed to clone profile")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to clone profile"})
	}

	log.Info().Str("source", id.String()).Str("clone", clone.ID.String()).Str("name", clone.Name).Msg("profile cloned")
	auditLog(c, "profile.clone", "profile", clone.ID.String(), clone.Name, "source="+id.String())
	return c.Status(fiber.StatusCreated).JSON(clone)
}
