package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/pg-swarm/pg-swarm/internal/satellite/agent"
)

func main() {
	log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).With().Timestamp().Logger()

	hostname, _ := os.Hostname()

	cfg := agent.Config{
		CentralAddr:             getEnv("CENTRAL_ADDR", "localhost:9090"),
		Hostname:                getEnv("HOSTNAME", hostname),
		K8sClusterName:          getEnv("K8S_CLUSTER_NAME", ""),
		Region:                  getEnv("REGION", ""),
		IdentitySecretName:      getEnv("IDENTITY_SECRET_NAME", "pg-swarm-satellite-identity"),
		IdentitySecretNamespace: getEnv("IDENTITY_SECRET_NAMESPACE", "pgswarm-system"),
		DeployNamespace:         getEnv("DEPLOY_NAMESPACE", "default"),
	}

	if cfg.K8sClusterName == "" {
		log.Fatal().Msg("K8S_CLUSTER_NAME is required")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

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
