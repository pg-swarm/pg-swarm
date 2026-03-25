package sidecar

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
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
