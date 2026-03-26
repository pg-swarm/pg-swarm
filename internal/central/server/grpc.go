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
	"github.com/pg-swarm/pg-swarm/internal/shared/reqid"
	"github.com/rs/zerolog"
)

type GRPCServer struct {
	pgswarmv1.UnimplementedRegistrationServiceServer
	pgswarmv1.UnimplementedSatelliteStreamServiceServer

	registry   *registry.Registry
	store      store.Store
	streams    *StreamManager
	logBuffer  *LogBuffer
	server     *grpc.Server
	wsHub      *WSHub
	opsTracker *OpsTracker
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

// NewGRPCServer creates a GRPCServer wired to the given registry and store,
// with authentication interceptors for both unary and streaming RPCs.
func NewGRPCServer(reg *registry.Registry, s store.Store) *GRPCServer {
	srv := &GRPCServer{
		registry:  reg,
		store:     s,
		streams:   NewStreamManager(),
		logBuffer: NewLogBuffer(),
	}

	srv.server = grpc.NewServer(
		grpc.ChainUnaryInterceptor(srv.unaryLoggingInterceptor, srv.unaryAuthInterceptor),
		grpc.ChainStreamInterceptor(srv.streamLoggingInterceptor, srv.streamAuthInterceptor),
	)

	pgswarmv1.RegisterRegistrationServiceServer(srv.server, srv)
	pgswarmv1.RegisterSatelliteStreamServiceServer(srv.server, srv)

	return srv
}

// NewStreamManager creates an empty StreamManager ready to track satellite streams.
func NewStreamManager() *StreamManager {
	return &StreamManager{
		streams: make(map[uuid.UUID]*SatelliteStream),
	}
}

// Start listens on the given address and serves gRPC requests. It blocks
// until the server is stopped or an error occurs.
func (s *GRPCServer) Start(addr string) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	log.Info().Str("addr", addr).Msg("gRPC server starting")
	return s.server.Serve(lis)
}

// Stop gracefully shuts down the gRPC server, waiting for in-flight RPCs to complete.
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

	// Push existing configs for this satellite on connect
	s.syncConfigs(ctx, satID, satStream)

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
					// Sync cluster config state with health-reported state
					if err := s.store.UpdateClusterConfigState(ctx, satID, report.ClusterName, h.State); err != nil {
						log.Warn().Err(err).Str("cluster", report.ClusterName).Msg("failed to update cluster config state")
					}
					// Push to dashboard immediately (50ms debounce in hub)
					if s.wsHub != nil {
						s.wsHub.Notify()
					}
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

			case *pgswarmv1.SatelliteMessage_SwitchoverProgress:
				prog := payload.SwitchoverProgress
				log.Debug().
					Str("satellite_id", satID.String()).
					Str("cluster", prog.ClusterName).
					Int32("step", prog.Step).
					Str("step_name", prog.StepName).
					Str("status", prog.Status).
					Msg("switchover progress")
				if s.opsTracker != nil {
					s.opsTracker.UpdateStep(prog.OperationId, prog.Step, prog.StepName, prog.Status, prog.TargetPod, prog.ErrorMessage, prog.PointOfNoReturn)
					if prog.StepName == "find_primary" && prog.Status == "completed" && prog.TargetPod != "" {
						s.opsTracker.SetPrimaryPod(prog.OperationId, prog.TargetPod)
					}
				}
				if s.wsHub != nil {
					s.wsHub.BroadcastJSON("switchover_progress", map[string]interface{}{
						"operation_id":       prog.OperationId,
						"cluster_name":       prog.ClusterName,
						"step":               prog.Step,
						"step_name":          prog.StepName,
						"status":             prog.Status,
						"target_pod":         prog.TargetPod,
						"error_message":      prog.ErrorMessage,
						"point_of_no_return": prog.PointOfNoReturn,
					})
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
				if s.opsTracker != nil && result.OperationId != "" {
					s.opsTracker.Complete(result.OperationId, result.Success, result.ErrorMessage)
				}
				if s.wsHub != nil && result.OperationId != "" {
					s.wsHub.BroadcastJSON("switchover_complete", map[string]interface{}{
						"operation_id": result.OperationId,
						"cluster_name": result.ClusterName,
						"success":      result.Success,
						"error":        result.ErrorMessage,
					})
				}

			case *pgswarmv1.SatelliteMessage_LogEntry:
				entry := payload.LogEntry
				ts := ""
				if entry.Timestamp != nil {
					ts = entry.Timestamp.AsTime().Format(time.RFC3339Nano)
				}
				s.logBuffer.Push(satID, &LogEntryJSON{
					Level:     entry.Level,
					Message:   entry.Message,
					Fields:    entry.Fields,
					Timestamp: ts,
					Logger:    entry.Logger,
				})

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

			case *pgswarmv1.SatelliteMessage_DatabaseStatus:
				report := payload.DatabaseStatus
				s.handleDatabaseStatusReport(ctx, satID, report)

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

// syncConfigs pushes all existing non-paused configs for a satellite when it
// (re)connects, ensuring it receives any configs that were created while it
// was offline.
func (s *GRPCServer) syncConfigs(ctx context.Context, satID uuid.UUID, satStream *SatelliteStream) {
	configs, err := s.store.GetClusterConfigsBySatellite(ctx, satID)
	if err != nil {
		log.Error().Err(err).Str("satellite_id", satID.String()).Msg("sync-configs: failed to list configs")
		return
	}

	for _, cfg := range configs {
		if cfg.Paused {
			continue
		}

		protoConfig, err := buildProtoClusterConfig(s.store, cfg, nil)
		if err != nil {
			log.Error().Err(err).
				Str("satellite_id", satID.String()).
				Str("config_id", cfg.ID.String()).
				Msg("sync-configs: failed to build proto config")
			continue
		}

		msg := &pgswarmv1.CentralMessage{
			Payload: &pgswarmv1.CentralMessage_ClusterConfig{
				ClusterConfig: protoConfig,
			},
		}

		select {
		case satStream.SendCh <- msg:
			log.Info().
				Str("satellite_id", satID.String()).
				Str("cluster", cfg.Name).
				Msg("sync-configs: pushed config on connect")
		default:
			log.Warn().
				Str("satellite_id", satID.String()).
				Str("cluster", cfg.Name).
				Msg("sync-configs: send channel full, skipping config")
		}
	}
}

// StreamManager methods

// Add registers a satellite stream, replacing any previous stream for the same ID.
func (sm *StreamManager) Add(id uuid.UUID, stream *SatelliteStream) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.streams[id] = stream
}

// Remove deletes the satellite stream associated with the given ID.
func (sm *StreamManager) Remove(id uuid.UUID) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.streams, id)
}

// Get returns the satellite stream for the given ID, or false if not connected.
func (sm *StreamManager) Get(id uuid.UUID) (*SatelliteStream, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	s, ok := sm.streams[id]
	return s, ok
}

// PushConfig sends a cluster configuration to the specified satellite over its
// active stream. Returns an error if the satellite is not connected or its
// send channel is full.
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

// RequestStorageClasses asks the specified satellite to report its available
// Kubernetes storage classes.
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

// PushSwitchover sends a switchover request to the specified satellite,
// instructing it to promote a replica to primary.
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

// PushSetLogLevel sends a log level change command to the specified satellite.
func (sm *StreamManager) PushSetLogLevel(satelliteID uuid.UUID, level string) error {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	stream, ok := sm.streams[satelliteID]
	if !ok {
		return fmt.Errorf("satellite %s not connected", satelliteID)
	}

	msg := &pgswarmv1.CentralMessage{
		Payload: &pgswarmv1.CentralMessage_SetLogLevel{
			SetLogLevel: &pgswarmv1.SetLogLevel{
				Level: level,
			},
		},
	}

	select {
	case stream.SendCh <- msg:
		return nil
	default:
		return fmt.Errorf("satellite %s send channel full", satelliteID)
	}
}

// PushDelete sends a cluster deletion command to the specified satellite.
func (sm *StreamManager) PushDelete(satelliteID uuid.UUID, del *pgswarmv1.DeleteCluster) error {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	stream, ok := sm.streams[satelliteID]
	if !ok {
		return fmt.Errorf("satellite %s not connected", satelliteID)
	}

	msg := &pgswarmv1.CentralMessage{
		Payload: &pgswarmv1.CentralMessage_DeleteCluster{
			DeleteCluster: del,
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

// GetLogBuffer returns the LogBuffer (needed by REST API for log endpoints).
func (s *GRPCServer) GetLogBuffer() *LogBuffer {
	return s.logBuffer
}

// SetWSHub sets the WebSocket hub for broadcasting switchover progress.
func (s *GRPCServer) SetWSHub(hub *WSHub) {
	s.wsHub = hub
}

// SetOpsTracker sets the operations tracker for switchover progress tracking.
func (s *GRPCServer) SetOpsTracker(ot *OpsTracker) {
	s.opsTracker = ot
}

// Auth interceptors

// unaryAuthInterceptor enforces authentication on all unary RPCs except
// Register and CheckApproval, which are part of the unauthenticated
// registration handshake.
func (s *GRPCServer) unaryAuthInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	// Registration RPCs don't need auth
	if info.FullMethod == pgswarmv1.RegistrationService_Register_FullMethodName ||
		info.FullMethod == pgswarmv1.RegistrationService_CheckApproval_FullMethodName {
		return handler(ctx, req)
	}

	// All other unary RPCs require authentication.
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Errorf(codes.Unauthenticated, "missing metadata")
	}
	tokens := md.Get("authorization")
	if len(tokens) == 0 {
		return nil, status.Errorf(codes.Unauthenticated, "missing authorization token")
	}
	tokenHash := auth.HashToken(tokens[0])
	sat, err := s.store.GetSatelliteByToken(ctx, tokenHash)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "invalid token")
	}
	ctx = contextWithSatelliteID(ctx, sat.ID)
	return handler(ctx, req)
}

// streamAuthInterceptor validates the authorization token in stream metadata
// and injects the authenticated satellite ID into the stream context.
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

// Logging interceptors

// unaryLoggingInterceptor logs every unary gRPC call with method, duration,
// status code, and request ID.
func (s *GRPCServer) unaryLoggingInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	// Extract or generate request ID from metadata.
	md, _ := metadata.FromIncomingContext(ctx)
	rid := ""
	if vals := md.Get("x-request-id"); len(vals) > 0 {
		rid = vals[0]
	}
	if rid == "" {
		rid = reqid.NewID()
	}
	ctx = reqid.WithRequestID(ctx, rid)

	start := time.Now()
	resp, err := handler(ctx, req)
	duration := time.Since(start)

	code := status.Code(err)
	level := grpcLogLevel(code)

	log.WithLevel(level).
		Str("request_id", rid).
		Str("grpc_method", info.FullMethod).
		Str("grpc_code", code.String()).
		Dur("duration", duration).
		Msg("grpc unary")

	return resp, err
}

// streamLoggingInterceptor logs gRPC stream lifecycle (open/close) with
// satellite ID, method, duration, and request ID.
func (s *GRPCServer) streamLoggingInterceptor(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx := ss.Context()
	md, _ := metadata.FromIncomingContext(ctx)
	rid := ""
	if vals := md.Get("x-request-id"); len(vals) > 0 {
		rid = vals[0]
	}
	if rid == "" {
		rid = reqid.NewID()
	}
	newCtx := reqid.WithRequestID(ctx, rid)
	wrapped := &wrappedServerStream{ServerStream: ss, ctx: newCtx}

	log.Info().
		Str("request_id", rid).
		Str("grpc_method", info.FullMethod).
		Msg("grpc stream opened")

	start := time.Now()
	err := handler(srv, wrapped)
	duration := time.Since(start)

	level := zerolog.InfoLevel
	if err != nil {
		level = zerolog.WarnLevel
	}
	log.WithLevel(level).
		Str("request_id", rid).
		Str("grpc_method", info.FullMethod).
		Dur("duration", duration).
		Err(err).
		Msg("grpc stream closed")

	return err
}

// grpcLogLevel returns the appropriate log level for a gRPC status code.
func grpcLogLevel(code codes.Code) zerolog.Level {
	switch code {
	case codes.OK:
		return zerolog.DebugLevel
	case codes.NotFound, codes.InvalidArgument, codes.AlreadyExists:
		return zerolog.WarnLevel
	case codes.Unauthenticated, codes.PermissionDenied:
		return zerolog.WarnLevel
	default:
		return zerolog.ErrorLevel
	}
}

// Context helpers for satellite ID

type satelliteIDKey struct{}

// contextWithSatelliteID returns a new context carrying the given satellite UUID.
func contextWithSatelliteID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, satelliteIDKey{}, id)
}

// satelliteIDFromContext extracts the satellite UUID previously stored by
// contextWithSatelliteID. The boolean is false if no ID is present.
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
	case pgswarmv1.ClusterState_CLUSTER_STATE_PAUSED:
		return models.ClusterStatePaused
	case pgswarmv1.ClusterState_CLUSTER_STATE_DELETING:
		return models.ClusterStateDeleting
	default:
		return models.ClusterStateCreating
	}
}

// instanceJSON mirrors InstanceHealth for JSON serialization into the store.
type instanceJSON struct {
	PodName               string             `json:"pod_name"`
	Role                  string             `json:"role"`
	Ready                 bool               `json:"ready"`
	ReplicationLagBytes   int64              `json:"replication_lag_bytes"`
	ReplicationLagSeconds float64            `json:"replication_lag_seconds,omitempty"`
	ConnectionsUsed       int32              `json:"connections_used,omitempty"`
	ConnectionsMax        int32              `json:"connections_max,omitempty"`
	DiskUsedBytes         int64              `json:"disk_used_bytes,omitempty"`
	TimelineID            int64              `json:"timeline_id,omitempty"`
	PgStartTime           string             `json:"pg_start_time,omitempty"`
	WalReceiverActive     bool               `json:"wal_receiver_active,omitempty"`
	ErrorMessage          string             `json:"error_message,omitempty"`
	WalRecords            int64              `json:"wal_records,omitempty"`
	WalBytes              int64              `json:"wal_bytes,omitempty"`
	WalBuffersFull        int64              `json:"wal_buffers_full,omitempty"`
	WalDiskBytes          int64              `json:"wal_disk_bytes,omitempty"`
	TableStats            []tableStatJSON    `json:"table_stats,omitempty"`
	DatabaseStats         []databaseStatJSON `json:"database_stats,omitempty"`
	SlowQueries           []slowQueryJSON    `json:"slow_queries,omitempty"`
}

type databaseStatJSON struct {
	DatabaseName  string  `json:"database_name"`
	SizeBytes     int64   `json:"size_bytes"`
	CacheHitRatio float64 `json:"cache_hit_ratio,omitempty"`
}

type slowQueryJSON struct {
	Query           string  `json:"query"`
	DatabaseName    string  `json:"database_name"`
	Calls           int64   `json:"calls"`
	TotalExecTimeMs float64 `json:"total_exec_time_ms"`
	MeanExecTimeMs  float64 `json:"mean_exec_time_ms"`
	MaxExecTimeMs   float64 `json:"max_exec_time_ms"`
	Rows            int64   `json:"rows"`
}

type tableStatJSON struct {
	SchemaName     string `json:"schema_name"`
	TableName      string `json:"table_name"`
	LiveTuples     int64  `json:"live_tuples"`
	DeadTuples     int64  `json:"dead_tuples"`
	SeqScan        int64  `json:"seq_scan"`
	IdxScan        int64  `json:"idx_scan"`
	NTupIns        int64  `json:"n_tup_ins"`
	NTupUpd        int64  `json:"n_tup_upd"`
	NTupDel        int64  `json:"n_tup_del"`
	LastVacuum     string `json:"last_vacuum,omitempty"`
	LastAutovacuum string `json:"last_autovacuum,omitempty"`
	TableSizeBytes int64  `json:"table_size_bytes"`
	DatabaseName   string `json:"database_name,omitempty"`
}

// protoRoleToString converts a protobuf InstanceRole enum value to a
// human-readable lowercase string (e.g. "primary", "replica").
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
			WalRecords:            inst.WalRecords,
			WalBytes:              inst.WalBytes,
			WalBuffersFull:        inst.WalBuffersFull,
			WalDiskBytes:          inst.WalDiskBytes,
		}
		if inst.PgStartTime != nil {
			ij.PgStartTime = inst.PgStartTime.AsTime().Format(time.RFC3339)
		}
		for _, ds := range inst.DatabaseStats {
			ij.DatabaseStats = append(ij.DatabaseStats, databaseStatJSON{
				DatabaseName:  ds.DatabaseName,
				SizeBytes:     ds.SizeBytes,
				CacheHitRatio: ds.CacheHitRatio,
			})
		}
		for _, sq := range inst.SlowQueries {
			ij.SlowQueries = append(ij.SlowQueries, slowQueryJSON{
				Query:           sq.Query,
				DatabaseName:    sq.DatabaseName,
				Calls:           sq.Calls,
				TotalExecTimeMs: sq.TotalExecTimeMs,
				MeanExecTimeMs:  sq.MeanExecTimeMs,
				MaxExecTimeMs:   sq.MaxExecTimeMs,
				Rows:            sq.Rows,
			})
		}
		for _, ts := range inst.TableStats {
			ij.TableStats = append(ij.TableStats, tableStatJSON{
				SchemaName:     ts.SchemaName,
				TableName:      ts.TableName,
				LiveTuples:     ts.LiveTuples,
				DeadTuples:     ts.DeadTuples,
				SeqScan:        ts.SeqScan,
				IdxScan:        ts.IdxScan,
				NTupIns:        ts.NTupIns,
				NTupUpd:        ts.NTupUpd,
				NTupDel:        ts.NTupDel,
				LastVacuum:     ts.LastVacuum,
				LastAutovacuum: ts.LastAutovacuum,
				TableSizeBytes: ts.TableSizeBytes,
				DatabaseName:   ts.DatabaseName,
			})
		}
		out = append(out, ij)
	}
	return json.Marshal(out)
}

// handleDatabaseStatusReport processes a database creation status report from a satellite.
func (s *GRPCServer) handleDatabaseStatusReport(ctx context.Context, satID uuid.UUID, report *pgswarmv1.DatabaseStatusReport) {
	log.Info().
		Str("satellite_id", satID.String()).
		Str("cluster", report.ClusterName).
		Str("db", report.DbName).
		Str("status", report.Status).
		Msg("database status report received")

	// Find the cluster config to get the cluster ID
	clusters, err := s.store.GetClusterConfigsBySatellite(ctx, satID)
	if err != nil {
		log.Error().Err(err).Msg("failed to get cluster configs for database status")
		return
	}

	for _, cfg := range clusters {
		if cfg.Name != report.ClusterName {
			continue
		}
		// Find the database record by name
		db, err := s.store.GetClusterDatabaseByName(ctx, cfg.ID, report.DbName)
		if err != nil {
			log.Warn().Err(err).Str("db", report.DbName).Msg("database record not found for status update")
			return
		}
		// Update status
		if err := s.store.UpdateClusterDatabaseStatus(ctx, db.ID, report.Status, report.ErrorMessage); err != nil {
			log.Error().Err(err).Str("db", report.DbName).Msg("failed to update database status")
			return
		}

		// Notify dashboard via WebSocket
		if s.wsHub != nil {
			s.wsHub.Notify()
		}
		return
	}

	log.Warn().Str("cluster", report.ClusterName).Msg("cluster not found for database status report")
}
