package server

import (
	"encoding/json"
	"testing"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/pg-swarm/pg-swarm/internal/shared/models"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestProtoStateToModel(t *testing.T) {
	tests := []struct {
		input pgswarmv1.ClusterState
		want  models.ClusterState
	}{
		{pgswarmv1.ClusterState_CLUSTER_STATE_RUNNING, models.ClusterStateRunning},
		{pgswarmv1.ClusterState_CLUSTER_STATE_DEGRADED, models.ClusterStateDegraded},
		{pgswarmv1.ClusterState_CLUSTER_STATE_FAILED, models.ClusterStateFailed},
		{pgswarmv1.ClusterState_CLUSTER_STATE_DELETING, models.ClusterStateDeleting},
		{pgswarmv1.ClusterState_CLUSTER_STATE_CREATING, models.ClusterStateCreating},
		{pgswarmv1.ClusterState_CLUSTER_STATE_UNSPECIFIED, models.ClusterStateCreating},
	}
	for _, tt := range tests {
		t.Run(tt.input.String(), func(t *testing.T) {
			got := protoStateToModel(tt.input)
			if got != tt.want {
				t.Errorf("protoStateToModel(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestProtoInstancesToJSON(t *testing.T) {
	instances := []*pgswarmv1.InstanceHealth{
		{
			PodName:               "pg-0",
			Role:                  pgswarmv1.InstanceRole_INSTANCE_ROLE_PRIMARY,
			Ready:                 true,
			ReplicationLagBytes:   1024,
			ConnectionsUsed:       15,
			ConnectionsMax:        100,
			DiskUsedBytes:         1073741824,
			TimelineId:            1,
			PgStartTime:           timestamppb.Now(),
		},
		{
			PodName:               "pg-1",
			Role:                  pgswarmv1.InstanceRole_INSTANCE_ROLE_REPLICA,
			Ready:                 true,
			ReplicationLagBytes:   512,
			ReplicationLagSeconds: 0.5,
			WalReceiverActive:     true,
			ConnectionsUsed:       8,
			ConnectionsMax:        100,
			DiskUsedBytes:         1073741824,
			TimelineId:            1,
		},
		{
			PodName:      "pg-2",
			Role:         pgswarmv1.InstanceRole_INSTANCE_ROLE_REPLICA,
			Ready:        false,
			ErrorMessage: "connection refused",
		},
	}

	raw, err := protoInstancesToJSON(instances)
	if err != nil {
		t.Fatalf("protoInstancesToJSON() error = %v", err)
	}

	var parsed []instanceJSON
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if len(parsed) != 3 {
		t.Fatalf("expected 3 instances, got %d", len(parsed))
	}

	// Primary
	p := parsed[0]
	if p.PodName != "pg-0" || p.Role != "primary" || !p.Ready {
		t.Errorf("instance[0] basic fields mismatch: %+v", p)
	}
	if p.ConnectionsUsed != 15 || p.ConnectionsMax != 100 {
		t.Errorf("instance[0] connections mismatch: %d/%d", p.ConnectionsUsed, p.ConnectionsMax)
	}
	if p.DiskUsedBytes != 1073741824 {
		t.Errorf("instance[0] disk mismatch: %d", p.DiskUsedBytes)
	}
	if p.TimelineID != 1 {
		t.Errorf("instance[0] timeline mismatch: %d", p.TimelineID)
	}
	if p.PgStartTime == "" {
		t.Error("instance[0] pg_start_time should be set")
	}

	// Replica with WAL
	r := parsed[1]
	if r.Role != "replica" || !r.WalReceiverActive {
		t.Errorf("instance[1] replica fields mismatch: %+v", r)
	}
	if r.ReplicationLagSeconds != 0.5 {
		t.Errorf("instance[1] lag_seconds mismatch: %f", r.ReplicationLagSeconds)
	}

	// Failed replica
	f := parsed[2]
	if f.Ready || f.ErrorMessage != "connection refused" {
		t.Errorf("instance[2] mismatch: %+v", f)
	}
}
