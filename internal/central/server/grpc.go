package server

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"

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
				log.Debug().Str("satellite_id", satID.String()).Msg("health report received (deferred to Phase 4)")

			case *pgswarmv1.SatelliteMessage_EventReport:
				log.Debug().Str("satellite_id", satID.String()).Msg("event report received (deferred to Phase 4)")
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
