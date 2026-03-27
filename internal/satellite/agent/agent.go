package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/pg-swarm/pg-swarm/internal/satellite/eventbus"
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
	// DefaultSentinelImage is the container image for the sentinel sidecar
	// when the cluster config does not specify one.
	DefaultSentinelImage string
	// SidecarListenAddr is the address the sidecar gRPC server listens on.
	// Defaults to ":9091".
	SidecarListenAddr string
}

// switchoverState holds the live context of an interactive switchover operation.
type switchoverState struct {
	session   *health.SwitchoverSession
	proceedCh chan bool // true = proceed to next step, false = abort
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
	eventBus      *eventbus.EventBus

	switchoverMu      sync.Mutex
	activeSwitchovers map[string]*switchoverState // keyed by operation_id
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

	a := &Agent{config: cfg, activeSwitchovers: make(map[string]*switchoverState)}
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
		a.operator = operator.New(a.k8sClient, a.config.K8sClusterName, a.config.DeployNamespace, a.config.DefaultSentinelImage)
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

	// Wire sidecar commander to operator for database creation commands
	if a.operator != nil {
		a.operator.SetSidecarCommander(a.streamManager)
	}

	// 3. Connect persistent stream — also give operator access to the stream
	//    for reporting database creation status back to central
	a.connector = stream.NewConnector(a.config.CentralAddr, a.identity.AuthToken)
	if a.operator != nil {
		a.operator.SetStreamSender(a.connector)
	}
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

	// 3c. Create EventBus and wire it to the connector and sidecar server
	a.eventBus = eventbus.New(a.connector.ForwardEvent)
	a.connector.OnEvent = a.eventBus.Publish
	sidecarSrv.SetOnSidecarEvent(func(evt *pgswarmv1.Event) {
		_ = a.eventBus.Publish(context.Background(), evt)
	})
	log.Info().Msg("event bus created and wired to connector + sidecar server")

	// 4. Register event handlers
	if a.operator != nil {
		// Event-driven path: LifecycleHandler processes cluster.* events
		lh := eventbus.NewLifecycleHandler(a.operator, a.eventBus)
		lh.Register()
		log.Info().Msg("lifecycle handler registered on event bus")
	}

	// 4b. Log rule handler: routes log.rule.* events from sidecars to commands
	lrh := eventbus.NewLogRuleHandler(a.streamManager, a.eventBus)
	lrh.Register()
	log.Info().Msg("log rule handler registered on event bus")

	// 5. Register event handlers for satellite-level events
	if a.k8sClient != nil {
		a.eventBus.Subscribe("satellite.storage_classes_requested", "storage-classes", func(ctx context.Context, evt *pgswarmv1.Event) error {
			report := a.gatherStorageClasses()
			if report == nil {
				return nil
			}
			respEvt := eventbus.NewEvent("satellite.storage_classes_discovered", "", "", "satellite")
			for i, sc := range report.StorageClasses {
				prefix := fmt.Sprintf("sc_%d_", i)
				eventbus.WithData(respEvt, prefix+"name", sc.Name)
				eventbus.WithData(respEvt, prefix+"provisioner", sc.Provisioner)
				eventbus.WithData(respEvt, prefix+"default", fmt.Sprintf("%t", sc.IsDefault))
			}
			eventbus.WithData(respEvt, "count", fmt.Sprintf("%d", len(report.StorageClasses)))
			_ = a.eventBus.Publish(ctx, respEvt)
			return nil
		})
		log.Info().Msg("storage class event handler registered")
	}

	if a.operator != nil && a.k8sClient != nil {
		a.eventBus.Subscribe("switchover.requested", "switchover", func(ctx context.Context, evt *pgswarmv1.Event) error {
			req := evt.GetSwitchoverRequest()
			if req == nil {
				log.Warn().Str("event_type", evt.GetType()).Msg("switchover event missing SwitchoverRequest payload")
				return nil
			}
			log.Info().
				Str("cluster", req.ClusterName).
				Str("target", req.TargetPod).
				Str("operation_id", req.OperationId).
				Bool("interactive", req.Interactive).
				Msg("processing switchover event")

			if req.Interactive {
				// Interactive path: run one step at a time, wait for user confirmation.
				go a.runInteractiveSwitchover(ctx, req)
			} else {
				// Non-interactive path: run all steps automatically.
				result := a.handleSwitchover(req, func(step int32, stepName, stepStatus, targetPod, errMsg string, ponr bool) {
					stepEvt := eventbus.NewPodEvent("switchover.step", req.ClusterName, req.Namespace, targetPod, "satellite")
					eventbus.WithOperationID(stepEvt, req.OperationId)
					eventbus.WithData(stepEvt, "step", fmt.Sprintf("%d", step))
					eventbus.WithData(stepEvt, "step_name", stepName)
					eventbus.WithData(stepEvt, "status", stepStatus)
					eventbus.WithData(stepEvt, "point_of_no_return", fmt.Sprintf("%t", ponr))
					if errMsg != "" {
						eventbus.WithData(stepEvt, "error", errMsg)
					}
					_ = a.eventBus.Publish(ctx, stepEvt)
				})
				resultEvt := eventbus.NewEvent("switchover.completed", req.ClusterName, req.Namespace, "satellite")
				eventbus.WithOperationID(resultEvt, req.OperationId)
				eventbus.WithData(resultEvt, "success", fmt.Sprintf("%t", result.Success))
				if result.ErrorMessage != "" {
					eventbus.WithData(resultEvt, "error", result.ErrorMessage)
					eventbus.WithSeverity(resultEvt, "error")
					resultEvt.Type = "switchover.failed"
				}
				_ = a.eventBus.Publish(ctx, resultEvt)
			}
			return nil
		})

		// switchover.step.execute: user confirmed — proceed to next step.
		a.eventBus.Subscribe("switchover.step.execute", "switchover-proceed", func(ctx context.Context, evt *pgswarmv1.Event) error {
			opID := evt.GetOperationId()
			a.switchoverMu.Lock()
			state, ok := a.activeSwitchovers[opID]
			a.switchoverMu.Unlock()
			if !ok {
				log.Warn().Str("operation_id", opID).Msg("switchover.step.execute: no active session found")
				return nil
			}
			select {
			case state.proceedCh <- true:
			default:
				log.Warn().Str("operation_id", opID).Msg("switchover.step.execute: proceed channel not ready, dropping")
			}
			return nil
		})

		// switchover.abort: user aborted — stop after current step and rollback.
		a.eventBus.Subscribe("switchover.abort", "switchover-abort", func(ctx context.Context, evt *pgswarmv1.Event) error {
			opID := evt.GetOperationId()
			a.switchoverMu.Lock()
			state, ok := a.activeSwitchovers[opID]
			a.switchoverMu.Unlock()
			if !ok {
				log.Warn().Str("operation_id", opID).Msg("switchover.abort: no active session found")
				return nil
			}
			select {
			case state.proceedCh <- false:
			default:
				log.Warn().Str("operation_id", opID).Msg("switchover.abort: proceed channel not ready, dropping")
			}
			return nil
		})

		log.Info().Msg("switchover event handlers registered (non-interactive + interactive)")
	}

	// Log level event handler
	a.eventBus.Subscribe("satellite.set_log_level", "log-level", func(ctx context.Context, evt *pgswarmv1.Event) error {
		level := evt.Data["level"]
		if level == "" {
			return nil
		}
		newLevel, err := logcapture.SetGlobalLevel(level)
		if err != nil {
			log.Warn().Str("level", level).Err(err).Msg("failed to set log level from event")
			return nil
		}
		hook.SetStreamLevel(newLevel)
		log.Info().Str("level", level).Msg("log level changed via event")
		return nil
	})
	log.Info().Msg("log level event handler registered")

	// 7. Start health monitor
	if a.operator != nil && a.k8sClient != nil {
		a.healthMon = health.New(a.k8sClient, a.operator, 30*time.Second)
		mon := a.healthMon
		mon.SetOnHealth(func(report *pgswarmv1.ClusterHealthReport) {
			evt := eventbus.NewEvent("health.report", report.ClusterName, "", "satellite")
			eventbus.WithData(evt, "state", report.State.String())
			eventbus.WithData(evt, "instance_count", fmt.Sprintf("%d", len(report.Instances)))
			evt.Payload = &pgswarmv1.Event_HealthReport{HealthReport: report}
			_ = a.eventBus.Publish(context.Background(), evt)
		})
		mon.SetOnEvent(func(event *pgswarmv1.EventReport) {
			evt := eventbus.NewEvent("cluster.state_changed", event.ClusterName, "", "satellite")
			eventbus.WithSeverity(evt, event.Severity)
			eventbus.WithData(evt, "message", event.Message)
			eventbus.WithData(evt, "source", event.Source)
			_ = a.eventBus.Publish(context.Background(), evt)
		})
		go mon.Run(ctx)
		log.Info().Msg("health monitor started")
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

// runInteractiveSwitchover executes a switchover one step at a time, pausing
// after each pre-PONR completed step to wait for user confirmation via the
// switchover.step.execute event.  It runs in its own goroutine.
func (a *Agent) runInteractiveSwitchover(ctx context.Context, req *pgswarmv1.SwitchoverRequest) {
	// Resolve namespace
	if a.operator != nil {
		req.Namespace = a.operator.ResolveNamespaceForCluster(req.ClusterName, req.Namespace)
	}

	// Boost health monitor
	if a.healthMon != nil {
		a.healthMon.Boost(2 * time.Minute)
	}

	session := health.NewSwitchoverSession(a.k8sClient, req, a.streamManager)
	proceedCh := make(chan bool, 1)

	a.switchoverMu.Lock()
	a.activeSwitchovers[req.OperationId] = &switchoverState{
		session:   session,
		proceedCh: proceedCh,
	}
	a.switchoverMu.Unlock()

	defer func() {
		a.switchoverMu.Lock()
		delete(a.activeSwitchovers, req.OperationId)
		a.switchoverMu.Unlock()
	}()

	emitStep := func(step int32, stepName, stepStatus, targetPod, errMsg string, ponr bool) {
		stepEvt := eventbus.NewPodEvent("switchover.step", req.ClusterName, req.Namespace, targetPod, "satellite")
		eventbus.WithOperationID(stepEvt, req.OperationId)
		eventbus.WithData(stepEvt, "step", fmt.Sprintf("%d", step))
		eventbus.WithData(stepEvt, "step_name", stepName)
		eventbus.WithData(stepEvt, "status", stepStatus)
		eventbus.WithData(stepEvt, "point_of_no_return", fmt.Sprintf("%t", ponr))
		if errMsg != "" {
			eventbus.WithData(stepEvt, "error", errMsg)
		}
		_ = a.eventBus.Publish(ctx, stepEvt)
	}

	emitResult := func(success bool, errMsg string) {
		resultEvt := eventbus.NewEvent("switchover.completed", req.ClusterName, req.Namespace, "satellite")
		eventbus.WithOperationID(resultEvt, req.OperationId)
		eventbus.WithData(resultEvt, "success", fmt.Sprintf("%t", success))
		if errMsg != "" {
			eventbus.WithData(resultEvt, "error", errMsg)
			eventbus.WithSeverity(resultEvt, "error")
			resultEvt.Type = "switchover.failed"
		}
		_ = a.eventBus.Publish(ctx, resultEvt)
	}

	const userTimeout = 15 * time.Minute

	for step := int32(1); step <= int32(session.TotalSteps()); step++ {
		name, targetPod, ponr := session.StepMeta(step)

		// Emit "starting" for this step
		emitStep(step, name, "starting", targetPod, "", ponr)

		// Execute the step; emitStep is called by session for result statuses
		ok, errMsg := session.ExecuteStep(ctx, step, emitStep)
		if !ok {
			emitResult(false, errMsg)
			return
		}

		// Past the point of no return — auto-continue without gating
		if ponr {
			continue
		}

		// Pre-PONR step completed: wait for user to click Continue or Abort
		select {
		case proceed := <-proceedCh:
			if !proceed {
				// User aborted: rollback (unfence) if we've fenced the primary
				if step >= 4 {
					session.Rollback(ctx)
				}
				emitResult(false, "aborted by user")
				return
			}
			// User clicked Continue — proceed to next step
		case <-time.After(userTimeout):
			log.Warn().Str("operation_id", req.OperationId).Msg("interactive switchover timed out waiting for user")
			if step >= 4 {
				session.Rollback(ctx)
			}
			emitResult(false, "timed out waiting for user confirmation")
			return
		case <-ctx.Done():
			if step >= 4 {
				session.Rollback(ctx)
			}
			emitResult(false, "context cancelled")
			return
		}
	}

	log.Info().
		Str("cluster", req.ClusterName).
		Str("primary", session.PrimaryPod()).
		Str("new_primary", req.TargetPod).
		Msg("interactive switchover completed")
	emitResult(true, "")
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
