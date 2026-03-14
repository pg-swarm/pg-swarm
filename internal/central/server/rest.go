package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/filesystem"
	"github.com/google/uuid"
	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/pg-swarm/pg-swarm/internal/central/registry"
	"github.com/pg-swarm/pg-swarm/internal/central/store"
	"github.com/pg-swarm/pg-swarm/internal/shared/models"
	"github.com/pg-swarm/pg-swarm/web"
	"github.com/rs/zerolog/log"
)

// RESTServer handles the REST API endpoints for the central server.
type RESTServer struct {
	store    store.Store
	registry *registry.Registry
	streams  *StreamManager
	app      *fiber.App
}

// NewRESTServer creates a new RESTServer.
func NewRESTServer(s store.Store, reg *registry.Registry, sm *StreamManager) *RESTServer {
	srv := &RESTServer{
		store:    s,
		registry: reg,
		streams:  sm,
		app:      fiber.New(fiber.Config{ErrorHandler: fiberErrorHandler}),
	}
	srv.setupRoutes()
	return srv
}

// Start starts the HTTP server on the given address.
func (s *RESTServer) Start(addr string) error {
	log.Info().Str("addr", addr).Msg("REST server starting")
	return s.app.Listen(addr)
}

// Shutdown gracefully shuts down the HTTP server.
func (s *RESTServer) Shutdown() error {
	return s.app.Shutdown()
}

func (s *RESTServer) setupRoutes() {
	// Dashboard (embedded React SPA)
	s.app.Use("/assets", filesystem.New(filesystem.Config{
		Root:       http.FS(web.StaticFS),
		PathPrefix: "static/assets",
	}))
	// SPA catch-all: serve index.html for all non-API routes (BrowserRouter)
	s.app.Use(func(c *fiber.Ctx) error {
		path := c.Path()
		if len(path) >= 4 && path[:4] == "/api" {
			return c.Next()
		}
		data, err := web.StaticFS.ReadFile("static/index.html")
		if err != nil {
			return c.Next()
		}
		c.Set("Content-Type", "text/html; charset=utf-8")
		return c.Send(data)
	})

	api := s.app.Group("/api/v1")

	// Satellites
	api.Get("/satellites", s.listSatellites)
	api.Post("/satellites/:id/approve", s.approveSatellite)
	api.Post("/satellites/:id/reject", s.rejectSatellite)
	api.Put("/satellites/:id/labels", s.updateSatelliteLabels)
	api.Post("/satellites/:id/refresh-storage-classes", s.refreshStorageClasses)

	// Cluster configs
	api.Get("/clusters", s.listClusterConfigs)
	api.Post("/clusters", s.createClusterConfig)
	api.Get("/clusters/:id", s.getClusterConfig)
	api.Put("/clusters/:id", s.updateClusterConfig)
	api.Delete("/clusters/:id", s.deleteClusterConfig)
	api.Post("/clusters/:id/pause", s.pauseCluster)
	api.Post("/clusters/:id/resume", s.resumeCluster)
	api.Post("/clusters/:id/switchover", s.switchoverCluster)

	// Deployment Rules
	api.Get("/deployment-rules", s.listDeploymentRules)
	api.Post("/deployment-rules", s.createDeploymentRule)
	api.Get("/deployment-rules/:id", s.getDeploymentRule)
	api.Put("/deployment-rules/:id", s.updateDeploymentRule)
	api.Delete("/deployment-rules/:id", s.deleteDeploymentRule)
	api.Get("/deployment-rules/:id/clusters", s.listDeploymentRuleClusters)

	// Profiles
	api.Get("/profiles", s.listProfiles)
	api.Post("/profiles", s.createProfile)
	api.Get("/profiles/:id", s.getProfile)
	api.Put("/profiles/:id", s.updateProfile)
	api.Delete("/profiles/:id", s.deleteProfile)
	api.Post("/profiles/:id/clone", s.cloneProfile)

	// Postgres Versions (admin)
	api.Get("/postgres-versions", s.listPostgresVersions)
	api.Post("/postgres-versions", s.createPostgresVersion)
	api.Put("/postgres-versions/:id", s.updatePostgresVersion)
	api.Delete("/postgres-versions/:id", s.deletePostgresVersion)
	api.Post("/postgres-versions/:id/default", s.setDefaultPostgresVersion)

	// Health
	api.Get("/health", s.listClusterHealth)

	// Events
	api.Get("/events", s.listEvents)
}

// --- Satellites ---

func (s *RESTServer) listSatellites(c *fiber.Ctx) error {
	satellites, err := s.store.ListSatellites(c.Context())
	if err != nil {
		log.Error().Err(err).Msg("failed to list satellites")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list satellites"})
	}
	return c.JSON(satellites)
}

func (s *RESTServer) approveSatellite(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid satellite id"})
	}

	authToken, err := s.registry.Approve(c.Context(), id)
	if err != nil {
		log.Error().Err(err).Str("satellite_id", id.String()).Msg("failed to approve satellite")
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}

	return c.JSON(fiber.Map{"auth_token": authToken})
}

func (s *RESTServer) rejectSatellite(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid satellite id"})
	}

	if err := s.registry.Reject(c.Context(), id); err != nil {
		log.Error().Err(err).Str("satellite_id", id.String()).Msg("failed to reject satellite")
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}

	return c.JSON(fiber.Map{"status": "rejected"})
}

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
	if err := models.ValidateDatabases(spec.Databases); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}

	cfg.ID = uuid.New()
	cfg.State = models.ClusterStateCreating

	if err := s.store.CreateClusterConfig(c.Context(), &cfg); err != nil {
		log.Error().Err(err).Msg("failed to create cluster config")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create cluster config"})
	}

	// Lock the source profile once a cluster is built from it
	if cfg.ProfileID != nil {
		if err := s.store.LockProfile(c.Context(), *cfg.ProfileID); err != nil {
			log.Warn().Err(err).Str("profile_id", cfg.ProfileID.String()).Msg("failed to lock profile")
		}
	}

	log.Info().Str("config_id", cfg.ID.String()).Str("name", cfg.Name).Msg("cluster config created")

	s.pushConfigToSatellite(&cfg)

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
	if err := models.ValidateDatabases(spec.Databases); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}

	cfg.ID = id

	if err := s.store.UpdateClusterConfig(c.Context(), &cfg); err != nil {
		log.Error().Err(err).Str("config_id", id.String()).Msg("failed to update cluster config")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to update cluster config"})
	}

	log.Info().Str("config_id", id.String()).Str("name", cfg.Name).Msg("cluster config updated")

	s.pushConfigToSatellite(&cfg)

	return c.JSON(cfg)
}

func (s *RESTServer) deleteClusterConfig(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid config id"})
	}

	if err := s.store.DeleteClusterConfig(c.Context(), id); err != nil {
		log.Error().Err(err).Str("config_id", id.String()).Msg("failed to delete cluster config")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to delete cluster config"})
	}

	log.Info().Str("config_id", id.String()).Msg("cluster config deleted")
	return c.JSON(fiber.Map{"status": "deleted"})
}

func (s *RESTServer) refreshStorageClasses(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid satellite id"})
	}

	if s.streams == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "stream manager not available"})
	}

	if err := s.streams.RequestStorageClasses(id); err != nil {
		log.Error().Err(err).Str("satellite_id", id.String()).Msg("failed to request storage classes")
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}

	log.Info().Str("satellite_id", id.String()).Msg("storage class refresh requested")
	return c.JSON(fiber.Map{"status": "refresh requested"})
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

	log.Info().Str("config_id", id.String()).Bool("paused", paused).Msg("cluster pause state changed")
	s.pushConfigToSatellite(cfg)
	return c.JSON(cfg)
}

func (s *RESTServer) switchoverCluster(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid cluster id"})
	}

	var body struct {
		TargetPod string `json:"target_pod"`
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

	req := &pgswarmv1.SwitchoverRequest{
		ClusterName: cfg.Name,
		Namespace:   cfg.Namespace,
		TargetPod:   body.TargetPod,
	}

	if err := s.streams.PushSwitchover(*cfg.SatelliteID, req); err != nil {
		log.Error().Err(err).Str("cluster", cfg.Name).Msg("failed to send switchover request")
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}

	log.Info().Str("cluster", cfg.Name).Str("target", body.TargetPod).Msg("switchover request sent")
	return c.JSON(fiber.Map{"status": "switchover initiated", "target_pod": body.TargetPod})
}

func (s *RESTServer) updateSatelliteLabels(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid satellite id"})
	}

	var body struct {
		Labels map[string]string `json:"labels"`
	}
	if err := c.BodyParser(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if body.Labels == nil {
		body.Labels = map[string]string{}
	}

	if err := s.store.UpdateSatelliteLabels(c.Context(), id, body.Labels); err != nil {
		log.Error().Err(err).Str("satellite_id", id.String()).Msg("failed to update satellite labels")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to update labels"})
	}

	log.Info().Str("satellite_id", id.String()).Interface("labels", body.Labels).Msg("satellite labels updated")

	// Pause clusters whose deployment rule no longer matches the new labels
	s.pauseUnmatchedClusters(c.Context(), id, body.Labels)

	// Re-evaluate deployment rules for this satellite (creates new clusters for newly matching rules)
	s.fanOutRulesForSatellite(c.Context(), id, body.Labels)

	sat, err := s.store.GetSatellite(c.Context(), id)
	if err != nil {
		return c.JSON(fiber.Map{"status": "labels updated"})
	}
	return c.JSON(sat)
}

// --- Health ---

func (s *RESTServer) listClusterHealth(c *fiber.Ctx) error {
	health, err := s.store.ListClusterHealth(c.Context())
	if err != nil {
		log.Error().Err(err).Msg("failed to list cluster health")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list cluster health"})
	}
	return c.JSON(health)
}

// --- Events ---

func (s *RESTServer) listEvents(c *fiber.Ctx) error {
	limit := 100
	if limitStr := c.Query("limit"); limitStr != "" {
		parsed, err := strconv.Atoi(limitStr)
		if err != nil || parsed <= 0 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid limit parameter"})
		}
		limit = parsed
	}

	events, err := s.store.ListEvents(c.Context(), limit)
	if err != nil {
		log.Error().Err(err).Msg("failed to list events")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list events"})
	}
	return c.JSON(events)
}

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

	// Lock the profile since it's now in use by a deployment rule
	if err := s.store.LockProfile(c.Context(), rule.ProfileID); err != nil {
		log.Warn().Err(err).Str("profile_id", rule.ProfileID.String()).Msg("failed to lock profile")
	}

	// Fan-out: create a ClusterConfig for each satellite matching the label selector
	s.fanOutDeploymentRule(c.Context(), &rule, profile)

	log.Info().Str("rule_id", rule.ID.String()).Str("name", rule.Name).Msg("deployment rule created")
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
		satID := sat.ID
		cfg := &models.ClusterConfig{
			Name:             rule.ClusterName,
			Namespace:        rule.Namespace,
			SatelliteID:      &satID,
			ProfileID:        &rule.ProfileID,
			DeploymentRuleID: &rule.ID,
			Config:           profile.Config,
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

		cfg := &models.ClusterConfig{
			Name:             rule.ClusterName,
			Namespace:        rule.Namespace,
			SatelliteID:      &satelliteID,
			ProfileID:        &rule.ProfileID,
			DeploymentRuleID: &rule.ID,
			Config:           profile.Config,
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

// labelsMatch returns true if the satellite labels contain all selector key-value pairs.
// An empty selector matches nothing. Satellites may have extra labels beyond the selector.
func labelsMatch(labels, selector map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

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
	if err := models.ValidateDatabases(spec.Databases); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}

	profile.ID = uuid.New()
	profile.Locked = false

	if err := s.store.CreateProfile(c.Context(), &profile); err != nil {
		log.Error().Err(err).Msg("failed to create profile")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create profile"})
	}

	log.Info().Str("profile_id", profile.ID.String()).Str("name", profile.Name).Msg("profile created")
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
	if err := models.ValidateDatabases(spec.Databases); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}

	profile.ID = id
	if err := s.store.UpdateProfile(c.Context(), &profile); err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}

	log.Info().Str("profile_id", id.String()).Str("name", profile.Name).Msg("profile updated")
	return c.JSON(profile)
}

func (s *RESTServer) deleteProfile(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid profile id"})
	}
	if err := s.store.DeleteProfile(c.Context(), id); err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}
	log.Info().Str("profile_id", id.String()).Msg("profile deleted")
	return c.JSON(fiber.Map{"status": "deleted"})
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
		Locked:      false,
	}

	if err := s.store.CreateProfile(c.Context(), clone); err != nil {
		log.Error().Err(err).Msg("failed to clone profile")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to clone profile"})
	}

	log.Info().Str("source", id.String()).Str("clone", clone.ID.String()).Str("name", clone.Name).Msg("profile cloned")
	return c.Status(fiber.StatusCreated).JSON(clone)
}

// --- Postgres Versions (admin) ---

func (s *RESTServer) listPostgresVersions(c *fiber.Ctx) error {
	versions, err := s.store.ListPostgresVersions(c.Context())
	if err != nil {
		log.Error().Err(err).Msg("failed to list postgres versions")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list postgres versions"})
	}
	return c.JSON(versions)
}

func (s *RESTServer) createPostgresVersion(c *fiber.Ctx) error {
	var pv models.PostgresVersion
	if err := c.BodyParser(&pv); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if pv.Version == "" || pv.Variant == "" || pv.ImageTag == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "version, variant, and image_tag are required"})
	}

	if err := s.store.CreatePostgresVersion(c.Context(), &pv); err != nil {
		log.Error().Err(err).Msg("failed to create postgres version")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create postgres version"})
	}

	log.Info().Str("version", pv.Version).Str("variant", pv.Variant).Msg("postgres version created")
	return c.Status(fiber.StatusCreated).JSON(pv)
}

func (s *RESTServer) updatePostgresVersion(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid version id"})
	}

	var pv models.PostgresVersion
	if err := c.BodyParser(&pv); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if pv.Version == "" || pv.Variant == "" || pv.ImageTag == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "version, variant, and image_tag are required"})
	}

	pv.ID = id
	if err := s.store.UpdatePostgresVersion(c.Context(), &pv); err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}

	log.Info().Str("id", id.String()).Str("version", pv.Version).Msg("postgres version updated")
	return c.JSON(pv)
}

func (s *RESTServer) deletePostgresVersion(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid version id"})
	}

	if err := s.store.DeletePostgresVersion(c.Context(), id); err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}

	log.Info().Str("id", id.String()).Msg("postgres version deleted")
	return c.JSON(fiber.Map{"status": "deleted"})
}

func (s *RESTServer) setDefaultPostgresVersion(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid version id"})
	}

	if err := s.store.SetDefaultPostgresVersion(c.Context(), id); err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}

	log.Info().Str("id", id.String()).Msg("default postgres version set")
	return c.JSON(fiber.Map{"status": "default set"})
}

// resolvePostgresImage looks up the image tag for a version+variant and prepends the registry.
func (s *RESTServer) resolvePostgresImage(spec *models.ClusterSpec) string {
	version := spec.Postgres.Version
	variant := spec.Postgres.Variant
	if variant == "" {
		variant = "alpine"
	}

	pv, err := s.store.GetPostgresVersionBySpec(context.Background(), version, variant)
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

	spec, err := cfg.ParseSpec()
	if err != nil {
		log.Error().Err(err).
			Str("config_id", cfg.ID.String()).
			Str("name", cfg.Name).
			Msg("failed to parse cluster spec for config push")
		return
	}

	// Resolve profile name and label selector for K8s labels
	var profileName string
	var labelSelector map[string]string
	if cfg.ProfileID != nil {
		if p, err := s.store.GetProfile(context.Background(), *cfg.ProfileID); err == nil {
			profileName = p.Name
		}
	}
	if cfg.DeploymentRuleID != nil {
		if r, err := s.store.GetDeploymentRule(context.Background(), *cfg.DeploymentRuleID); err == nil {
			labelSelector = r.LabelSelector
		}
	}

	// Resolve the image from the postgres_versions table
	resolvedImage := s.resolvePostgresImage(spec)

	protoConfig := &pgswarmv1.ClusterConfig{
		ClusterName:   cfg.Name,
		Namespace:     cfg.Namespace,
		Replicas:      spec.Replicas,
		ConfigVersion: cfg.ConfigVersion,
		ProfileName:   profileName,
		LabelSelector: labelSelector,
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
		if spec.Archive.ArchiveStorage != nil {
			protoConfig.Archive.ArchiveStorage = &pgswarmv1.ArchiveStorageSpec{
				Size:         spec.Archive.ArchiveStorage.Size,
				StorageClass: spec.Archive.ArchiveStorage.StorageClass,
			}
		}
		if spec.Archive.CredentialsSecret != nil {
			protoConfig.Archive.CredentialsSecret = &pgswarmv1.SecretRef{
				Name: spec.Archive.CredentialsSecret.Name,
			}
		}
	}

	for _, db := range spec.Databases {
		protoConfig.Databases = append(protoConfig.Databases, &pgswarmv1.DatabaseSpec{
			Name:     db.Name,
			User:     db.User,
			Password: db.Password,
		})
	}

	if spec.Failover != nil && spec.Failover.Enabled {
		protoConfig.Failover = &pgswarmv1.FailoverSpec{
			Enabled:                    true,
			HealthCheckIntervalSeconds: spec.Failover.HealthCheckIntervalSeconds,
			SidecarImage:               spec.Failover.SidecarImage,
		}
	}

	if err := s.streams.PushConfig(*cfg.SatelliteID, protoConfig); err != nil {
		log.Error().Err(err).
			Str("satellite_id", cfg.SatelliteID.String()).
			Str("config_id", cfg.ID.String()).
			Msg("failed to push config to satellite")
	}
}

// --- Error handler ---

func fiberErrorHandler(c *fiber.Ctx, err error) error {
	code := fiber.StatusInternalServerError
	if e, ok := err.(*fiber.Error); ok {
		code = e.Code
	}
	return c.Status(code).JSON(fiber.Map{"error": err.Error()})
}
