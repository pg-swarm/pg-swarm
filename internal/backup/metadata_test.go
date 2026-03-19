package backup

import (
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func testMetaDB(t *testing.T) *MetadataDB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "backups.db")
	m, err := OpenMetadata(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func TestMetadata_CreateAndListSets(t *testing.T) {
	m := testMetaDB(t)

	id1, err := m.CreateBackupSet("17", "0/1000000")
	if err != nil {
		t.Fatal(err)
	}
	if id1 == "" {
		t.Fatal("expected non-empty set ID")
	}

	sets, err := m.ListSets()
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 {
		t.Fatalf("expected 1 set, got %d", len(sets))
	}
	if sets[0].Status != "active" {
		t.Errorf("expected status=active, got %q", sets[0].Status)
	}
}

func TestMetadata_SealAndCreateNewSet(t *testing.T) {
	m := testMetaDB(t)

	_, _ = m.CreateBackupSet("17", "0/1000000")
	if err := m.SealActiveSet(); err != nil {
		t.Fatal(err)
	}

	_, _ = m.CreateBackupSet("17", "0/2000000")

	sets, _ := m.ListSets()
	if len(sets) != 2 {
		t.Fatalf("expected 2 sets, got %d", len(sets))
	}
	// One should be sealed, one should be active
	statuses := map[string]bool{}
	for _, s := range sets {
		statuses[s.Status] = true
	}
	if !statuses["sealed"] {
		t.Error("expected one set to be sealed")
	}
	if !statuses["active"] {
		t.Error("expected one set to be active")
	}
}

func TestMetadata_ActiveSetID(t *testing.T) {
	m := testMetaDB(t)

	// No active set
	id, err := m.ActiveSetID()
	if err != nil {
		t.Fatal(err)
	}
	if id != "" {
		t.Errorf("expected empty, got %q", id)
	}

	// Create one
	created, _ := m.CreateBackupSet("17", "")
	id, _ = m.ActiveSetID()
	if id != created {
		t.Errorf("expected %q, got %q", created, id)
	}

	// Seal it
	m.SealActiveSet()
	id, _ = m.ActiveSetID()
	if id != "" {
		t.Errorf("expected empty after seal, got %q", id)
	}
}

func TestMetadata_InsertAndCompleteBackup(t *testing.T) {
	m := testMetaDB(t)
	setID, _ := m.CreateBackupSet("17", "0/1000000")

	rec := &BackupRecord{
		ID:        "backup-1",
		SetID:     setID,
		Type:      "base",
		Filename:  "20260317T000000Z.tar.gz",
		Subfolder: "base",
		Status:    "running",
	}
	if err := m.InsertBackup(rec); err != nil {
		t.Fatal(err)
	}

	if err := m.CompleteBackup("backup-1", 1024*1024, "0/1000000", "0/2000000", "completed", ""); err != nil {
		t.Fatal(err)
	}

	backups, err := m.BackupsForSet(setID)
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("expected 1 backup, got %d", len(backups))
	}
	if backups[0].Status != "completed" {
		t.Errorf("expected completed, got %q", backups[0].Status)
	}
	if backups[0].SizeBytes != 1024*1024 {
		t.Errorf("expected size 1048576, got %d", backups[0].SizeBytes)
	}
}

func TestMetadata_InsertWALSegment(t *testing.T) {
	m := testMetaDB(t)
	setID, _ := m.CreateBackupSet("17", "")

	seg := &WALSegment{
		Name:      "000000010000000000000001",
		SetID:     setID,
		SizeBytes: 16 * 1024 * 1024,
		Timeline:  1,
		LSNStart:  "0/1000000",
		LSNEnd:    "0/2000000",
	}
	if err := m.InsertWALSegment(seg); err != nil {
		t.Fatal(err)
	}

	segs, err := m.WALSegmentsForSet(setID)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 1 {
		t.Fatalf("expected 1 WAL segment, got %d", len(segs))
	}
	if segs[0].Timeline != 1 {
		t.Errorf("expected timeline 1, got %d", segs[0].Timeline)
	}
}

func TestMetadata_DeleteSetCascades(t *testing.T) {
	m := testMetaDB(t)
	setID, _ := m.CreateBackupSet("17", "")

	m.InsertBackup(&BackupRecord{
		ID: "b1", SetID: setID, Type: "base", Filename: "test.tar.gz", Subfolder: "base", Status: "completed",
	})
	m.InsertWALSegment(&WALSegment{
		Name: "000000010000000000000001", SetID: setID, SizeBytes: 100, Timeline: 1,
	})
	m.InsertBackupStats("b1", 10.0, 100.0, 1024)

	if err := m.DeleteSet(setID); err != nil {
		t.Fatal(err)
	}

	backups, _ := m.BackupsForSet(setID)
	if len(backups) != 0 {
		t.Errorf("expected 0 backups after cascade, got %d", len(backups))
	}
	segs, _ := m.WALSegmentsForSet(setID)
	if len(segs) != 0 {
		t.Errorf("expected 0 WAL segments after cascade, got %d", len(segs))
	}
}

func TestMetadata_FindBaseBackupForTime(t *testing.T) {
	m := testMetaDB(t)
	setID, _ := m.CreateBackupSet("17", "")

	m.InsertBackup(&BackupRecord{
		ID: "b1", SetID: setID, Type: "base", Filename: "old.tar.gz", Subfolder: "base", Status: "completed",
	})
	m.CompleteBackup("b1", 100, "", "", "completed", "")

	rec, err := m.FindBaseBackupForTime("2099-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil {
		t.Fatal("expected to find base backup")
	}
	if rec.ID != "b1" {
		t.Errorf("expected b1, got %s", rec.ID)
	}
}

func TestMetadata_LatestCompletedBackup(t *testing.T) {
	m := testMetaDB(t)
	setID, _ := m.CreateBackupSet("17", "")

	m.InsertBackup(&BackupRecord{
		ID: "b1", SetID: setID, Type: "base", Filename: "first.tar.gz", Subfolder: "base",
		StartedAt: "2026-03-17T00:00:00Z", Status: "running",
	})
	// Complete with an explicit earlier timestamp
	m.db.Exec(`UPDATE backups SET completed_at = '2026-03-17T01:00:00Z', status = 'completed' WHERE id = 'b1'`)

	m.InsertBackup(&BackupRecord{
		ID: "b2", SetID: setID, Type: "base", Filename: "second.tar.gz", Subfolder: "base",
		StartedAt: "2026-03-17T06:00:00Z", Status: "running",
	})
	// Complete with a later timestamp
	m.db.Exec(`UPDATE backups SET completed_at = '2026-03-17T07:00:00Z', status = 'completed' WHERE id = 'b2'`)

	rec, err := m.LatestCompletedBackup("base")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil {
		t.Fatal("expected to find latest base backup")
	}
	if rec.ID != "b2" {
		t.Errorf("expected b2 (latest), got %s", rec.ID)
	}
}

func TestMetadata_Vacuum(t *testing.T) {
	m := testMetaDB(t)
	if err := m.Vacuum(); err != nil {
		t.Fatal(err)
	}
}

func TestMetadata_DbPath(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	m, _ := OpenMetadata(dbPath)
	defer m.Close()

	if m.Path() != dbPath {
		t.Errorf("expected path %q, got %q", dbPath, m.Path())
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("db file should exist: %v", err)
	}
}
