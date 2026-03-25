package operator

import (
	"crypto/rand"
	"encoding/hex"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
)

// buildSecret creates a Secret with superuser, replication, and per-database user passwords.
func buildSecret(cfg *pgswarmv1.ClusterConfig) *corev1.Secret {
	data := map[string]string{
		"superuser-password":   randomPassword(24),
		"replication-password": randomPassword(24),
		"sidecar-stream-token": randomPassword(32),
	}

	// NOTE: Cluster-level database passwords are NOT stored in the Secret.
	// They are passed directly via sidecar CreateDatabaseCmd proto message.

	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName(cfg.ClusterName, "secret"),
			Namespace: cfg.Namespace,
			Labels:    clusterLabels(cfg.ClusterName, cfg.ProfileName, cfg.LabelSelector),
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: data,
	}
}

// randomPassword generates a cryptographically random hex string of the given length.
func randomPassword(length int) string {
	b := make([]byte, (length+1)/2)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)[:length]
}
