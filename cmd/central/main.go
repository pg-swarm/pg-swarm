package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/pg-swarm/pg-swarm/internal/central/registry"
	"github.com/pg-swarm/pg-swarm/internal/central/server"
	"github.com/pg-swarm/pg-swarm/internal/central/store"
)

func main() {
	// Setup zerolog with console writer
	log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).With().Timestamp().Logger()

	// Read config from env
	dbURL := buildDatabaseURL()
	grpcAddr := getEnv("GRPC_ADDR", ":9090")
	httpAddr := getEnv("HTTP_ADDR", ":8080")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Connect to PostgreSQL
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect to database")
	}
	defer pool.Close()

	// Run migrations
	if err := store.RunMigrations(ctx, pool); err != nil {
		log.Fatal().Err(err).Msg("failed to run migrations")
	}

	// Initialize components
	pgStore := store.NewPostgresStore(pool)
	reg := registry.New(pgStore)
	grpcServer := server.NewGRPCServer(reg, pgStore)
	restServer := server.NewRESTServer(pgStore, reg, grpcServer.GetStreams())

	// Start gRPC server
	go func() {
		if err := grpcServer.Start(grpcAddr); err != nil {
			log.Fatal().Err(err).Msg("gRPC server failed")
		}
	}()

	// Start HTTP server
	go func() {
		if err := restServer.Start(httpAddr); err != nil {
			log.Fatal().Err(err).Msg("HTTP server failed")
		}
	}()

	log.Info().Str("grpc", grpcAddr).Str("http", httpAddr).Msg("pg-swarm central started")

	<-ctx.Done()
	log.Info().Msg("shutting down...")
	grpcServer.Stop()
	if err := restServer.Shutdown(); err != nil {
		log.Error().Err(err).Msg("REST server shutdown error")
	}
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func buildDatabaseURL() string {
	host     := getEnv("PG_HOST",     "localhost")
	port     := getEnv("PG_PORT",     "5432")
	user     := getEnv("PG_USER",     "pgswarm")
	password := getEnv("PG_PASSWORD", "pgswarm")
	db       := getEnv("PG_DB",       "pgswarm")
	sslMode  := getEnv("PG_SSL_MODE", "disable")
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s", user, password, host, port, db, sslMode)
}
