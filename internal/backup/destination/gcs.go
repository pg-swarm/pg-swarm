package destination

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

// GCS stores backups in a Google Cloud Storage bucket using the Go client library.
type GCS struct {
	Bucket string
	Prefix string
	client *storage.Client
}

func NewGCSFromEnv() *GCS {
	ctx := context.Background()
	client, err := storage.NewClient(ctx)
	if err != nil {
		panic(fmt.Errorf("failed to initialize GCS client: %w", err))
	}

	return &GCS{
		Bucket: os.Getenv("GCS_BUCKET"),
		Prefix: os.Getenv("GCS_PREFIX"),
		client: client,
	}
}

func (g *GCS) Upload(ctx context.Context, remotePath string, r io.Reader) error {
	obj := g.client.Bucket(g.Bucket).Object(g.Prefix + remotePath)
	w := obj.NewWriter(ctx)
	if _, err := io.Copy(w, r); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

func (g *GCS) Download(ctx context.Context, remotePath string, w io.Writer) error {
	obj := g.client.Bucket(g.Bucket).Object(g.Prefix + remotePath)
	r, err := obj.NewReader(ctx)
	if err != nil {
		return err
	}
	defer r.Close()
	_, err = io.Copy(w, r)
	return err
}

func (g *GCS) List(ctx context.Context, prefix string) ([]string, error) {
	fullPrefix := g.Prefix + prefix
	it := g.client.Bucket(g.Bucket).Objects(ctx, &storage.Query{Prefix: fullPrefix})
	var keys []string
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		// Return the path relative to the base prefix to match other implementations
		relPath := strings.TrimPrefix(attrs.Name, g.Prefix)
		if relPath != "" {
			keys = append(keys, relPath)
		}
	}
	return keys, nil
}

func (g *GCS) Delete(ctx context.Context, remotePath string) error {
	obj := g.client.Bucket(g.Bucket).Object(g.Prefix + remotePath)
	return obj.Delete(ctx)
}

func (g *GCS) Exists(ctx context.Context, remotePath string) (bool, error) {
	obj := g.client.Bucket(g.Bucket).Object(g.Prefix + remotePath)
	_, err := obj.Attrs(ctx)
	if err == storage.ErrObjectNotExist {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
