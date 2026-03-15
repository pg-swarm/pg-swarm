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
	centralAddr     string
	authToken       string
	sendCh          chan *pgswarmv1.SatelliteMessage
	OnConfig              func(*pgswarmv1.ClusterConfig) error
	OnDelete              func(*pgswarmv1.DeleteCluster) error
	OnStorageClassRequest func() *pgswarmv1.StorageClassReport
	OnSwitchover          func(*pgswarmv1.SwitchoverRequest) *pgswarmv1.SwitchoverResult
	OnSetLogLevel         func(string)
	OnRestoreCommand      func(*pgswarmv1.RestoreCommand)
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
// No logging here — the log capture hook calls this method, so any log
// statement would create an infinite feedback loop.
func (c *Connector) SendMessage(msg *pgswarmv1.SatelliteMessage) {
	select {
	case c.sendCh <- msg:
	default:
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
		// Reset backoff after a connection was established (and then broke).
		backoff = time.Second
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
	log.Trace().Str("addr", c.centralAddr).Msg("connect attempt")
	conn, err := grpc.NewClient(c.centralAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	log.Trace().Msg("gRPC dial succeeded")

	client := pgswarmv1.NewSatelliteStreamServiceClient(conn)

	md := metadata.New(map[string]string{"authorization": c.authToken})
	streamCtx := metadata.NewOutgoingContext(ctx, md)

	stream, err := client.Connect(streamCtx)
	if err != nil {
		return err
	}
	log.Trace().Msg("bidi stream opened")

	// Reset backoff on successful connection
	log.Info().Msg("connected to central stream")

	// Combined write loop (serializes all writes — gRPC streams are not concurrent-send-safe)
	go c.writeLoop(ctx, stream)

	// Send storage classes on connect
	c.sendStorageClasses()

	// Read loop
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}

		log.Trace().Msg("received message from central")
		switch payload := msg.Payload.(type) {
		case *pgswarmv1.CentralMessage_ClusterConfig:
			log.Trace().Str("cluster", payload.ClusterConfig.ClusterName).Msg("received ClusterConfig message")
			log.Info().
				Str("cluster", payload.ClusterConfig.ClusterName).
				Int64("version", payload.ClusterConfig.ConfigVersion).
				Msg("received cluster config")
			c.handleConfig(payload.ClusterConfig)
		case *pgswarmv1.CentralMessage_DeleteCluster:
			log.Trace().Str("cluster", payload.DeleteCluster.ClusterName).Msg("received DeleteCluster message")
			log.Info().
				Str("cluster", payload.DeleteCluster.ClusterName).
				Msg("received delete cluster")
			c.handleDelete(payload.DeleteCluster)
		case *pgswarmv1.CentralMessage_HeartbeatAck:
			log.Trace().Msg("received HeartbeatAck message")
			log.Debug().Msg("heartbeat ack received")
		case *pgswarmv1.CentralMessage_RequestStorageClasses:
			log.Trace().Msg("received RequestStorageClasses message")
			log.Info().Msg("storage class refresh requested by central")
			c.sendStorageClasses()
		case *pgswarmv1.CentralMessage_Switchover:
			log.Trace().Str("cluster", payload.Switchover.ClusterName).Msg("received Switchover message")
			log.Info().
				Str("cluster", payload.Switchover.ClusterName).
				Str("target", payload.Switchover.TargetPod).
				Msg("switchover requested by central")
			c.handleSwitchover(payload.Switchover)
		case *pgswarmv1.CentralMessage_SetLogLevel:
			log.Info().Str("level", payload.SetLogLevel.Level).Msg("log level change requested by central")
			if c.OnSetLogLevel != nil {
				c.OnSetLogLevel(payload.SetLogLevel.Level)
			}
		case *pgswarmv1.CentralMessage_RestoreCommand:
			log.Info().
				Str("cluster", payload.RestoreCommand.ClusterName).
				Str("restore_id", payload.RestoreCommand.RestoreId).
				Str("type", payload.RestoreCommand.RestoreType).
				Msg("restore command received from central")
			if c.OnRestoreCommand != nil {
				c.OnRestoreCommand(payload.RestoreCommand)
			}
		}
	}
}

func (c *Connector) handleConfig(cfg *pgswarmv1.ClusterConfig) {
	log.Trace().Str("cluster", cfg.ClusterName).Msg("handleConfig entry")
	if c.OnConfig == nil {
		return
	}

	err := c.OnConfig(cfg)
	log.Trace().Str("cluster", cfg.ClusterName).Bool("success", err == nil).Msg("handleConfig callback returned")

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

func (c *Connector) sendStorageClasses() {
	log.Trace().Msg("sendStorageClasses entry")
	if c.OnStorageClassRequest == nil {
		return
	}
	report := c.OnStorageClassRequest()
	if report == nil {
		return
	}
	log.Trace().Int("count", len(report.StorageClasses)).Msg("sendStorageClasses sending report")
	c.SendMessage(&pgswarmv1.SatelliteMessage{
		Payload: &pgswarmv1.SatelliteMessage_StorageClassReport{
			StorageClassReport: report,
		},
	})
	log.Info().Int("count", len(report.StorageClasses)).Msg("sent storage class report to central")
}

func (c *Connector) handleSwitchover(req *pgswarmv1.SwitchoverRequest) {
	if c.OnSwitchover == nil {
		return
	}
	result := c.OnSwitchover(req)
	if result == nil {
		return
	}
	c.SendMessage(&pgswarmv1.SatelliteMessage{
		Payload: &pgswarmv1.SatelliteMessage_SwitchoverResult{
			SwitchoverResult: result,
		},
	})
	if result.Success {
		log.Info().Str("cluster", req.ClusterName).Str("target", req.TargetPod).Msg("switchover completed")
	} else {
		log.Error().Str("cluster", req.ClusterName).Str("error", result.ErrorMessage).Msg("switchover failed")
	}
}

func (c *Connector) handleDelete(del *pgswarmv1.DeleteCluster) {
	log.Trace().Str("cluster", del.ClusterName).Msg("handleDelete entry")
	if c.OnDelete == nil {
		return
	}

	if err := c.OnDelete(del); err != nil {
		log.Error().Err(err).
			Str("cluster", del.ClusterName).
			Msg("delete handling failed")
	} else {
		log.Trace().Str("cluster", del.ClusterName).Msg("handleDelete completed")
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
			log.Trace().Msg("sending heartbeat")
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
