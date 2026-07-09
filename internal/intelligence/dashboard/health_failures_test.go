package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func seedFailure(t *testing.T, server *Server, hash, summary, session string, ts time.Time, retries, succeeded int) {
	t.Helper()
	stamp := ts.UTC().Format(time.RFC3339Nano)
	_, err := server.opts.DB.Exec(
		`INSERT INTO projects (root_path, name, created_at) VALUES ('/p', 'proj', ?)
		 ON CONFLICT DO NOTHING`, stamp,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = server.opts.DB.Exec(
		`INSERT INTO sessions (id, project_id, tool, started_at) VALUES (?, 1, 'claude-code', ?)
		 ON CONFLICT DO NOTHING`, session, stamp,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = server.opts.DB.Exec(
		`INSERT INTO actions (session_id, project_id, tool, timestamp, action_type)
		 VALUES (?, 1, 'claude-code', ?, 'tool_call')`, session, stamp,
	)
	if err != nil {
		t.Fatal(err)
	}
	var actionID int64
	if err := server.opts.DB.QueryRow(`SELECT last_insert_rowid()`).Scan(&actionID); err != nil {
		t.Fatal(err)
	}
	_, err = server.opts.DB.Exec(
		`INSERT INTO failure_context (action_id, session_id, project_id, timestamp,
		    command_hash, command_summary, exit_code, error_category, error_message,
		    retry_count, eventually_succeeded)
		 VALUES (?, ?, 1, ?, ?, ?, 1, 'test_failure', 'boom', ?, ?)`,
		actionID, session, stamp, hash, summary, retries, succeeded,
	)
	if err != nil {
		t.Fatal(err)
	}
}

// TestHealthFailures pins the P4.11 surface: grouping by command,
// recovered = any attempt eventually succeeded, latest-row fields ride
// the MAX(timestamp) row, and the window filter applies.
func TestHealthFailures(t *testing.T) {
	server, _ := wizardTestServer(t)
	now := time.Now().UTC()

	// Three fails of one command, one of which eventually succeeded.
	seedFailure(t, server, "h1", "make test", "s-old", now.Add(-3*time.Hour), 2, 0)
	seedFailure(t, server, "h1", "make test", "s-new", now.Add(-time.Hour), 1, 1)
	// A second, unrecovered command.
	seedFailure(t, server, "h2", "npm run lint", "s-new", now.Add(-30*time.Minute), 0, 0)
	// Outside the window — must not appear.
	seedFailure(t, server, "h3", "ancient", "s-old", now.Add(-40*24*time.Hour), 0, 0)

	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/health/failures", nil))
	if rr.Code != 200 {
		t.Fatalf("status %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		WindowDays int `json:"window_days"`
		Total      int `json:"total"`
		Failures   []struct {
			Command   string `json:"command"`
			Fails     int    `json:"fails"`
			Retries   int    `json:"retries"`
			Recovered bool   `json:"recovered"`
			SessionID string `json:"session_id"`
			Project   string `json:"project"`
		} `json:"failures"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Total != 3 {
		t.Errorf("total: got %d want 3 (window filter)", got.Total)
	}
	if len(got.Failures) != 2 {
		t.Fatalf("groups: got %d want 2 (%+v)", len(got.Failures), got.Failures)
	}
	// Ordered by most recent first: npm run lint, then make test.
	if got.Failures[0].Command != "npm run lint" || got.Failures[0].Recovered {
		t.Errorf("first group: %+v", got.Failures[0])
	}
	mk := got.Failures[1]
	if mk.Command != "make test" || mk.Fails != 2 || mk.Retries != 3 || !mk.Recovered {
		t.Errorf("make test group: %+v", mk)
	}
	if mk.SessionID != "s-new" {
		t.Errorf("latest-row session: got %q want s-new", mk.SessionID)
	}
	if mk.Project != "proj" {
		t.Errorf("project: got %q", mk.Project)
	}

	// Method guard.
	rr = httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/health/failures", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: got %d want 405", rr.Code)
	}
}
