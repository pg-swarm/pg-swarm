// Deployment rule REST API handlers.
package server

import (
	"context"
	"fmt"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/pg-swarm/pg-swarm/internal/shared/models"
)

// --- Deployment Rules ---

func (s *RESTServer) listDeploymentRules(c *fiber.Ctx) error {
	rules, err := s.store.ListDeploymentRules(c.Context())
	if err != nil {
		log.Error().Err(err).Msg("failed to list deployment rules")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list deployment rules"})
	}
	return c.JSON(rules)
}

func (s *RESTServer) createDeploymentRule(c *fiber.Ctx) error {
	var rule models.DeploymentRule
	if err := c.BodyParser(&rule); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if rule.Name == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "name is required"})
	}
	if rule.ProfileID == uuid.Nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "profile_id is required"})
	}
	if rule.ClusterName == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "cluster_name is required"})
	}
	if rule.LabelSelector == nil {
		rule.LabelSelector = map[string]string{}
	}
	if len(rule.LabelSelector) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "label_selector must have at least one key-value pair"})
	}

	// Verify profile exists and fetch its config
	profile, err := s.store.GetProfile(c.Context(), rule.ProfileID)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "profile not found"})
	}

	rule.ID = uuid.New()

	if err := s.store.CreateDeploymentRule(c.Context(), &rule); err != nil {
		log.Error().Err(err).Msg("failed to create deployment rule")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create deployment rule"})
	}

	// Fan-out: create a ClusterConfig for each satellite matching the label selector
	s.fanOutDeploymentRule(c.Context(), &rule, profile)

	log.Info().Str("rule_id", rule.ID.String()).Str("name", rule.Name).Msg("deployment rule created")
	auditLog(c, "deployment_rule.create", "deployment_rule", rule.ID.String(), rule.Name, "")
	return c.Status(fiber.StatusCreated).JSON(rule)
}

func (s *RESTServer) getDeploymentRule(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid deployment rule id"})
	}

	rule, err := s.store.GetDeploymentRule(c.Context(), id)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "deployment rule not found"})
	}

	return c.JSON(rule)
}

func (s *RESTServer) updateDeploymentRule(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid deployment rule id"})
	}

	var rule models.DeploymentRule
	if err := c.BodyParser(&rule); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if rule.ProfileID == uuid.Nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "profile_id is required"})
	}
	if rule.ClusterName == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "cluster_name is required"})
	}
	if rule.LabelSelector == nil {
		rule.LabelSelector = map[string]string{}
	}
	if len(rule.LabelSelector) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "label_selector must have at least one key-value pair"})
	}

	// Verify profile exists
	profile, err := s.store.GetProfile(c.Context(), rule.ProfileID)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "profile not found"})
	}

	rule.ID = id
	if err := s.store.UpdateDeploymentRule(c.Context(), &rule); err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}

	// Pause clusters whose satellites no longer match the updated selector
	existing, _ := s.store.GetClusterConfigsByDeploymentRule(c.Context(), rule.ID)
	for _, cfg := range existing {
		if cfg.SatelliteID == nil {
			continue
		}
		sat, err := s.store.GetSatellite(c.Context(), *cfg.SatelliteID)
		if err != nil {
			continue
		}
		if !labelsMatch(sat.Labels, rule.LabelSelector) && !cfg.Paused {
			updated, err := s.store.SetClusterPaused(c.Context(), cfg.ID, true)
			if err == nil {
				s.pushConfigToSatellite(updated)
			}
		}
	}

	// Fan-out to newly matching satellites
	s.fanOutDeploymentRule(c.Context(), &rule, profile)

	log.Info().Str("rule_id", id.String()).Str("name", rule.Name).Msg("deployment rule updated")
	auditLog(c, "deployment_rule.update", "deployment_rule", id.String(), rule.Name, "")
	return c.JSON(rule)
}

func (s *RESTServer) deleteDeploymentRule(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid deployment rule id"})
	}

	// Check if any clusters are still linked to this rule
	clusters, err := s.store.GetClusterConfigsByDeploymentRule(c.Context(), id)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to check clusters"})
	}
	if len(clusters) > 0 {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fmt.Sprintf("cannot delete deployment rule with %d cluster(s) — remove clusters first", len(clusters)),
		})
	}

	if err := s.store.DeleteDeploymentRule(c.Context(), id); err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}

	log.Info().Str("rule_id", id.String()).Msg("deployment rule deleted")
	auditLog(c, "deployment_rule.delete", "deployment_rule", id.String(), "", "")
	return c.JSON(fiber.Map{"status": "deleted"})
}

func (s *RESTServer) listDeploymentRuleClusters(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid deployment rule id"})
	}

	clusters, err := s.store.GetClusterConfigsByDeploymentRule(c.Context(), id)
	if err != nil {
		log.Error().Err(err).Msg("failed to list deployment rule clusters")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list clusters"})
	}
	return c.JSON(clusters)
}

// fanOutDeploymentRule creates a ClusterConfig for each satellite matching the rule's label selector.
func (s *RESTServer) fanOutDeploymentRule(ctx context.Context, rule *models.DeploymentRule, profile *models.ClusterProfile) {
	satellites, err := s.store.ListSatellitesByLabelSelector(ctx, rule.LabelSelector)
	if err != nil {
		log.Error().Err(err).Str("rule_id", rule.ID.String()).Msg("fan-out: failed to list satellites by label selector")
		return
	}

	// Build a set of satellite IDs that already have a config for this rule
	existingConfigs, _ := s.store.GetClusterConfigsByDeploymentRule(ctx, rule.ID)
	hasConfig := make(map[uuid.UUID]bool, len(existingConfigs))
	for _, ec := range existingConfigs {
		if ec.SatelliteID != nil {
			hasConfig[*ec.SatelliteID] = true
		}
	}

	for _, sat := range satellites {
		if hasConfig[sat.ID] {
			continue
		}

		resolvedConfig, err := resolveStorageTiers(profile.Config, sat.TierMappings)
		if err != nil {
			log.Warn().Err(err).
				Str("rule_id", rule.ID.String()).
				Str("satellite_id", sat.ID.String()).
				Msg("fan-out: skipping satellite due to missing tier mappings")
			continue
		}

		satID := sat.ID
		profileID := rule.ProfileID
		cfg := &models.ClusterConfig{
			Name:             rule.ClusterName,
			Namespace:        rule.Namespace,
			SatelliteID:      &satID,
			ProfileID:        &profileID,
			DeploymentRuleID: &rule.ID,
			Config:           resolvedConfig,
		}
		// Initialize applied_profile_version to avoid stale "update available"
		if versions, err := s.store.ListConfigVersions(ctx, profileID); err == nil && len(versions) > 0 {
			cfg.AppliedProfileVersion = versions[0].Version
		}
		if err := s.store.CreateClusterConfig(ctx, cfg); err != nil {
			log.Error().Err(err).
				Str("rule_id", rule.ID.String()).
				Str("satellite_id", sat.ID.String()).
				Msg("fan-out: failed to create cluster config")
			continue
		}
		log.Info().
			Str("rule_id", rule.ID.String()).
			Str("satellite_id", sat.ID.String()).
			Str("config_id", cfg.ID.String()).
			Msg("fan-out: cluster config created")
		s.pushConfigToSatellite(cfg)
	}
}

// fanOutRulesForSatellite evaluates all deployment rules against the satellite's labels
// and creates ClusterConfigs for matching rules that don't already have one.
func (s *RESTServer) fanOutRulesForSatellite(ctx context.Context, satelliteID uuid.UUID, labels map[string]string) {
	rules, err := s.store.ListDeploymentRules(ctx)
	if err != nil {
		log.Error().Err(err).Str("satellite_id", satelliteID.String()).Msg("fan-out: failed to list deployment rules")
		return
	}

	// Fetch satellite once for tier mappings
	sat, err := s.store.GetSatellite(ctx, satelliteID)
	if err != nil {
		log.Error().Err(err).Str("satellite_id", satelliteID.String()).Msg("fan-out: failed to get satellite")
		return
	}

	for _, rule := range rules {
		if !labelsMatch(labels, rule.LabelSelector) {
			continue
		}

		// Check if a cluster config already exists for this rule + satellite
		existing, err := s.store.GetClusterConfigsByDeploymentRule(ctx, rule.ID)
		if err != nil {
			log.Error().Err(err).Str("rule_id", rule.ID.String()).Msg("fan-out: failed to check existing configs")
			continue
		}
		alreadyExists := false
		for _, ec := range existing {
			if ec.SatelliteID != nil && *ec.SatelliteID == satelliteID {
				alreadyExists = true
				break
			}
		}
		if alreadyExists {
			continue
		}

		profile, err := s.store.GetProfile(ctx, rule.ProfileID)
		if err != nil {
			log.Error().Err(err).Str("rule_id", rule.ID.String()).Msg("fan-out: failed to get profile for rule")
			continue
		}

		resolvedConfig, err := resolveStorageTiers(profile.Config, sat.TierMappings)
		if err != nil {
			log.Warn().Err(err).
				Str("rule_id", rule.ID.String()).
				Str("satellite_id", satelliteID.String()).
				Msg("fan-out: skipping rule for satellite due to missing tier mappings")
			continue
		}

		cfg := &models.ClusterConfig{
			Name:             rule.ClusterName,
			Namespace:        rule.Namespace,
			SatelliteID:      &satelliteID,
			ProfileID:        &rule.ProfileID,
			DeploymentRuleID: &rule.ID,
			Config:           resolvedConfig,
		}
		// Initialize applied_profile_version to avoid stale "update available"
		if versions, err := s.store.ListConfigVersions(ctx, rule.ProfileID); err == nil && len(versions) > 0 {
			cfg.AppliedProfileVersion = versions[0].Version
		}
		if err := s.store.CreateClusterConfig(ctx, cfg); err != nil {
			log.Error().Err(err).
				Str("rule_id", rule.ID.String()).
				Str("satellite_id", satelliteID.String()).
				Msg("fan-out: failed to create cluster config for satellite")
			continue
		}
		log.Info().
			Str("rule_id", rule.ID.String()).
			Str("satellite_id", satelliteID.String()).
			Str("config_id", cfg.ID.String()).
			Msg("fan-out: cluster config created for satellite")
		s.pushConfigToSatellite(cfg)
	}
}

// pauseUnmatchedClusters pauses clusters on a satellite whose deployment rule's
// label selector no longer matches the satellite's updated labels.
func (s *RESTServer) pauseUnmatchedClusters(ctx context.Context, satelliteID uuid.UUID, newLabels map[string]string) {
	clusters, err := s.store.GetClusterConfigsBySatellite(ctx, satelliteID)
	if err != nil {
		log.Error().Err(err).Str("satellite_id", satelliteID.String()).Msg("pause-unmatched: failed to list clusters")
		return
	}

	for _, cfg := range clusters {
		if cfg.DeploymentRuleID == nil {
			continue // manually created cluster, not managed by a rule
		}
		rule, err := s.store.GetDeploymentRule(ctx, *cfg.DeploymentRuleID)
		if err != nil {
			log.Error().Err(err).Str("rule_id", cfg.DeploymentRuleID.String()).Msg("pause-unmatched: failed to get rule")
			continue
		}
		if labelsMatch(newLabels, rule.LabelSelector) {
			// Still matches — if it was paused due to label mismatch, resume it
			if cfg.Paused {
				updated, err := s.store.SetClusterPaused(ctx, cfg.ID, false)
				if err != nil {
					log.Error().Err(err).Str("config_id", cfg.ID.String()).Msg("pause-unmatched: failed to resume cluster")
					continue
				}
				log.Info().Str("config_id", cfg.ID.String()).Msg("pause-unmatched: cluster resumed (labels match again)")
				s.pushConfigToSatellite(updated)
			}
			continue
		}
		// No longer matches — pause it
		if !cfg.Paused {
			updated, err := s.store.SetClusterPaused(ctx, cfg.ID, true)
			if err != nil {
				log.Error().Err(err).Str("config_id", cfg.ID.String()).Msg("pause-unmatched: failed to pause cluster")
				continue
			}
			log.Info().
				Str("config_id", cfg.ID.String()).
				Str("satellite_id", satelliteID.String()).
				Msg("pause-unmatched: cluster paused (labels no longer match rule)")
			s.pushConfigToSatellite(updated)
		}
	}
}
