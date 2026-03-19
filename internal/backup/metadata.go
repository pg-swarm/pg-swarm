package backup

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// MetadataDB wraps the SQLite backups.db for backup chain tracking.
type MetadataDB struct {
	db   *sql.DB
	path string
}

// OpenMetadata opens (or creates) the SQLite metadata database at path.
func OpenMetadata(path string) (*MetadataDB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open metadata db: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite is single-writer
	m := &MetadataDB{db: db, path: path}
	if err := m.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return m, nil
}

func (m *MetadataDB) Close() error { return m.db.Close() }
func (m *MetadataDB) Path() string { return m.path }

func (m *MetadataDB) migrate() error {
	_, err := m.db.Exec(`
CREATE TABLE IF NOT EXISTS backup_sets (
    id              TEXT PRIMARY KEY,
    started_at      TEXT NOT NULL,
    sealed_at       TEXT,
    status          TEXT DEFAULT 'active',
    pg_version      TEXT,
    wal_start_lsn   TEXT,
    wal_end_lsn     TEXT
);

CREATE TABLE IF NOT EXISTS backups (
    id              TEXT PRIMARY KEY,
    set_id          TEXT NOT NULL REFERENCES backup_sets(id) ON DELETE CASCADE,
    type            TEXT NOT NULL,
    filename        TEXT NOT NULL,
    subfolder       TEXT NOT NULL,
    started_at      TEXT NOT NULL,
    completed_at    TEXT,
    size_bytes      INTEGER DEFAULT 0,
    parent_id       TEXT,
    wal_start_lsn   TEXT,
    wal_end_lsn     TEXT,
    status          TEXT DEFAULT 'running',
    error           TEXT DEFAULT '',
    database_name   TEXT
);

CREATE TABLE IF NOT EXISTS wal_segments (
    name            TEXT PRIMARY KEY,
    set_id          TEXT NOT NULL REFERENCES backup_sets(id) ON DELETE CASCADE,
    archived_at     TEXT NOT NULL,
    size_bytes      INTEGER DEFAULT 0,
    timeline        INTEGER NOT NULL,
    lsn_start       TEXT,
    lsn_end         TEXT
);

CREATE TABLE IF NOT EXISTS backup_stats (
    backup_id       TEXT PRIMARY KEY REFERENCES backups(id) ON DELETE CASCADE,
    duration_secs   REAL,
    throughput_mbps  REAL,
    tables_count    INTEGER,
    db_size_bytes   INTEGER,
    extra_json      TEXT
);

PRAGMA foreign_keys = ON;
`)
	return err
}

// BackupSet represents a backup cycle starting from a base backup.
type BackupSet struct {
	ID          string
	StartedAt   string
	SealedAt    string
	Status      string
	PGVersion   string
	WALStartLSN string
	WALEndLSN   string
}

// BackupRecord represents a single backup (base, incremental, or logical).
type BackupRecord struct {
	ID           string
	SetID        string
	Type         string
	Filename     string
	Subfolder    string
	StartedAt    string
	CompletedAt  string
	SizeBytes    int64
	ParentID     string
	WALStartLSN  string
	WALEndLSN    string
	Status       string
	Error        string
	DatabaseName string
}

// WALSegment represents an archived WAL segment.
type WALSegment struct {
	Name       string
	SetID      string
	ArchivedAt string
	SizeBytes  int64
	Timeline   int
	LSNStart   string
	LSNEnd     string
}

// CreateBackupSet creates a new active backup set.
func (m *MetadataDB) CreateBackupSet(pgVersion, walStartLSN string) (string, error) {
	id := uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := m.db.Exec(
		`INSERT INTO backup_sets (id, started_at, status, pg_version, wal_start_lsn) VALUES (?, ?, 'active', ?, ?)`,
		id, now, pgVersion, walStartLSN,
	)
	return id, err
}

// SealActiveSet seals the currently active backup set. Called when a new base backup arrives.
func (m *MetadataDB) SealActiveSet() error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := m.db.Exec(
		`UPDATE backup_sets SET sealed_at = ?, status = 'sealed' WHERE status = 'active'`,
		now,
	)
	return err
}

// ActiveSetID returns the ID of the active (unsealed) backup set, or "" if none.
func (m *MetadataDB) ActiveSetID() (string, error) {
	var id string
	err := m.db.QueryRow(`SELECT id FROM backup_sets WHERE status = 'active' ORDER BY started_at DESC LIMIT 1`).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

// InsertBackup adds a new backup record.
func (m *MetadataDB) InsertBackup(rec *BackupRecord) error {
	if rec.ID == "" {
		rec.ID = uuid.New().String()
	}
	if rec.StartedAt == "" {
		rec.StartedAt = time.Now().UTC().Format(time.RFC3339)
	}
	_, err := m.db.Exec(
		`INSERT INTO backups (id, set_id, type, filename, subfolder, started_at, parent_id, wal_start_lsn, wal_end_lsn, status, database_name)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.SetID, rec.Type, rec.Filename, rec.Subfolder,
		rec.StartedAt, rec.ParentID, rec.WALStartLSN, rec.WALEndLSN,
		rec.Status, rec.DatabaseName,
	)
	return err
}

// CompleteBackup marks a backup as completed or failed.
func (m *MetadataDB) CompleteBackup(id string, sizeBytes int64, walStartLSN, walEndLSN, status, errMsg string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	if status == "" {
		status = "completed"
	}
	_, err := m.db.Exec(
		`UPDATE backups SET completed_at = ?, size_bytes = ?, wal_start_lsn = ?, wal_end_lsn = ?, status = ?, error = ? WHERE id = ?`,
		now, sizeBytes, walStartLSN, walEndLSN, status, errMsg, id,
	)
	return err
}

// InsertWALSegment records an archived WAL segment.
func (m *MetadataDB) InsertWALSegment(seg *WALSegment) error {
	if seg.ArchivedAt == "" {
		seg.ArchivedAt = time.Now().UTC().Format(time.RFC3339)
	}
	_, err := m.db.Exec(
		`INSERT OR REPLACE INTO wal_segments (name, set_id, archived_at, size_bytes, timeline, lsn_start, lsn_end)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		seg.Name, seg.SetID, seg.ArchivedAt, seg.SizeBytes, seg.Timeline, seg.LSNStart, seg.LSNEnd,
	)
	return err
}

// InsertBackupStats records statistics for a backup.
func (m *MetadataDB) InsertBackupStats(backupID string, durationSecs float64, throughputMBps float64, dbSizeBytes int64) error {
	_, err := m.db.Exec(
		`INSERT OR REPLACE INTO backup_stats (backup_id, duration_secs, throughput_mbps, db_size_bytes) VALUES (?, ?, ?, ?)`,
		backupID, durationSecs, throughputMBps, dbSizeBytes,
	)
	return err
}

// UpdateSetWALEndLSN updates the WAL end LSN for the active backup set.
func (m *MetadataDB) UpdateSetWALEndLSN(setID, lsn string) error {
	_, err := m.db.Exec(`UPDATE backup_sets SET wal_end_lsn = ? WHERE id = ?`, lsn, setID)
	return err
}

// ListSets returns all backup sets ordered by start time descending.
func (m *MetadataDB) ListSets() ([]BackupSet, error) {
	rows, err := m.db.Query(`SELECT id, started_at, COALESCE(sealed_at,''), status, COALESCE(pg_version,''), COALESCE(wal_start_lsn,''), COALESCE(wal_end_lsn,'') FROM backup_sets ORDER BY started_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sets []BackupSet
	for rows.Next() {
		var s BackupSet
		if err := rows.Scan(&s.ID, &s.StartedAt, &s.SealedAt, &s.Status, &s.PGVersion, &s.WALStartLSN, &s.WALEndLSN); err != nil {
			return nil, err
		}
		sets = append(sets, s)
	}
	return sets, rows.Err()
}

// BackupsForSet returns all backups in a set ordered by start time.
func (m *MetadataDB) BackupsForSet(setID string) ([]BackupRecord, error) {
	rows, err := m.db.Query(
		`SELECT id, set_id, type, filename, subfolder, started_at, COALESCE(completed_at,''), size_bytes,
		        COALESCE(parent_id,''), COALESCE(wal_start_lsn,''), COALESCE(wal_end_lsn,''), status, COALESCE(error,''), COALESCE(database_name,'')
		 FROM backups WHERE set_id = ? ORDER BY started_at ASC`, setID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var backups []BackupRecord
	for rows.Next() {
		var b BackupRecord
		if err := rows.Scan(&b.ID, &b.SetID, &b.Type, &b.Filename, &b.Subfolder, &b.StartedAt, &b.CompletedAt,
			&b.SizeBytes, &b.ParentID, &b.WALStartLSN, &b.WALEndLSN, &b.Status, &b.Error, &b.DatabaseName); err != nil {
			return nil, err
		}
		backups = append(backups, b)
	}
	return backups, rows.Err()
}

// WALSegmentsForSet returns all WAL segments in a set ordered by name.
func (m *MetadataDB) WALSegmentsForSet(setID string) ([]WALSegment, error) {
	rows, err := m.db.Query(
		`SELECT name, set_id, archived_at, size_bytes, timeline, COALESCE(lsn_start,''), COALESCE(lsn_end,'')
		 FROM wal_segments WHERE set_id = ? ORDER BY name ASC`, setID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var segs []WALSegment
	for rows.Next() {
		var s WALSegment
		if err := rows.Scan(&s.Name, &s.SetID, &s.ArchivedAt, &s.SizeBytes, &s.Timeline, &s.LSNStart, &s.LSNEnd); err != nil {
			return nil, err
		}
		segs = append(segs, s)
	}
	return segs, rows.Err()
}

// FindBaseBackupForTime returns the latest completed base backup at or before targetTime.
func (m *MetadataDB) FindBaseBackupForTime(targetTime string) (*BackupRecord, error) {
	var b BackupRecord
	err := m.db.QueryRow(
		`SELECT id, set_id, type, filename, subfolder, started_at, COALESCE(completed_at,''), size_bytes,
		        COALESCE(parent_id,''), COALESCE(wal_start_lsn,''), COALESCE(wal_end_lsn,''), status, COALESCE(error,''), COALESCE(database_name,'')
		 FROM backups WHERE type='base' AND status='completed' AND completed_at <= ?
		 ORDER BY completed_at DESC LIMIT 1`, targetTime,
	).Scan(&b.ID, &b.SetID, &b.Type, &b.Filename, &b.Subfolder, &b.StartedAt, &b.CompletedAt,
		&b.SizeBytes, &b.ParentID, &b.WALStartLSN, &b.WALEndLSN, &b.Status, &b.Error, &b.DatabaseName)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &b, err
}

// DeleteSet removes a backup set and all its dependent rows (via CASCADE).
func (m *MetadataDB) DeleteSet(setID string) error {
	_, err := m.db.Exec(`DELETE FROM backup_sets WHERE id = ?`, setID)
	return err
}

// Vacuum runs VACUUM on the database to reclaim space.
func (m *MetadataDB) Vacuum() error {
	_, err := m.db.Exec(`VACUUM`)
	return err
}

// LatestCompletedBackup returns the most recent completed backup of a given type across all sets.
func (m *MetadataDB) LatestCompletedBackup(backupType string) (*BackupRecord, error) {
	var b BackupRecord
	err := m.db.QueryRow(
		`SELECT id, set_id, type, filename, subfolder, started_at, COALESCE(completed_at,''), size_bytes,
		        COALESCE(parent_id,''), COALESCE(wal_start_lsn,''), COALESCE(wal_end_lsn,''), status, COALESCE(error,''), COALESCE(database_name,'')
		 FROM backups WHERE type = ? AND status = 'completed'
		 ORDER BY completed_at DESC LIMIT 1`, backupType,
	).Scan(&b.ID, &b.SetID, &b.Type, &b.Filename, &b.Subfolder, &b.StartedAt, &b.CompletedAt,
		&b.SizeBytes, &b.ParentID, &b.WALStartLSN, &b.WALEndLSN, &b.Status, &b.Error, &b.DatabaseName)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &b, err
}
