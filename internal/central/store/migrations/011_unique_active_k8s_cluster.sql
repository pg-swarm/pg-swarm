-- Prevent multiple active satellites on the same Kubernetes cluster.
-- Only one satellite per k8s_cluster_name may be approved or connected.
-- Pending satellites are allowed (a rebuilt cluster registers while the old
-- satellite is still active; replacement happens at approval time).
CREATE UNIQUE INDEX IF NOT EXISTS idx_satellites_active_k8s_cluster
    ON satellites (k8s_cluster_name)
    WHERE state IN ('approved', 'connected');
