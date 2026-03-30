package sentinel

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// ExecInPod is the exported form of execInPod for use by sub-packages (e.g. backup).
func ExecInPod(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, podName, namespace, command string) error {
	return execInPod(ctx, client, restConfig, podName, namespace, command)
}

// ExecInPodOutput is the exported form of execInPodOutput for use by sub-packages.
func ExecInPodOutput(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, podName, namespace, command string) (string, error) {
	return execInPodOutput(ctx, client, restConfig, podName, namespace, command)
}

// execInPod runs a shell command inside the "postgres" container of the given pod
// using the Kubernetes exec API (SPDY). Returns an error if the command fails.
func execInPod(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, podName, namespace, command string) error {
	script := fmt.Sprintf("set -e\n%s", command)
	req := client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "postgres",
			Command:   []string{"bash", "-c", script},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("exec setup: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return fmt.Errorf("exec failed (stderr: %s): %w", stderr.String(), err)
	}
	return nil
}

// execInPodOutput is like execInPod but returns the stdout output.
func execInPodOutput(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, podName, namespace, command string) (string, error) {
	script := fmt.Sprintf("set -e\n%s", command)
	req := client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "postgres",
			Command:   []string{"bash", "-c", script},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("exec setup: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return "", fmt.Errorf("exec failed (stderr: %s): %w", stderr.String(), err)
	}
	return stdout.String(), nil
}
