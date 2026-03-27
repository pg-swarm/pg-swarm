package sentinel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/pg-swarm/pg-swarm/internal/shared/pgfence"
)

// SidecarConnector connects the sentinel sidecar to the satellite's gRPC server,
// receives commands, executes them against localhost PostgreSQL, and sends results.
type SidecarConnector struct {
	satelliteAddr string
	authToken     string
	identity      *pgswarmv1.SidecarIdentity
	connString    string // localhost PG connection string
	sendCh        chan *pgswarmv1.SidecarMessage

	// K8s exec capability for restart/rewind/rebuild commands
	k8sClient  kubernetes.Interface
	restConfig *rest.Config
}

// NewSidecarConnector creates a new connector for the sidecar-to-satellite stream.
func NewSidecarConnector(satelliteAddr, authToken string, identity *pgswarmv1.SidecarIdentity, connString string, k8sClient kubernetes.Interface, restConfig *rest.Config) *SidecarConnector {
	return &SidecarConnector{
		satelliteAddr: satelliteAddr,
		authToken:     authToken,
		identity:      identity,
		connString:    connString,
		sendCh:        make(chan *pgswarmv1.SidecarMessage, 16),
		k8sClient:     k8sClient,
		restConfig:    restConfig,
	}
}

// EmitEvent sends a detection event to the satellite via the gRPC stream.
// Non-blocking: drops the event if the send channel is full.
func (c *SidecarConnector) EmitEvent(evt *pgswarmv1.Event) {
	select {
	case c.sendCh <- &pgswarmv1.SidecarMessage{
		Payload: &pgswarmv1.SidecarMessage_Event{Event: evt},
	}:
	default:
		log.Warn().Str("type", evt.GetType()).Msg("logwatcher: send channel full, event dropped")
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
	case *pgswarmv1.SidecarCommand_CreateDatabase:
		result = c.handleCreateDatabase(ctx, cmd)
	case *pgswarmv1.SidecarCommand_ReloadConf:
		result = c.handleReloadConf(ctx, cmd)
	case *pgswarmv1.SidecarCommand_Event:
		c.handleEventCommand(ctx, cmd)
		return // event handler sends its own response
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

	promoteAmbiguous := false
	if _, err := conn.Exec(ctx, "SELECT pg_promote()"); err != nil {
		// 57P01 = admin_shutdown: PG terminated the connection during promotion.
		// The promote signal was likely delivered; fall through to poll loop.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "57P01" {
			promoteAmbiguous = true
			log.Warn().Err(err).Msg("pg_promote() connection terminated during promotion — polling for recovery exit")
		} else {
			result.Success = false
			result.Error = fmt.Sprintf("pg_promote: %v", err)
			return result
		}
	}

	// Close the original connection — PG resets connections during role change.
	conn.Close(ctx)

	// Poll until promotion completes or timeout, reconnecting each tick.
	promoteCmd := cmd.GetPromote()
	waitTimeout := time.Duration(promoteCmd.WaitTimeoutSeconds) * time.Second
	if waitTimeout <= 0 {
		waitTimeout = 60 * time.Second
	}

	deadline := time.After(waitTimeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			result.Success = false
			if promoteAmbiguous {
				result.Error = "pg_promote() connection was terminated (57P01) and target did not exit recovery within timeout — promotion may not have been delivered"
			} else {
				result.Error = "pg_promote() called but target did not exit recovery within timeout"
			}
			return result
		case <-ctx.Done():
			result.Success = false
			result.Error = ctx.Err().Error()
			return result
		case <-ticker.C:
			pollConn, err := pgx.Connect(ctx, c.connString)
			if err != nil {
				continue // PG may still be restarting
			}
			var stillRecovery bool
			err = pollConn.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&stillRecovery)
			pollConn.Close(ctx)
			if err == nil && !stillRecovery {
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

func (c *SidecarConnector) handleCreateDatabase(ctx context.Context, cmd *pgswarmv1.SidecarCommand) *pgswarmv1.CommandResult {
	result := &pgswarmv1.CommandResult{RequestId: cmd.RequestId, Success: true}

	createCmd := cmd.GetCreateDatabase()
	if createCmd == nil {
		result.Success = false
		result.Error = "missing create_database payload"
		return result
	}

	conn, err := pgx.Connect(ctx, c.connString)
	if err != nil {
		result.Success = false
		result.Error = fmt.Sprintf("connect: %v", err)
		return result
	}
	defer conn.Close(ctx)

	// Create role if not exists (idempotent)
	_, err = conn.Exec(ctx, fmt.Sprintf(
		`DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='%s') THEN CREATE ROLE %s WITH LOGIN PASSWORD '%s'; END IF; END $$;`,
		createCmd.DbUser, createCmd.DbUser, createCmd.Password,
	))
	if err != nil {
		result.Success = false
		result.Error = fmt.Sprintf("create role %s: %v", createCmd.DbUser, err)
		return result
	}

	// Create database if not exists (idempotent)
	var exists bool
	conn.QueryRow(ctx, `SELECT EXISTS(SELECT FROM pg_database WHERE datname = $1)`, createCmd.DbName).Scan(&exists)
	if !exists {
		// CREATE DATABASE cannot run inside a transaction, use a separate connection
		_, err = conn.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s OWNER %s`, createCmd.DbName, createCmd.DbUser))
		if err != nil {
			result.Success = false
			result.Error = fmt.Sprintf("create database %s: %v", createCmd.DbName, err)
			return result
		}
	}

	log.Info().Str("db", createCmd.DbName).Str("user", createCmd.DbUser).Msg("sidecar: database created")
	return result
}

func (c *SidecarConnector) handleReloadConf(ctx context.Context, cmd *pgswarmv1.SidecarCommand) *pgswarmv1.CommandResult {
	result := &pgswarmv1.CommandResult{RequestId: cmd.RequestId, Success: true}

	conn, err := pgx.Connect(ctx, c.connString)
	if err != nil {
		result.Success = false
		result.Error = fmt.Sprintf("connect: %v", err)
		return result
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, "SELECT pg_reload_conf()"); err != nil {
		result.Success = false
		result.Error = fmt.Sprintf("pg_reload_conf: %v", err)
	}
	log.Info().Msg("sidecar: pg_reload_conf() executed")
	return result
}

func (c *SidecarConnector) handleRestart(ctx context.Context, cmd *pgswarmv1.SidecarCommand) *pgswarmv1.CommandResult {
	result := &pgswarmv1.CommandResult{RequestId: cmd.RequestId, Success: true}

	if err := execInPod(ctx, c.k8sClient, c.restConfig, c.identity.PodName, c.identity.Namespace,
		"pg_ctl stop -m fast -D /var/lib/postgresql/data/pgdata"); err != nil {
		result.Success = false
		result.Error = fmt.Sprintf("restart: %v", err)
	}
	log.Info().Msg("sidecar: pg_ctl stop executed (wrapper will restart)")
	return result
}

func (c *SidecarConnector) handleRewind(ctx context.Context, cmd *pgswarmv1.SidecarCommand) *pgswarmv1.CommandResult {
	result := &pgswarmv1.CommandResult{RequestId: cmd.RequestId, Success: true}

	rwSvc := fmt.Sprintf("%s-rw", c.identity.ClusterName)
	primaryHost := fmt.Sprintf("%s.%s.svc.cluster.local", rwSvc, c.identity.Namespace)

	script := fmt.Sprintf(`set -e
PGDATA="/var/lib/postgresql/data/pgdata"
touch "$PGDATA/standby.signal"
sed -i '/^primary_conninfo/d' "$PGDATA/postgresql.auto.conf" 2>/dev/null || true
sed -i '/^default_transaction_read_only/d' "$PGDATA/postgresql.auto.conf" 2>/dev/null || true
echo "primary_conninfo = 'host=%s port=5432 user=repl_user password=$REPLICATION_PASSWORD application_name=%s'" >> "$PGDATA/postgresql.auto.conf"
if [ -f "$PGDATA/backup_label" ]; then rm -f "$PGDATA/backup_label"; fi
if [ -f "$PGDATA/tablespace_map" ]; then rm -f "$PGDATA/tablespace_map"; fi
pg_ctl -D "$PGDATA" stop -m fast`, primaryHost, c.identity.PodName)

	if err := execInPod(ctx, c.k8sClient, c.restConfig, c.identity.PodName, c.identity.Namespace, script); err != nil {
		result.Success = false
		result.Error = fmt.Sprintf("rewind: %v", err)
	}
	log.Info().Msg("sidecar: standby conversion executed (wrapper will handle pg_rewind)")
	return result
}

func (c *SidecarConnector) handleRebuild(ctx context.Context, cmd *pgswarmv1.SidecarCommand) *pgswarmv1.CommandResult {
	result := &pgswarmv1.CommandResult{RequestId: cmd.RequestId, Success: true}

	ruleName := "unknown"
	if evt := cmd.GetEvent(); evt != nil {
		if rn, ok := evt.Data["rule_name"]; ok {
			ruleName = rn
		}
	}

	script := fmt.Sprintf(
		"echo 'rule:%s' > /var/lib/postgresql/data/.pg-swarm-needs-basebackup && pg_ctl stop -m fast -D /var/lib/postgresql/data/pgdata",
		ruleName,
	)
	if err := execInPod(ctx, c.k8sClient, c.restConfig, c.identity.PodName, c.identity.Namespace, script); err != nil {
		result.Success = false
		result.Error = fmt.Sprintf("rebuild: %v", err)
	}
	log.Info().Msg("sidecar: rebuild marker written + pg_ctl stop (wrapper will rebuild)")
	return result
}

// handleEventCommand processes an event-based command from the satellite.
// It dispatches based on event type, executes the corresponding action, and
// sends back an event-based result (command.*.completed) via SidecarMessage_Event.
func (c *SidecarConnector) handleEventCommand(ctx context.Context, cmd *pgswarmv1.SidecarCommand) {
	evt := cmd.GetEvent()
	if evt == nil {
		return
	}

	eventType := evt.GetType()
	operationID := evt.GetOperationId()

	log.Info().
		Str("command", eventType).
		Str("pod", evt.GetPodName()).
		Str("operation_id", operationID).
		Msg("sidecar: processing event command")

	// Dispatch to existing handlers by building legacy commands
	var result *pgswarmv1.CommandResult
	switch eventType {
	case "command.fence":
		legacyCmd := &pgswarmv1.SidecarCommand{RequestId: operationID}
		legacyCmd.Cmd = &pgswarmv1.SidecarCommand_Fence{Fence: &pgswarmv1.FenceCmd{}}
		result = c.handleFence(ctx, legacyCmd)
	case "command.unfence":
		legacyCmd := &pgswarmv1.SidecarCommand{RequestId: operationID}
		legacyCmd.Cmd = &pgswarmv1.SidecarCommand_Unfence{Unfence: &pgswarmv1.UnfenceCmd{}}
		result = c.handleUnfence(ctx, legacyCmd)
	case "command.checkpoint":
		legacyCmd := &pgswarmv1.SidecarCommand{RequestId: operationID}
		legacyCmd.Cmd = &pgswarmv1.SidecarCommand_Checkpoint{Checkpoint: &pgswarmv1.CheckpointCmd{}}
		result = c.handleCheckpoint(ctx, legacyCmd)
	case "command.promote":
		legacyCmd := &pgswarmv1.SidecarCommand{RequestId: operationID}
		legacyCmd.Cmd = &pgswarmv1.SidecarCommand_Promote{Promote: &pgswarmv1.PromoteCmd{}}
		result = c.handlePromote(ctx, legacyCmd)
	case "command.status":
		legacyCmd := &pgswarmv1.SidecarCommand{RequestId: operationID}
		legacyCmd.Cmd = &pgswarmv1.SidecarCommand_Status{Status: &pgswarmv1.StatusCmd{}}
		result = c.handleStatus(ctx, legacyCmd)
	case "command.create_database":
		legacyCmd := &pgswarmv1.SidecarCommand{RequestId: operationID}
		legacyCmd.Cmd = &pgswarmv1.SidecarCommand_CreateDatabase{CreateDatabase: &pgswarmv1.CreateDatabaseCmd{
			DbName:   evt.Data["db_name"],
			DbUser:   evt.Data["db_user"],
			Password: evt.Data["password"],
		}}
		result = c.handleCreateDatabase(ctx, legacyCmd)
	case "command.reload_conf":
		legacyCmd := &pgswarmv1.SidecarCommand{RequestId: operationID}
		legacyCmd.Cmd = &pgswarmv1.SidecarCommand_ReloadConf{ReloadConf: &pgswarmv1.ReloadConfCmd{}}
		result = c.handleReloadConf(ctx, legacyCmd)
	case "command.restart":
		legacyCmd := &pgswarmv1.SidecarCommand{RequestId: operationID}
		result = c.handleRestart(ctx, legacyCmd)
	case "command.rewind":
		legacyCmd := &pgswarmv1.SidecarCommand{RequestId: operationID}
		result = c.handleRewind(ctx, legacyCmd)
	case "command.rebuild":
		legacyCmd := &pgswarmv1.SidecarCommand{RequestId: operationID, Cmd: cmd.Cmd}
		result = c.handleRebuild(ctx, legacyCmd)
	default:
		log.Warn().Str("command", eventType).Msg("sidecar: unknown event command type")
		result = &pgswarmv1.CommandResult{
			RequestId: operationID,
			Error:     fmt.Sprintf("unknown event command: %s", eventType),
		}
	}

	// Send result back as an event
	resultEvt := &pgswarmv1.Event{
		Id:          fmt.Sprintf("%s-result", operationID),
		Type:        eventType + ".completed",
		ClusterName: evt.GetClusterName(),
		Namespace:   evt.GetNamespace(),
		PodName:     c.identity.PodName,
		Severity:    "info",
		Source:      "sidecar",
		Timestamp:   timestamppb.Now(),
		OperationId: operationID,
		Data: map[string]string{
			"success": fmt.Sprintf("%t", result.Success),
		},
	}
	if result.Error != "" {
		resultEvt.Data["error"] = result.Error
		resultEvt.Severity = "error"
	}
	if result.InRecovery {
		resultEvt.Data["in_recovery"] = "true"
	}
	if result.IsFenced {
		resultEvt.Data["is_fenced"] = "true"
	}

	log.Info().
		Str("command", eventType).
		Bool("success", result.Success).
		Str("operation_id", operationID).
		Msg("sidecar: event command completed")

	c.sendCh <- &pgswarmv1.SidecarMessage{
		Payload: &pgswarmv1.SidecarMessage_Event{Event: resultEvt},
	}
}
