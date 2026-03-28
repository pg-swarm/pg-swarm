package eventbus

import (
	"context"
	"sync"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/pg-swarm/pg-swarm/internal/satellite/sidecar"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// actionToCommand maps recovery rule action names to sidecar event command types.
var actionToCommand = map[string]string{
	"restart":      "command.restart",
	"rewind":       "command.rewind",
	"rebasebackup": "command.rebuild",
	"rebuild":      "command.rebuild",
	"reload":       "command.reload_conf",
}

// actionSeverity orders actions by destructiveness for the per-pod mutex.
var actionSeverity = map[string]int{
	"reload":       0,
	"restart":      1,
	"rewind":       2,
	"rebasebackup": 3,
	"rebuild":      3,
}

// LogRuleHandler receives log.rule.* events from sentinel sidecars and sends
// the appropriate command back through the sidecar stream.
type LogRuleHandler struct {
	streams *sidecar.SidecarStreamManager
	bus     *EventBus
	logger  zerolog.Logger

	mu      sync.Mutex
	running map[string]string // "ns/pod" → action currently executing
}

// NewLogRuleHandler creates a handler that routes log rule events to sidecar commands.
func NewLogRuleHandler(sm *sidecar.SidecarStreamManager, bus *EventBus) *LogRuleHandler {
	return &LogRuleHandler{
		streams: sm,
		bus:     bus,
		logger:  log.With().Str("component", "logrule-handler").Logger(),
		running: make(map[string]string),
	}
}

// Register subscribes this handler to log.rule.* events on the EventBus.
func (h *LogRuleHandler) Register() {
	h.bus.Subscribe("log.rule.*", "log-rule-handler", h.handleLogRule)
}

func (h *LogRuleHandler) handleLogRule(ctx context.Context, evt *pgswarmv1.Event) error {
	action := evt.Data["action"]
	ruleName := evt.Data["rule_name"]
	podName := evt.GetPodName()
	namespace := evt.GetNamespace()

	commandType, ok := actionToCommand[action]
	if !ok {
		// No command mapping — event-only rule, just forwarded for observability
		h.logger.Debug().Str("rule", ruleName).Str("action", action).Msg("no command mapping, event-only")
		return nil
	}

	podKey := namespace + "/" + podName

	// Per-pod severity mutex: only one destructive action at a time per pod.
	// Higher severity supersedes; lower/equal severity is dropped.
	h.mu.Lock()
	if runningAction, busy := h.running[podKey]; busy {
		runningSev := actionSeverity[runningAction]
		incomingSev := actionSeverity[action]
		if incomingSev <= runningSev {
			h.mu.Unlock()
			h.logger.Info().Str("pod", podKey).Str("action", action).Str("running", runningAction).
				Msg("action dropped (lower/equal severity already running)")
			return nil
		}
		h.logger.Info().Str("pod", podKey).Str("action", action).Str("running", runningAction).
			Msg("higher severity action will execute after current completes")
	}
	h.running[podKey] = action
	h.mu.Unlock()

	go func() {
		defer func() {
			h.mu.Lock()
			delete(h.running, podKey)
			h.mu.Unlock()
		}()

		h.logger.Info().Str("rule", ruleName).Str("command", commandType).Str("pod", podKey).
			Msg("dispatching command for log rule")

		result, err := h.streams.SendEventCommandAndWait(ctx, namespace, podName, commandType, evt.Data)

		resultEvt := NewPodEvent("log.rule.action.completed", evt.GetClusterName(), namespace, podName, "satellite")
		WithData(resultEvt, "rule_name", ruleName)
		WithData(resultEvt, "action", action)
		WithData(resultEvt, "command", commandType)

		if err != nil {
			h.logger.Error().Err(err).Str("rule", ruleName).Str("command", commandType).
				Msg("command dispatch failed")
			WithData(resultEvt, "success", "false")
			WithData(resultEvt, "error", err.Error())
			WithSeverity(resultEvt, "error")
		} else if !result.Success {
			h.logger.Warn().Str("rule", ruleName).Str("error", result.Error).
				Msg("command executed but failed")
			WithData(resultEvt, "success", "false")
			WithData(resultEvt, "error", result.Error)
			WithSeverity(resultEvt, "warning")
		} else {
			h.logger.Info().Str("rule", ruleName).Str("command", commandType).
				Msg("command completed successfully")
			WithData(resultEvt, "success", "true")
		}

		_ = h.bus.Publish(ctx, resultEvt)
	}()

	return nil
}
