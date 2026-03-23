package backup

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// backupConfigCMName returns the ConfigMap name for backup configuration.
func backupConfigCMName(clusterName string) string {
	return clusterName + "-backup-config"
}

// ConfigWatcher polls a backup-config ConfigMap for schedule/retention changes
// and invokes a callback when the config differs from the current state.
type ConfigWatcher struct {
	namespace   string
	clusterName string
	client      kubernetes.Interface
	onChange    func(ScheduleConfig)
	interval   time.Duration
}

// ScheduleConfig holds the reloadable subset of backup configuration.
type ScheduleConfig struct {
	BaseSchedule  string
	IncrSchedule  string
	LogicSchedule string
	RetentionSets int
	RetentionDays int
}

// NewConfigWatcher creates a watcher that polls the backup-config ConfigMap.
func NewConfigWatcher(namespace, clusterName string, onChange func(ScheduleConfig)) *ConfigWatcher {
	w := &ConfigWatcher{
		namespace:   namespace,
		clusterName: clusterName,
		onChange:    onChange,
		interval:   15 * time.Second,
	}
	k8sCfg, err := rest.InClusterConfig()
	if err != nil {
		log.Warn().Err(err).Msg("K8s client unavailable for config watcher")
		return w
	}
	client, err := kubernetes.NewForConfig(k8sCfg)
	if err != nil {
		log.Warn().Err(err).Msg("K8s client creation failed for config watcher")
		return w
	}
	w.client = client
	return w
}

// Run polls the ConfigMap until ctx is cancelled.
func (w *ConfigWatcher) Run(ctx context.Context, current ScheduleConfig) {
	if w.client == nil {
		log.Warn().Msg("config watcher disabled (no K8s client)")
		return
	}

	log.Info().
		Str("configmap", backupConfigCMName(w.clusterName)).
		Dur("interval", w.interval).
		Msg("config watcher started")

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sc, err := w.readConfig(ctx)
			if err != nil {
				log.Debug().Err(err).Msg("config watcher: failed to read ConfigMap")
				continue
			}
			if sc != current {
				log.Info().
					Str("base", sc.BaseSchedule).
					Str("incr", sc.IncrSchedule).
					Str("logical", sc.LogicSchedule).
					Int("retention_sets", sc.RetentionSets).
					Int("retention_days", sc.RetentionDays).
					Msg("config change detected")
				current = sc
				w.onChange(sc)
			}
		}
	}
}

// readConfig reads the backup-config ConfigMap and parses schedule/retention fields.
func (w *ConfigWatcher) readConfig(ctx context.Context) (ScheduleConfig, error) {
	cmName := backupConfigCMName(w.clusterName)
	cm, err := w.client.CoreV1().ConfigMaps(w.namespace).Get(ctx, cmName, metav1.GetOptions{})
	if err != nil {
		return ScheduleConfig{}, fmt.Errorf("get configmap %s: %w", cmName, err)
	}

	sc := ScheduleConfig{
		BaseSchedule:  cm.Data["base_schedule"],
		IncrSchedule:  cm.Data["incr_schedule"],
		LogicSchedule: cm.Data["logical_schedule"],
	}
	if v, err := strconv.Atoi(cm.Data["retention_sets"]); err == nil {
		sc.RetentionSets = v
	}
	if v, err := strconv.Atoi(cm.Data["retention_days"]); err == nil {
		sc.RetentionDays = v
	}
	return sc, nil
}
