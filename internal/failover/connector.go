package failover

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/pg-swarm/pg-swarm/internal/shared/pgfence"
)

// SidecarConnector connects the failover sidecar to the satellite's gRPC server,
// receives commands, executes them against localhost PostgreSQL, and sends results.
type SidecarConnector struct {
	satelliteAddr string
	authToken     string
	identity      *pgswarmv1.SidecarIdentity
	connString    string // localhost PG connection string
	sendCh        chan *pgswarmv1.SidecarMessage
}

// NewSidecarConnector creates a new connector for the sidecar-to-satellite stream.
func NewSidecarConnector(satelliteAddr, authToken string, identity *pgswarmv1.SidecarIdentity, connString string) *SidecarConnector {
	return &SidecarConnector{
		satelliteAddr: satelliteAddr,
		authToken:     authToken,
		identity:      identity,
		connString:    connString,
		sendCh:        make(chan *pgswarmv1.SidecarMessage, 16),
	}
}

// Run connects to the satellite and processes commands in a loop.
// On disconnection it reconnects with exponential backoff (max 30s).
func (c *SidecarConnector) Run(ctx context.Context) error {
	backoff := time.Second

	for {
		err := c.connect(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Warn().Err(err).Dur("backoff", backoff).Msg("sidecar stream disconnected, reconnecting...")

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

func (c *SidecarConnector) connect(ctx context.Context) error {
	log.Trace().Str("addr", c.satelliteAddr).Msg("sidecar connect attempt")
	conn, err := grpc.NewClient(c.satelliteAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()

	client := pgswarmv1.NewSidecarStreamServiceClient(conn)

	md := metadata.New(map[string]string{"authorization": c.authToken})
	streamCtx := metadata.NewOutgoingContext(ctx, md)

	stream, err := client.Connect(streamCtx)
	if err != nil {
		return err
	}

	// Send identity as first message
	if err := stream.Send(&pgswarmv1.SidecarMessage{
		Payload: &pgswarmv1.SidecarMessage_Identity{
			Identity: c.identity,
		},
	}); err != nil {
		return fmt.Errorf("send identity: %w", err)
	}

	log.Info().Str("pod", c.identity.PodName).Msg("connected to satellite sidecar stream")

	// Write loop (serializes sends)
	go c.writeLoop(ctx, stream)

	// Read loop — receive commands from satellite
	for {
		cmd, err := stream.Recv()
		if err != nil {
			return err
		}

		go c.handleCommand(ctx, cmd)
	}
}

func (c *SidecarConnector) handleCommand(ctx context.Context, cmd *pgswarmv1.SidecarCommand) {
	var result *pgswarmv1.CommandResult

	switch cmd.Cmd.(type) {
	case *pgswarmv1.SidecarCommand_Fence:
		result = c.handleFence(ctx, cmd)
	case *pgswarmv1.SidecarCommand_Checkpoint:
		result = c.handleCheckpoint(ctx, cmd)
	case *pgswarmv1.SidecarCommand_Promote:
		result = c.handlePromote(ctx, cmd)
	case *pgswarmv1.SidecarCommand_Unfence:
		result = c.handleUnfence(ctx, cmd)
	case *pgswarmv1.SidecarCommand_Status:
		result = c.handleStatus(ctx, cmd)
	case *pgswarmv1.SidecarCommand_HeartbeatAck:
		return // no response needed
	default:
		result = &pgswarmv1.CommandResult{
			RequestId: cmd.RequestId,
			Error:     "unknown command",
		}
	}

	if result != nil {
		c.sendCh <- &pgswarmv1.SidecarMessage{
			Payload: &pgswarmv1.SidecarMessage_CommandResult{
				CommandResult: result,
			},
		}
	}
}

func (c *SidecarConnector) handleFence(ctx context.Context, cmd *pgswarmv1.SidecarCommand) *pgswarmv1.CommandResult {
	result := &pgswarmv1.CommandResult{RequestId: cmd.RequestId, Success: true}

	conn, err := pgx.Connect(ctx, c.connString)
	if err != nil {
		result.Success = false
		result.Error = fmt.Sprintf("connect: %v", err)
		return result
	}
	defer conn.Close(ctx)

	fenceCmd := cmd.GetFence()
	drainTimeout := time.Duration(fenceCmd.DrainTimeoutSeconds) * time.Second

	if err := pgfence.FencePrimaryWithOpts(ctx, conn, pgfence.FenceOpts{
		DrainTimeout: drainTimeout,
	}); err != nil {
		result.Success = false
		result.Error = fmt.Sprintf("fence: %v", err)
	}
	return result
}

func (c *SidecarConnector) handleCheckpoint(ctx context.Context, cmd *pgswarmv1.SidecarCommand) *pgswarmv1.CommandResult {
	result := &pgswarmv1.CommandResult{RequestId: cmd.RequestId, Success: true}

	conn, err := pgx.Connect(ctx, c.connString)
	if err != nil {
		result.Success = false
		result.Error = fmt.Sprintf("connect: %v", err)
		return result
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, "CHECKPOINT"); err != nil {
		result.Success = false
		result.Error = fmt.Sprintf("checkpoint: %v", err)
	}
	return result
}

func (c *SidecarConnector) handlePromote(ctx context.Context, cmd *pgswarmv1.SidecarCommand) *pgswarmv1.CommandResult {
	result := &pgswarmv1.CommandResult{RequestId: cmd.RequestId, Success: true}

	conn, err := pgx.Connect(ctx, c.connString)
	if err != nil {
		result.Success = false
		result.Error = fmt.Sprintf("connect: %v", err)
		return result
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, "SELECT pg_promote()"); err != nil {
		result.Success = false
		result.Error = fmt.Sprintf("pg_promote: %v", err)
		return result
	}

	// Poll until promotion completes or timeout
	promoteCmd := cmd.GetPromote()
	waitTimeout := time.Duration(promoteCmd.WaitTimeoutSeconds) * time.Second
	if waitTimeout <= 0 {
		waitTimeout = 15 * time.Second
	}

	deadline := time.After(waitTimeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			result.Success = false
			result.Error = "pg_promote() called but target did not exit recovery within timeout"
			return result
		case <-ctx.Done():
			result.Success = false
			result.Error = ctx.Err().Error()
			return result
		case <-ticker.C:
			var stillRecovery bool
			if err := conn.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&stillRecovery); err == nil && !stillRecovery {
				return result // success — no longer in recovery
			}
		}
	}
}

func (c *SidecarConnector) handleUnfence(ctx context.Context, cmd *pgswarmv1.SidecarCommand) *pgswarmv1.CommandResult {
	result := &pgswarmv1.CommandResult{RequestId: cmd.RequestId, Success: true}

	conn, err := pgx.Connect(ctx, c.connString)
	if err != nil {
		result.Success = false
		result.Error = fmt.Sprintf("connect: %v", err)
		return result
	}
	defer conn.Close(ctx)

	if err := pgfence.UnfencePrimary(ctx, conn); err != nil {
		result.Success = false
		result.Error = fmt.Sprintf("unfence: %v", err)
	}
	return result
}

func (c *SidecarConnector) handleStatus(ctx context.Context, cmd *pgswarmv1.SidecarCommand) *pgswarmv1.CommandResult {
	result := &pgswarmv1.CommandResult{RequestId: cmd.RequestId, Success: true}

	conn, err := pgx.Connect(ctx, c.connString)
	if err != nil {
		result.Success = false
		result.Error = fmt.Sprintf("connect: %v", err)
		return result
	}
	defer conn.Close(ctx)

	var inRecovery bool
	if err := conn.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery); err != nil {
		result.Success = false
		result.Error = fmt.Sprintf("pg_is_in_recovery: %v", err)
		return result
	}
	result.InRecovery = inRecovery
	result.IsFenced = pgfence.IsFenced(ctx, conn)

	return result
}

func (c *SidecarConnector) writeLoop(ctx context.Context, stream pgswarmv1.SidecarStreamService_ConnectClient) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-c.sendCh:
			if err := stream.Send(msg); err != nil {
				log.Warn().Err(err).Msg("sidecar: failed to send message")
				return
			}
		case <-ticker.C:
			err := stream.Send(&pgswarmv1.SidecarMessage{
				Payload: &pgswarmv1.SidecarMessage_Heartbeat{
					Heartbeat: &pgswarmv1.SidecarHeartbeat{
						Timestamp: timestamppb.Now(),
					},
				},
			})
			if err != nil {
				log.Warn().Err(err).Msg("sidecar: failed to send heartbeat")
				return
			}
		}
	}
}
