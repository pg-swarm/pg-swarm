package stream

import (
	"context"
	"time"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Connector maintains a persistent bidirectional gRPC stream to central,
// automatically reconnecting with exponential backoff on disconnection.
type Connector struct {
	centralAddr string
	authToken   string
	sendCh      chan *pgswarmv1.SatelliteMessage
	OnConfig    func(*pgswarmv1.ClusterConfig) error
	OnDelete    func(*pgswarmv1.DeleteCluster) error
}

// NewConnector creates a new stream Connector targeting the given central
// address and authenticating with the provided token.
func NewConnector(addr, token string) *Connector {
	return &Connector{
		centralAddr: addr,
		authToken:   token,
		sendCh:      make(chan *pgswarmv1.SatelliteMessage, 32),
	}
}

// SendMessage enqueues a message for sending to central (non-blocking).
func (c *Connector) SendMessage(msg *pgswarmv1.SatelliteMessage) {
	select {
	case c.sendCh <- msg:
	default:
		log.Warn().Msg("send channel full, dropping outbound message")
	}
}

// Run connects to central and processes messages in a loop. On disconnection
// it reconnects with exponential backoff (max 30s). It blocks until ctx is
// cancelled.
func (c *Connector) Run(ctx context.Context) error {
	backoff := time.Second

	for {
		err := c.connect(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Warn().Err(err).Dur("backoff", backoff).Msg("stream disconnected, reconnecting...")

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		// Exponential backoff, max 30s
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func (c *Connector) connect(ctx context.Context) error {
	conn, err := grpc.NewClient(c.centralAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()

	client := pgswarmv1.NewSatelliteStreamServiceClient(conn)

	md := metadata.New(map[string]string{"authorization": c.authToken})
	streamCtx := metadata.NewOutgoingContext(ctx, md)

	stream, err := client.Connect(streamCtx)
	if err != nil {
		return err
	}

	// Reset backoff on successful connection
	log.Info().Msg("connected to central stream")

	// Combined write loop (serializes all writes — gRPC streams are not concurrent-send-safe)
	go c.writeLoop(ctx, stream)

	// Read loop
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}

		switch payload := msg.Payload.(type) {
		case *pgswarmv1.CentralMessage_ClusterConfig:
			log.Info().
				Str("cluster", payload.ClusterConfig.ClusterName).
				Int64("version", payload.ClusterConfig.ConfigVersion).
				Msg("received cluster config")
			c.handleConfig(payload.ClusterConfig)
		case *pgswarmv1.CentralMessage_DeleteCluster:
			log.Info().
				Str("cluster", payload.DeleteCluster.ClusterName).
				Msg("received delete cluster")
			c.handleDelete(payload.DeleteCluster)
		case *pgswarmv1.CentralMessage_HeartbeatAck:
			log.Debug().Msg("heartbeat ack received")
		}
	}
}

func (c *Connector) handleConfig(cfg *pgswarmv1.ClusterConfig) {
	if c.OnConfig == nil {
		return
	}

	err := c.OnConfig(cfg)

	ack := &pgswarmv1.ConfigAck{
		ClusterName:   cfg.ClusterName,
		ConfigVersion: cfg.ConfigVersion,
		Success:       err == nil,
	}
	if err != nil {
		ack.ErrorMessage = err.Error()
		log.Error().Err(err).
			Str("cluster", cfg.ClusterName).
			Int64("version", cfg.ConfigVersion).
			Msg("config handling failed")
	}

	c.SendMessage(&pgswarmv1.SatelliteMessage{
		Payload: &pgswarmv1.SatelliteMessage_ConfigAck{
			ConfigAck: ack,
		},
	})
}

func (c *Connector) handleDelete(del *pgswarmv1.DeleteCluster) {
	if c.OnDelete == nil {
		return
	}

	if err := c.OnDelete(del); err != nil {
		log.Error().Err(err).
			Str("cluster", del.ClusterName).
			Msg("delete handling failed")
	}
}

func (c *Connector) writeLoop(ctx context.Context, stream pgswarmv1.SatelliteStreamService_ConnectClient) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-c.sendCh:
			if err := stream.Send(msg); err != nil {
				log.Warn().Err(err).Msg("failed to send message")
				return
			}
		case <-ticker.C:
			err := stream.Send(&pgswarmv1.SatelliteMessage{
				Payload: &pgswarmv1.SatelliteMessage_Heartbeat{
					Heartbeat: &pgswarmv1.Heartbeat{
						Timestamp: timestamppb.Now(),
					},
				},
			})
			if err != nil {
				log.Warn().Err(err).Msg("failed to send heartbeat")
				return
			}
		}
	}
}
