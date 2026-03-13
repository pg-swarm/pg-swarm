package operator

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
)

// buildSecret creates a Secret with superuser, replication, and per-database user passwords.
func buildSecret(cfg *pgswarmv1.ClusterConfig) *corev1.Secret {
	data := map[string]string{
		"superuser-password":   randomPassword(24),
		"replication-password": randomPassword(24),
	}

	// Add per-database user passwords
	for _, db := range cfg.Databases {
		key := fmt.Sprintf("password-%s", db.User)
		data[key] = db.Password
	}

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName(cfg.ClusterName, "secret"),
			Namespace: cfg.Namespace,
			Labels:    clusterLabels(cfg.ClusterName),
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: data,
	}
}

func randomPassword(length int) string {
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)[:length]
}
