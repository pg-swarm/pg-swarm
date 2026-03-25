package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/pg-swarm/pg-swarm/internal/satellite/agent"
	"github.com/pg-swarm/pg-swarm/internal/satellite/logcapture"
)

func main() {
	log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).With().Timestamp().Logger()

	// Set initial log level from env (default: info)
	if lvl := os.Getenv("LOG_LEVEL"); lvl != "" {
		if _, err := logcapture.SetGlobalLevel(lvl); err != nil {
			log.Warn().Str("level", lvl).Msg("invalid LOG_LEVEL, defaulting to info")
		} else {
			log.Info().Str("level", lvl).Msg("log level set from LOG_LEVEL env var")
		}
	}
	log.Trace().Msg("satellite process starting")

	hostname, _ := os.Hostname()

	cfg := agent.Config{
		CentralAddr:             getEnv("CENTRAL_ADDR", "localhost:9090"),
		Hostname:                getEnv("HOSTNAME", hostname),
		K8sClusterName:          getEnv("K8S_CLUSTER_NAME", ""),
		Region:                  getEnv("REGION", ""),
		IdentitySecretName:      getEnv("IDENTITY_SECRET_NAME", "pg-swarm-satellite-identity"),
		IdentitySecretNamespace: getEnv("IDENTITY_SECRET_NAMESPACE", "pgswarm-system"),
		DeployNamespace:         getEnv("DEPLOY_NAMESPACE", "default"),
		DefaultFailoverImage:    getEnv("DEFAULT_FAILOVER_IMAGE", "ghcr.io/pg-swarm/pg-swarm-failover:latest"),
		SidecarListenAddr:       getEnv("SIDECAR_LISTEN_ADDR", ":9091"),
	}

	if cfg.K8sClusterName == "" {
		log.Fatal().Msg("K8S_CLUSTER_NAME is required")
	}

	log.Trace().Msg("config loaded, creating agent")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	log.Trace().Msg("signal context created")

	a := agent.New(cfg)
	if err := a.Run(ctx); err != nil {
		log.Fatal().Err(err).Msg("satellite agent failed")
	}
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
