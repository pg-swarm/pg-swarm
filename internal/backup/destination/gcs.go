package destination

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// GCS stores backups in a Google Cloud Storage bucket using gsutil.
type GCS struct {
	Bucket string
	Prefix string
}

func NewGCSFromEnv() *GCS {
	return &GCS{
		Bucket: os.Getenv("GCS_BUCKET"),
		Prefix: os.Getenv("GCS_PREFIX"),
	}
}

func (g *GCS) gsURI(remotePath string) string {
	return fmt.Sprintf("gs://%s/%s%s", g.Bucket, g.Prefix, remotePath)
}

func (g *GCS) Upload(ctx context.Context, remotePath string, r io.Reader) error {
	cmd := exec.CommandContext(ctx, "gsutil", "cp", "-", g.gsURI(remotePath))
	cmd.Stdin = r
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (g *GCS) Download(ctx context.Context, remotePath string, w io.Writer) error {
	cmd := exec.CommandContext(ctx, "gsutil", "cp", g.gsURI(remotePath), "-")
	cmd.Stdout = w
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (g *GCS) List(ctx context.Context, prefix string) ([]string, error) {
	uri := fmt.Sprintf("gs://%s/%s%s", g.Bucket, g.Prefix, prefix)
	cmd := exec.CommandContext(ctx, "gsutil", "ls", uri)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			keys = append(keys, line)
		}
	}
	return keys, nil
}

func (g *GCS) Delete(ctx context.Context, remotePath string) error {
	cmd := exec.CommandContext(ctx, "gsutil", "rm", g.gsURI(remotePath))
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (g *GCS) Exists(ctx context.Context, remotePath string) (bool, error) {
	cmd := exec.CommandContext(ctx, "gsutil", "stat", g.gsURI(remotePath))
	err := cmd.Run()
	if err != nil {
		return false, nil
	}
	return true, nil
}
