package agent

import (
	"context"
	"fmt"

	"time"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/pg-swarm/pg-swarm/internal/satellite/health"
	"github.com/pg-swarm/pg-swarm/internal/satellite/operator"
	"github.com/pg-swarm/pg-swarm/internal/satellite/stream"
	"github.com/rs/zerolog/log"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	// DefaultFailoverImage is the container image for the failover sidecar
	// when the cluster config does not specify one.
	DefaultFailoverImage string
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
		a.operator = operator.New(a.k8sClient, a.config.K8sClusterName, a.config.DeployNamespace, a.config.DefaultFailoverImage)
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

	// 5. Wire storage class callback
	if a.k8sClient != nil {
		a.connector.OnStorageClassRequest = a.gatherStorageClasses
	}

	// 6. Wire switchover callback
	if a.operator != nil && a.k8sClient != nil {
		a.connector.OnSwitchover = a.handleSwitchover
	}

	// 7. Start health monitor
	if a.operator != nil && a.k8sClient != nil {
		mon := health.New(a.k8sClient, a.operator, 30*time.Second)
		mon.SetOnHealth(func(report *pgswarmv1.ClusterHealthReport) {
			a.connector.SendMessage(&pgswarmv1.SatelliteMessage{
				Payload: &pgswarmv1.SatelliteMessage_HealthReport{
					HealthReport: report,
				},
			})
		})
		mon.SetOnEvent(func(event *pgswarmv1.EventReport) {
			a.connector.SendMessage(&pgswarmv1.SatelliteMessage{
				Payload: &pgswarmv1.SatelliteMessage_EventReport{
					EventReport: event,
				},
			})
		})
		go mon.Run(ctx)
	}

	log.Info().Str("satellite_id", a.identity.SatelliteID).Msg("satellite agent started")

	return a.connector.Run(ctx)
}

// handleSwitchover handles a switchover request from central.
func (a *Agent) handleSwitchover(req *pgswarmv1.SwitchoverRequest) *pgswarmv1.SwitchoverResult {
	if a.k8sClient == nil || a.operator == nil {
		return &pgswarmv1.SwitchoverResult{
			ClusterName:  req.ClusterName,
			ErrorMessage: "K8s client or operator unavailable",
		}
	}

	// Resolve namespace from operator if not provided
	ns := a.operator.ResolveNamespaceForCluster(req.ClusterName, req.Namespace)
	req.Namespace = ns

	// Read the superuser password
	secretName := req.ClusterName + "-secret"
	secret, err := a.k8sClient.CoreV1().Secrets(ns).Get(context.Background(), secretName, metav1.GetOptions{})
	if err != nil {
		return &pgswarmv1.SwitchoverResult{
			ClusterName:  req.ClusterName,
			ErrorMessage: fmt.Sprintf("cannot read secret %s: %v", secretName, err),
		}
	}
	password := string(secret.Data["superuser-password"])

	return health.Switchover(context.Background(), a.k8sClient, req, password)
}

// gatherStorageClasses queries K8s for all StorageClasses and returns a proto report.
func (a *Agent) gatherStorageClasses() *pgswarmv1.StorageClassReport {
	if a.k8sClient == nil {
		return nil
	}

	list, err := a.k8sClient.StorageV1().StorageClasses().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		log.Error().Err(err).Msg("failed to list storage classes")
		return nil
	}

	report := &pgswarmv1.StorageClassReport{}
	for _, sc := range list.Items {
		info := &pgswarmv1.StorageClassInfo{
			Name:        sc.Name,
			Provisioner: sc.Provisioner,
		}
		if sc.ReclaimPolicy != nil {
			info.ReclaimPolicy = string(*sc.ReclaimPolicy)
		}
		if sc.VolumeBindingMode != nil {
			info.VolumeBindingMode = string(*sc.VolumeBindingMode)
		}
		if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			info.IsDefault = true
		}
		report.StorageClasses = append(report.StorageClasses, info)
	}

	return report
}
