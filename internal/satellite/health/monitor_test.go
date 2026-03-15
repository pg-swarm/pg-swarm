package health

import (
	"testing"
	"time"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
)

func TestDeriveClusterState(t *testing.T) {
	mature := 20 * time.Minute // well past the 10-minute grace period
	young := 2 * time.Minute   // within the grace period

	tests := []struct {
		name             string
		instances        []*pgswarmv1.InstanceHealth
		expectedReplicas int32
		age              time.Duration
		want             pgswarmv1.ClusterState
	}{
		{
			name:             "no instances, mature cluster",
			instances:        nil,
			expectedReplicas: 3,
			age:              mature,
			want:             pgswarmv1.ClusterState_CLUSTER_STATE_FAILED,
		},
		{
			name:             "no instances, young cluster",
			instances:        nil,
			expectedReplicas: 3,
			age:              young,
			want:             pgswarmv1.ClusterState_CLUSTER_STATE_CREATING,
		},
		{
			name: "all ready with primary",
			instances: []*pgswarmv1.InstanceHealth{
				{PodName: "pg-0", Role: pgswarmv1.InstanceRole_INSTANCE_ROLE_PRIMARY, Ready: true},
				{PodName: "pg-1", Role: pgswarmv1.InstanceRole_INSTANCE_ROLE_REPLICA, Ready: true},
				{PodName: "pg-2", Role: pgswarmv1.InstanceRole_INSTANCE_ROLE_REPLICA, Ready: true},
			},
			expectedReplicas: 3,
			age:              mature,
			want:             pgswarmv1.ClusterState_CLUSTER_STATE_RUNNING,
		},
		{
			name: "primary ready but replica not ready",
			instances: []*pgswarmv1.InstanceHealth{
				{PodName: "pg-0", Role: pgswarmv1.InstanceRole_INSTANCE_ROLE_PRIMARY, Ready: true},
				{PodName: "pg-1", Role: pgswarmv1.InstanceRole_INSTANCE_ROLE_REPLICA, Ready: true},
				{PodName: "pg-2", Role: pgswarmv1.InstanceRole_INSTANCE_ROLE_REPLICA, Ready: false},
			},
			expectedReplicas: 3,
			age:              mature,
			want:             pgswarmv1.ClusterState_CLUSTER_STATE_DEGRADED,
		},
		{
			name: "primary ready but missing replicas",
			instances: []*pgswarmv1.InstanceHealth{
				{PodName: "pg-0", Role: pgswarmv1.InstanceRole_INSTANCE_ROLE_PRIMARY, Ready: true},
			},
			expectedReplicas: 3,
			age:              mature,
			want:             pgswarmv1.ClusterState_CLUSTER_STATE_DEGRADED,
		},
		{
			name: "no primary ready, mature cluster",
			instances: []*pgswarmv1.InstanceHealth{
				{PodName: "pg-0", Role: pgswarmv1.InstanceRole_INSTANCE_ROLE_REPLICA, Ready: true},
				{PodName: "pg-1", Role: pgswarmv1.InstanceRole_INSTANCE_ROLE_REPLICA, Ready: true},
			},
			expectedReplicas: 3,
			age:              mature,
			want:             pgswarmv1.ClusterState_CLUSTER_STATE_FAILED,
		},
		{
			name: "no primary ready, young cluster",
			instances: []*pgswarmv1.InstanceHealth{
				{PodName: "pg-0", Role: pgswarmv1.InstanceRole_INSTANCE_ROLE_REPLICA, Ready: true},
				{PodName: "pg-1", Role: pgswarmv1.InstanceRole_INSTANCE_ROLE_REPLICA, Ready: true},
			},
			expectedReplicas: 3,
			age:              young,
			want:             pgswarmv1.ClusterState_CLUSTER_STATE_CREATING,
		},
		{
			name: "primary not ready, mature cluster",
			instances: []*pgswarmv1.InstanceHealth{
				{PodName: "pg-0", Role: pgswarmv1.InstanceRole_INSTANCE_ROLE_PRIMARY, Ready: false},
				{PodName: "pg-1", Role: pgswarmv1.InstanceRole_INSTANCE_ROLE_REPLICA, Ready: true},
			},
			expectedReplicas: 2,
			age:              mature,
			want:             pgswarmv1.ClusterState_CLUSTER_STATE_FAILED,
		},
		{
			name: "primary not ready, young cluster",
			instances: []*pgswarmv1.InstanceHealth{
				{PodName: "pg-0", Role: pgswarmv1.InstanceRole_INSTANCE_ROLE_PRIMARY, Ready: false},
				{PodName: "pg-1", Role: pgswarmv1.InstanceRole_INSTANCE_ROLE_REPLICA, Ready: true},
			},
			expectedReplicas: 2,
			age:              young,
			want:             pgswarmv1.ClusterState_CLUSTER_STATE_CREATING,
		},
		{
			name: "single node cluster running",
			instances: []*pgswarmv1.InstanceHealth{
				{PodName: "pg-0", Role: pgswarmv1.InstanceRole_INSTANCE_ROLE_PRIMARY, Ready: true},
			},
			expectedReplicas: 1,
			age:              mature,
			want:             pgswarmv1.ClusterState_CLUSTER_STATE_RUNNING,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DeriveClusterState(tt.instances, tt.expectedReplicas, tt.age)
			if got != tt.want {
				t.Errorf("DeriveClusterState() = %v, want %v", got, tt.want)
			}
		})
	}
}
