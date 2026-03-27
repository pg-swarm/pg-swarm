//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// K8sHelper wraps kubectl and k8s client operations for test helpers.
type K8sHelper struct {
	Client    kubernetes.Interface
	Namespace string
}

// PodInfo holds pod metadata extracted from kubectl JSON output.
type PodInfo struct {
	Name   string
	Phase  string
	Role   string
	Labels map[string]string
}

// GetClusterPods returns pods matching the cluster label.
func (h *K8sHelper) GetClusterPods(clusterName string) ([]PodInfo, error) {
	pods, err := h.Client.CoreV1().Pods(h.Namespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: "pg-swarm.io/cluster=" + clusterName,
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	var result []PodInfo
	for _, p := range pods.Items {
		result = append(result, PodInfo{
			Name:   p.Name,
			Phase:  string(p.Status.Phase),
			Role:   p.Labels["pg-swarm.io/role"],
			Labels: p.Labels,
		})
	}
	return result, nil
}

// GetPrimaryPod returns the pod name with role=primary, or error if none/multiple.
func (h *K8sHelper) GetPrimaryPod(clusterName string) (string, error) {
	pods, err := h.GetClusterPods(clusterName)
	if err != nil {
		return "", err
	}
	var primaries []string
	for _, p := range pods {
		if p.Role == "primary" {
			primaries = append(primaries, p.Name)
		}
	}
	if len(primaries) == 0 {
		return "", fmt.Errorf("no primary pod found")
	}
	if len(primaries) > 1 {
		return "", fmt.Errorf("SPLIT-BRAIN: multiple primaries: %v", primaries)
	}
	return primaries[0], nil
}

// GetReplicaPod returns the first pod with role=replica.
func (h *K8sHelper) GetReplicaPod(clusterName string) (string, error) {
	pods, err := h.GetClusterPods(clusterName)
	if err != nil {
		return "", err
	}
	for _, p := range pods {
		if p.Role == "replica" {
			return p.Name, nil
		}
	}
	return "", fmt.Errorf("no replica pod found")
}

// CountPrimaries returns the number of pods labeled role=primary.
func (h *K8sHelper) CountPrimaries(clusterName string) (int, []string) {
	pods, err := h.GetClusterPods(clusterName)
	if err != nil {
		return 0, nil
	}
	var primaries []string
	for _, p := range pods {
		if p.Role == "primary" {
			primaries = append(primaries, p.Name)
		}
	}
	return len(primaries), primaries
}

// Kubectl runs a kubectl command and returns stdout.
func (h *K8sHelper) Kubectl(args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// DeletePod force-deletes a pod.
func (h *K8sHelper) DeletePod(name string) error {
	_, err := h.Kubectl("delete", "pod", name, "-n", h.Namespace, "--force", "--grace-period=0")
	return err
}

// ExecInPod runs a command in a container.
func (h *K8sHelper) ExecInPod(pod, container string, cmd ...string) (string, error) {
	args := []string{"exec", pod, "-n", h.Namespace, "-c", container, "--"}
	args = append(args, cmd...)
	return h.Kubectl(args...)
}

// DeletePGDATA removes PGDATA contents inside a pod.
func (h *K8sHelper) DeletePGDATA(pod string) error {
	_, err := h.ExecInPod(pod, "postgres", "sh", "-c", "rm -rf /var/lib/postgresql/data/pgdata/*")
	return err
}

// StopPostgres issues pg_ctl stop -m immediate.
func (h *K8sHelper) StopPostgres(pod string) error {
	_, err := h.ExecInPod(pod, "postgres", "pg_ctl", "stop", "-m", "immediate", "-D", "/var/lib/postgresql/data/pgdata")
	return err
}

// DeleteFile removes a specific file inside PGDATA.
func (h *K8sHelper) DeleteFile(pod, path string) error {
	_, err := h.ExecInPod(pod, "postgres", "rm", "-f", path)
	return err
}

// StartPortForward starts kubectl port-forward to the central pod and waits for it to be ready.
func StartPortForward(ctx context.Context, k8sClient kubernetes.Interface, systemNS, httpPort, grpcPort string) error {
	pods, err := k8sClient.CoreV1().Pods(systemNS).List(context.Background(), metav1.ListOptions{
		LabelSelector: "app=pg-swarm-central",
	})
	if err != nil || len(pods.Items) == 0 {
		return fmt.Errorf("central pod not found in %s: %v", systemNS, err)
	}
	podName := pods.Items[0].Name

	cmd := exec.CommandContext(ctx, "kubectl", "port-forward",
		"-n", systemNS, "pod/"+podName,
		httpPort+":8080", grpcPort+":9090")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start port-forward: %w", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", "localhost:"+httpPort, time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("port-forward not ready after 15s")
}

// WaitFor polls a condition with a timeout. Returns error on timeout.
// Poll interval is 5s for timeouts > 30s, otherwise 2s.
func WaitFor(timeout time.Duration, desc string, check func() bool) error {
	return WaitForInterval(timeout, 0, desc, check)
}

// WaitForInterval polls a condition at the given interval. If interval is 0,
// it defaults to 5s for timeouts > 30s, otherwise 2s.
func WaitForInterval(timeout, interval time.Duration, desc string, check func() bool) error {
	if interval == 0 {
		interval = 5 * time.Second
		if timeout <= 30*time.Second {
			interval = 2 * time.Second
		}
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return nil
		}
		time.Sleep(interval)
	}
	return fmt.Errorf("timeout waiting for %s (%v)", desc, timeout)
}

// WatchPodsReady uses a K8s watch to wait until all cluster pods are Running
// and Ready, with at least one primary and one replica. Returns immediately
// when the condition is met instead of polling on an interval.
func (h *K8sHelper) WatchPodsReady(clusterName string, minReplicas int, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	selector := "pg-swarm.io/cluster=" + clusterName
	watcher, err := h.Client.CoreV1().Pods(h.Namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return fmt.Errorf("start watch: %w", err)
	}
	defer watcher.Stop()

	// Check current state first (the watch might have missed prior events)
	if h.podsHealthy(clusterName, minReplicas) {
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for pods ready (%v)", timeout)
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return fmt.Errorf("watch channel closed")
			}
			_ = event // we just use the event as a trigger to recheck
			if h.podsHealthy(clusterName, minReplicas) {
				return nil
			}
		}
	}
}

// podsHealthy returns true if all pods are Running+Ready with at least 1
// primary and minReplicas replicas.
func (h *K8sHelper) podsHealthy(clusterName string, minReplicas int) bool {
	pods, err := h.Client.CoreV1().Pods(h.Namespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: "pg-swarm.io/cluster=" + clusterName,
	})
	if err != nil || len(pods.Items) == 0 {
		return false
	}

	primaries, replicas := 0, 0
	for _, p := range pods.Items {
		if p.Status.Phase != "Running" {
			return false
		}
		ready := false
		for _, c := range p.Status.ContainerStatuses {
			if c.Name == "postgres" && c.Ready {
				ready = true
			}
		}
		if !ready {
			return false
		}
		switch p.Labels["pg-swarm.io/role"] {
		case "primary":
			primaries++
		case "replica":
			replicas++
		}
	}
	return primaries == 1 && replicas >= minReplicas
}

// GetPodUID returns the UID of a named pod.
func (h *K8sHelper) GetPodUID(name string) (string, error) {
	pod, err := h.Client.CoreV1().Pods(h.Namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return string(pod.UID), nil
}

// WatchForNewPrimary uses a K8s watch to wait until a primary pod exists whose
// UID differs from excludeUID. This handles StatefulSet pods which get recreated
// with the same name but a different UID after deletion.
func (h *K8sHelper) WatchForNewPrimary(clusterName, excludeUID string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	selector := "pg-swarm.io/cluster=" + clusterName
	watcher, err := h.Client.CoreV1().Pods(h.Namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return "", fmt.Errorf("start watch: %w", err)
	}
	defer watcher.Stop()

	// Check current state first
	if name := h.findPrimaryExcludingUID(clusterName, excludeUID); name != "" {
		return name, nil
	}

	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("timeout waiting for new primary (%v)", timeout)
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return "", fmt.Errorf("watch channel closed")
			}
			_ = event
			if name := h.findPrimaryExcludingUID(clusterName, excludeUID); name != "" {
				return name, nil
			}
		}
	}
}

func (h *K8sHelper) findPrimaryExcludingUID(clusterName, excludeUID string) string {
	pods, err := h.Client.CoreV1().Pods(h.Namespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: "pg-swarm.io/cluster=" + clusterName + ",pg-swarm.io/role=primary",
	})
	if err != nil {
		return ""
	}
	for _, p := range pods.Items {
		if string(p.UID) != excludeUID {
			return p.Name
		}
	}
	return ""
}

// PGClient runs SQL against the cluster via kubectl exec + psql inside a pod.
// No local psql or port-forward needed.
type PGClient struct {
	k8s         *K8sHelper
	clusterName string
}

// NewPGClient creates a client that executes SQL via kubectl exec on the
// cluster's primary pod using the RW service as the host.
func NewPGClient(k8s *K8sHelper, clusterName, _ string) (*PGClient, error) {
	// Verify we can find the primary pod
	_, err := k8s.GetPrimaryPod(clusterName)
	if err != nil {
		return nil, fmt.Errorf("no primary pod available: %w", err)
	}
	return &PGClient{k8s: k8s, clusterName: clusterName}, nil
}

// Exec runs SQL on the current primary pod via kubectl exec + psql.
// Connects to localhost (same pod) so it always hits the local PG instance.
func (p *PGClient) Exec(sql string) (string, error) {
	primary, err := p.k8s.GetPrimaryPod(p.clusterName)
	if err != nil {
		return "", fmt.Errorf("no primary pod: %w", err)
	}
	out, err := p.k8s.ExecInPod(primary, "postgres",
		"psql", "-U", "postgres", "-d", "postgres", "-tAc", sql)
	return strings.TrimSpace(out), err
}

// ExecOnReplica runs SQL on a replica pod (for verifying replication).
func (p *PGClient) ExecOnReplica(sql string) (string, error) {
	replica, err := p.k8s.GetReplicaPod(p.clusterName)
	if err != nil {
		return "", fmt.Errorf("no replica pod: %w", err)
	}
	out, err := p.k8s.ExecInPod(replica, "postgres",
		"psql", "-U", "postgres", "-d", "postgres", "-tAc", sql)
	return strings.TrimSpace(out), err
}

// Close is a no-op (no port-forward to stop).
func (p *PGClient) Close() {}

// PodLabelHistory tracks pod role labels over time for split-brain detection.
type PodLabelHistory struct {
	snapshots []labelSnapshot
}

type labelSnapshot struct {
	time      time.Time
	primaries []string
}

// Observe records a snapshot of which pods have role=primary.
func (h *PodLabelHistory) Observe(pods []PodInfo) {
	var primaries []string
	for _, p := range pods {
		if p.Role == "primary" {
			primaries = append(primaries, p.Name)
		}
	}
	h.snapshots = append(h.snapshots, labelSnapshot{
		time:      time.Now(),
		primaries: primaries,
	})
}

// MaxPrimaries returns the maximum number of simultaneous primaries observed.
func (h *PodLabelHistory) MaxPrimaries() (int, time.Time) {
	max := 0
	var when time.Time
	for _, s := range h.snapshots {
		if len(s.primaries) > max {
			max = len(s.primaries)
			when = s.time
		}
	}
	return max, when
}

// Report returns a human-readable summary.
func (h *PodLabelHistory) Report() string {
	var sb strings.Builder
	max, when := h.MaxPrimaries()
	fmt.Fprintf(&sb, "observations=%d, max_primaries=%d", len(h.snapshots), max)
	if max > 1 {
		fmt.Fprintf(&sb, " (at %s)", when.Format("15:04:05"))
	}
	return sb.String()
}

// FormatEvents returns a readable summary of recent events.
func FormatEvents(events []Event, limit int) string {
	var sb strings.Builder
	for i, e := range events {
		if i >= limit {
			break
		}
		fmt.Fprintf(&sb, "  [%s] %s: %s (%s)\n", e.Severity, e.ClusterName, e.Message, e.Source)
	}
	return sb.String()
}

// HealthSummary returns a readable summary of cluster health.
func HealthSummary(h ClusterHealth) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "cluster=%s state=%s instances=%d", h.ClusterName, h.State, len(h.Instances))
	for _, inst := range h.Instances {
		ready := "ready"
		if !inst.Ready {
			ready = "NOT_READY"
		}
		fmt.Fprintf(&sb, "\n    %s: role=%s %s lag=%.0f", inst.PodName, inst.Role, ready, inst.ReplicationLag)
	}
	return sb.String()
}

// ParseInstancesFromHealth attempts to parse the instances field from raw health JSON.
func ParseInstancesFromHealth(raw json.RawMessage) []InstanceInfo {
	var instances []InstanceInfo
	_ = json.Unmarshal(raw, &instances)
	return instances
}
