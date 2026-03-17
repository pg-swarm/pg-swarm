package destination

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocal_UploadDownload(t *testing.T) {
	dir := t.TempDir()
	l := NewLocal(dir)
	ctx := context.Background()

	data := []byte("hello, backup world!")
	if err := l.Upload(ctx, "test/backup.tar.gz", bytes.NewReader(data)); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}

	// Check file was created
	fullPath := filepath.Join(dir, "test", "backup.tar.gz")
	if _, err := os.Stat(fullPath); err != nil {
		t.Fatalf("file not created: %v", err)
	}

	// Download
	var buf bytes.Buffer
	if err := l.Download(ctx, "test/backup.tar.gz", &buf); err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Errorf("downloaded data mismatch: got %q, want %q", buf.String(), string(data))
	}
}

func TestLocal_Exists(t *testing.T) {
	dir := t.TempDir()
	l := NewLocal(dir)
	ctx := context.Background()

	exists, err := l.Exists(ctx, "nonexistent.txt")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("expected not exists")
	}

	l.Upload(ctx, "exists.txt", strings.NewReader("data"))
	exists, _ = l.Exists(ctx, "exists.txt")
	if !exists {
		t.Error("expected exists after upload")
	}
}

func TestLocal_Delete(t *testing.T) {
	dir := t.TempDir()
	l := NewLocal(dir)
	ctx := context.Background()

	l.Upload(ctx, "todelete.txt", strings.NewReader("data"))
	if err := l.Delete(ctx, "todelete.txt"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	exists, _ := l.Exists(ctx, "todelete.txt")
	if exists {
		t.Error("file should not exist after delete")
	}
}

func TestLocal_List(t *testing.T) {
	dir := t.TempDir()
	l := NewLocal(dir)
	ctx := context.Background()

	l.Upload(ctx, "base/file1.tar.gz", strings.NewReader("1"))
	l.Upload(ctx, "base/file2.tar.gz", strings.NewReader("2"))
	l.Upload(ctx, "wal/seg1.gz", strings.NewReader("3"))

	keys, err := l.List(ctx, "base")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(keys) != 2 {
		t.Errorf("expected 2 keys under base/, got %d", len(keys))
	}
}

func TestLocal_ListNonexistent(t *testing.T) {
	dir := t.TempDir()
	l := NewLocal(dir)
	ctx := context.Background()

	keys, err := l.List(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("List on nonexistent should not error, got: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("expected 0 keys, got %d", len(keys))
	}
}

func TestNewFromEnv_Local(t *testing.T) {
	t.Setenv("LOCAL_BACKUP_PATH", t.TempDir())
	d := NewFromEnv("local")
	if _, ok := d.(*Local); !ok {
		t.Errorf("expected *Local, got %T", d)
	}
}

func TestNewFromEnv_Unknown(t *testing.T) {
	d := NewFromEnv("unknown")
	if _, ok := d.(*Local); !ok {
		t.Errorf("expected *Local fallback for unknown type, got %T", d)
	}
}
