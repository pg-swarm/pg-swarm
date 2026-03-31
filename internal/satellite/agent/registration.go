package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// errNeedReRegister is returned when the central rejects our temp token,
// meaning we must discard the in-flight registration and start fresh.
var errNeedReRegister = errors.New("registration credentials rejected, re-registration required")

// ensureIdentity loads a persisted identity or drives the full
// register → wait-for-approval flow, retrying indefinitely until
// the context is cancelled.
func (a *Agent) ensureIdentity(ctx context.Context) error {
	log.Trace().Msg("attempting to load identity")
	identity, err := a.loadIdentity(ctx)
	if err == nil {
		a.identity = identity
		log.Info().Str("satellite_id", identity.SatelliteID).Msg("loaded existing identity")
		return nil
	}
	log.Trace().Err(err).Msg("identity load failed, will register")
	if !os.IsNotExist(err) {
		log.Warn().Err(err).Msg("identity unreadable, will re-register")
	}

	backoff := 2 * time.Second
	const maxBackoff = 60 * time.Second

	for {
		err := a.registerAndWaitForApproval(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			return nil
		}

		if errors.Is(err, errNeedReRegister) {
			log.Warn().Msg("re-registering with central...")
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			backoff = 2 * time.Second
			continue
		}

		log.Warn().Err(err).Dur("retry_in", backoff).Msg("registration failed, retrying...")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}

// registerAndWaitForApproval performs one complete registration attempt:
// dials central, calls Register, then polls CheckApproval until approved.
//
// Error classification:
//   - nil                  → approved and identity saved
//   - errNeedReRegister    → central rejected temp token; caller must re-register
//   - ctx.Err()            → context cancelled; caller must stop
//   - other error          → transient failure; caller should retry with backoff
func (a *Agent) registerAndWaitForApproval(ctx context.Context) error {
	log.Trace().Str("central_addr", a.config.CentralAddr).Msg("registration: dialing central")
	conn, err := grpc.NewClient(
		a.config.CentralAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("dial central: %w", err)
	}
	defer conn.Close()

	client := pgswarmv1.NewRegistrationServiceClient(conn)

	// --- Step 1: Register (retry on transient errors) ---
	log.Trace().Msg("registration: calling Register RPC")
	resp, err := a.callRegister(ctx, client)
	if err != nil {
		return err
	}
	log.Info().Str("satellite_id", resp.SatelliteId).
		Msg("registered with central, waiting for approval...")

	// --- Step 2: Poll CheckApproval ---
	log.Trace().Str("satellite_id", resp.SatelliteId).Msg("registration: starting approval poll")
	return a.pollApproval(ctx, client, resp)
}

// callRegister calls the Register RPC, retrying on transient gRPC errors.
func (a *Agent) callRegister(ctx context.Context, client pgswarmv1.RegistrationServiceClient) (*pgswarmv1.RegisterResponse, error) {
	backoff := time.Second

	for {
		resp, err := client.Register(ctx, &pgswarmv1.RegisterRequest{
			Hostname:       a.config.Hostname,
			K8SClusterName: a.config.K8sClusterName,
			Region:         a.config.Region,
			Labels:         a.config.Labels,
		})
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		code := status.Code(err)
		switch {
		case isTransient(code):
			log.Warn().Err(err).Str("code", code.String()).
				Dur("retry_in", backoff).Msg("Register RPC failed (transient), retrying...")
		case code == codes.AlreadyExists:
			log.Warn().Dur("retry_in", backoff).Msg("Register returned AlreadyExists, retrying...")
		default:
			return nil, fmt.Errorf("register RPC: %w", err)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// pollApproval loops on CheckApproval until the satellite is approved,
// the context is cancelled, or a non-retryable error occurs.
func (a *Agent) pollApproval(ctx context.Context, client pgswarmv1.RegistrationServiceClient, reg *pgswarmv1.RegisterResponse) error {
	const basePollInterval = 5 * time.Second
	const maxPollInterval = 30 * time.Second

	consecutiveErrs := 0

	ticker := time.NewTicker(basePollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-ticker.C:
			resp, err := client.CheckApproval(ctx, &pgswarmv1.CheckApprovalRequest{
				SatelliteId: reg.SatelliteId,
				TempToken:   reg.TempToken,
			})
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}

				code := status.Code(err)
				switch {
				case code == codes.Unauthenticated || code == codes.NotFound:
					log.Warn().Str("code", code.String()).
						Str("satellite_id", reg.SatelliteId).
						Msg("temp token rejected by central")
					return errNeedReRegister

				case !isTransient(code):
					return fmt.Errorf("check approval: %w", err)

				default:
					consecutiveErrs++
					next := backoffDuration(basePollInterval, consecutiveErrs, maxPollInterval)
					log.Warn().Err(err).
						Str("code", code.String()).
						Int("consecutive_errors", consecutiveErrs).
						Dur("next_poll_in", next).
						Msg("CheckApproval transient error, backing off...")
					ticker.Reset(next)
					continue
				}
			}

			if consecutiveErrs > 0 {
				consecutiveErrs = 0
				ticker.Reset(basePollInterval)
			}

			if !resp.Approved {
				log.Trace().Str("satellite_id", reg.SatelliteId).Msg("poll tick: not yet approved")
				log.Debug().Str("satellite_id", reg.SatelliteId).Msg("pending approval...")
				continue
			}

			log.Trace().Str("satellite_id", reg.SatelliteId).Msg("approval received")
			// Approved — set identity in memory immediately (works without pod restart).
			a.identity = &Identity{
				SatelliteID: reg.SatelliteId,
				AuthToken:   resp.AuthToken,
			}
			if err := a.saveIdentity(ctx); err != nil {
				return fmt.Errorf("save identity: %w", err)
			}
			log.Trace().Str("satellite_id", reg.SatelliteId).Msg("identity saved to K8s secret")
			log.Info().Str("satellite_id", reg.SatelliteId).Msg("approved by central")
			return nil
		}
	}
}

// loadIdentity reads the satellite identity. It first checks env vars
// (PG_SWARM_SATELLITE_ID, PG_SWARM_AUTH_TOKEN) which are the fast path for
// in-cluster pods with projected secret volumes. If the env vars are empty and
// a K8s client is available, it falls back to reading the identity secret
// directly via the K8s API.
func (a *Agent) loadIdentity(ctx context.Context) (*Identity, error) {
	satID := os.Getenv("PG_SWARM_SATELLITE_ID")
	authToken := os.Getenv("PG_SWARM_AUTH_TOKEN")
	if satID != "" && authToken != "" {
		log.Trace().Msg("identity loaded from env vars")
		return &Identity{SatelliteID: satID, AuthToken: authToken}, nil
	}

	if a.k8sClient == nil {
		log.Trace().Msg("env vars empty and K8s client unavailable, no identity found")
		return nil, os.ErrNotExist
	}

	ns := a.config.IdentitySecretNamespace
	name := a.config.IdentitySecretName
	log.Trace().Str("secret", ns+"/"+name).Msg("env vars empty, trying K8s secret")

	secret, err := a.k8sClient.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Trace().Str("secret", ns+"/"+name).Msg("identity secret not found")
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("read identity secret %s/%s: %w", ns, name, err)
	}

	satID = string(secret.Data["satellite-id"])
	authToken = string(secret.Data["auth-token"])
	if satID == "" || authToken == "" {
		log.Warn().Str("secret", ns+"/"+name).Msg("identity secret exists but missing required keys")
		return nil, os.ErrNotExist
	}

	log.Trace().Str("secret", ns+"/"+name).Msg("identity loaded from K8s secret")
	return &Identity{SatelliteID: satID, AuthToken: authToken}, nil
}

// saveIdentity upserts the K8s secret with the satellite identity.
// The secret keys are projected as env vars into the pod on next restart.
func (a *Agent) saveIdentity(ctx context.Context) error {
	if a.k8sClient == nil {
		return fmt.Errorf("K8s client unavailable — cannot persist identity secret")
	}
	if err := a.upsertIdentitySecret(ctx); err != nil {
		return err
	}
	log.Info().
		Str("secret", a.config.IdentitySecretNamespace+"/"+a.config.IdentitySecretName).
		Msg("identity saved to K8s secret")
	return nil
}

// upsertIdentitySecret creates or updates the K8s secret holding the
// satellite identity. The secret keys map to env vars injected into the pod.
func (a *Agent) upsertIdentitySecret(ctx context.Context) error {
	ns := a.config.IdentitySecretNamespace
	name := a.config.IdentitySecretName

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/part-of":   "pg-swarm",
				"app.kubernetes.io/component": "satellite-identity",
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"satellite-id": a.identity.SatelliteID,
			"auth-token":   a.identity.AuthToken,
		},
	}

	_, err := a.k8sClient.CoreV1().Secrets(ns).Create(ctx, secret, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create secret %s/%s: %w", ns, name, err)
	}

	_, err = a.k8sClient.CoreV1().Secrets(ns).Update(ctx, secret, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update secret %s/%s: %w", ns, name, err)
	}
	return nil
}

// buildK8sClient builds a Kubernetes client from in-cluster config (preferred
// when running as a pod) or from the default kubeconfig path (local dev).
func buildK8sClient() (*kubernetes.Clientset, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			loadingRules, &clientcmd.ConfigOverrides{},
		).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("build kubeconfig: %w", err)
		}
	}
	return kubernetes.NewForConfig(cfg)
}

// isTransient returns true for gRPC status codes that represent temporary
// conditions worth retrying (network unavailable, timeouts, overload).
func isTransient(code codes.Code) bool {
	switch code {
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted:
		return true
	}
	return false
}

// backoffDuration computes exponential backoff capped at max.
func backoffDuration(base time.Duration, attempt int, max time.Duration) time.Duration {
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	return d
}
