package dashboard

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
)

// demoTestSeeder returns a fake DemoSeeder that opens a real
// (migrated) temp database carrying exactly one marker session, plus
// counters for seed/cleanup invocations — the test's way to tell
// WHICH database a data endpoint served and whether lifecycle hooks
// fired exactly once.
func demoTestSeeder(t *testing.T) (seeder func(ctx context.Context) (*sql.DB, func() error, error), seeds, cleanups *int) {
	t.Helper()
	seeds, cleanups = new(int), new(int)
	seeder = func(ctx context.Context) (*sql.DB, func() error, error) {
		*seeds++
		database, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "demo.db")})
		if err != nil {
			return nil, nil, err
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := database.Exec(
			`INSERT INTO projects (root_path, name, created_at) VALUES ('/demo-p', 'demo-proj', ?)`, now,
		); err != nil {
			return nil, nil, err
		}
		if _, err := database.Exec(
			`INSERT INTO sessions (id, project_id, tool, started_at)
			 VALUES ('demo-marker', (SELECT id FROM projects WHERE root_path = '/demo-p'), 'claude-code', ?)`, now,
		); err != nil {
			return nil, nil, err
		}
		cleanup := func() error {
			*cleanups++
			return database.Close()
		}
		return database, cleanup, nil
	}
	return seeder, seeds, cleanups
}

func demoStatusSessions(t *testing.T, s *Server) int {
	t.Helper()
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("/api/status = %d: %s", rr.Code, rr.Body.String())
	}
	var snap struct {
		Counts struct {
			Sessions int `json:"sessions"`
		} `json:"counts"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
		t.Fatalf("status decode: %v", err)
	}
	return snap.Counts.Sessions
}

func demoState(t *testing.T, s *Server, method, path string) (code int, available, active bool) {
	t.Helper()
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(method, path, nil))
	var body struct {
		Available bool `json:"available"`
		Active    bool `json:"active"`
	}
	if rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s %s decode: %v", method, path, err)
		}
	}
	return rr.Code, body.Available, body.Active
}

// TestDemoMode pins the full lifecycle: inactive → start (data
// endpoints swap to the seeded DB) → idempotent re-start (no reseed)
// → stop (one cleanup; reads back on the real DB).
func TestDemoMode(t *testing.T) {
	tdir := t.TempDir()
	realDB, err := db.Open(context.Background(), db.Options{Path: filepath.Join(tdir, "real.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { realDB.Close() })

	seeder, seeds, cleanups := demoTestSeeder(t)
	server, err := New(Options{DB: realDB, DemoSeeder: seeder})
	if err != nil {
		t.Fatal(err)
	}

	if code, available, active := demoState(t, server, http.MethodGet, "/api/demo"); code != 200 || !available || active {
		t.Fatalf("initial state = %d available=%v active=%v, want 200 true false", code, available, active)
	}
	if n := demoStatusSessions(t, server); n != 0 {
		t.Fatalf("real DB sessions = %d, want 0", n)
	}

	if code, _, active := demoState(t, server, http.MethodPost, "/api/demo/start"); code != 200 || !active {
		t.Fatalf("start = %d active=%v, want 200 true", code, active)
	}
	if n := demoStatusSessions(t, server); n != 1 {
		t.Fatalf("demo-mode sessions = %d, want 1 (the marker row — data endpoints must serve the demo DB)", n)
	}

	// Idempotent: a second start neither reseeds nor errors.
	if code, _, active := demoState(t, server, http.MethodPost, "/api/demo/start"); code != 200 || !active {
		t.Fatalf("re-start = %d active=%v, want 200 true", code, active)
	}
	if *seeds != 1 {
		t.Fatalf("seeder ran %d times, want 1", *seeds)
	}

	if code, _, active := demoState(t, server, http.MethodPost, "/api/demo/stop"); code != 200 || active {
		t.Fatalf("stop = %d active=%v, want 200 false", code, active)
	}
	if *cleanups != 1 {
		t.Fatalf("cleanup ran %d times, want 1", *cleanups)
	}
	if n := demoStatusSessions(t, server); n != 0 {
		t.Fatalf("post-stop sessions = %d, want 0 (back on the real DB)", n)
	}

	// Stop again: idempotent, no second cleanup.
	if code, _, active := demoState(t, server, http.MethodPost, "/api/demo/stop"); code != 200 || active {
		t.Fatalf("re-stop = %d active=%v, want 200 false", code, active)
	}
	if *cleanups != 1 {
		t.Fatalf("cleanup ran %d times after re-stop, want 1", *cleanups)
	}
}

// TestDemoModeUnavailable pins the no-seeder posture (tests, older
// assemblies): state reports unavailable and start refuses honestly.
func TestDemoModeUnavailable(t *testing.T) {
	realDB, err := db.Open(context.Background(), db.Options{Path: filepath.Join(t.TempDir(), "real.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { realDB.Close() })
	server, err := New(Options{DB: realDB})
	if err != nil {
		t.Fatal(err)
	}

	if code, available, active := demoState(t, server, http.MethodGet, "/api/demo"); code != 200 || available || active {
		t.Fatalf("state = %d available=%v active=%v, want 200 false false", code, available, active)
	}
	if code, _, _ := demoState(t, server, http.MethodPost, "/api/demo/start"); code != http.StatusServiceUnavailable {
		t.Fatalf("start without seeder = %d, want 503", code)
	}

	// Method guards.
	if code, _, _ := demoState(t, server, http.MethodPost, "/api/demo"); code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/demo = %d, want 405", code)
	}
	if code, _, _ := demoState(t, server, http.MethodGet, "/api/demo/start"); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/demo/start = %d, want 405", code)
	}
	if code, _, _ := demoState(t, server, http.MethodGet, "/api/demo/stop"); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/demo/stop = %d, want 405", code)
	}
}
