package backup

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"
)

// APIServer serves the backup sidecar HTTP API on :8442.
type APIServer struct {
	sidecar  *Sidecar
	server   *http.Server
	listener net.Listener
}

// NewAPIServer creates a new HTTP API server.
func NewAPIServer(s *Sidecar) *APIServer {
	return &APIServer{sidecar: s}
}

// Start begins serving HTTP. Blocks until the server is stopped.
func (a *APIServer) Start(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", a.handleHealthz)
	mux.HandleFunc("/wal/push", a.handleWALPush)
	mux.HandleFunc("/wal/fetch", a.handleWALFetch)
	mux.HandleFunc("/backup/complete", a.handleBackupComplete)

	a.server = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal().Err(err).Str("addr", addr).Msg("failed to listen")
	}
	a.listener = ln
	log.Info().Str("addr", addr).Msg("API server started")
	if err := a.server.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Error().Err(err).Msg("API server error")
	}
}

// Stop gracefully shuts down the server.
func (a *APIServer) Stop() {
	if a.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.server.Shutdown(ctx)
	}
}

func (a *APIServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	role := a.sidecar.CurrentRole()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
		"role":   role.String(),
	})
}

// handleWALPush receives a WAL segment from archive_command (curl -F file=@%p -F name=%f).
// Only active on primary.
func (a *APIServer) handleWALPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if a.sidecar.CurrentRole() != RolePrimary {
		http.Error(w, "not primary", http.StatusServiceUnavailable)
		return
	}

	walName := r.FormValue("name")
	if walName == "" {
		http.Error(w, "name parameter required", http.StatusBadRequest)
		return
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, fmt.Sprintf("file parameter required: %v", err), http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Compress and upload
	remotePath := a.sidecar.destPrefix() + "wal/" + walName + ".gz"

	pr, pw := io.Pipe()
	go func() {
		gw := gzip.NewWriter(pw)
		_, copyErr := io.Copy(gw, file)
		gw.Close()
		pw.CloseWithError(copyErr)
	}()

	if err := a.sidecar.dest.Upload(r.Context(), remotePath, pr); err != nil {
		log.Error().Err(err).Str("wal", walName).Msg("failed to upload WAL segment")
		http.Error(w, "upload failed", http.StatusInternalServerError)
		return
	}

	// Record in metadata
	if a.sidecar.meta != nil {
		activeSetID, _ := a.sidecar.meta.ActiveSetID()
		if activeSetID != "" {
			timeline := walTimeline(walName)
			a.sidecar.meta.InsertWALSegment(&WALSegment{
				Name:     walName,
				SetID:    activeSetID,
				SizeBytes: r.ContentLength,
				Timeline: timeline,
			})
		}
	}

	log.Debug().Str("wal", walName).Msg("WAL segment archived")
	w.WriteHeader(http.StatusOK)
}

// handleWALFetch serves WAL segments for restore_command (curl -o %p ...?name=%f).
func (a *APIServer) handleWALFetch(w http.ResponseWriter, r *http.Request) {
	walName := r.URL.Query().Get("name")
	if walName == "" {
		http.Error(w, "name parameter required", http.StatusBadRequest)
		return
	}

	remotePath := a.sidecar.destPrefix() + "wal/" + walName + ".gz"

	// Check if it exists
	exists, err := a.sidecar.dest.Exists(r.Context(), remotePath)
	if err != nil || !exists {
		http.Error(w, "WAL segment not found", http.StatusNotFound)
		return
	}

	// Download and decompress
	pr, pw := io.Pipe()
	go func() {
		err := a.sidecar.dest.Download(r.Context(), remotePath, pw)
		pw.CloseWithError(err)
	}()

	gr, err := gzip.NewReader(pr)
	if err != nil {
		http.Error(w, "decompress failed", http.StatusInternalServerError)
		return
	}
	defer gr.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	io.Copy(w, gr)
}

// BackupCompleteRequest is the payload from replica → primary notification.
type BackupCompleteRequest struct {
	ID           string  `json:"id"`
	Type         string  `json:"type"`          // base, incremental, logical
	Filename     string  `json:"filename"`
	Subfolder    string  `json:"subfolder"`
	SizeBytes    int64   `json:"size_bytes"`
	WALStartLSN  string  `json:"wal_start_lsn"`
	WALEndLSN    string  `json:"wal_end_lsn"`
	DurationSecs float64 `json:"duration_secs"`
	PGVersion    string  `json:"pg_version"`
	DatabaseName string  `json:"database_name"`
	Error        string  `json:"error"`
}

// handleBackupComplete processes backup completion notifications from the replica sidecar.
func (a *APIServer) handleBackupComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if a.sidecar.CurrentRole() != RolePrimary {
		http.Error(w, "not primary", http.StatusServiceUnavailable)
		return
	}
	if a.sidecar.meta == nil {
		http.Error(w, "metadata not ready", http.StatusServiceUnavailable)
		return
	}

	var req BackupCompleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	status := "completed"
	if req.Error != "" {
		status = "failed"
	}

	// If this is a new base backup, seal the current set and create a new one
	if req.Type == "base" && req.Error == "" {
		if err := a.sidecar.meta.SealActiveSet(); err != nil {
			log.Warn().Err(err).Msg("failed to seal active set")
		}
		if _, err := a.sidecar.meta.CreateBackupSet(req.PGVersion, req.WALStartLSN); err != nil {
			log.Error().Err(err).Msg("failed to create new backup set")
			http.Error(w, "metadata error", http.StatusInternalServerError)
			return
		}
	}

	activeSetID, _ := a.sidecar.meta.ActiveSetID()
	if activeSetID == "" {
		http.Error(w, "no active backup set", http.StatusInternalServerError)
		return
	}

	rec := &BackupRecord{
		ID:          req.ID,
		SetID:       activeSetID,
		Type:        req.Type,
		Filename:    req.Filename,
		Subfolder:   req.Subfolder,
		SizeBytes:   req.SizeBytes,
		WALStartLSN: req.WALStartLSN,
		WALEndLSN:   req.WALEndLSN,
		Status:      status,
		Error:       req.Error,
		DatabaseName: req.DatabaseName,
	}
	if err := a.sidecar.meta.InsertBackup(rec); err != nil {
		log.Error().Err(err).Str("backup_id", req.ID).Msg("failed to insert backup record")
		http.Error(w, "metadata error", http.StatusInternalServerError)
		return
	}

	// Record stats
	if req.DurationSecs > 0 {
		throughput := 0.0
		if req.DurationSecs > 0 && req.SizeBytes > 0 {
			throughput = float64(req.SizeBytes) / (1024 * 1024) / req.DurationSecs
		}
		a.sidecar.meta.InsertBackupStats(req.ID, req.DurationSecs, throughput, 0)
	}

	// Upload updated backups.db
	remoteMeta := a.sidecar.destPrefix() + "backups.db"
	if err := uploadFile(r.Context(), a.sidecar.dest, a.sidecar.meta.Path(), remoteMeta); err != nil {
		log.Warn().Err(err).Msg("failed to upload backups.db")
	}

	// Run retention after base backup
	if req.Type == "base" && req.Error == "" && a.sidecar.ret != nil {
		go a.sidecar.ret.RunOnce(context.Background())
	}

	// Update status reporter
	if a.sidecar.reporter != nil {
		a.sidecar.reporter.ReportBackup(r.Context(), req.Type, status, req.SizeBytes, req.Error)
	}

	log.Info().Str("type", req.Type).Str("id", req.ID).Str("status", status).Msg("backup recorded")
	w.WriteHeader(http.StatusOK)
}

// walTimeline extracts the timeline number from a WAL segment name.
// WAL names are like 000000010000000000000001 where first 8 hex chars = timeline.
func walTimeline(walName string) int {
	if len(walName) < 8 {
		return 1
	}
	tl, err := strconv.ParseInt(walName[:8], 16, 64)
	if err != nil {
		return 1
	}
	return int(tl)
}

// tempFile creates a temporary file and returns its path.
func tempFile(prefix string) string {
	f, err := os.CreateTemp("", prefix)
	if err != nil {
		return fmt.Sprintf("/tmp/%s-%d", prefix, time.Now().UnixNano())
	}
	f.Close()
	return f.Name()
}
