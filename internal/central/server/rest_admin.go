// Admin, health, and reference data REST API handlers.
package server

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/pg-swarm/pg-swarm/internal/shared/models"
	"github.com/rs/zerolog/log"
)

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

// ── Event Rule Sets ─────────────────────────────────────────────────────────

func (s *RESTServer) listEventRuleSets(c *fiber.Ctx) error {
	sets, err := s.store.ListEventRuleSets(c.Context())
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list event rule sets"})
	}
	if sets == nil {
		sets = []*models.EventRuleSet{}
	}
	return c.JSON(sets)
}

func (s *RESTServer) createEventRuleSet(c *fiber.Ctx) error {
	var rs models.EventRuleSet
	if err := c.BodyParser(&rs); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if rs.Name == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "name is required"})
	}
	rs.Builtin = false
	if err := s.store.CreateEventRuleSet(c.Context(), &rs); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create event rule set"})
	}
	auditLog(c, "event_rule_set.create", "event_rule_set", rs.ID.String(), rs.Name, "")
	return c.Status(fiber.StatusCreated).JSON(rs)
}

func (s *RESTServer) getEventRuleSet(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid id"})
	}
	rs, err := s.store.GetEventRuleSet(c.Context(), id)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	return c.JSON(rs)
}

func (s *RESTServer) updateEventRuleSet(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid id"})
	}
	var rs models.EventRuleSet
	if err := c.BodyParser(&rs); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	rs.ID = id
	if rs.Name == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "name is required"})
	}
	if err := s.store.UpdateEventRuleSet(c.Context(), &rs); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to update event rule set"})
	}
	auditLog(c, "event_rule_set.update", "event_rule_set", id.String(), rs.Name, "")
	return c.JSON(rs)
}

func (s *RESTServer) deleteEventRuleSet(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid id"})
	}
	if err := s.store.DeleteEventRuleSet(c.Context(), id); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to delete event rule set"})
	}
	auditLog(c, "event_rule_set.delete", "event_rule_set", id.String(), "", "")
	return c.JSON(fiber.Map{"status": "deleted"})
}

func (s *RESTServer) listRuleSetHandlers(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid id"})
	}
	handlers, err := s.store.ListRuleSetHandlers(c.Context(), id)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list handlers"})
	}
	if handlers == nil {
		handlers = []*models.EventHandlerDetail{}
	}
	return c.JSON(handlers)
}

func (s *RESTServer) addHandlerToRuleSet(c *fiber.Ctx) error {
	ruleSetID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid id"})
	}
	var body struct {
		HandlerID uuid.UUID `json:"handler_id"`
	}
	if err := c.BodyParser(&body); err != nil || body.HandlerID == uuid.Nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "handler_id is required"})
	}
	if err := s.store.AddHandlerToRuleSet(c.Context(), ruleSetID, body.HandlerID); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to add handler"})
	}
	return c.JSON(fiber.Map{"status": "added"})
}

func (s *RESTServer) removeHandlerFromRuleSet(c *fiber.Ctx) error {
	ruleSetID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid id"})
	}
	handlerID, err := uuid.Parse(c.Params("hid"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid handler id"})
	}
	if err := s.store.RemoveHandlerFromRuleSet(c.Context(), ruleSetID, handlerID); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to remove handler"})
	}
	return c.JSON(fiber.Map{"status": "removed"})
}

// ── Event Rules (global) ─────────────────────────────────────────────────────

func (s *RESTServer) listEventRules(c *fiber.Ctx) error {
	rules, err := s.store.ListEventRules(c.Context())
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list event rules"})
	}
	if rules == nil {
		rules = []*models.EventRule{}
	}
	return c.JSON(rules)
}

func (s *RESTServer) createEventRule(c *fiber.Ctx) error {
	var r models.EventRule
	if err := c.BodyParser(&r); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if r.Name == "" || r.Pattern == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "name and pattern are required"})
	}
	r.Builtin = false
	if err := s.store.CreateEventRule(c.Context(), &r); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create event rule"})
	}
	return c.Status(fiber.StatusCreated).JSON(r)
}

func (s *RESTServer) updateEventRule(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid id"})
	}
	var r models.EventRule
	if err := c.BodyParser(&r); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	r.ID = id
	if err := s.store.UpdateEventRule(c.Context(), &r); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to update event rule"})
	}
	return c.JSON(r)
}

func (s *RESTServer) deleteEventRule(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid id"})
	}
	if err := s.store.DeleteEventRule(c.Context(), id); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to delete event rule"})
	}
	return c.JSON(fiber.Map{"status": "deleted"})
}

// ── Event Actions (global) ───────────────────────────────────────────────────

func (s *RESTServer) listEventActions(c *fiber.Ctx) error {
	actions, err := s.store.ListEventActions(c.Context())
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list event actions"})
	}
	if actions == nil {
		actions = []*models.EventAction{}
	}
	return c.JSON(actions)
}

func (s *RESTServer) createEventAction(c *fiber.Ctx) error {
	var a models.EventAction
	if err := c.BodyParser(&a); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if a.Name == "" || a.Type == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "name and type are required"})
	}
	if err := s.store.CreateEventAction(c.Context(), &a); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create event action"})
	}
	return c.Status(fiber.StatusCreated).JSON(a)
}

func (s *RESTServer) updateEventAction(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid id"})
	}
	var a models.EventAction
	if err := c.BodyParser(&a); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	a.ID = id
	if err := s.store.UpdateEventAction(c.Context(), &a); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to update event action"})
	}
	return c.JSON(a)
}

func (s *RESTServer) deleteEventAction(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid id"})
	}
	if err := s.store.DeleteEventAction(c.Context(), id); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to delete event action"})
	}
	return c.JSON(fiber.Map{"status": "deleted"})
}

// ── Event Handlers (global) ──────────────────────────────────────────────────

func (s *RESTServer) listEventHandlers(c *fiber.Ctx) error {
	handlers, err := s.store.ListEventHandlers(c.Context())
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list event handlers"})
	}
	if handlers == nil {
		handlers = []*models.EventHandlerDetail{}
	}
	return c.JSON(handlers)
}

func (s *RESTServer) createEventHandler(c *fiber.Ctx) error {
	var h models.EventHandler
	if err := c.BodyParser(&h); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if h.EventRuleID == uuid.Nil || h.EventActionID == uuid.Nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "event_rule_id and event_action_id are required"})
	}
	if err := s.store.CreateEventHandler(c.Context(), &h); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create event handler"})
	}
	return c.Status(fiber.StatusCreated).JSON(h)
}

func (s *RESTServer) updateEventHandler(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid id"})
	}
	var h models.EventHandler
	if err := c.BodyParser(&h); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	h.ID = id
	if err := s.store.UpdateEventHandler(c.Context(), &h); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to update event handler"})
	}
	return c.JSON(h)
}

func (s *RESTServer) deleteEventHandler(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid id"})
	}
	if err := s.store.DeleteEventHandler(c.Context(), id); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to delete event handler"})
	}
	return c.JSON(fiber.Map{"status": "deleted"})
}

// --- Backup Stores ---

func (s *RESTServer) computeCredentialsSet(bs *models.BackupStore) {
	if bs.Credentials == nil || s.encryptor == nil {
		bs.CredentialsSet = map[string]bool{}
		bs.Credentials = nil
		return
	}
	plaintext, err := s.encryptor.Decrypt(bs.Credentials)
	if err != nil {
		bs.CredentialsSet = map[string]bool{}
		bs.Credentials = nil
		return
	}
	switch bs.StoreType {
	case "gcs":
		var creds models.GCSCredentials
		if json.Unmarshal(plaintext, &creds) == nil {
			bs.CredentialsSet = map[string]bool{
				"service_account_json": creds.ServiceAccountJSON != "",
			}
		}
	case "sftp":
		var creds models.SFTPCredentials
		if json.Unmarshal(plaintext, &creds) == nil {
			bs.CredentialsSet = map[string]bool{
				"password":    creds.Password != "",
				"private_key": creds.PrivateKey != "",
			}
		}
	default:
		bs.CredentialsSet = map[string]bool{}
	}
	bs.Credentials = nil
}

func (s *RESTServer) listBackupStores(c *fiber.Ctx) error {
	stores, err := s.store.ListBackupStores(c.Context())
	if err != nil {
		log.Error().Err(err).Msg("failed to list backup stores")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to list backup stores"})
	}
	for _, bs := range stores {
		s.computeCredentialsSet(bs)
	}
	return c.JSON(stores)
}

func (s *RESTServer) getBackupStore(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid backup store id"})
	}
	bs, err := s.store.GetBackupStore(c.Context(), id)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "backup store not found"})
	}
	s.computeCredentialsSet(bs)
	return c.JSON(bs)
}

func (s *RESTServer) createBackupStore(c *fiber.Ctx) error {
	var body struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		StoreType   string          `json:"store_type"`
		Config      json.RawMessage `json:"config"`
		Credentials json.RawMessage `json:"credentials"`
	}
	if err := c.BodyParser(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}

	bs := &models.BackupStore{
		Name:        body.Name,
		Description: body.Description,
		StoreType:   body.StoreType,
		Config:      body.Config,
	}
	if err := models.ValidateBackupStore(bs); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}

	if len(body.Credentials) > 0 && string(body.Credentials) != "null" && s.encryptor != nil {
		encrypted, err := s.encryptor.Encrypt(body.Credentials)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to encrypt credentials"})
		}
		bs.Credentials = encrypted
	}

	if err := s.store.CreateBackupStore(c.Context(), bs); err != nil {
		log.Error().Err(err).Msg("failed to create backup store")
		if strings.Contains(err.Error(), "duplicate key") {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "backup store name already exists"})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to create backup store"})
	}

	s.computeCredentialsSet(bs)
	auditLog(c, "backup_store.create", "backup_store", bs.ID.String(), bs.Name, "type="+bs.StoreType)
	return c.Status(fiber.StatusCreated).JSON(bs)
}

func (s *RESTServer) updateBackupStore(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid backup store id"})
	}

	var body struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		StoreType   string          `json:"store_type"`
		Config      json.RawMessage `json:"config"`
		Credentials json.RawMessage `json:"credentials"`
	}
	if err := c.BodyParser(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}

	bs := &models.BackupStore{
		ID:          id,
		Name:        body.Name,
		Description: body.Description,
		StoreType:   body.StoreType,
		Config:      body.Config,
	}
	if err := models.ValidateBackupStore(bs); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}

	if len(body.Credentials) > 0 && string(body.Credentials) != "null" && s.encryptor != nil {
		encrypted, err := s.encryptor.Encrypt(body.Credentials)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to encrypt credentials"})
		}
		bs.Credentials = encrypted
	}

	if err := s.store.UpdateBackupStore(c.Context(), bs); err != nil {
		log.Error().Err(err).Msg("failed to update backup store")
		if strings.Contains(err.Error(), "duplicate key") {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "backup store name already exists"})
		}
		if strings.Contains(err.Error(), "not found") {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to update backup store"})
	}

	// Reload to get current credentials for CredentialsSet
	updated, err := s.store.GetBackupStore(c.Context(), id)
	if err != nil {
		s.computeCredentialsSet(bs)
		auditLog(c, "backup_store.update", "backup_store", id.String(), bs.Name, "")
		return c.JSON(bs)
	}
	s.computeCredentialsSet(updated)
	auditLog(c, "backup_store.update", "backup_store", id.String(), updated.Name, "")
	return c.JSON(updated)
}

func (s *RESTServer) deleteBackupStore(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid backup store id"})
	}
	if err := s.store.DeleteBackupStore(c.Context(), id); err != nil {
		log.Error().Err(err).Msg("failed to delete backup store")
		if strings.Contains(err.Error(), "cannot be deleted") {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": err.Error()})
		}
		if strings.Contains(err.Error(), "not found") {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to delete backup store"})
	}
	auditLog(c, "backup_store.delete", "backup_store", id.String(), "", "")
	return c.JSON(fiber.Map{"status": "deleted"})
}

// --- Service Healthcheck ---

// healthCheck reports the overall health of the central service, including
// database connectivity and connected satellite count. Designed for K8s
// liveness/readiness probes and load balancer health checks.
func (s *RESTServer) healthCheck(c *fiber.Ctx) error {
	dbOK := true
	dbErr := ""
	if err := s.store.Ping(c.Context()); err != nil {
		dbOK = false
		dbErr = err.Error()
	}

	connectedSatellites := 0
	if s.streams != nil {
		connectedSatellites = s.streams.Count()
	}

	status := "ok"
	httpCode := fiber.StatusOK
	if !dbOK {
		status = "degraded"
		httpCode = fiber.StatusServiceUnavailable
	}

	return c.Status(httpCode).JSON(fiber.Map{
		"status": status,
		"checks": fiber.Map{
			"database": fiber.Map{
				"status": boolToStatus(dbOK),
				"error":  dbErr,
			},
		},
		"connected_satellites": connectedSatellites,
	})
}

func boolToStatus(ok bool) string {
	if ok {
		return "ok"
	}
	return "fail"
}

// --- Cluster Health ---

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
