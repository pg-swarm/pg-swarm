package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/filesystem"
	"github.com/google/uuid"
	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/pg-swarm/pg-swarm/internal/central/crypto"
	"github.com/pg-swarm/pg-swarm/internal/central/registry"
	"github.com/pg-swarm/pg-swarm/internal/central/store"
	"github.com/pg-swarm/pg-swarm/internal/shared/models"
	"github.com/pg-swarm/pg-swarm/dashboard"
	"github.com/rs/zerolog/log"
)

// RESTServer handles the REST API endpoints for the central server.
type RESTServer struct {
	store      store.Store
	registry   *registry.Registry
	streams    *StreamManager
	logBuffer  *LogBuffer
	opsTracker *OpsTracker
	encryptor  *crypto.Encryptor
	app        *fiber.App
	wsHub      *WSHub
}

// NewRESTServer creates a new RESTServer.
func NewRESTServer(s store.Store, reg *registry.Registry, sm *StreamManager, lb *LogBuffer, ot *OpsTracker, enc *crypto.Encryptor) *RESTServer {
	srv := &RESTServer{
		store:      s,
		registry:   reg,
		streams:    sm,
		logBuffer:  lb,
		opsTracker: ot,
		encryptor:  enc,
		app:        fiber.New(fiber.Config{ErrorHandler: fiberErrorHandler}),
	}
	srv.wsHub = newWSHub(srv)
	go srv.wsHub.Run()
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

// GetWSHub returns the WebSocket hub for cross-server wiring.
func (s *RESTServer) GetWSHub() *WSHub {
	return s.wsHub
}

func (s *RESTServer) setupRoutes() {
	// HTTP request logging middleware (must be first)
	s.app.Use(s.requestLogMiddleware)

	// Dashboard (embedded React SPA)
	s.app.Use("/assets", filesystem.New(filesystem.Config{
		Root:       http.FS(dashboard.StaticFS),
		PathPrefix: "static/assets",
	}))
	// Serve favicon from embedded static root
	s.app.Get("/favicon.svg", func(c *fiber.Ctx) error {
		data, err := dashboard.StaticFS.ReadFile("static/favicon.svg")
		if err != nil {
			return c.SendStatus(fiber.StatusNotFound)
		}
		c.Set("Content-Type", "image/svg+xml")
		return c.Send(data)
	})

	// SPA catch-all: serve index.html for all non-API routes (BrowserRouter)
	s.app.Use(func(c *fiber.Ctx) error {
		path := c.Path()
		if len(path) >= 4 && path[:4] == "/api" {
			return c.Next()
		}
		data, err := dashboard.StaticFS.ReadFile("static/index.html")
		if err != nil {
			return c.Next()
		}
		c.Set("Content-Type", "text/html; charset=utf-8")
		return c.Send(data)
	})

	api := s.app.Group("/api/v1")

	// WebSocket — real-time state push (dashboard connects here first, falls back to REST polling)
	api.Use("/ws", upgradeMiddleware)
	api.Get("/ws", websocket.New(s.wsHub.handleWS))

	// Notify WebSocket clients after successful mutations.
	api.Use(func(c *fiber.Ctx) error {
		err := c.Next()
		if err == nil && c.Method() != fiber.MethodGet && c.Response().StatusCode() < 400 {
			s.wsHub.Notify()
		}
		return err
	})

	// Satellites
	api.Get("/satellites", s.listSatellites)
	api.Post("/satellites/:id/approve", s.approveSatellite)
	api.Post("/satellites/:id/reject", s.rejectSatellite)
	api.Put("/satellites/:id/labels", s.updateSatelliteLabels)
	api.Post("/satellites/:id/refresh-storage-classes", s.refreshStorageClasses)
	api.Put("/satellites/:id/tier-mappings", s.updateSatelliteTierMappings)

	// Storage Tiers (admin)
	api.Get("/storage-tiers", s.listStorageTiers)
	api.Post("/storage-tiers", s.createStorageTier)
	api.Put("/storage-tiers/:id", s.updateStorageTier)
	api.Delete("/storage-tiers/:id", s.deleteStorageTier)
	api.Get("/recovery-rule-sets", s.listRecoveryRuleSets)
	api.Post("/recovery-rule-sets", s.createRecoveryRuleSet)
	api.Get("/recovery-rule-sets/:id", s.getRecoveryRuleSet)
	api.Put("/recovery-rule-sets/:id", s.updateRecoveryRuleSet)
	api.Delete("/recovery-rule-sets/:id", s.deleteRecoveryRuleSet)
	api.Get("/satellites/:id/logs", s.getSatelliteLogs)
	api.Get("/satellites/:id/logs/stream", s.streamSatelliteLogs)
	api.Post("/satellites/:id/log-level", s.setSatelliteLogLevel)

	// Cluster configs
	api.Get("/clusters", s.listClusterConfigs)
	api.Post("/clusters", s.createClusterConfig)
	api.Get("/clusters/:id", s.getClusterConfig)
	api.Put("/clusters/:id", s.updateClusterConfig)
	api.Delete("/clusters/:id", s.deleteClusterConfig)
	api.Post("/clusters/:id/pause", s.pauseCluster)
	api.Post("/clusters/:id/resume", s.resumeCluster)
	api.Post("/clusters/:id/switchover", s.switchoverCluster)
	api.Get("/clusters/:id/profile-diff", s.clusterProfileDiff)
	api.Post("/clusters/:id/apply", s.applyCluster)
	api.Get("/clusters/:id/databases", s.listClusterDatabases)
	api.Post("/clusters/:id/databases", s.createClusterDatabase)
	api.Put("/clusters/:id/databases/:dbid", s.updateClusterDatabase)
	api.Delete("/clusters/:id/databases/:dbid", s.deleteClusterDatabase)

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
	api.Get("/profiles/:id/cascade-preview", s.cascadePreview)
	api.Post("/profiles/:id/clone", s.cloneProfile)
	api.Post("/profiles/:id/apply", s.applyProfile)
	api.Get("/profiles/:id/versions", s.listProfileVersions)
	api.Get("/profiles/:id/versions/:version", s.getProfileVersion)
	api.Post("/profiles/:id/revert", s.revertProfile)

	// PG Parameter Classifications (admin)
	api.Get("/pg-param-classifications", s.listPgParamClassifications)
	api.Post("/pg-param-classifications", s.upsertPgParamClassification)
	api.Delete("/pg-param-classifications/:name", s.deletePgParamClassification)

	// Postgres Variants (admin)
	api.Get("/postgres-variants", s.listPostgresVariants)
	api.Post("/postgres-variants", s.createPostgresVariant)
	api.Delete("/postgres-variants/:id", s.deletePostgresVariant)

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

	var body struct {
		Name string `json:"name"`
	}
	if err := c.BodyParser(&body); err != nil || body.Name == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "name is required"})
	}

	// Attempt to update the name first to enforce uniqueness before approving
	if err := s.store.UpdateSatelliteName(c.Context(), id, body.Name); err != nil {
		if strings.Contains(err.Error(), "duplicate key value") || strings.Contains(err.Error(), "unique constraint") {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "satellite name already in use"})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to update satellite name"})
	}

	replace := c.Query("replace") == "true"

	replacedID, authToken, err := s.registry.Approve(c.Context(), id, replace)
	if err != nil {
		log.Error().Err(err).Str("satellite_id", id.String()).Msg("failed to approve satellite")
		// Return 409 for cluster conflicts so the UI can show a confirmation dialog
		if !replace {
			if conflict, _ := s.registry.ConflictingSatellite(c.Context(), id); conflict != nil {
				return c.Status(fiber.StatusConflict).JSON(fiber.Map{
					"error":                 err.Error(),
					"conflicting_satellite": conflict,
				})
			}
		}
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}

	result := fiber.Map{"auth_token": authToken}
	if replacedID != nil {
		result["replaced_satellite_id"] = replacedID.String()
		// Disconnect the old satellite's stream if it's still connected
		s.streams.Remove(*replacedID)
	}
	auditLog(c, "satellite.approve", "satellite", id.String(), body.Name, "")
	return c.JSON(result)
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

	auditLog(c, "satellite.reject", "satellite", id.String(), "", "")
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
	if err := validatePostgresImage(s.store, spec); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}

	cfg.ID = uuid.New()
	cfg.State = models.ClusterStateCreating

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
		if err := s.streams.PushDelete(*cfg.SatelliteID, del); err != nil {
			log.Warn().Err(err).Str("config_id", id.String()).Msg("failed to push delete to satellite (may be offline)")
		}
	}

	log.Info().Str("config_id", id.String()).Msg("cluster config deleted")
	auditLog(c, "cluster.delete", "cluster", id.String(), cfg.Name, "")
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

func (s *RESTServer) getSatelliteLogs(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid satellite id"})
	}

	limit := 200
	if limitStr := c.Query("limit"); limitStr != "" {
		if parsed, err := strconv.Atoi(limitStr); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	level := c.Query("level", "trace")

	entries := s.logBuffer.Recent(id, limit, level)
	if entries == nil {
		entries = make([]*LogEntryJSON, 0)
	}
	return c.JSON(entries)
}

func (s *RESTServer) streamSatelliteLogs(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid satellite id"})
	}

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")

	ch, unsub := s.logBuffer.Subscribe(id)

	c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
		defer unsub()
		for entry := range ch {
			data, err := json.Marshal(entry)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			if err := w.Flush(); err != nil {
				return
			}
		}
	})
	return nil
}

func (s *RESTServer) setSatelliteLogLevel(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid satellite id"})
	}

	var body struct {
		Level string `json:"level"`
	}
	if err := c.BodyParser(&body); err != nil || body.Level == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "level is required"})
	}

	if s.streams == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "stream manager not available"})
	}

	if err := s.streams.PushSetLogLevel(id, body.Level); err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}

	log.Info().Str("satellite_id", id.String()).Str("level", body.Level).Msg("log level change sent")
	auditLog(c, "satellite.set_log_level", "satellite", id.String(), "", "level="+body.Level)
	return c.JSON(fiber.Map{"status": "level change sent", "level": body.Level})
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

	operationID := uuid.New().String()
	req := &pgswarmv1.SwitchoverRequest{
		ClusterName: cfg.Name,
		Namespace:   cfg.Namespace,
		TargetPod:   body.TargetPod,
		OperationId: operationID,
	}

	if s.opsTracker != nil {
		s.opsTracker.Start(operationID, cfg.Name, "", body.TargetPod)
	}

	if err := s.streams.PushSwitchover(*cfg.SatelliteID, req); err != nil {
		log.Error().Err(err).Str("cluster", cfg.Name).Msg("failed to send switchover request")
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}

	log.Info().Str("cluster", cfg.Name).Str("target", body.TargetPod).Str("operation_id", operationID).Msg("switchover request sent")
	auditLog(c, "cluster.switchover", "cluster", id.String(), cfg.Name, "target_pod="+body.TargetPod)
	return c.JSON(fiber.Map{"status": "switchover initiated", "target_pod": body.TargetPod, "operation_id": operationID})
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

	auditLog(c, "satellite.update_labels", "satellite", id.String(), "", "")
	sat, err := s.store.GetSatellite(c.Context(), id)
	if err != nil {
		return c.JSON(fiber.Map{"status": "labels updated"})
	}
	return c.JSON(sat)
}

func (s *RESTServer) updateSatelliteTierMappings(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid satellite id"})
	}

	var body struct {
		TierMappings map[string]string `json:"tier_mappings"`
	}
	if err := c.BodyParser(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if body.TierMappings == nil {
		body.TierMappings = map[string]string{}
	}

	if err := s.store.UpdateSatelliteTierMappings(c.Context(), id, body.TierMappings); err != nil {
		log.Error().Err(err).Str("satellite_id", id.String()).Msg("failed to update satellite tier mappings")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to update tier mappings"})
	}

	log.Info().Str("satellite_id", id.String()).Interface("tier_mappings", body.TierMappings).Msg("satellite tier mappings updated")

	sat, err := s.store.GetSatellite(c.Context(), id)
	if err != nil {
		return c.JSON(fiber.Map{"status": "tier mappings updated"})
	}
	return c.JSON(sat)
}

func (s *RESTServer) listStorageTiers(c *fiber.Ctx) error {
	tiers, err := s.store.ListStorageTiers(c.Context())
	if err != nil {
		log.Error().Err(err).Msg("failed to list storage tiers")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list storage tiers"})
	}
	return c.JSON(tiers)
}

func (s *RESTServer) createStorageTier(c *fiber.Ctx) error {
	var tier models.StorageTier
	if err := c.BodyParser(&tier); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if tier.Name == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "name is required"})
	}
	if err := s.store.CreateStorageTier(c.Context(), &tier); err != nil {
		log.Error().Err(err).Msg("failed to create storage tier")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create storage tier"})
	}
	auditLog(c, "storage_tier.create", "storage_tier", tier.ID.String(), tier.Name, "")
	return c.Status(fiber.StatusCreated).JSON(tier)
}

func (s *RESTServer) updateStorageTier(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid tier id"})
	}
	var tier models.StorageTier
	if err := c.BodyParser(&tier); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	tier.ID = id
	if tier.Name == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "name is required"})
	}
	if err := s.store.UpdateStorageTier(c.Context(), &tier); err != nil {
		log.Error().Err(err).Msg("failed to update storage tier")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to update storage tier"})
	}
	auditLog(c, "storage_tier.update", "storage_tier", id.String(), tier.Name, "")
	return c.JSON(tier)
}

func (s *RESTServer) deleteStorageTier(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid tier id"})
	}
	if err := s.store.DeleteStorageTier(c.Context(), id); err != nil {
		log.Error().Err(err).Msg("failed to delete storage tier")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to delete storage tier"})
	}
	auditLog(c, "storage_tier.delete", "storage_tier", id.String(), "", "")
	return c.JSON(fiber.Map{"status": "deleted"})
}

// ── Recovery Rule Sets ──────────────────────────────────────────────────────

func (s *RESTServer) listRecoveryRuleSets(c *fiber.Ctx) error {
	sets, err := s.store.ListRecoveryRuleSets(c.Context())
	if err != nil {
		log.Error().Err(err).Msg("failed to list recovery rule sets")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list recovery rule sets"})
	}
	if sets == nil {
		sets = []*models.RecoveryRuleSet{}
	}
	return c.JSON(sets)
}

func (s *RESTServer) createRecoveryRuleSet(c *fiber.Ctx) error {
	var rs models.RecoveryRuleSet
	if err := c.BodyParser(&rs); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if rs.Name == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "name is required"})
	}
	rs.Builtin = false // users cannot create built-in rule sets
	if err := s.store.CreateRecoveryRuleSet(c.Context(), &rs); err != nil {
		log.Error().Err(err).Msg("failed to create recovery rule set")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create recovery rule set"})
	}
	auditLog(c, "recovery_rule_set.create", "recovery_rule_set", rs.ID.String(), rs.Name, "")
	return c.Status(fiber.StatusCreated).JSON(rs)
}

func (s *RESTServer) getRecoveryRuleSet(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid id"})
	}
	rs, err := s.store.GetRecoveryRuleSet(c.Context(), id)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	return c.JSON(rs)
}

func (s *RESTServer) updateRecoveryRuleSet(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid id"})
	}
	var rs models.RecoveryRuleSet
	if err := c.BodyParser(&rs); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	rs.ID = id
	if rs.Name == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "name is required"})
	}
	if err := s.store.UpdateRecoveryRuleSet(c.Context(), &rs); err != nil {
		log.Error().Err(err).Msg("failed to update recovery rule set")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to update recovery rule set"})
	}
	auditLog(c, "recovery_rule_set.update", "recovery_rule_set", id.String(), rs.Name, "")
	return c.JSON(rs)
}

func (s *RESTServer) deleteRecoveryRuleSet(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid id"})
	}
	if err := s.store.DeleteRecoveryRuleSet(c.Context(), id); err != nil {
		log.Error().Err(err).Msg("failed to delete recovery rule set")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to delete recovery rule set"})
	}
	auditLog(c, "recovery_rule_set.delete", "recovery_rule_set", id.String(), "", "")
	return c.JSON(fiber.Map{"status": "deleted"})
}

// resolveStorageTiers replaces "tier:X" storage class values in a config with
// concrete class names from the satellite's tier mappings. Returns an error
// listing any unmapped tiers.
func resolveStorageTiers(rawConfig json.RawMessage, tierMappings map[string]string) (json.RawMessage, error) {
	var spec models.ClusterSpec
	if err := json.Unmarshal(rawConfig, &spec); err != nil {
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

	var missing []string

	if resolved, err := resolve(spec.Storage.StorageClass); err != nil {
		missing = append(missing, err.Error())
	} else {
		spec.Storage.StorageClass = resolved
	}

	if spec.WalStorage != nil {
		if resolved, err := resolve(spec.WalStorage.StorageClass); err != nil {
			missing = append(missing, err.Error())
		} else {
			spec.WalStorage.StorageClass = resolved
		}
	}

	if len(missing) > 0 {
		return rawConfig, fmt.Errorf("missing tier mappings: %s", strings.Join(missing, ", "))
	}

	resolved, err := json.Marshal(spec)
	if err != nil {
		return rawConfig, fmt.Errorf("marshal resolved config: %w", err)
	}
	return resolved, nil
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
		cfg := &models.ClusterConfig{
			Name:             rule.ClusterName,
			Namespace:        rule.Namespace,
			SatelliteID:      &satID,
			ProfileID:        &rule.ProfileID,
			DeploymentRuleID: &rule.ID,
			Config:           resolvedConfig,
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
				"affected_clusters":    clusterNames,
				"reload_changes":       diff.ReloadChanges,
				"sequential_changes":   diff.SequentialChanges,
				"full_restart_changes": diff.FullRestartChanges,
				"apply_strategy":       diff.ApplyStrategy(),
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

// clusterProfileDiff returns the diff between a cluster's current config and its
// profile's latest config, so the user can review changes before applying.
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

	paramModes := s.loadParamClassifications(c.Context())
	diff := classifyChanges(oldSpec, &newSpec, paramModes)

	return c.JSON(fiber.Map{
		"cluster_name":            cfg.Name,
		"profile_name":           profile.Name,
		"applied_profile_version": cfg.AppliedProfileVersion,
		"latest_profile_version":  latestVersion,
		"reload_changes":         diff.ReloadChanges,
		"sequential_changes":     diff.SequentialChanges,
		"full_restart_changes":   diff.FullRestartChanges,
		"immutable_errors":       diff.ImmutableErrors,
		"scale_up":               diff.ScaleUp,
		"scale_down":             diff.ScaleDown,
		"apply_strategy":         diff.ApplyStrategy(),
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
				"affected_clusters":    clusterNames,
				"reload_changes":       diff.ReloadChanges,
				"sequential_changes":   diff.SequentialChanges,
				"full_restart_changes": diff.FullRestartChanges,
				"apply_strategy":       diff.ApplyStrategy(),
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
				if pushErr := s.streams.PushDelete(*cfg.SatelliteID, del); pushErr != nil {
					log.Warn().Err(pushErr).Str("cluster", cfg.Name).Msg("cascade: failed to push delete to satellite")
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

// --- Postgres Variants (admin) ---

func (s *RESTServer) listPostgresVariants(c *fiber.Ctx) error {
	variants, err := s.store.ListPostgresVariants(c.Context())
	if err != nil {
		log.Error().Err(err).Msg("failed to list postgres variants")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list postgres variants"})
	}
	return c.JSON(variants)
}

func (s *RESTServer) createPostgresVariant(c *fiber.Ctx) error {
	var v models.PostgresVariant
	if err := c.BodyParser(&v); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if v.Name == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "name is required"})
	}

	if err := s.store.CreatePostgresVariant(c.Context(), &v); err != nil {
		log.Error().Err(err).Msg("failed to create postgres variant")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create postgres variant"})
	}

	log.Info().Str("name", v.Name).Msg("postgres variant created")
	auditLog(c, "pg_variant.create", "pg_variant", v.ID.String(), v.Name, "")
	return c.Status(fiber.StatusCreated).JSON(v)
}

func (s *RESTServer) deletePostgresVariant(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid variant id"})
	}

	if err := s.store.DeletePostgresVariant(c.Context(), id); err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}

	log.Info().Str("id", id.String()).Msg("postgres variant deleted")
	auditLog(c, "pg_variant.delete", "pg_variant", id.String(), "", "")
	return c.JSON(fiber.Map{"status": "deleted"})
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
	auditLog(c, "pg_version.create", "pg_version", pv.ID.String(), pv.Version, "variant="+pv.Variant)
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
	auditLog(c, "pg_version.update", "pg_version", id.String(), pv.Version, "variant="+pv.Variant)
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
	auditLog(c, "pg_version.delete", "pg_version", id.String(), "", "")
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
	auditLog(c, "pg_version.set_default", "pg_version", id.String(), "", "")
	return c.JSON(fiber.Map{"status": "default set"})
}

// validatePostgresImage checks that a postgres image can be resolved for the
// given spec — either via the postgres_versions table or an explicit image field.
// Returns an error suitable for returning to the API caller.
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

	if err := s.streams.PushConfig(*cfg.SatelliteID, protoConfig); err != nil {
		log.Error().Err(err).
			Str("satellite_id", cfg.SatelliteID.String()).
			Str("config_id", cfg.ID.String()).
			Msg("failed to push config to satellite")
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

	if spec.Failover != nil && spec.Failover.Enabled {
		protoConfig.Failover = &pgswarmv1.FailoverSpec{
			Enabled:                    true,
			HealthCheckIntervalSeconds: spec.Failover.HealthCheckIntervalSeconds,
			SidecarImage:               spec.Failover.SidecarImage,
		}
	}

	// Resolve recovery rules from profile's recovery_rule_set_id
	if cfg.ProfileID != nil {
		if p, err := st.GetProfile(context.Background(), *cfg.ProfileID); err == nil && p.RecoveryRuleSetID != nil {
			if rs, err := st.GetRecoveryRuleSet(context.Background(), *p.RecoveryRuleSetID); err == nil && rs.Config != nil {
				var rules []struct {
					Name            string `json:"name"`
					Pattern         string `json:"pattern"`
					Severity        string `json:"severity"`
					Action          string `json:"action"`
					ExecCommand     string `json:"exec_command"`
					CooldownSeconds int32  `json:"cooldown_seconds"`
					Enabled         bool   `json:"enabled"`
					Category        string `json:"category"`
				}
				if err := json.Unmarshal(rs.Config, &rules); err == nil {
					for _, r := range rules {
						protoConfig.RecoveryRules = append(protoConfig.RecoveryRules, &pgswarmv1.RecoveryRule{
							Name:            r.Name,
							Pattern:         r.Pattern,
							Severity:        r.Severity,
							Action:          r.Action,
							ExecCommand:     r.ExecCommand,
							CooldownSeconds: r.CooldownSeconds,
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

// --- PG Parameter Classifications ---

// loadParamClassifications loads the parameter update modes from the database.
func (s *RESTServer) loadParamClassifications(ctx context.Context) ParamClassifications {
	classifications, err := s.store.ListPgParamClassifications(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("failed to load pg param classifications, using defaults")
		return DefaultParamClassifications
	}
	result := make(ParamClassifications, len(classifications))
	for _, c := range classifications {
		result[c.Name] = c.RestartMode
	}
	return result
}

func (s *RESTServer) listPgParamClassifications(c *fiber.Ctx) error {
	classifications, err := s.store.ListPgParamClassifications(c.Context())
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	if classifications == nil {
		classifications = []*models.PgParamClassification{}
	}
	return c.JSON(classifications)
}

func (s *RESTServer) upsertPgParamClassification(c *fiber.Ctx) error {
	var p models.PgParamClassification
	if err := c.BodyParser(&p); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if p.Name == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "name is required"})
	}
	if p.RestartMode != "reload" && p.RestartMode != "sequential" && p.RestartMode != "full_restart" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "restart_mode must be 'reload', 'sequential', or 'full_restart'"})
	}
	if err := s.store.UpsertPgParamClassification(c.Context(), &p); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	auditLog(c, "pg_param.upsert", "pg_param_classification", p.Name, p.Name, "restart_mode="+p.RestartMode)
	return c.Status(fiber.StatusOK).JSON(p)
}

func (s *RESTServer) deletePgParamClassification(c *fiber.Ctx) error {
	name := c.Params("name")
	if name == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "name is required"})
	}
	if err := s.store.DeletePgParamClassification(c.Context(), name); err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
	}
	auditLog(c, "pg_param.delete", "pg_param_classification", name, name, "")
	return c.SendStatus(fiber.StatusNoContent)
}

// --- Error handler ---

func fiberErrorHandler(c *fiber.Ctx, err error) error {
	code := fiber.StatusInternalServerError
	if e, ok := err.(*fiber.Error); ok {
		code = e.Code
	}
	resp := fiber.Map{"error": err.Error()}
	if rid := requestIDFromFiber(c); rid != "" {
		resp["request_id"] = rid
	}
	return c.Status(code).JSON(resp)
}
