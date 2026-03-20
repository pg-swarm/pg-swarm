package server

import (
	"sync"
	"time"
)

// SwitchoverStep represents a single step in a switchover operation.
type SwitchoverStep struct {
	Step         int32  `json:"step"`
	StepName     string `json:"step_name"`
	Status       string `json:"status"`
	TargetPod    string `json:"target_pod"`
	ErrorMessage string `json:"error_message"`
	PONR         bool   `json:"ponr"`
}

// SwitchoverOp tracks the state of an in-flight switchover operation.
type SwitchoverOp struct {
	OperationID string                    `json:"operation_id"`
	ClusterName string                    `json:"cluster_name"`
	PrimaryPod  string                    `json:"primary_pod"`
	TargetPod   string                    `json:"target_pod"`
	Done        bool                      `json:"done"`
	Success     bool                      `json:"success"`
	Error       string                    `json:"error,omitempty"`
	Steps       map[int32]*SwitchoverStep `json:"steps"`
	startedAt   time.Time
}

// OpsTracker manages in-memory state for active switchover operations.
type OpsTracker struct {
	mu  sync.RWMutex
	ops map[string]*SwitchoverOp
}

// NewOpsTracker creates an OpsTracker that auto-expires entries after 5 minutes.
func NewOpsTracker() *OpsTracker {
	t := &OpsTracker{
		ops: make(map[string]*SwitchoverOp),
	}
	go t.expireLoop()
	return t
}

// Start registers a new switchover operation.
func (t *OpsTracker) Start(operationID, clusterName, primaryPod, targetPod string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ops[operationID] = &SwitchoverOp{
		OperationID: operationID,
		ClusterName: clusterName,
		PrimaryPod:  primaryPod,
		TargetPod:   targetPod,
		Steps:       make(map[int32]*SwitchoverStep),
		startedAt:   time.Now(),
	}
}

// UpdateStep updates a single step within a tracked operation.
func (t *OpsTracker) UpdateStep(operationID string, step int32, stepName, status, targetPod, errorMsg string, ponr bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	op, ok := t.ops[operationID]
	if !ok {
		return
	}
	op.Steps[step] = &SwitchoverStep{
		Step:         step,
		StepName:     stepName,
		Status:       status,
		TargetPod:    targetPod,
		ErrorMessage: errorMsg,
		PONR:         ponr,
	}
}

// SetPrimaryPod updates the primary pod name for a tracked operation.
func (t *OpsTracker) SetPrimaryPod(operationID, podName string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if op, ok := t.ops[operationID]; ok {
		op.PrimaryPod = podName
	}
}

// Complete marks a switchover operation as finished.
func (t *OpsTracker) Complete(operationID string, success bool, errMsg string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	op, ok := t.ops[operationID]
	if !ok {
		return
	}
	op.Done = true
	op.Success = success
	op.Error = errMsg
}

// GetActiveOps returns a snapshot of all tracked operations.
func (t *OpsTracker) GetActiveOps() map[string]*SwitchoverOp {
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := make(map[string]*SwitchoverOp, len(t.ops))
	for k, v := range t.ops {
		result[k] = v
	}
	return result
}

func (t *OpsTracker) expireLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		t.mu.Lock()
		now := time.Now()
		for id, op := range t.ops {
			if now.Sub(op.startedAt) > 5*time.Minute {
				delete(t.ops, id)
			}
		}
		t.mu.Unlock()
	}
}
