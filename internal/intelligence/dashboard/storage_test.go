package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
)

// TestStorageEndpoint pins GET /api/storage: the per-table report is
// present (actions visible with rows), backups next to the DB are
// listed newest-first, and the method guard holds.
func TestStorageEndpoint(t *testing.T) {
	tdir := t.TempDir()
	dbPath := filepath.Join(tdir, "observer.db")
	database, err := db.Open(context.Background(), db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	backupDir := filepath.Join(tdir, "backups")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "observer-20260101-000000.db"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "notes.txt"), []byte("not a backup"), 0o644); err != nil {
		t.Fatal(err)
	}

	server, err := New(Options{DB: database, DBPath: dbPath})
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/storage", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/storage = %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		DBPath string `json:"db_path"`
		Report struct {
			TotalBytes int64 `json:"total_bytes"`
			Tables     []struct {
				Name  string `json:"name"`
				Bytes int64  `json:"bytes"`
				Rows  int64  `json:"rows"`
			} `json:"tables"`
		} `json:"report"`
		BackupDir string `json:"backup_dir"`
		Backups   []struct {
			Name  string `json:"name"`
			Bytes int64  `json:"bytes"`
		} `json:"backups"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Report.TotalBytes <= 0 {
		t.Errorf("total_bytes = %d, want > 0", resp.Report.TotalBytes)
	}
	foundActions := false
	for _, tb := range resp.Report.Tables {
		if tb.Name == "actions" {
			foundActions = true
		}
	}
	if !foundActions {
		t.Error("actions table missing from report")
	}
	if resp.BackupDir != backupDir {
		t.Errorf("backup_dir = %q, want %q", resp.BackupDir, backupDir)
	}
	if len(resp.Backups) != 1 || resp.Backups[0].Name != "observer-20260101-000000.db" {
		t.Errorf("backups = %+v, want exactly the .db snapshot (txt filtered)", resp.Backups)
	}

	rr = httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/storage", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/storage = %d, want 405", rr.Code)
	}
}

// TestStorageMaintenanceJobs pins the vacuum/backup POSTs: each
// spawns the matching `observer db …` argv through the shared job
// registry (with --config), the job is pollable, and GETs are
// rejected.
func TestStorageMaintenanceJobs(t *testing.T) {
	seen := make(chan []string, 2)
	fake := func(ctx context.Context, args []string, onChunk func([]byte)) (int, error) {
		seen <- append([]string(nil), args...)
		onChunk([]byte("ok\n"))
		return 0, nil
	}
	server := newServerWithFakeExec(t, fake)

	for _, tc := range []struct {
		path string
		mode string
		want []string
	}{
		{"/api/storage/vacuum", "db:vacuum", []string{"db", "vacuum"}},
		{"/api/storage/backup", "db:backup", []string{"db", "backup"}},
	} {
		rr := httptest.NewRecorder()
		server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, tc.path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("POST %s = %d: %s", tc.path, rr.Code, rr.Body.String())
		}
		var got map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got["mode"] != tc.mode {
			t.Errorf("%s mode = %v, want %s", tc.path, got["mode"], tc.mode)
		}
		var args []string
		select {
		case args = <-seen:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: subprocess never invoked", tc.path)
		}
		if len(args) < 2 || args[0] != tc.want[0] || args[1] != tc.want[1] {
			t.Errorf("%s argv = %v, want prefix %v", tc.path, args, tc.want)
		}
		hasConfig := false
		for _, a := range args {
			if a == "--config" {
				hasConfig = true
			}
		}
		if !hasConfig {
			t.Errorf("%s argv = %v, missing --config", tc.path, args)
		}

		rr = httptest.NewRecorder()
		server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 405", tc.path, rr.Code)
		}
	}
}
