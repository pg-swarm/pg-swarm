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
	OnEvent     func(context.Context, *pgswarmv1.Event) error
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

// ForwardEvent wraps an Event in a SatelliteMessage and enqueues it for
// sending to central. Used as the EventBus forward function.
func (c *Connector) ForwardEvent(evt *pgswarmv1.Event) {
	c.SendMessage(&pgswarmv1.SatelliteMessage{
		Payload: &pgswarmv1.SatelliteMessage_Event{Event: evt},
	})
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
		backoff = time.Second
		log.Warn().Err(err).Dur("backoff", backoff).Msg("stream disconnected, reconnecting...")

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

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

	client := pgswarmv1.NewSatelliteStreamServiceClient(conn)
	md := metadata.New(map[string]string{"authorization": c.authToken})
	streamCtx := metadata.NewOutgoingContext(ctx, md)

	stream, err := client.Connect(streamCtx)
	if err != nil {
		return err
	}

	log.Info().Msg("connected to central stream")

	// Write loop (serializes all writes — gRPC streams are not concurrent-send-safe)
	go c.writeLoop(ctx, stream)

	// Read loop — all messages from central are events or heartbeat acks
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}

		switch payload := msg.Payload.(type) {
		case *pgswarmv1.CentralMessage_Event:
			evt := payload.Event
			log.Info().
				Str("event_type", evt.GetType()).
				Str("cluster", evt.GetClusterName()).
				Str("source", evt.GetSource()).
				Msg("received event from central")
			if c.OnEvent != nil {
				if err := c.OnEvent(ctx, evt); err != nil {
					log.Warn().Err(err).
						Str("event_type", evt.GetType()).
						Str("cluster", evt.GetClusterName()).
						Msg("failed to process event from central")
				}
			}
		case *pgswarmv1.CentralMessage_HeartbeatAck:
			log.Debug().Msg("heartbeat ack received")
		}
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
