package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/pg-swarm/pg-swarm/internal/satellite/agent"
	"github.com/pg-swarm/pg-swarm/internal/satellite/logcapture"
)

func main() {
	log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).With().Timestamp().Str("component", "satellite").Logger()

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
		DefaultSentinelImage:    getEnv("DEFAULT_SENTINEL_IMAGE", "ghcr.io/pg-swarm/pg-swarm-sentinel:latest"),
		SidecarListenAddr:       getEnv("SIDECAR_LISTEN_ADDR", ":9091"),
	}

	if cfg.K8sClusterName == "" {
		log.Fatal().Msg("K8S_CLUSTER_NAME is required")
	}

	log.Trace().Msg("config loaded, creating agent")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	log.Trace().Msg("signal context created")

	// Start a minimal HTTP health server for K8s probes (no logging).
	healthAddr := getEnv("HEALTH_ADDR", ":8081")
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		})
		srv := &http.Server{Addr: healthAddr, Handler: mux}
		go func() {
			<-ctx.Done()
			_ = srv.Close()
		}()
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error().Err(err).Str("addr", healthAddr).Msg("health server failed")
		}
	}()
	log.Info().Str("addr", healthAddr).Msg("health server started")

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
