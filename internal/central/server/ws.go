package server

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog/log"
)

// wsClient represents a single WebSocket connection.
type wsClient struct {
	conn *websocket.Conn
	send chan []byte
}

// WSHub manages WebSocket connections and broadcasts state updates.
type WSHub struct {
	mu      sync.RWMutex
	clients map[*wsClient]bool
	server  *RESTServer
	notify  chan struct{}
}

// newWSHub creates a new WebSocket hub.
func newWSHub(srv *RESTServer) *WSHub {
	return &WSHub{
		clients: make(map[*wsClient]bool),
		server:  srv,
		notify:  make(chan struct{}, 1),
	}
}

// Run starts the hub's broadcast loop. Call in a goroutine.
func (h *WSHub) Run() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			h.broadcast()
		case <-h.notify:
			// Small debounce so rapid mutations coalesce into one broadcast.
			time.Sleep(50 * time.Millisecond)
			// Drain any extra notifications.
			for {
				select {
				case <-h.notify:
				default:
					goto done
				}
			}
		done:
			h.broadcast()
		}
	}
}

// Notify triggers an immediate broadcast to all connected clients.
func (h *WSHub) Notify() {
	select {
	case h.notify <- struct{}{}:
	default:
		// Already pending.
	}
}

func (h *WSHub) register(c *wsClient) {
	h.mu.Lock()
	h.clients[c] = true
	h.mu.Unlock()
	log.Debug().Int("clients", len(h.clients)).Msg("ws client connected")
}

func (h *WSHub) unregister(c *wsClient) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	close(c.send)
	log.Debug().Int("clients", len(h.clients)).Msg("ws client disconnected")
}

// wsMessage is the envelope sent to WebSocket clients.
type wsMessage struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

// broadcast fetches current state and pushes it to all clients.
func (h *WSHub) broadcast() {
	h.mu.RLock()
	n := len(h.clients)
	h.mu.RUnlock()
	if n == 0 {
		return
	}

	state := h.fetchState()
	msg, err := json.Marshal(wsMessage{Type: "full_state", Data: state})
	if err != nil {
		log.Error().Err(err).Msg("ws: marshal state")
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		select {
		case c.send <- msg:
		default:
			// Client too slow; drop message.
		}
	}
}

// fetchState gathers the same data the dashboard polls via REST.
func (h *WSHub) fetchState() map[string]interface{} {
	ctx := context.Background()
	s := h.server.store
	state := make(map[string]interface{})

	if v, err := s.ListSatellites(ctx); err == nil {
		state["satellites"] = v
	}
	if v, err := s.ListClusterConfigs(ctx); err == nil {
		state["clusters"] = v
	}
	if v, err := s.ListClusterHealth(ctx); err == nil {
		state["health"] = v
	}
	if v, err := s.ListEvents(ctx, 50); err == nil {
		state["events"] = v
	}
	if v, err := s.ListProfiles(ctx); err == nil {
		state["profiles"] = v
	}
	if v, err := s.ListDeploymentRules(ctx); err == nil {
		state["deploymentRules"] = v
	}
	if v, err := s.ListPostgresVersions(ctx); err == nil {
		state["postgresVersions"] = v
	}
	if v, err := s.ListPostgresVariants(ctx); err == nil {
		state["postgresVariants"] = v
	}
	if v, err := s.ListBackupProfiles(ctx); err == nil {
		state["backupProfiles"] = v
	}
	if v, err := s.ListStorageTiers(ctx); err == nil {
		state["storageTiers"] = v
	}
	if v, err := s.ListRecoveryRuleSets(ctx); err == nil {
		state["recoveryRuleSets"] = v
	}

	return state
}

// upgradeMiddleware checks if a request is a WebSocket upgrade.
func upgradeMiddleware(c *fiber.Ctx) error {
	if websocket.IsWebSocketUpgrade(c) {
		return c.Next()
	}
	return fiber.ErrUpgradeRequired
}

// handleWS is the per-connection WebSocket handler.
func (h *WSHub) handleWS(c *websocket.Conn) {
	client := &wsClient{
		conn: c,
		send: make(chan []byte, 16),
	}
	h.register(client)
	defer h.unregister(client)

	// Send initial state immediately.
	state := h.fetchState()
	if msg, err := json.Marshal(wsMessage{Type: "full_state", Data: state}); err == nil {
		if err := c.WriteMessage(websocket.TextMessage, msg); err != nil {
			return
		}
	}

	// Writer goroutine: sends queued messages to the client.
	go func() {
		for msg := range client.send {
			if err := c.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		}
	}()

	// Reader loop: keep connection alive, handle pings/close.
	for {
		if _, _, err := c.ReadMessage(); err != nil {
			break
		}
	}
}
