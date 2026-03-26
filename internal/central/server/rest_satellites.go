// Satellite REST API handlers.
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

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
