package destination

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Local stores backups on a local filesystem path.
type Local struct {
	BasePath string
}

func NewLocal(basePath string) *Local {
	return &Local{BasePath: basePath}
}

func (l *Local) Upload(_ context.Context, remotePath string, r io.Reader) error {
	full := filepath.Join(l.BasePath, remotePath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	f, err := os.Create(full)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, r)
	return err
}

func (l *Local) Download(_ context.Context, remotePath string, w io.Writer) error {
	f, err := os.Open(filepath.Join(l.BasePath, remotePath))
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

func (l *Local) List(_ context.Context, prefix string) ([]string, error) {
	root := filepath.Join(l.BasePath, prefix)
	var keys []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			rel, _ := filepath.Rel(l.BasePath, path)
			keys = append(keys, rel)
		}
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	return keys, err
}

func (l *Local) Delete(_ context.Context, remotePath string) error {
	return os.Remove(filepath.Join(l.BasePath, remotePath))
}

func (l *Local) Exists(_ context.Context, remotePath string) (bool, error) {
	_, err := os.Stat(filepath.Join(l.BasePath, remotePath))
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

// NewFromEnv creates a Destination from environment variables.
func NewFromEnv(destType string) Destination {
	switch strings.ToLower(destType) {
	case "s3":
		return NewS3FromEnv()
	case "gcs":
		return NewGCSFromEnv()
	case "sftp":
		return NewSFTPFromEnv()
	case "local":
		return NewLocal(envOr("LOCAL_BACKUP_PATH", "/backup-storage"))
	default:
		return NewLocal("/tmp/backups")
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
