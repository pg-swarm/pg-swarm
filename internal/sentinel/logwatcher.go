package sentinel

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/google/uuid"
	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/rs/zerolog/log"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// RecoveryRule is a single log-matching rule loaded from the mounted ConfigMap.
type RecoveryRule struct {
	Name                   string `json:"name"`
	Pattern                string `json:"pattern"`
	Severity               string `json:"severity"`
	Action                 string `json:"action"`
	ExecCommand            string `json:"exec_command,omitempty"`
	CooldownSeconds        int32  `json:"cooldown_seconds"`
	Enabled                bool   `json:"enabled"`
	Category               string `json:"category"`
	Threshold              int32  `json:"threshold"`
	ThresholdWindowSeconds int32  `json:"threshold_window_seconds"`
}

// compiledRule is a RecoveryRule with a pre-compiled regexp.
type compiledRule struct {
	RecoveryRule
	re *regexp.Regexp
}

// EventEmitter sends detection events to the satellite via the gRPC stream.
// SidecarConnector implements this interface.
type EventEmitter interface {
	EmitEvent(evt *pgswarmv1.Event)
}

// LogWatcher tails PostgreSQL container logs and emits events when recovery
// rules match. It does not execute any actions — the satellite decides what
// commands to send back through the sidecar command infrastructure.
type LogWatcher struct {
	client      kubernetes.Interface
	emitter     EventEmitter
	rulesPath   string
	podName     string
	namespace   string
	clusterName string

	mu           sync.Mutex
	rules        []compiledRule
	cooldowns    map[string]time.Time
	rulesModTime time.Time

	matchTimes map[string][]time.Time // per-rule sliding window of match timestamps
}

// NewLogWatcher creates a log watcher that reads rules from the given file path.
func NewLogWatcher(client kubernetes.Interface, emitter EventEmitter, rulesPath, podName, namespace, clusterName string) *LogWatcher {
	return &LogWatcher{
		client:      client,
		emitter:     emitter,
		rulesPath:   rulesPath,
		podName:     podName,
		namespace:   namespace,
		clusterName: clusterName,
		cooldowns:   make(map[string]time.Time),
		matchTimes:  make(map[string][]time.Time),
	}
}

// Run starts the log watcher. It loads rules, tails PG logs, and fires actions.
func (lw *LogWatcher) Run(ctx context.Context) {
	// Load initial rules
	lw.reloadRules()

	// Watch for rule file changes in background
	go lw.watchRuleFile(ctx)

	// Tail PG logs
	lw.tailLogs(ctx)
}

// reloadRules reads the rules JSON file and compiles regexps.
func (lw *LogWatcher) reloadRules() {
	data, err := os.ReadFile(lw.rulesPath)
	if err != nil {
		log.Warn().Err(err).Str("path", lw.rulesPath).Msg("logwatcher: failed to read rules file")
		return
	}

	info, _ := os.Stat(lw.rulesPath)
	if info != nil && info.ModTime().Equal(lw.rulesModTime) {
		return // unchanged
	}

	var raw []RecoveryRule
	if err := json.Unmarshal(data, &raw); err != nil {
		log.Error().Err(err).Msg("logwatcher: failed to parse rules JSON")
		return
	}

	var compiled []compiledRule
	for _, r := range raw {
		if !r.Enabled {
			continue
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			log.Warn().Str("rule", r.Name).Str("pattern", r.Pattern).Err(err).Msg("logwatcher: invalid regex, skipping rule")
			continue
		}
		compiled = append(compiled, compiledRule{RecoveryRule: r, re: re})
	}

	lw.mu.Lock()
	lw.rules = compiled
	if info != nil {
		lw.rulesModTime = info.ModTime()
	}
	lw.mu.Unlock()

	log.Info().Int("rules", len(compiled)).Msg("logwatcher: rules loaded")
}

// watchRuleFile polls the rules file for changes (K8s ConfigMap updates propagate within ~60s).
func (lw *LogWatcher) watchRuleFile(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			lw.reloadRules()
		}
	}
}

// tailLogs uses the K8s log API to follow the postgres container's stdout/stderr.
func (lw *LogWatcher) tailLogs(ctx context.Context) {
	for {
		if err := lw.streamLogs(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warn().Err(err).Msg("logwatcher: log stream ended, retrying in 5s")
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}

func (lw *LogWatcher) streamLogs(ctx context.Context) error {
	sinceSeconds := int64(10)
	req := lw.client.CoreV1().Pods(lw.namespace).GetLogs(lw.podName, &corev1.PodLogOptions{
		Container:    "postgres",
		Follow:       true,
		SinceSeconds: &sinceSeconds,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return fmt.Errorf("open log stream: %w", err)
	}
	defer stream.Close()

	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		line := scanner.Text()
		lw.matchLine(line)
	}
	return scanner.Err()
}

// matchLine checks a log line against all compiled rules.
func (lw *LogWatcher) matchLine(line string) {
	lw.mu.Lock()
	rules := lw.rules
	lw.mu.Unlock()

	for i := range rules {
		r := &rules[i]
		if !r.re.MatchString(line) {
			continue
		}

		// Check cooldown
		if last, ok := lw.cooldowns[r.Name]; ok {
			if time.Since(last) < time.Duration(r.CooldownSeconds)*time.Second {
				continue
			}
		}

		// Threshold check: count matches within the window
		thresh := r.Threshold
		if thresh <= 0 {
			thresh = 1 // default: fire on first match
		}
		if thresh > 1 && r.ThresholdWindowSeconds > 0 {
			window := time.Duration(r.ThresholdWindowSeconds) * time.Second
			now := time.Now()
			cutoff := now.Add(-window)

			// Append this match, prune old entries
			times := lw.matchTimes[r.Name]
			times = append(times, now)
			pruned := times[:0]
			for _, t := range times {
				if t.After(cutoff) {
					pruned = append(pruned, t)
				}
			}
			lw.matchTimes[r.Name] = pruned

			if int32(len(pruned)) < thresh {
				continue // threshold not yet reached
			}
			// Threshold breached — clear the window and fire
			lw.matchTimes[r.Name] = nil
		}

		lw.cooldowns[r.Name] = time.Now()

		log.Warn().
			Str("rule", r.Name).
			Str("action", r.Action).
			Str("severity", r.Severity).
			Str("matched", truncate(line, 120)).
			Msg("logwatcher: rule fired")

		lw.emitEvent(r, line)
	}
}

// emitEvent sends a detection event to the satellite via the EventEmitter.
// The satellite decides what action to take and sends a command back.
func (lw *LogWatcher) emitEvent(r *compiledRule, matchedLine string) {
	if lw.emitter == nil {
		return
	}

	evt := &pgswarmv1.Event{
		Id:          uuid.NewString(),
		Type:        "log.rule." + r.Name,
		ClusterName: lw.clusterName,
		Namespace:   lw.namespace,
		PodName:     lw.podName,
		Severity:    r.Severity,
		Source:      "sidecar",
		Timestamp:   timestamppb.Now(),
		Data: map[string]string{
			"rule_name":    r.Name,
			"action":       r.Action,
			"category":     r.Category,
			"matched_line": truncate(matchedLine, 200),
		},
	}

	lw.emitter.EmitEvent(evt)
}

// execInContainer is a convenience wrapper for the shared execInPod helper.
func (m *Monitor) execInContainer(ctx context.Context, command string) {
	if err := execInPod(ctx, m.client, m.cfg.RestConfig, m.cfg.PodName, m.cfg.Namespace, command); err != nil {
		log.Error().Err(err).Str("command", command).Msg("exec failed")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
