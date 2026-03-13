package server

import (
	"strconv"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/pg-swarm/pg-swarm/internal/central/registry"
	"github.com/pg-swarm/pg-swarm/internal/central/store"
	"github.com/pg-swarm/pg-swarm/internal/shared/models"
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
	api := s.app.Group("/api/v1")

	// Satellites
	api.Get("/satellites", s.listSatellites)
	api.Post("/satellites/:id/approve", s.approveSatellite)
	api.Post("/satellites/:id/reject", s.rejectSatellite)

	// Cluster configs
	api.Get("/clusters", s.listClusterConfigs)
	api.Post("/clusters", s.createClusterConfig)
	api.Get("/clusters/:id", s.getClusterConfig)
	api.Put("/clusters/:id", s.updateClusterConfig)
	api.Delete("/clusters/:id", s.deleteClusterConfig)

	// Edge groups
	api.Get("/groups", s.listGroups)
	api.Post("/groups", s.createGroup)
	api.Post("/groups/:id/satellites/:satelliteId", s.assignSatelliteToGroup)

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

// --- Edge Groups ---

func (s *RESTServer) listGroups(c *fiber.Ctx) error {
	groups, err := s.store.ListGroups(c.Context())
	if err != nil {
		log.Error().Err(err).Msg("failed to list groups")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list groups"})
	}
	return c.JSON(groups)
}

func (s *RESTServer) createGroup(c *fiber.Ctx) error {
	var group models.EdgeGroup
	if err := c.BodyParser(&group); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}

	group.ID = uuid.New()

	if err := s.store.CreateGroup(c.Context(), &group); err != nil {
		log.Error().Err(err).Msg("failed to create group")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create group"})
	}

	log.Info().Str("group_id", group.ID.String()).Str("name", group.Name).Msg("edge group created")
	return c.Status(fiber.StatusCreated).JSON(group)
}

func (s *RESTServer) assignSatelliteToGroup(c *fiber.Ctx) error {
	groupID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid group id"})
	}

	satelliteID, err := uuid.Parse(c.Params("satelliteId"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid satellite id"})
	}

	if err := s.store.AssignSatelliteToGroup(c.Context(), satelliteID, groupID); err != nil {
		log.Error().Err(err).
			Str("satellite_id", satelliteID.String()).
			Str("group_id", groupID.String()).
			Msg("failed to assign satellite to group")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to assign satellite to group"})
	}

	log.Info().
		Str("satellite_id", satelliteID.String()).
		Str("group_id", groupID.String()).
		Msg("satellite assigned to group")
	return c.JSON(fiber.Map{"status": "assigned"})
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

	protoConfig := &pgswarmv1.ClusterConfig{
		ClusterName:   cfg.Name,
		Namespace:     cfg.Namespace,
		Replicas:      spec.Replicas,
		ConfigVersion: cfg.ConfigVersion,
		Postgres: &pgswarmv1.PostgresSpec{
			Version: spec.Postgres.Version,
			Image:   spec.Postgres.Image,
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
