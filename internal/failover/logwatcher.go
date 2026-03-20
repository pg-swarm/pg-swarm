package failover

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// RecoveryRule is a single log-matching rule loaded from the mounted ConfigMap.
type RecoveryRule struct {
	Name            string `json:"name"`
	Pattern         string `json:"pattern"`
	Severity        string `json:"severity"`
	Action          string `json:"action"`
	ExecCommand     string `json:"exec_command,omitempty"`
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

// Action severity ordering for the mutex: higher number = more destructive.
var actionSeverity = map[string]int{
	"event":       0,
	"exec":        1,
	"restart":     2,
	"rewind":      3,
	"rebasebackup": 4,
}

// LogWatcher tails PostgreSQL container logs and fires recovery rules.
type LogWatcher struct {
	monitor   *Monitor
	client    kubernetes.Interface
	rulesPath string
	podName   string
	namespace string

	mu            sync.Mutex
	rules         []compiledRule
	cooldowns     map[string]time.Time
	actionRunning string
	pendingAction *compiledRule
	rulesModTime  time.Time

	matchTimes map[string][]time.Time // per-rule sliding window of match timestamps
}

// NewLogWatcher creates a log watcher that reads rules from the given file path.
func NewLogWatcher(mon *Monitor, client kubernetes.Interface, rulesPath, podName, namespace string) *LogWatcher {
	return &LogWatcher{
		monitor:   mon,
		client:    client,
		rulesPath: rulesPath,
		podName:   podName,
		namespace: namespace,
		cooldowns:  make(map[string]time.Time),
		matchTimes: make(map[string][]time.Time),
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

		// Events are non-destructive — always fire, skip mutex
		if r.Action == "event" {
			continue
		}

		lw.dispatchAction(r)
	}
}

// dispatchAction handles the action mutex: one action at a time, higher severity supersedes.
func (lw *LogWatcher) dispatchAction(r *compiledRule) {
	lw.mu.Lock()

	if lw.actionRunning != "" {
		runningSev := actionSeverity[lw.actionRunning]
		incomingSev := actionSeverity[r.Action]

		if incomingSev <= runningSev {
			log.Info().Str("rule", r.Name).Str("action", r.Action).Str("running", lw.actionRunning).Msg("logwatcher: action dropped (lower/equal severity already running)")
			lw.mu.Unlock()
			return
		}

		// Queue as pending (supersedes any existing pending)
		log.Info().Str("rule", r.Name).Str("action", r.Action).Str("running", lw.actionRunning).Msg("logwatcher: action queued (supersedes running)")
		rc := *r
		lw.pendingAction = &rc
		lw.mu.Unlock()
		return
	}

	lw.actionRunning = r.Action
	lw.mu.Unlock()

	go lw.executeAction(r)
}

// executeAction runs the recovery action, then checks for a pending action.
func (lw *LogWatcher) executeAction(r *compiledRule) {
	log.Info().Str("rule", r.Name).Str("action", r.Action).Msg("logwatcher: executing action")

	ctx := context.Background()
	switch r.Action {
	case "restart":
		// Stop PG — wrapper loop handles recovery
		lw.monitor.execInContainer(ctx, "pg_ctl stop -m fast -D /var/lib/postgresql/data/pgdata")
	case "rewind":
		// Create standby.signal + stop PG — wrapper runs pg_rewind
		if err := lw.monitor.rewindOrReinit(ctx); err != nil {
			log.Error().Err(err).Str("rule", r.Name).Msg("logwatcher: rewind action failed")
		}
	case "rebasebackup":
		// Write marker + stop PG — wrapper nukes PGDATA and rebuilds
		lw.monitor.execInContainer(ctx, fmt.Sprintf(
			"echo 'rule:%s' > %s && pg_ctl stop -m fast -D /var/lib/postgresql/data/pgdata",
			r.Name, pgSwarmNeedsBasebackup))
	case "exec":
		if r.ExecCommand != "" {
			lw.monitor.execInContainer(ctx, r.ExecCommand)
		}
	}

	// Clear running, check pending
	lw.mu.Lock()
	lw.actionRunning = ""
	pending := lw.pendingAction
	lw.pendingAction = nil
	lw.mu.Unlock()

	if pending != nil {
		log.Info().Str("rule", pending.Name).Str("action", pending.Action).Msg("logwatcher: running queued action")
		lw.dispatchAction(pending)
	}
}

// execInContainer is a convenience wrapper for the monitor's exec capability.
func (m *Monitor) execInContainer(ctx context.Context, command string) {
	script := fmt.Sprintf("set -e\n%s", command)
	req := m.client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(m.cfg.PodName).
		Namespace(m.cfg.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "postgres",
			Command:   []string{"bash", "-c", script},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(m.cfg.RestConfig, "POST", req.URL())
	if err != nil {
		log.Error().Err(err).Str("command", command).Msg("logwatcher: exec setup failed")
		return
	}

	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		log.Error().Err(err).Str("stdout", stdout.String()).Str("stderr", stderr.String()).Msg("logwatcher: exec failed")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
