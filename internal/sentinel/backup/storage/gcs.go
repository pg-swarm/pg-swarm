package storage

import (
	"context"
	"fmt"
	"io"

	"cloud.google.com/go/storage"
	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/rs/zerolog"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// gcsBackend implements Backend for Google Cloud Storage.
type gcsBackend struct {
	client     *storage.Client
	bucket     *storage.BucketHandle
	pathPrefix string
	log        zerolog.Logger
}

func newGCS(ctx context.Context, dest *pgswarmv1.GCSDestination, log zerolog.Logger) (*gcsBackend, error) {
	if dest.Bucket == "" {
		return nil, fmt.Errorf("gcs bucket is required")
	}
	if dest.ServiceAccountJson == "" {
		return nil, fmt.Errorf("gcs service_account_json is required")
	}

	client, err := storage.NewClient(ctx,
		option.WithCredentialsJSON([]byte(dest.ServiceAccountJson)),
	)
	if err != nil {
		return nil, fmt.Errorf("creating gcs client: %w", err)
	}

	return &gcsBackend{
		client:     client,
		bucket:     client.Bucket(dest.Bucket),
		pathPrefix: dest.PathPrefix,
		log:        log.With().Str("backend", "gcs").Str("bucket", dest.Bucket).Logger(),
	}, nil
}

// fullKey prepends the configured path prefix to the relative key.
func (g *gcsBackend) fullKey(key string) string {
	if g.pathPrefix == "" {
		return key
	}
	return g.pathPrefix + "/" + key
}

func (g *gcsBackend) Upload(ctx context.Context, key string, r io.Reader) error {
	obj := g.bucket.Object(g.fullKey(key))
	w := obj.NewWriter(ctx)

	if _, err := io.Copy(w, r); err != nil {
		w.Close()
		return fmt.Errorf("uploading %s: %w", key, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("closing writer for %s: %w", key, err)
	}

	g.log.Debug().Str("key", key).Msg("uploaded object")
	return nil
}

func (g *gcsBackend) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	obj := g.bucket.Object(g.fullKey(key))
	r, err := obj.NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", key, err)
	}
	return r, nil
}

func (g *gcsBackend) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	it := g.bucket.Objects(ctx, &storage.Query{
		Prefix: g.fullKey(prefix),
	})

	var objects []ObjectInfo
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("listing objects with prefix %s: %w", prefix, err)
		}
		objects = append(objects, ObjectInfo{
			Key:          attrs.Name,
			Size:         attrs.Size,
			LastModified: attrs.Updated,
		})
	}
	return objects, nil
}

func (g *gcsBackend) Delete(ctx context.Context, key string) error {
	obj := g.bucket.Object(g.fullKey(key))
	err := obj.Delete(ctx)
	if err == storage.ErrObjectNotExist {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting %s: %w", key, err)
	}
	g.log.Debug().Str("key", key).Msg("deleted object")
	return nil
}

func (g *gcsBackend) Stat(ctx context.Context, key string) (*ObjectInfo, error) {
	obj := g.bucket.Object(g.fullKey(key))
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", key, err)
	}
	return &ObjectInfo{
		Key:          attrs.Name,
		Size:         attrs.Size,
		LastModified: attrs.Updated,
	}, nil
}

func (g *gcsBackend) Close() error {
	return g.client.Close()
}
