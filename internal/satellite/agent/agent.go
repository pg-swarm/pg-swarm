package agent

import (
	"context"
	"fmt"

	"github.com/pg-swarm/pg-swarm/internal/satellite/operator"
	"github.com/pg-swarm/pg-swarm/internal/satellite/stream"
	"github.com/rs/zerolog/log"
	"k8s.io/client-go/kubernetes"
)

// Config holds the satellite agent configuration.
type Config struct {
	CentralAddr    string
	Hostname       string
	K8sClusterName string
	Region         string
	Labels         map[string]string
	// IdentitySecretName is the K8s Secret used to persist the satellite identity.
	// Defaults to "pg-swarm-satellite-identity". Set to "" to disable K8s secret storage.
	IdentitySecretName string
	// IdentitySecretNamespace is the namespace for the identity secret.
	// Defaults to "pgswarm-system".
	IdentitySecretNamespace string
	// DeployNamespace is the default K8s namespace for deploying PostgreSQL clusters
	// when the cluster config does not specify one. Defaults to "default".
	DeployNamespace string
}

// Agent manages the satellite lifecycle: registration, approval, and streaming.
type Agent struct {
	config    Config
	identity  *Identity
	connector *stream.Connector
	operator  *operator.Operator
	k8sClient *kubernetes.Clientset // nil if K8s is unavailable or secret disabled
}

// Identity stores the satellite's registration info.
type Identity struct {
	SatelliteID string `json:"satellite_id"`
	AuthToken   string `json:"auth_token"`
}

// New creates a new satellite Agent with the given configuration.
func New(cfg Config) *Agent {
	if cfg.IdentitySecretName == "" {
		cfg.IdentitySecretName = "pg-swarm-satellite-identity"
	}
	if cfg.IdentitySecretNamespace == "" {
		cfg.IdentitySecretNamespace = "pgswarm-system"
	}

	a := &Agent{config: cfg}
	client, err := buildK8sClient()
	if err != nil {
		log.Warn().Err(err).Msg("K8s client unavailable — identity secret storage disabled")
	} else {
		a.k8sClient = client
	}
	return a
}

// Run starts the satellite agent. It ensures the agent has an identity
// (loading from disk or registering and waiting for approval), then
// connects a persistent bidirectional stream to central.
func (a *Agent) Run(ctx context.Context) error {
	// 1. Load or register identity
	if err := a.ensureIdentity(ctx); err != nil {
		return fmt.Errorf("ensure identity: %w", err)
	}

	// 2. Create operator (requires K8s client)
	if a.k8sClient != nil {
		a.operator = operator.New(a.k8sClient, a.config.K8sClusterName, a.config.DeployNamespace)
	} else {
		log.Warn().Msg("K8s client unavailable — operator disabled, configs will be logged only")
	}

	// 3. Connect persistent stream
	a.connector = stream.NewConnector(a.config.CentralAddr, a.identity.AuthToken)

	// 4. Wire operator callbacks
	if a.operator != nil {
		a.connector.OnConfig = a.operator.HandleConfig
		a.connector.OnDelete = a.operator.HandleDelete
	}

	log.Info().Str("satellite_id", a.identity.SatelliteID).Msg("satellite agent started")

	return a.connector.Run(ctx)
}
