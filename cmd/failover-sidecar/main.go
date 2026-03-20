package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/pg-swarm/pg-swarm/internal/failover"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).
		With().Timestamp().Str("component", "failover-sidecar").Logger()

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

	mon := failover.NewMonitor(failover.Config{
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

	if err := mon.Run(ctx); err != nil {
		log.Fatal().Err(err).Msg("failover monitor exited with error")
	}
}
