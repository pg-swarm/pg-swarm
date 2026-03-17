package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/pg-swarm/pg-swarm/internal/backup"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func main() {
	log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).
		With().Timestamp().Str("component", "backup-sidecar").Logger()

	cfg := backup.ConfigFromEnv()

	if cfg.ClusterName == "" {
		log.Fatal().Msg("CLUSTER_NAME env var is required")
	}
	if cfg.PGUser == "" {
		cfg.PGUser = "backup_user"
	}

	log.Info().
		Str("cluster", cfg.ClusterName).
		Str("satellite", cfg.SatelliteID).
		Str("dest_type", cfg.DestType).
		Msg("backup sidecar starting")

	sidecar := backup.New(cfg)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := sidecar.Run(ctx); err != nil {
		log.Fatal().Err(err).Msg("backup sidecar exited with error")
	}
}
