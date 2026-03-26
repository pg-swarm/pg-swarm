package sidecar

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
)

// TokenValidator validates a sidecar stream token. The satellite implements
// this by checking against the cluster secrets it manages.
type TokenValidator func(token string) bool

// Server is the gRPC server that sidecars connect to.
type Server struct {
	pgswarmv1.UnimplementedSidecarStreamServiceServer

	manager        *SidecarStreamManager
	validateToken  TokenValidator
	server         *grpc.Server
}

// NewServer creates a sidecar gRPC server.
func NewServer(manager *SidecarStreamManager, validate TokenValidator) *Server {
	s := &Server{
		manager:       manager,
		validateToken: validate,
	}

	s.server = grpc.NewServer(
		grpc.ChainStreamInterceptor(s.streamLoggingInterceptor, s.streamAuthInterceptor),
	)

	pgswarmv1.RegisterSidecarStreamServiceServer(s.server, s)
	return s
}

// Start listens on the given address and serves gRPC requests.
func (s *Server) Start(addr string) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	log.Info().Str("addr", addr).Msg("sidecar gRPC server starting")
	return s.server.Serve(lis)
}

// Stop gracefully shuts down the gRPC server.
func (s *Server) Stop() {
	s.server.GracefulStop()
}

// Connect handles an incoming sidecar bidi stream.
func (s *Server) Connect(stream grpc.BidiStreamingServer[pgswarmv1.SidecarMessage, pgswarmv1.SidecarCommand]) error {
	// First message must be SidecarIdentity
	msg, err := stream.Recv()
	if err != nil {
		return err
	}

	identity, ok := msg.Payload.(*pgswarmv1.SidecarMessage_Identity)
	if !ok {
		return status.Error(codes.InvalidArgument, "first message must be SidecarIdentity")
	}

	id := identity.Identity
	log.Info().
		Str("pod", id.PodName).
		Str("cluster", id.ClusterName).
		Str("namespace", id.Namespace).
		Msg("sidecar connected")

	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	sidecarStream := NewSidecarStream(id.PodName, id.ClusterName, id.Namespace, cancel)

	s.manager.Add(id.Namespace, id.PodName, sidecarStream)
	defer s.manager.Remove(id.Namespace, id.PodName)

	// Read loop (goroutine)
	errCh := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				errCh <- err
				return
			}

			switch payload := msg.Payload.(type) {
			case *pgswarmv1.SidecarMessage_Heartbeat:
				log.Trace().Str("pod", id.PodName).Msg("sidecar heartbeat received")
				// Send heartbeat ack (non-blocking)
				ack := &pgswarmv1.SidecarCommand{
					Cmd: &pgswarmv1.SidecarCommand_HeartbeatAck{
						HeartbeatAck: &pgswarmv1.SidecarHeartbeatAck{},
					},
				}
				select {
				case sidecarStream.SendCh <- ack:
				default:
				}
			case *pgswarmv1.SidecarMessage_CommandResult:
				sidecarStream.deliverResult(payload.CommandResult)
			}
		}
	}()

	// Write loop
	for {
		select {
		case cmd := <-sidecarStream.SendCh:
			if err := stream.Send(cmd); err != nil {
				return err
			}
		case err := <-errCh:
			log.Info().Str("pod", id.PodName).Msg("sidecar disconnected")
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// streamAuthInterceptor validates the authorization token in stream metadata.
func (s *Server) streamAuthInterceptor(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	md, ok := metadata.FromIncomingContext(ss.Context())
	if !ok {
		return status.Error(codes.Unauthenticated, "missing metadata")
	}

	tokens := md.Get("authorization")
	if len(tokens) == 0 {
		return status.Error(codes.Unauthenticated, "missing authorization token")
	}

	if s.validateToken != nil && !s.validateToken(tokens[0]) {
		return status.Error(codes.Unauthenticated, "invalid token")
	}

	return handler(srv, ss)
}

// streamLoggingInterceptor logs sidecar stream lifecycle (open/close).
func (s *Server) streamLoggingInterceptor(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	log.Info().
		Str("grpc_method", info.FullMethod).
		Msg("sidecar stream opened")

	start := time.Now()
	err := handler(srv, ss)
	duration := time.Since(start)

	level := zerolog.InfoLevel
	if err != nil {
		level = zerolog.WarnLevel
	}
	log.WithLevel(level).
		Str("grpc_method", info.FullMethod).
		Dur("duration", duration).
		Err(err).
		Msg("sidecar stream closed")

	return err
}

// Manager returns the stream manager (for wiring into other components).
func (s *Server) Manager() *SidecarStreamManager {
	return s.manager
}
