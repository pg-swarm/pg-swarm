package agent

import (
	"context"
	"fmt"

	"time"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/pg-swarm/pg-swarm/internal/satellite/health"
	"github.com/pg-swarm/pg-swarm/internal/satellite/logcapture"
	"github.com/pg-swarm/pg-swarm/internal/satellite/operator"
	"github.com/pg-swarm/pg-swarm/internal/satellite/sidecar"
	"github.com/pg-swarm/pg-swarm/internal/satellite/stream"
	"github.com/rs/zerolog"
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
	// SidecarListenAddr is the address the sidecar gRPC server listens on.
	// Defaults to ":9091".
	SidecarListenAddr string
}

// Agent manages the satellite lifecycle: registration, approval, and streaming.
type Agent struct {
	config        Config
	identity      *Identity
	connector     *stream.Connector
	operator      *operator.Operator
	k8sClient     *kubernetes.Clientset // nil if K8s is unavailable or secret disabled
	streamManager *sidecar.SidecarStreamManager
	healthMon     *health.Monitor
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
	log.Trace().Msg("ensuring identity")

	// 1. Load or register identity
	if err := a.ensureIdentity(ctx); err != nil {
		return fmt.Errorf("ensure identity: %w", err)
	}

	log.Trace().Str("satellite_id", a.identity.SatelliteID).Msg("identity loaded")

	// 2. Create operator (requires K8s client)
	if a.k8sClient != nil {
		a.operator = operator.New(a.k8sClient, a.config.K8sClusterName, a.config.DeployNamespace, a.config.DefaultFailoverImage, a.identity.SatelliteID)
		log.Trace().Msg("operator created")
	} else {
		log.Warn().Msg("K8s client unavailable — operator disabled, configs will be logged only")
	}

	// 2b. Create sidecar stream manager and server
	a.streamManager = sidecar.NewSidecarStreamManager()
	sidecarSrv := sidecar.NewServer(a.streamManager, a.validateSidecarToken)
	go func() {
		if err := sidecarSrv.Start(a.config.SidecarListenAddr); err != nil {
			log.Error().Err(err).Msg("sidecar gRPC server failed")
		}
	}()
	go func() {
		<-ctx.Done()
		sidecarSrv.Stop()
	}()
	log.Trace().Str("addr", a.config.SidecarListenAddr).Msg("sidecar gRPC server started")

	// 3. Connect persistent stream
	a.connector = stream.NewConnector(a.config.CentralAddr, a.identity.AuthToken)
	log.Trace().Str("central_addr", a.config.CentralAddr).Msg("connector created")

	// 3b. Attach log capture hook → streams log entries to central
	hook := logcapture.NewStreamHook("agent", zerolog.InfoLevel)
	go hook.Drain(ctx, func(entry *pgswarmv1.LogEntry) {
		a.connector.SendMessage(&pgswarmv1.SatelliteMessage{
			Payload: &pgswarmv1.SatelliteMessage_LogEntry{
				LogEntry: entry,
			},
		})
	})
	log.Logger = log.Logger.Hook(hook)
	log.Trace().Msg("log capture hook attached")

	// 3c. Wire OnSetLogLevel callback
	a.connector.OnSetLogLevel = func(level string) {
		newLevel, err := logcapture.SetGlobalLevel(level)
		if err != nil {
			log.Warn().Str("level", level).Err(err).Msg("failed to set log level")
			return
		}
		hook.SetStreamLevel(newLevel)
		log.Info().Str("level", level).Msg("log level changed by central")
	}
	log.Trace().Msg("OnSetLogLevel callback wired")

	// 4. Wire operator callbacks
	if a.operator != nil {
		a.connector.OnConfig = a.operator.HandleConfig
		a.connector.OnDelete = a.operator.HandleDelete
		log.Trace().Msg("operator callbacks wired")
	}

	// 5. Wire storage class callback
	if a.k8sClient != nil {
		a.connector.OnStorageClassRequest = a.gatherStorageClasses
		log.Trace().Msg("storage class callback wired")
	}

	// 6. Wire switchover callback
	if a.operator != nil && a.k8sClient != nil {
		a.connector.OnSwitchover = a.handleSwitchover
		log.Trace().Msg("switchover callback wired")
	}

	// 6b. Wire restore command callback
	if a.k8sClient != nil {
		a.connector.OnRestoreCommand = func(cmd *pgswarmv1.RestoreCommand) {
			a.connector.SendMessage(&pgswarmv1.SatelliteMessage{
				Payload: &pgswarmv1.SatelliteMessage_RestoreStatus{
					RestoreStatus: &pgswarmv1.RestoreStatusReport{
						ClusterName: cmd.ClusterName,
						Namespace:   cmd.Namespace,
						RestoreId:   cmd.RestoreId,
						Status:      "running",
					},
				},
			})
			log.Info().Str("cluster", cmd.ClusterName).Str("restore_id", cmd.RestoreId).Msg("restore command acknowledged")
		}
		log.Trace().Msg("restore command callback wired")
	}

	// 7. Start health monitor
	if a.operator != nil && a.k8sClient != nil {
		a.healthMon = health.New(a.k8sClient, a.operator, 30*time.Second)
		mon := a.healthMon
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
		mon.SetOnBackup(func(report *pgswarmv1.BackupStatusReport) {
			a.connector.SendMessage(&pgswarmv1.SatelliteMessage{
				Payload: &pgswarmv1.SatelliteMessage_BackupStatus{
					BackupStatus: report,
				},
			})
		})
		go mon.Run(ctx)
		log.Trace().Msg("health monitor started")
	}

	// 8. Start orphan checker and drift reconciler
	if a.operator != nil {
		go a.operator.StartOrphanChecker(ctx)
		go a.operator.StartDriftReconciler(ctx)
		log.Trace().Msg("orphan checker and drift reconciler started")
	}

	log.Info().Str("satellite_id", a.identity.SatelliteID).Msg("satellite agent started")

	return a.connector.Run(ctx)
}

// handleSwitchover handles a switchover request from central.
func (a *Agent) handleSwitchover(req *pgswarmv1.SwitchoverRequest, onProgress func(int32, string, string, string, string, bool)) *pgswarmv1.SwitchoverResult {
	log.Trace().Str("cluster", req.ClusterName).Str("target", req.TargetPod).Msg("handleSwitchover entry")
	if a.k8sClient == nil || a.operator == nil {
		return &pgswarmv1.SwitchoverResult{
			ClusterName:  req.ClusterName,
			ErrorMessage: "K8s client or operator unavailable",
		}
	}

	// Resolve namespace from operator if not provided
	ns := a.operator.ResolveNamespaceForCluster(req.ClusterName, req.Namespace)
	req.Namespace = ns
	log.Trace().Str("namespace", ns).Msg("handleSwitchover resolved namespace")

	// Boost health monitor to fast polling during and after switchover
	if a.healthMon != nil {
		a.healthMon.Boost(2 * time.Minute)
	}

	result := health.Switchover(context.Background(), a.k8sClient, req, a.streamManager, health.ProgressFunc(onProgress))
	log.Trace().Bool("success", result.Success).Msg("handleSwitchover result")
	return result
}

// validateSidecarToken checks if the provided token matches any cluster's
// sidecar-stream-token. Returns true if valid.
func (a *Agent) validateSidecarToken(token string) bool {
	if a.k8sClient == nil || a.operator == nil {
		return false
	}
	return a.operator.ValidateSidecarToken(token)
}

// gatherStorageClasses queries K8s for all StorageClasses and returns a proto report.
func (a *Agent) gatherStorageClasses() *pgswarmv1.StorageClassReport {
	log.Trace().Msg("gatherStorageClasses entry")
	if a.k8sClient == nil {
		return nil
	}

	list, err := a.k8sClient.StorageV1().StorageClasses().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		log.Error().Err(err).Msg("failed to list storage classes")
		return nil
	}

	log.Trace().Int("count", len(list.Items)).Msg("gatherStorageClasses found classes")
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
