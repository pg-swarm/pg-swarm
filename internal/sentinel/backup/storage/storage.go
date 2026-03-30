// Package storage provides backend implementations for uploading and downloading
// PostgreSQL backups and WAL segments to remote storage destinations.
package storage

import (
	"context"
	"fmt"
	"io"
	"time"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/rs/zerolog"
)

// ObjectInfo describes a single object in the storage backend.
type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// Backend is the interface every storage destination must implement.
type Backend interface {
	// Upload writes the contents of r to the given key (relative path within the
	// destination). Existing objects are overwritten.
	Upload(ctx context.Context, key string, r io.Reader) error

	// Download returns a ReadCloser for the object at key. The caller must close
	// the returned reader.
	Download(ctx context.Context, key string) (io.ReadCloser, error)

	// List returns all objects whose key starts with the given prefix.
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)

	// Delete removes the object at key. It is not an error if the object does
	// not exist.
	Delete(ctx context.Context, key string) error

	// Stat returns metadata for the object at key, or an error if it does not
	// exist.
	Stat(ctx context.Context, key string) (*ObjectInfo, error)

	// Close releases any resources held by the backend (e.g. SFTP connections).
	Close() error
}

// New creates a Backend from a proto BackupDestination. The caller must call
// Close on the returned Backend when done.
func New(ctx context.Context, dest *pgswarmv1.BackupDestination, log zerolog.Logger) (Backend, error) {
	if dest == nil {
		return nil, fmt.Errorf("backup destination is nil")
	}

	switch dest.Type {
	case "gcs":
		if dest.Gcs == nil {
			return nil, fmt.Errorf("gcs destination config is nil")
		}
		return newGCS(ctx, dest.Gcs, log)
	case "sftp":
		if dest.Sftp == nil {
			return nil, fmt.Errorf("sftp destination config is nil")
		}
		return newSFTP(dest.Sftp, log)
	case "s3":
		if dest.S3 == nil {
			return nil, fmt.Errorf("s3 destination config is nil")
		}
		return newS3(ctx, dest.S3, log)
	default:
		return nil, fmt.Errorf("unsupported storage type: %q", dest.Type)
	}
}
