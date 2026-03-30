// REST server core: struct, constructor, route registration, and shared helpers.
package server

import (
	"net/http"

	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/filesystem"
	"github.com/pg-swarm/pg-swarm/dashboard"
	"github.com/pg-swarm/pg-swarm/internal/central/crypto"
	"github.com/pg-swarm/pg-swarm/internal/central/registry"
	"github.com/pg-swarm/pg-swarm/internal/central/store"
	"github.com/pg-swarm/pg-swarm/internal/shared/models"
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

	// Service healthcheck (outside /api/v1 — for K8s probes and load balancers)
	s.app.Get("/healthz", s.healthCheck)

	api := s.app.Group("/api/v1")

	// WebSocket — real-time state push
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
	api.Get("/satellites/:id/logs", s.getSatelliteLogs)
	api.Get("/satellites/:id/logs/stream", s.streamSatelliteLogs)
	api.Post("/satellites/:id/log-level", s.setSatelliteLogLevel)

	// Admin: Storage Tiers + Recovery Rule Sets + Backup Stores
	api.Get("/storage-tiers", s.listStorageTiers)
	api.Post("/storage-tiers", s.createStorageTier)
	api.Put("/storage-tiers/:id", s.updateStorageTier)
	api.Delete("/storage-tiers/:id", s.deleteStorageTier)
	// Event Rule Sets
	api.Get("/event-rule-sets", s.listEventRuleSets)
	api.Post("/event-rule-sets", s.createEventRuleSet)
	api.Get("/event-rule-sets/:id", s.getEventRuleSet)
	api.Put("/event-rule-sets/:id", s.updateEventRuleSet)
	api.Delete("/event-rule-sets/:id", s.deleteEventRuleSet)
	api.Get("/event-rule-sets/:id/handlers", s.listRuleSetHandlers)
	api.Post("/event-rule-sets/:id/handlers", s.addHandlerToRuleSet)
	api.Delete("/event-rule-sets/:id/handlers/:hid", s.removeHandlerFromRuleSet)
	// Event Rules (global)
	api.Get("/event-rules", s.listEventRules)
	api.Post("/event-rules", s.createEventRule)
	api.Put("/event-rules/:id", s.updateEventRule)
	api.Delete("/event-rules/:id", s.deleteEventRule)
	// Event Actions (global)
	api.Get("/event-actions", s.listEventActions)
	api.Post("/event-actions", s.createEventAction)
	api.Put("/event-actions/:id", s.updateEventAction)
	api.Delete("/event-actions/:id", s.deleteEventAction)
	// Event Handlers (global)
	api.Get("/event-handlers", s.listEventHandlers)
	api.Post("/event-handlers", s.createEventHandler)
	api.Put("/event-handlers/:id", s.updateEventHandler)
	api.Delete("/event-handlers/:id", s.deleteEventHandler)
	api.Get("/backup-stores", s.listBackupStores)
	api.Post("/backup-stores", s.createBackupStore)
	api.Get("/backup-stores/:id", s.getBackupStore)
	api.Put("/backup-stores/:id", s.updateBackupStore)
	api.Delete("/backup-stores/:id", s.deleteBackupStore)

	// Clusters
	api.Get("/clusters", s.listClusterConfigs)
	api.Post("/clusters", s.createClusterConfig)
	api.Get("/clusters/:id", s.getClusterConfig)
	api.Put("/clusters/:id", s.updateClusterConfig)
	api.Delete("/clusters/:id", s.deleteClusterConfig)
	api.Post("/clusters/:id/pause", s.pauseCluster)
	api.Post("/clusters/:id/resume", s.resumeCluster)
	api.Post("/clusters/:id/switchover", s.switchoverCluster)
	api.Post("/clusters/:id/switchover/continue", s.switchoverContinue)
	api.Post("/clusters/:id/switchover/abort", s.switchoverAbort)
	api.Get("/clusters/:id/profile-diff", s.clusterProfileDiff)
	api.Post("/clusters/:id/apply", s.applyCluster)
	api.Get("/clusters/:id/databases", s.listClusterDatabases)
	api.Post("/clusters/:id/databases", s.createClusterDatabase)
	api.Put("/clusters/:id/databases/:dbid", s.updateClusterDatabase)
	api.Delete("/clusters/:id/databases/:dbid", s.deleteClusterDatabase)

	// Backup & Restore
	api.Get("/clusters/:id/backups", s.listBackupInventory)
	api.Post("/clusters/:id/trigger-backup", s.triggerBackup)
	api.Get("/clusters/:id/restores", s.listRestoreOperations)
	api.Post("/clusters/:id/restore", s.triggerRestore)

	// Sidecar management
	api.Post("/clusters/:id/sidecar-log-level", s.setSidecarLogLevel)

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

	// Admin: PG Parameter Classifications, Variants, Versions
	api.Get("/pg-param-classifications", s.listPgParamClassifications)
	api.Post("/pg-param-classifications", s.upsertPgParamClassification)
	api.Delete("/pg-param-classifications/:name", s.deletePgParamClassification)
	api.Get("/postgres-variants", s.listPostgresVariants)
	api.Post("/postgres-variants", s.createPostgresVariant)
	api.Delete("/postgres-variants/:id", s.deletePostgresVariant)
	api.Get("/postgres-versions", s.listPostgresVersions)
	api.Post("/postgres-versions", s.createPostgresVersion)
	api.Put("/postgres-versions/:id", s.updatePostgresVersion)
	api.Delete("/postgres-versions/:id", s.deletePostgresVersion)
	api.Post("/postgres-versions/:id/default", s.setDefaultPostgresVersion)

	// Health & Events
	api.Get("/health", s.listClusterHealth)
	api.Get("/events", s.listEvents)
}

// --- Shared helpers ---

// serverDefaultPgParams mirrors the satellite-side defaultPgParams so that diff
// computation can account for values the satellite applies automatically.
var serverDefaultPgParams = map[string]string{
	"max_connections": "100",
	"shared_buffers":  "256MB", "effective_cache_size": "4GB", "work_mem": "4MB",
	"maintenance_work_mem": "128MB", "huge_pages": "try",
	"wal_buffers": "16MB", "min_wal_size": "1GB", "max_wal_size": "4GB",
	"checkpoint_timeout": "15min", "checkpoint_completion_target": "0.9",
	"random_page_cost": "1.1", "seq_page_cost": "1.0",
	"effective_io_concurrency": "200", "default_statistics_target": "100", "jit": "on",
	"track_commit_timestamp": "on", "synchronous_commit": "on",
	"wal_receiver_timeout": "60s", "wal_sender_timeout": "60s",
	"log_min_duration_statement": "200", "log_statement": "none",
	"log_line_prefix": "'%m [%p] %q[user=%u,db=%d] '",
	"log_checkpoints": "on", "log_connections": "off", "log_disconnections": "off",
	"log_lock_waits": "off", "log_temp_files": "-1", "log_autovacuum_min_duration": "-1",
	"autovacuum": "on", "autovacuum_max_workers": "3", "autovacuum_naptime": "1min",
	"autovacuum_vacuum_threshold": "50", "autovacuum_vacuum_scale_factor": "0.2",
	"autovacuum_analyze_threshold": "50", "autovacuum_analyze_scale_factor": "0.1",
	"timezone": "'UTC'", "statement_timeout": "0",
	"idle_in_transaction_session_timeout": "0", "lock_timeout": "0",
	"default_text_search_config": "'pg_catalog.english'",
}

// fillDefaultPgParams backfills server-side default values into a spec's PgParams
// so that diff comparisons don't flag defaults as changes.
func fillDefaultPgParams(spec *models.ClusterSpec) {
	if spec.PgParams == nil {
		spec.PgParams = make(map[string]string, len(serverDefaultPgParams))
	}
	for k, v := range serverDefaultPgParams {
		if _, exists := spec.PgParams[k]; !exists {
			spec.PgParams[k] = v
		}
	}
}

// labelsMatch returns true if labels contain all key-value pairs in selector.
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
