package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
)

// Notifier sends backup completion notifications from replica to primary sidecar.
type Notifier struct {
	clusterName string
	namespace   string
	httpClient  *http.Client
}

// NewNotifier creates a new replica→primary notifier.
func NewNotifier(clusterName, namespace string) *Notifier {
	return &Notifier{
		clusterName: clusterName,
		namespace:   namespace,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// NotifyBackupComplete sends the backup completion to the primary pod's sidecar.
// It discovers the primary via the headless service and pod-0 (ordinal 0 is typically primary).
// Retries with backoff on failure.
func (n *Notifier) NotifyBackupComplete(ctx context.Context, req *BackupCompleteRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal notification: %w", err)
	}

	// Try to reach primary via headless service pod DNS
	// Pattern: {cluster}-0.{cluster}-headless.{namespace}.svc.cluster.local:8442
	primaryURL := fmt.Sprintf("http://%s-0.%s-headless.%s.svc.cluster.local:8442/backup/complete",
		n.clusterName, n.clusterName, n.namespace)

	// Retry with backoff
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt*attempt) * time.Second
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, primaryURL, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := n.httpClient.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("POST %s: %w", primaryURL, err)
			log.Warn().Err(lastErr).Int("attempt", attempt+1).Msg("backup notification failed, retrying")
			continue
		}
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			log.Info().Str("backup_id", req.ID).Msg("backup notification sent to primary")
			return nil
		}
		lastErr = fmt.Errorf("primary returned status %d", resp.StatusCode)
		log.Warn().Err(lastErr).Int("attempt", attempt+1).Msg("backup notification failed, retrying")
	}

	return fmt.Errorf("backup notification failed after 5 attempts: %w", lastErr)
}
