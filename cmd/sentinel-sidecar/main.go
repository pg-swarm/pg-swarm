package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/pg-swarm/pg-swarm/internal/sentinel"
	"github.com/pg-swarm/pg-swarm/internal/sentinel/backup"
	"github.com/pg-swarm/pg-swarm/internal/shared/loglevel"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).
		With().Timestamp().Str("component", "sentinel-sidecar").Logger()

	// Set log level from env (default: info)
	if lvl := os.Getenv("LOG_LEVEL"); lvl != "" {
		if _, err := loglevel.SetGlobalLevel(lvl); err != nil {
			log.Warn().Str("level", lvl).Msg("invalid LOG_LEVEL, defaulting to info")
		} else {
			log.Info().Str("level", lvl).Msg("log level set from LOG_LEVEL env var")
		}
	}

	podName := os.Getenv("POD_NAME")
	namespace := os.Getenv("POD_NAMESPACE")
	clusterName := os.Getenv("CLUSTER_NAME")
	pgPassword := os.Getenv("POSTGRES_PASSWORD")
	replPassword := os.Getenv("REPLICATION_PASSWORD")
	primaryHost := os.Getenv("PRIMARY_HOST")

	if podName == "" || namespace == "" || clusterName == "" {
		log.Fatal().Msg("POD_NAME, POD_NAMESPACE, and CLUSTER_NAME env vars are required")
	}

	interval := 5 * time.Second
	if v := os.Getenv("HEALTH_CHECK_INTERVAL"); v != "" {
		secs, err := strconv.Atoi(v)
		if err != nil || secs <= 0 {
			log.Fatal().Str("value", v).Msg("invalid HEALTH_CHECK_INTERVAL")
		}
		interval = time.Duration(secs) * time.Second
	}

	connString := fmt.Sprintf(
		"host=localhost port=5432 user=postgres password=%s dbname=postgres sslmode=disable",
		pgPassword,
	)

	k8sCfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal().Err(err).Msg("failed to get in-cluster K8s config")
	}
	client, err := kubernetes.NewForConfig(k8sCfg)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create K8s client")
	}

	recoveryRulesPath := os.Getenv("RECOVERY_RULES_PATH")
	if recoveryRulesPath == "" {
		recoveryRulesPath = "/etc/recovery-rules/rules.json"
	}

	mon := sentinel.NewMonitor(sentinel.Config{
		PodName:             podName,
		Namespace:           namespace,
		ClusterName:         clusterName,
		Interval:            interval,
		PGConnString:        connString,
		RestConfig:          k8sCfg,
		ReplicationPassword: replPassword,
		PrimaryHost:         primaryHost,
		PGPassword:          pgPassword,
		RecoveryRulesPath:   recoveryRulesPath,
	}, client)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// Start sidecar connector (connects to satellite's gRPC server for remote commands)
	satelliteAddr := os.Getenv("SATELLITE_ADDR")
	sidecarToken := os.Getenv("SIDECAR_STREAM_TOKEN")
	if satelliteAddr != "" && sidecarToken != "" {
		connector := sentinel.NewSidecarConnector(
			satelliteAddr,
			sidecarToken,
			&pgswarmv1.SidecarIdentity{
				PodName:     podName,
				ClusterName: clusterName,
				Namespace:   namespace,
			},
			connString,
			client,
			k8sCfg,
		)
		// Wire connector as the event emitter for the log watcher
		mon.SetEventEmitter(connector)

		// Create backup manager and wire it into monitor + connector
		bm := backup.NewManager(
			backup.PodConfig{
				PodName:     podName,
				Namespace:   namespace,
				ClusterName: clusterName,
			},
			connector,
			client,
			k8sCfg,
			sentinel.ExecInPod,
			sentinel.ExecInPodOutput,
			sentinel.ExecInPodStream,
		)
		mon.SetBackupManager(bm)
		connector.SetBackupManager(bm)

		go func() {
			if err := connector.Run(ctx); err != nil && ctx.Err() == nil {
				log.Error().Err(err).Msg("sidecar connector exited with error")
			}
		}()
		log.Info().Str("satellite_addr", satelliteAddr).Msg("sidecar connector started")
	}

	if err := mon.Run(ctx); err != nil {
		log.Fatal().Err(err).Msg("sentinel monitor exited with error")
	}
}
