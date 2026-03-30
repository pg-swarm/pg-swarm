package storage

import (
	"testing"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/rs/zerolog"
)

func TestNewNilDestination(t *testing.T) {
	log := zerolog.Nop()
	_, err := New(t.Context(), nil, log)
	if err == nil {
		t.Fatal("expected error for nil destination")
	}
}

func TestNewUnsupportedType(t *testing.T) {
	log := zerolog.Nop()
	_, err := New(t.Context(), &pgswarmv1.BackupDestination{Type: "s3"}, log)
	if err == nil {
		t.Fatal("expected error for unsupported type")
	}
}

func TestNewGCSNilConfig(t *testing.T) {
	log := zerolog.Nop()
	_, err := New(t.Context(), &pgswarmv1.BackupDestination{Type: "gcs"}, log)
	if err == nil {
		t.Fatal("expected error for nil gcs config")
	}
}

func TestNewGCSMissingBucket(t *testing.T) {
	log := zerolog.Nop()
	_, err := New(t.Context(), &pgswarmv1.BackupDestination{
		Type: "gcs",
		Gcs:  &pgswarmv1.GCSDestination{ServiceAccountJson: "{}"},
	}, log)
	if err == nil {
		t.Fatal("expected error for missing bucket")
	}
}

func TestNewGCSMissingCredentials(t *testing.T) {
	log := zerolog.Nop()
	_, err := New(t.Context(), &pgswarmv1.BackupDestination{
		Type: "gcs",
		Gcs:  &pgswarmv1.GCSDestination{Bucket: "test-bucket"},
	}, log)
	if err == nil {
		t.Fatal("expected error for missing credentials")
	}
}

func TestNewSFTPNilConfig(t *testing.T) {
	log := zerolog.Nop()
	_, err := New(t.Context(), &pgswarmv1.BackupDestination{Type: "sftp"}, log)
	if err == nil {
		t.Fatal("expected error for nil sftp config")
	}
}

func TestNewSFTPMissingHost(t *testing.T) {
	log := zerolog.Nop()
	_, err := New(t.Context(), &pgswarmv1.BackupDestination{
		Type: "sftp",
		Sftp: &pgswarmv1.SFTPDestination{User: "u", Password: "p"},
	}, log)
	if err == nil {
		t.Fatal("expected error for missing host")
	}
}

func TestNewSFTPMissingUser(t *testing.T) {
	log := zerolog.Nop()
	_, err := New(t.Context(), &pgswarmv1.BackupDestination{
		Type: "sftp",
		Sftp: &pgswarmv1.SFTPDestination{Host: "h", Password: "p"},
	}, log)
	if err == nil {
		t.Fatal("expected error for missing user")
	}
}

func TestNewSFTPMissingAuth(t *testing.T) {
	log := zerolog.Nop()
	_, err := New(t.Context(), &pgswarmv1.BackupDestination{
		Type: "sftp",
		Sftp: &pgswarmv1.SFTPDestination{Host: "h", User: "u"},
	}, log)
	if err == nil {
		t.Fatal("expected error for missing auth")
	}
}

func TestGCSFullKey(t *testing.T) {
	g := &gcsBackend{pathPrefix: "backups"}
	if got := g.fullKey("wal/seg1.gz"); got != "backups/wal/seg1.gz" {
		t.Fatalf("expected backups/wal/seg1.gz, got %s", got)
	}

	g2 := &gcsBackend{}
	if got := g2.fullKey("wal/seg1.gz"); got != "wal/seg1.gz" {
		t.Fatalf("expected wal/seg1.gz, got %s", got)
	}
}

func TestSFTPFullPath(t *testing.T) {
	s := &sftpBackend{basePath: "/data/backups"}
	if got := s.fullPath("wal/seg1.gz"); got != "/data/backups/wal/seg1.gz" {
		t.Fatalf("expected /data/backups/wal/seg1.gz, got %s", got)
	}

	s2 := &sftpBackend{}
	if got := s2.fullPath("wal/seg1.gz"); got != "wal/seg1.gz" {
		t.Fatalf("expected wal/seg1.gz, got %s", got)
	}
}
