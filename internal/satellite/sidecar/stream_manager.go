package sidecar

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/rs/zerolog/log"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SidecarStreamManager tracks connected sidecar streams and provides
// synchronous request-response over the bidi stream.
type SidecarStreamManager struct {
	mu      sync.RWMutex
	streams map[string]*SidecarStream // key: "{namespace}/{podName}"
}

// SidecarStream represents a single connected sidecar's stream state.
type SidecarStream struct {
	PodName   string
	Cluster   string
	Namespace string
	SendCh    chan *pgswarmv1.SidecarCommand // buffered, cap 16
	Cancel    context.CancelFunc

	// IsPrimary is updated by the health monitor when the pod's role is known.
	// Used by the backup handler to prefer replica pods for physical backups.
	IsPrimary atomic.Bool

	mu      sync.Mutex
	pending map[string]chan *pgswarmv1.CommandResult // requestID → response
}

// NewSidecarStreamManager creates an empty stream manager.
func NewSidecarStreamManager() *SidecarStreamManager {
	return &SidecarStreamManager{
		streams: make(map[string]*SidecarStream),
	}
}

func streamKey(namespace, podName string) string {
	return namespace + "/" + podName
}

// Add registers a sidecar stream, replacing any previous stream for the same pod.
func (m *SidecarStreamManager) Add(namespace, podName string, s *SidecarStream) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := streamKey(namespace, podName)
	if old, ok := m.streams[key]; ok {
		old.Cancel()
	}
	m.streams[key] = s
}

// Remove deletes the sidecar stream for the given pod.
func (m *SidecarStreamManager) Remove(namespace, podName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.streams, streamKey(namespace, podName))
}

// Get returns the sidecar stream for the given pod.
func (m *SidecarStreamManager) Get(namespace, podName string) *SidecarStream {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.streams[streamKey(namespace, podName)]
}

// ListByCluster returns all connected sidecar streams for the given cluster.
func (m *SidecarStreamManager) ListByCluster(namespace, clusterName string) []*SidecarStream {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []*SidecarStream
	for _, s := range m.streams {
		if s.Namespace == namespace && s.Cluster == clusterName {
			result = append(result, s)
		}
	}
	return result
}

// UpdateRole sets the IsPrimary flag on a sidecar stream by pod name.
// Called by the satellite's health monitor when role changes are observed.
func (m *SidecarStreamManager) UpdateRole(namespace, podName string, isPrimary bool) {
	m.mu.RLock()
	s := m.streams[streamKey(namespace, podName)]
	m.mu.RUnlock()
	if s != nil {
		s.IsPrimary.Store(isPrimary)
	}
}

// PreferReplica returns the streams for a cluster, sorted so replicas come
// first. Falls back to all streams if none are known replicas.
func (m *SidecarStreamManager) PreferReplica(namespace, clusterName string) []*SidecarStream {
	streams := m.ListByCluster(namespace, clusterName)
	if len(streams) <= 1 {
		return streams
	}
	var replicas, others []*SidecarStream
	for _, s := range streams {
		if s.IsPrimary.Load() {
			others = append(others, s)
		} else {
			replicas = append(replicas, s)
		}
	}
	if len(replicas) > 0 {
		return append(replicas, others...)
	}
	return streams // role unknown yet, return all
}

// SendCommandAndWait sends a command to a sidecar and waits for the response.
func (m *SidecarStreamManager) SendCommandAndWait(
	ctx context.Context, namespace, podName string, cmd *pgswarmv1.SidecarCommand,
) (*pgswarmv1.CommandResult, error) {
	stream := m.Get(namespace, podName)
	if stream == nil {
		return nil, fmt.Errorf("sidecar %s/%s not connected", namespace, podName)
	}

	cmd.RequestId = uuid.NewString()
	respCh := stream.registerPending(cmd.RequestId)
	defer stream.removePending(cmd.RequestId)

	select {
	case stream.SendCh <- cmd:
	default:
		return nil, fmt.Errorf("sidecar %s/%s send channel full", namespace, podName)
	}

	select {
	case result := <-respCh:
		return result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// registerPending creates a pending response channel for a request ID.
func (s *SidecarStream) registerPending(requestID string) chan *pgswarmv1.CommandResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan *pgswarmv1.CommandResult, 1)
	s.pending[requestID] = ch
	return ch
}

// removePending removes a pending response channel.
func (s *SidecarStream) removePending(requestID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, requestID)
}

// NewSidecarStream creates a SidecarStream with its pending map initialized.
func NewSidecarStream(podName, cluster, namespace string, cancel context.CancelFunc) *SidecarStream {
	return &SidecarStream{
		PodName:   podName,
		Cluster:   cluster,
		Namespace: namespace,
		SendCh:    make(chan *pgswarmv1.SidecarCommand, 16),
		Cancel:    cancel,
		pending:   make(map[string]chan *pgswarmv1.CommandResult),
	}
}

// DeliverResult delivers a command result to the pending channel for its request ID.
func (s *SidecarStream) DeliverResult(result *pgswarmv1.CommandResult) {
	s.deliverResult(result)
}

// DeliverEventResult delivers an event-based command result using the event's
// operation_id to correlate with the pending request.
func (s *SidecarStream) DeliverEventResult(evt *pgswarmv1.Event) {
	opID := evt.GetOperationId()
	if opID == "" {
		return
	}
	// Convert event result to CommandResult for the pending channel
	result := &pgswarmv1.CommandResult{
		RequestId: opID,
		Success:   evt.Data["success"] == "true",
		Error:     evt.Data["error"],
	}
	if v, ok := evt.Data["in_recovery"]; ok {
		result.InRecovery = v == "true"
	}
	if v, ok := evt.Data["is_fenced"]; ok {
		result.IsFenced = v == "true"
	}
	s.deliverResult(result)
}

// SendEventCommandAndWait sends a command as an Event to a sidecar and waits for
// the event-based result. The command type (e.g., "command.fence") is the event type.
// Command parameters are carried in the event's data map.
func (m *SidecarStreamManager) SendEventCommandAndWait(
	ctx context.Context, namespace, podName string, commandType string, params map[string]string,
) (*pgswarmv1.CommandResult, error) {
	stream := m.Get(namespace, podName)
	if stream == nil {
		return nil, fmt.Errorf("sidecar %s/%s not connected", namespace, podName)
	}

	operationID := uuid.NewString()

	evt := &pgswarmv1.Event{
		Id:          uuid.NewString(),
		Type:        commandType,
		ClusterName: stream.Cluster,
		Namespace:   namespace,
		PodName:     podName,
		Severity:    "info",
		Source:      "satellite",
		Timestamp:   timestamppb.Now(),
		OperationId: operationID,
		Data:        params,
	}
	if evt.Data == nil {
		evt.Data = make(map[string]string)
	}

	cmd := &pgswarmv1.SidecarCommand{
		RequestId: operationID,
		Cmd:       &pgswarmv1.SidecarCommand_Event{Event: evt},
	}

	respCh := stream.registerPending(operationID)
	defer stream.removePending(operationID)

	log.Debug().
		Str("command", commandType).
		Str("pod", podName).
		Str("namespace", namespace).
		Str("operation_id", operationID).
		Msg("sending event command to sidecar")

	select {
	case stream.SendCh <- cmd:
	default:
		return nil, fmt.Errorf("sidecar %s/%s send channel full", namespace, podName)
	}

	select {
	case result := <-respCh:
		log.Debug().
			Str("command", commandType).
			Str("pod", podName).
			Bool("success", result.Success).
			Str("operation_id", operationID).
			Msg("event command result received")
		return result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// eventCommandTypeToLegacy maps event command types to legacy SidecarCommand
// constructors for backward compatibility with old sidecars.
func eventCommandTypeToLegacy(commandType string, params map[string]string) *pgswarmv1.SidecarCommand {
	cmd := &pgswarmv1.SidecarCommand{}
	switch commandType {
	case "command.fence":
		cmd.Cmd = &pgswarmv1.SidecarCommand_Fence{Fence: &pgswarmv1.FenceCmd{}}
	case "command.unfence":
		cmd.Cmd = &pgswarmv1.SidecarCommand_Unfence{Unfence: &pgswarmv1.UnfenceCmd{}}
	case "command.checkpoint":
		cmd.Cmd = &pgswarmv1.SidecarCommand_Checkpoint{Checkpoint: &pgswarmv1.CheckpointCmd{}}
	case "command.promote":
		cmd.Cmd = &pgswarmv1.SidecarCommand_Promote{Promote: &pgswarmv1.PromoteCmd{}}
	case "command.status":
		cmd.Cmd = &pgswarmv1.SidecarCommand_Status{Status: &pgswarmv1.StatusCmd{}}
	case "command.reload_conf":
		cmd.Cmd = &pgswarmv1.SidecarCommand_ReloadConf{ReloadConf: &pgswarmv1.ReloadConfCmd{}}
	case "command.create_database":
		cmd.Cmd = &pgswarmv1.SidecarCommand_CreateDatabase{CreateDatabase: &pgswarmv1.CreateDatabaseCmd{
			DbName:   params["db_name"],
			DbUser:   params["db_user"],
			Password: params["password"],
		}}
	case "command.restart", "command.rewind", "command.rebuild":
		// These commands are handled via the event command path in the connector.
		// They don't have legacy SidecarCommand equivalents — the event carries
		// the command type and params directly.
		cmd.Cmd = &pgswarmv1.SidecarCommand_Event{Event: &pgswarmv1.Event{
			Type: commandType,
			Data: params,
		}}
	default:
		return nil
	}
	return cmd
}

// deliverResult delivers a command result to the pending channel for its request ID.
func (s *SidecarStream) deliverResult(result *pgswarmv1.CommandResult) {
	s.mu.Lock()
	ch, ok := s.pending[result.RequestId]
	s.mu.Unlock()
	if ok {
		select {
		case ch <- result:
		default:
		}
	}
}
