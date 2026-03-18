// Package backup implements the backup sidecar for PostgreSQL HA clusters.
// The sidecar detects its role (primary vs replica) and activates the
// appropriate responsibilities: WAL archiving + metadata on primary,
// base/incremental/logical backups on replica.
package backup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL driver registration
	_ "modernc.org/sqlite"             // SQLite driver registration

	"github.com/pg-swarm/pg-swarm/internal/backup/destination"
	"github.com/rs/zerolog/log"
)

// Role represents the PostgreSQL role of the local instance.
type Role int

const (
	RoleUnknown Role = iota
	RolePrimary
	RoleReplica
)

func (r Role) String() string {
	switch r {
	case RolePrimary:
		return "primary"
	case RoleReplica:
		return "replica"
	default:
		return "unknown"
	}
}

// Config holds the sidecar configuration, populated from environment variables.
type Config struct {
	SatelliteID   string
	ClusterName   string
	PodName       string
	Namespace     string
	DestType      string
	BaseSchedule  string
	IncrSchedule  string
	LogicSchedule string
	RetentionSets int
	RetentionDays int
	PGUser        string
	PGPassword    string
	PGHost        string
	PGPort        string
	ListenAddr    string
}

// ConfigFromEnv reads sidecar configuration from environment variables.
func ConfigFromEnv() Config {
	retSets := 3 // default
	retDays := 30
	if v := os.Getenv("RETENTION_SETS"); v != "" {
		fmt.Sscanf(v, "%d", &retSets)
	}
	if v := os.Getenv("RETENTION_DAYS"); v != "" {
		fmt.Sscanf(v, "%d", &retDays)
	}
	pgHost := os.Getenv("PGHOST")
	if pgHost == "" {
		pgHost = "localhost"
	}
	pgPort := os.Getenv("PGPORT")
	if pgPort == "" {
		pgPort = "5432"
	}
	listenAddr := os.Getenv("LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = ":8442"
	}
	return Config{
		SatelliteID:   os.Getenv("SATELLITE_ID"),
		ClusterName:   os.Getenv("CLUSTER_NAME"),
		PodName:       os.Getenv("POD_NAME"),
		Namespace:     os.Getenv("NAMESPACE"),
		DestType:      os.Getenv("DEST_TYPE"),
		BaseSchedule:  os.Getenv("BASE_SCHEDULE"),
		IncrSchedule:  os.Getenv("INCR_SCHEDULE"),
		LogicSchedule: os.Getenv("LOGICAL_SCHEDULE"),
		RetentionSets: retSets,
		RetentionDays: retDays,
		PGUser:        os.Getenv("PGUSER"),
		PGPassword:    os.Getenv("PGPASSWORD"),
		PGHost:        pgHost,
		PGPort:        pgPort,
		ListenAddr:    listenAddr,
	}
}

// Sidecar is the main backup sidecar process.
type Sidecar struct {
	cfg        Config
	dest       destination.Destination
	meta       *MetadataDB
	role       Role
	mu         sync.RWMutex
	api        *APIServer
	sched      *Scheduler
	ret        *RetentionWorker
	reporter   *Reporter
	notifier   *Notifier
	cancel     context.CancelFunc
	roleCtx    context.Context
	roleCancel context.CancelFunc
}

// New creates a new Sidecar.
func New(cfg Config) *Sidecar {
	return &Sidecar{
		cfg: cfg,
	}
}

// destPrefix returns the folder prefix for this satellite+cluster combination.
func (s *Sidecar) destPrefix() string {
	return fmt.Sprintf("%s-%s/", s.cfg.SatelliteID, s.cfg.ClusterName)
}

// Run starts the sidecar and blocks until ctx is cancelled.
func (s *Sidecar) Run(ctx context.Context) error {
	ctx, s.cancel = context.WithCancel(ctx)
	defer s.cancel()

	// 1. Initialize destination
	s.dest = destination.NewFromEnv(s.cfg.DestType)
	log.Info().Str("dest_type", s.cfg.DestType).Msg("destination initialized")

	// 2. Detect role
	role, err := s.detectRole(ctx)
	if err != nil {
		return fmt.Errorf("detect role: %w", err)
	}
	s.mu.Lock()
	s.role = role
	s.mu.Unlock()
	log.Info().Str("role", role.String()).Msg("role detected")

	// 3. Initialize based on role
	if err := s.activateRole(ctx, role); err != nil {
		return fmt.Errorf("activate role %s: %w", role, err)
	}

	// 4. Start role-change watcher
	go s.watchRoleChanges(ctx)

	// 5. Block until context cancelled
	<-ctx.Done()
	s.shutdown()
	return nil
}

// detectRole queries the local PostgreSQL to determine if it's primary or
// replica, retrying for up to 60s. Use this only at sidecar startup when PG
// may still be coming up. For periodic checks use checkRole instead.
func (s *Sidecar) detectRole(ctx context.Context) (Role, error) {
	var role Role
	var err error
	for i := 0; i < 30; i++ {
		role, err = s.checkRole(ctx)
		if err == nil {
			return role, nil
		}
		select {
		case <-ctx.Done():
			return RoleUnknown, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return RoleUnknown, fmt.Errorf("connect to pg after 30 retries: %w", err)
}

// checkRole makes a single connection attempt to determine the PG role.
// Returns an error if PG is not reachable — callers should skip the tick
// rather than retrying in a loop.
func (s *Sidecar) checkRole(ctx context.Context) (Role, error) {
	connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=postgres sslmode=disable",
		s.cfg.PGHost, s.cfg.PGPort, s.cfg.PGUser, s.cfg.PGPassword)

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		return RoleUnknown, fmt.Errorf("open: %w", err)
	}
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		return RoleUnknown, fmt.Errorf("ping: %w", err)
	}

	var inRecovery bool
	if err := db.QueryRowContext(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery); err != nil {
		return RoleUnknown, fmt.Errorf("pg_is_in_recovery: %w", err)
	}
	if inRecovery {
		return RoleReplica, nil
	}
	return RolePrimary, nil
}

// activateRole starts the responsibilities for the given role.
// Each activation gets its own child context, cancelled in deactivate().
func (s *Sidecar) activateRole(ctx context.Context, role Role) error {
	s.roleCtx, s.roleCancel = context.WithCancel(ctx)
	switch role {
	case RolePrimary:
		return s.activatePrimary(s.roleCtx)
	case RoleReplica:
		return s.activateReplica(s.roleCtx)
	default:
		return fmt.Errorf("cannot activate unknown role")
	}
}

func (s *Sidecar) activatePrimary(ctx context.Context) error {
	log.Info().Msg("activating primary responsibilities")

	// Download or create backups.db
	metaPath := "/tmp/backups.db"
	remoteMeta := s.destPrefix() + "backups.db"
	if err := downloadFile(ctx, s.dest, remoteMeta, metaPath); err != nil {
		log.Info().Msg("no existing backups.db — creating new")
	}
	meta, err := OpenMetadata(metaPath)
	if err != nil {
		return fmt.Errorf("open metadata: %w", err)
	}
	s.meta = meta

	// Ensure an active backup set exists
	activeID, err := meta.ActiveSetID()
	if err != nil {
		return fmt.Errorf("check active set: %w", err)
	}
	if activeID == "" {
		_, err = meta.CreateBackupSet("", "")
		if err != nil {
			return fmt.Errorf("create initial backup set: %w", err)
		}
		log.Info().Msg("created initial backup set")
	}

	// Start HTTP API (backup/complete, /healthz, legacy WAL push/fetch)
	s.api = NewAPIServer(s)
	go s.api.Start(s.cfg.ListenAddr)

	// Start file-based WAL staging watcher (replaces HTTP WAL push)
	go s.WatchWALStaging(ctx)

	// Start file-based WAL restore watcher (replaces HTTP WAL fetch)
	go s.WatchWALRestore(ctx)

	// Start retention worker
	s.ret = NewRetentionWorker(s, s.cfg.RetentionSets, s.cfg.RetentionDays)

	// Start reporter
	s.reporter = NewReporter(s.cfg.Namespace, s.cfg.ClusterName)

	return nil
}

func (s *Sidecar) activateReplica(ctx context.Context) error {
	log.Info().Msg("activating replica responsibilities")

	// Start HTTP API (/healthz only)
	s.api = NewAPIServer(s)
	go s.api.Start(s.cfg.ListenAddr)

	// Start file-based WAL restore watcher. Replicas need this because PG's
	// restore_command writes to /wal-restore/.request when it needs WAL segments
	// or timeline history files (e.g. after a failover, PG must fetch
	// 00000002.history to follow the new timeline). Without this watcher the
	// request times out, PG gets FATAL and crash-loops.
	go s.WatchWALRestore(ctx)

	// Start reporter
	s.reporter = NewReporter(s.cfg.Namespace, s.cfg.ClusterName)

	// Start notifier (to reach primary)
	s.notifier = NewNotifier(s.cfg.ClusterName, s.cfg.Namespace)

	// Start backup scheduler
	s.sched = NewScheduler(s)
	go s.sched.Run(ctx)

	return nil
}

// watchRoleChanges periodically checks pg_is_in_recovery() and switches
// responsibilities if the role changes (failover).
func (s *Sidecar) watchRoleChanges(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			newRole, err := s.checkRole(ctx)
			if err != nil {
				log.Warn().Err(err).Msg("role check failed")
				continue
			}
			s.mu.RLock()
			currentRole := s.role
			s.mu.RUnlock()

			if newRole != currentRole {
				log.Info().Str("from", currentRole.String()).Str("to", newRole.String()).Msg("role change detected")
				s.deactivate()
				s.mu.Lock()
				s.role = newRole
				s.mu.Unlock()
				if err := s.activateRole(ctx, newRole); err != nil {
					log.Error().Err(err).Str("role", newRole.String()).Msg("failed to activate new role")
				}
			}
		}
	}
}

// deactivate stops current role's responsibilities.
func (s *Sidecar) deactivate() {
	// Cancel the role-scoped context first so WAL watcher goroutines exit.
	if s.roleCancel != nil {
		s.roleCancel()
		s.roleCancel = nil
	}
	if s.sched != nil {
		s.sched.Stop()
		s.sched = nil
	}
	if s.api != nil {
		s.api.Stop()
		s.api = nil
	}
	if s.ret != nil {
		s.ret = nil
	}
	if s.meta != nil {
		remoteMeta := s.destPrefix() + "backups.db"
		if err := uploadFile(context.Background(), s.dest, s.meta.Path(), remoteMeta); err != nil {
			log.Warn().Err(err).Msg("failed to upload metadata before deactivation")
		}
		s.meta.Close()
		s.meta = nil
	}
}

func (s *Sidecar) shutdown() {
	s.deactivate()
	log.Info().Msg("sidecar shutdown complete")
}

// CurrentRole returns the sidecar's current detected role.
func (s *Sidecar) CurrentRole() Role {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.role
}

// downloadFile downloads a remote file to a local path via the destination.
func downloadFile(ctx context.Context, dest destination.Destination, remotePath, localPath string) error {
	f, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return dest.Download(ctx, remotePath, f)
}

// uploadFile uploads a local file to the destination.
func uploadFile(ctx context.Context, dest destination.Destination, localPath, remotePath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return dest.Upload(ctx, remotePath, f)
}
