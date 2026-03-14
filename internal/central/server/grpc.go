package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/pg-swarm/pg-swarm/internal/central/auth"
	"github.com/pg-swarm/pg-swarm/internal/central/registry"
	"github.com/pg-swarm/pg-swarm/internal/central/store"
	"github.com/pg-swarm/pg-swarm/internal/shared/models"
)

type GRPCServer struct {
	pgswarmv1.UnimplementedRegistrationServiceServer
	pgswarmv1.UnimplementedSatelliteStreamServiceServer

	registry *registry.Registry
	store    store.Store
	streams  *StreamManager
	server   *grpc.Server
}

type StreamManager struct {
	mu      sync.RWMutex
	streams map[uuid.UUID]*SatelliteStream
}

type SatelliteStream struct {
	SatelliteID uuid.UUID
	SendCh      chan *pgswarmv1.CentralMessage
	Cancel      context.CancelFunc
}

func NewGRPCServer(reg *registry.Registry, s store.Store) *GRPCServer {
	srv := &GRPCServer{
		registry: reg,
		store:    s,
		streams:  NewStreamManager(),
	}

	srv.server = grpc.NewServer(
		grpc.UnaryInterceptor(srv.unaryAuthInterceptor),
		grpc.StreamInterceptor(srv.streamAuthInterceptor),
	)

	pgswarmv1.RegisterRegistrationServiceServer(srv.server, srv)
	pgswarmv1.RegisterSatelliteStreamServiceServer(srv.server, srv)

	return srv
}

func NewStreamManager() *StreamManager {
	return &StreamManager{
		streams: make(map[uuid.UUID]*SatelliteStream),
	}
}

func (s *GRPCServer) Start(addr string) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	log.Info().Str("addr", addr).Msg("gRPC server starting")
	return s.server.Serve(lis)
}

func (s *GRPCServer) Stop() {
	s.server.GracefulStop()
}

// Register handles satellite registration (no auth required).
func (s *GRPCServer) Register(ctx context.Context, req *pgswarmv1.RegisterRequest) (*pgswarmv1.RegisterResponse, error) {
	labels := req.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}

	id, tempToken, err := s.registry.Register(ctx, req.GetHostname(), req.GetK8SClusterName(), req.GetRegion(), labels)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "registration failed: %v", err)
	}

	return &pgswarmv1.RegisterResponse{
		SatelliteId: id.String(),
		TempToken:   tempToken,
	}, nil
}

// CheckApproval checks if a satellite has been approved.
func (s *GRPCServer) CheckApproval(ctx context.Context, req *pgswarmv1.CheckApprovalRequest) (*pgswarmv1.CheckApprovalResponse, error) {
	satID, err := uuid.Parse(req.GetSatelliteId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid satellite_id: %v", err)
	}

	approved, authToken, err := s.registry.CheckApproval(ctx, satID, req.GetTempToken())
	if err != nil {
		if strings.Contains(err.Error(), "invalid temp token") {
			return nil, status.Errorf(codes.Unauthenticated, "check approval failed: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "check approval failed: %v", err)
	}

	return &pgswarmv1.CheckApprovalResponse{
		Approved:  approved,
		AuthToken: authToken,
	}, nil
}

// Connect handles the bidirectional stream (Phase 2 - scaffold for now).
func (s *GRPCServer) Connect(stream grpc.BidiStreamingServer[pgswarmv1.SatelliteMessage, pgswarmv1.CentralMessage]) error {
	// Get satellite ID from context (set by auth interceptor)
	satID, ok := satelliteIDFromContext(stream.Context())
	if !ok {
		return status.Error(codes.Unauthenticated, "no satellite ID in context")
	}

	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	satStream := &SatelliteStream{
		SatelliteID: satID,
		SendCh:      make(chan *pgswarmv1.CentralMessage, 64),
		Cancel:      cancel,
	}

	s.streams.Add(satID, satStream)
	defer s.streams.Remove(satID)

	// Update satellite state to connected
	if err := s.store.UpdateSatelliteState(ctx, satID, models.SatelliteStateConnected); err != nil {
		log.Error().Err(err).Str("satellite_id", satID.String()).Msg("failed to update satellite state")
	}

	log.Info().Str("satellite_id", satID.String()).Msg("satellite connected")

	// Read loop (in goroutine)
	errCh := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				errCh <- err
				return
			}

			switch payload := msg.Payload.(type) {
			case *pgswarmv1.SatelliteMessage_Heartbeat:
				if err := s.store.UpdateSatelliteHeartbeat(ctx, satID); err != nil {
					log.Error().Err(err).Str("satellite_id", satID.String()).Msg("failed to update heartbeat")
				}
				// Send HeartbeatAck (non-blocking)
				ack := &pgswarmv1.CentralMessage{
					Payload: &pgswarmv1.CentralMessage_HeartbeatAck{
						HeartbeatAck: &pgswarmv1.HeartbeatAck{
							Timestamp: payload.Heartbeat.Timestamp,
						},
					},
				}
				select {
				case satStream.SendCh <- ack:
				default:
					log.Warn().Str("satellite_id", satID.String()).Msg("send channel full, dropping heartbeat ack")
				}

			case *pgswarmv1.SatelliteMessage_ConfigAck:
				ack := payload.ConfigAck
				logger := log.With().
					Str("satellite_id", satID.String()).
					Str("cluster", ack.ClusterName).
					Int64("version", ack.ConfigVersion).
					Bool("success", ack.Success).
					Logger()
				if ack.Success {
					logger.Info().Msg("config ack received")
				} else {
					logger.Warn().Str("error", ack.ErrorMessage).Msg("config ack received with error")
				}

			case *pgswarmv1.SatelliteMessage_HealthReport:
				report := payload.HealthReport
				instances, err := protoInstancesToJSON(report.Instances)
				if err != nil {
					log.Error().Err(err).Str("satellite_id", satID.String()).Msg("failed to marshal instances")
					break
				}
				h := models.ClusterHealth{
					SatelliteID: satID,
					ClusterName: report.ClusterName,
					State:       protoStateToModel(report.State),
					Instances:   instances,
					UpdatedAt:   time.Now(),
				}
				if err := s.store.UpsertClusterHealth(ctx, &h); err != nil {
					log.Error().Err(err).Str("satellite_id", satID.String()).Str("cluster", report.ClusterName).Msg("failed to upsert health")
				} else {
					log.Debug().Str("satellite_id", satID.String()).Str("cluster", report.ClusterName).Str("state", string(h.State)).Msg("health report processed")
				}

			case *pgswarmv1.SatelliteMessage_EventReport:
				report := payload.EventReport
				evt := models.Event{
					ID:          uuid.New(),
					SatelliteID: satID,
					ClusterName: report.ClusterName,
					Severity:    report.Severity,
					Message:     report.Message,
					Source:      report.Source,
					CreatedAt:   time.Now(),
				}
				if err := s.store.CreateEvent(ctx, &evt); err != nil {
					log.Error().Err(err).Str("satellite_id", satID.String()).Msg("failed to create event")
				} else {
					log.Info().Str("satellite_id", satID.String()).Str("cluster", report.ClusterName).Str("severity", report.Severity).Msg("event recorded")
				}

			case *pgswarmv1.SatelliteMessage_SwitchoverResult:
				result := payload.SwitchoverResult
				if result.Success {
					log.Info().Str("satellite_id", satID.String()).Str("cluster", result.ClusterName).Msg("switchover succeeded")
					_ = s.store.CreateEvent(ctx, &models.Event{
						ID: uuid.New(), SatelliteID: satID, ClusterName: result.ClusterName,
						Severity: "info", Message: "planned switchover completed successfully", Source: "switchover",
						CreatedAt: time.Now(),
					})
				} else {
					log.Warn().Str("satellite_id", satID.String()).Str("cluster", result.ClusterName).Str("error", result.ErrorMessage).Msg("switchover failed")
					_ = s.store.CreateEvent(ctx, &models.Event{
						ID: uuid.New(), SatelliteID: satID, ClusterName: result.ClusterName,
						Severity: "error", Message: "switchover failed: " + result.ErrorMessage, Source: "switchover",
						CreatedAt: time.Now(),
					})
				}

			case *pgswarmv1.SatelliteMessage_StorageClassReport:
				report := payload.StorageClassReport
				classes := make([]models.StorageClassInfo, 0, len(report.StorageClasses))
				for _, sc := range report.StorageClasses {
					classes = append(classes, models.StorageClassInfo{
						Name:              sc.Name,
						Provisioner:       sc.Provisioner,
						ReclaimPolicy:     sc.ReclaimPolicy,
						VolumeBindingMode: sc.VolumeBindingMode,
						IsDefault:         sc.IsDefault,
					})
				}
				if err := s.store.UpdateSatelliteStorageClasses(ctx, satID, classes); err != nil {
					log.Error().Err(err).Str("satellite_id", satID.String()).Msg("failed to store storage classes")
				} else {
					log.Info().Str("satellite_id", satID.String()).Int("count", len(classes)).Msg("storage classes updated")
				}
			}
		}
	}()

	// Write loop
	for {
		select {
		case msg := <-satStream.SendCh:
			if err := stream.Send(msg); err != nil {
				return err
			}
		case err := <-errCh:
			// Mark as disconnected
			_ = s.store.UpdateSatelliteState(context.Background(), satID, models.SatelliteStateDisconnected)
			log.Info().Str("satellite_id", satID.String()).Msg("satellite disconnected")
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// StreamManager methods

func (sm *StreamManager) Add(id uuid.UUID, stream *SatelliteStream) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.streams[id] = stream
}

func (sm *StreamManager) Remove(id uuid.UUID) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.streams, id)
}

func (sm *StreamManager) Get(id uuid.UUID) (*SatelliteStream, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	s, ok := sm.streams[id]
	return s, ok
}

func (sm *StreamManager) PushConfig(satelliteID uuid.UUID, config *pgswarmv1.ClusterConfig) error {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	stream, ok := sm.streams[satelliteID]
	if !ok {
		return fmt.Errorf("satellite %s not connected", satelliteID)
	}

	msg := &pgswarmv1.CentralMessage{
		Payload: &pgswarmv1.CentralMessage_ClusterConfig{
			ClusterConfig: config,
		},
	}

	select {
	case stream.SendCh <- msg:
		return nil
	default:
		return fmt.Errorf("satellite %s send channel full", satelliteID)
	}
}

func (sm *StreamManager) RequestStorageClasses(satelliteID uuid.UUID) error {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	stream, ok := sm.streams[satelliteID]
	if !ok {
		return fmt.Errorf("satellite %s not connected", satelliteID)
	}

	msg := &pgswarmv1.CentralMessage{
		Payload: &pgswarmv1.CentralMessage_RequestStorageClasses{
			RequestStorageClasses: &pgswarmv1.RequestStorageClasses{},
		},
	}

	select {
	case stream.SendCh <- msg:
		return nil
	default:
		return fmt.Errorf("satellite %s send channel full", satelliteID)
	}
}

func (sm *StreamManager) PushSwitchover(satelliteID uuid.UUID, req *pgswarmv1.SwitchoverRequest) error {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	stream, ok := sm.streams[satelliteID]
	if !ok {
		return fmt.Errorf("satellite %s not connected", satelliteID)
	}

	msg := &pgswarmv1.CentralMessage{
		Payload: &pgswarmv1.CentralMessage_Switchover{
			Switchover: req,
		},
	}

	select {
	case stream.SendCh <- msg:
		return nil
	default:
		return fmt.Errorf("satellite %s send channel full", satelliteID)
	}
}

// GetStreams returns the StreamManager (needed by REST API for config push).
func (s *GRPCServer) GetStreams() *StreamManager {
	return s.streams
}

// Auth interceptors

func (s *GRPCServer) unaryAuthInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	// Registration RPCs don't need auth
	if info.FullMethod == pgswarmv1.RegistrationService_Register_FullMethodName ||
		info.FullMethod == pgswarmv1.RegistrationService_CheckApproval_FullMethodName {
		return handler(ctx, req)
	}
	return handler(ctx, req)
}

func (s *GRPCServer) streamAuthInterceptor(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx := ss.Context()
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing metadata")
	}

	tokens := md.Get("authorization")
	if len(tokens) == 0 {
		return status.Error(codes.Unauthenticated, "missing authorization token")
	}

	tokenHash := auth.HashToken(tokens[0])
	sat, err := s.store.GetSatelliteByToken(ctx, tokenHash)
	if err != nil {
		return status.Error(codes.Unauthenticated, "invalid token")
	}

	newCtx := contextWithSatelliteID(ctx, sat.ID)
	wrapped := &wrappedServerStream{ServerStream: ss, ctx: newCtx}
	return handler(srv, wrapped)
}

// Context helpers for satellite ID

type satelliteIDKey struct{}

func contextWithSatelliteID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, satelliteIDKey{}, id)
}

func satelliteIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(satelliteIDKey{}).(uuid.UUID)
	return id, ok
}

// wrappedServerStream passes a modified context through the stream.
type wrappedServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedServerStream) Context() context.Context {
	return w.ctx
}

// protoStateToModel converts a proto ClusterState enum to the model string.
func protoStateToModel(s pgswarmv1.ClusterState) models.ClusterState {
	switch s {
	case pgswarmv1.ClusterState_CLUSTER_STATE_RUNNING:
		return models.ClusterStateRunning
	case pgswarmv1.ClusterState_CLUSTER_STATE_DEGRADED:
		return models.ClusterStateDegraded
	case pgswarmv1.ClusterState_CLUSTER_STATE_FAILED:
		return models.ClusterStateFailed
	case pgswarmv1.ClusterState_CLUSTER_STATE_DELETING:
		return models.ClusterStateDeleting
	default:
		return models.ClusterStateCreating
	}
}

// instanceJSON mirrors InstanceHealth for JSON serialization into the store.
type instanceJSON struct {
	PodName               string  `json:"pod_name"`
	Role                  string  `json:"role"`
	Ready                 bool    `json:"ready"`
	ReplicationLagBytes   int64   `json:"replication_lag_bytes"`
	ReplicationLagSeconds float64 `json:"replication_lag_seconds,omitempty"`
	ConnectionsUsed       int32   `json:"connections_used,omitempty"`
	ConnectionsMax        int32   `json:"connections_max,omitempty"`
	DiskUsedBytes         int64   `json:"disk_used_bytes,omitempty"`
	TimelineID            int64   `json:"timeline_id,omitempty"`
	PgStartTime           string  `json:"pg_start_time,omitempty"`
	WalReceiverActive     bool    `json:"wal_receiver_active,omitempty"`
	ErrorMessage          string  `json:"error_message,omitempty"`
}

func protoRoleToString(r pgswarmv1.InstanceRole) string {
	switch r {
	case pgswarmv1.InstanceRole_INSTANCE_ROLE_PRIMARY:
		return "primary"
	case pgswarmv1.InstanceRole_INSTANCE_ROLE_REPLICA:
		return "replica"
	case pgswarmv1.InstanceRole_INSTANCE_ROLE_FAILED_PRIMARY:
		return "failed_primary"
	default:
		return "unknown"
	}
}

// protoInstancesToJSON converts proto InstanceHealth list to json.RawMessage.
func protoInstancesToJSON(instances []*pgswarmv1.InstanceHealth) (json.RawMessage, error) {
	out := make([]instanceJSON, 0, len(instances))
	for _, inst := range instances {
		ij := instanceJSON{
			PodName:               inst.PodName,
			Role:                  protoRoleToString(inst.Role),
			Ready:                 inst.Ready,
			ReplicationLagBytes:   inst.ReplicationLagBytes,
			ReplicationLagSeconds: inst.ReplicationLagSeconds,
			ConnectionsUsed:       inst.ConnectionsUsed,
			ConnectionsMax:        inst.ConnectionsMax,
			DiskUsedBytes:         inst.DiskUsedBytes,
			TimelineID:            inst.TimelineId,
			WalReceiverActive:     inst.WalReceiverActive,
			ErrorMessage:          inst.ErrorMessage,
		}
		if inst.PgStartTime != nil {
			ij.PgStartTime = inst.PgStartTime.AsTime().Format(time.RFC3339)
		}
		out = append(out, ij)
	}
	return json.Marshal(out)
}
