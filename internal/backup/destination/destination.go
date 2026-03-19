// Package destination provides an abstraction for backup storage destinations.
package destination

import (
	"context"
	"io"
)

// Destination is the interface all backup storage backends implement.
type Destination interface {
	// Upload writes data from r to the given remote path.
	Upload(ctx context.Context, remotePath string, r io.Reader) error
	// Download reads the remote path and writes to w.
	Download(ctx context.Context, remotePath string, w io.Writer) error
	// List returns all object keys under prefix.
	List(ctx context.Context, prefix string) ([]string, error)
	// Delete removes the object at remotePath.
	Delete(ctx context.Context, remotePath string) error
	// Exists returns true if remotePath exists.
	Exists(ctx context.Context, remotePath string) (bool, error)
}
