package dashboard

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
)

// Storage manager (usability arc P6.8): per-table sizes, vacuum, and
// one-click backup. The heavy operations run as `observer db vacuum` /
// `observer db backup` subprocesses through the shared backfill job
// registry — same allow-list discipline as backfill/prune/scan, and
// the CLI twin is the identical code path.

// handleStorage serves GET /api/storage — the per-table breakdown
// plus existing backups. dbstat walks every page, so this is fetched
// on section open / explicit refresh only, never polled.
func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	// s.opts.DB, not s.db(): the storage manager describes (and
	// maintains) the real database even while demo mode is active.
	rep, err := db.StorageStats(r.Context(), s.opts.DB)
	if err != nil {
		writeErr(w, err)
		return
	}
	type backupFile struct {
		Name     string `json:"name"`
		Bytes    int64  `json:"bytes"`
		Modified string `json:"modified"`
	}
	resp := map[string]any{
		"db_path": s.opts.DBPath,
		"report":  rep,
	}
	if s.opts.DBPath != "" {
		backupDir := filepath.Join(filepath.Dir(s.opts.DBPath), "backups")
		resp["backup_dir"] = backupDir
		files := []backupFile{}
		if entries, err := os.ReadDir(backupDir); err == nil {
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".db") {
					continue
				}
				info, err := e.Info()
				if err != nil {
					continue
				}
				files = append(files, backupFile{
					Name:     e.Name(),
					Bytes:    info.Size(),
					Modified: info.ModTime().UTC().Format(time.RFC3339),
				})
			}
		}
		sort.Slice(files, func(i, j int) bool { return files[i].Modified > files[j].Modified })
		resp["backups"] = files
	}
	writeJSON(w, resp)
}

// handleStorageVacuum serves POST /api/storage/vacuum — spawns
// `observer db vacuum` through the job registry (mode "db:vacuum",
// pollable like any backfill job). Vacuum needs the write lock; the
// job output reports the honest outcome either way.
func (s *Server) handleStorageVacuum(w http.ResponseWriter, r *http.Request) {
	s.runDBMaintenanceJob(w, r, "db:vacuum", []string{"db", "vacuum"})
}

// handleStorageBackup serves POST /api/storage/backup — spawns
// `observer db backup` (VACUUM INTO a timestamped snapshot next to
// the live DB; online-safe). The job output carries the written path
// and the restore instructions.
func (s *Server) handleStorageBackup(w http.ResponseWriter, r *http.Request) {
	s.runDBMaintenanceJob(w, r, "db:backup", []string{"db", "backup"})
}

// runDBMaintenanceJob is the shared POST body for the two storage
// operations: registry entry + subprocess with the dashboard's
// resolved config path, mirroring handlePruneRun.
func (s *Server) runDBMaintenanceJob(w http.ResponseWriter, r *http.Request, mode string, args []string) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	jobID, err := newJobID()
	if err != nil {
		writeErr(w, err)
		return
	}
	job := &backfillJob{
		ID:        jobID,
		Mode:      mode,
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	s.backfillMu.Lock()
	s.backfillSeq++
	job.seq = s.backfillSeq
	s.backfillJobs[jobID] = job
	s.backfillMu.Unlock()

	if s.opts.ConfigPath != "" {
		args = append(args, "--config", s.opts.ConfigPath)
	}
	go s.runBackfillJob(jobID, args)

	writeJSON(w, map[string]any{
		"job_id":     jobID,
		"mode":       mode,
		"status":     "running",
		"started_at": job.StartedAt.Format(time.RFC3339),
	})
}
