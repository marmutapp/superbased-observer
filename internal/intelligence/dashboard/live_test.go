package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// seedLiveSession inserts one session with a project plus n actions
// ending at `last` (one per minute, newest last).
func seedLiveSession(t *testing.T, server *Server, id, tool string, started, last time.Time, n int) {
	t.Helper()
	if _, err := server.opts.DB.Exec(
		`INSERT INTO projects (root_path, name, created_at) VALUES ('/live-p', 'live-proj', ?)
		 ON CONFLICT DO NOTHING`, started.UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := server.opts.DB.Exec(
		`INSERT INTO sessions (id, project_id, tool, started_at)
		 VALUES (?, (SELECT id FROM projects WHERE root_path = '/live-p'), ?, ?)`,
		id, tool, started.UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		ts := last.Add(-time.Duration(n-1-i) * time.Minute).UTC().Format(time.RFC3339Nano)
		if _, err := server.opts.DB.Exec(
			`INSERT INTO actions (session_id, project_id, tool, timestamp, action_type, target, success)
			 VALUES (?, (SELECT id FROM projects WHERE root_path = '/live-p'), ?, ?, 'run_command', ?, ?)`,
			id, tool, ts, "cmd-"+id, i%2,
		); err != nil {
			t.Fatal(err)
		}
	}
}

// TestHandleLive pins the P6.1 surface: window membership, newest-first
// ordering, lifetime token/cost rollup with proxy dedup, the recent
// feed shape, and the method guard.
func TestHandleLive(t *testing.T) {
	server, _ := wizardTestServer(t)
	now := time.Now().UTC()

	// Active session: actions 5m..1m ago, one proxy turn + one jsonl
	// token row that duplicates it (must dedup) + one independent jsonl
	// row (must count).
	seedLiveSession(t, server, "live-a", "claude-code", now.Add(-30*time.Minute), now.Add(-time.Minute), 5)
	if _, err := server.opts.DB.Exec(
		`INSERT INTO api_turns (session_id, timestamp, provider, model, request_id,
		    input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, cost_usd)
		 VALUES ('live-a', ?, 'anthropic', 'claude-opus-4-8', 'req-1', 1000, 200, 5000, 300, 0.25)`,
		now.Add(-2*time.Minute).Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := server.opts.DB.Exec(
		`INSERT INTO token_usage (session_id, timestamp, tool, model, input_tokens, output_tokens,
		    cache_read_tokens, cache_creation_tokens, source, source_file, source_event_id)
		 VALUES ('live-a', ?, 'claude-code', 'claude-opus-4-8', 1000, 200, 5000, 300, 'jsonl', 'f1', 'req-1')`,
		now.Add(-2*time.Minute).Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := server.opts.DB.Exec(
		`INSERT INTO token_usage (session_id, timestamp, tool, model, input_tokens, output_tokens,
		    cache_read_tokens, cache_creation_tokens, estimated_cost_usd, source, source_file, source_event_id)
		 VALUES ('live-a', ?, 'claude-code', 'claude-haiku-4-5', 50, 10, 0, 0, 0.01, 'jsonl', 'f1', 'evt-2')`,
		now.Add(-90*time.Second).Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
	// A second, more recently active session — must sort first.
	seedLiveSession(t, server, "live-b", "codex", now.Add(-10*time.Minute), now.Add(-10*time.Second), 2)
	// A stale session outside the window — must not appear.
	seedLiveSession(t, server, "live-old", "cursor", now.Add(-3*time.Hour), now.Add(-2*time.Hour), 3)

	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/live?window_minutes=15", nil))
	if rr.Code != 200 {
		t.Fatalf("status %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		WindowMinutes int `json:"window_minutes"`
		Active        []struct {
			SessionID    string   `json:"session_id"`
			Tool         string   `json:"tool"`
			ProjectRoot  string   `json:"project_root"`
			ActionsTotal int      `json:"actions_total"`
			Turns        int      `json:"turns"`
			Models       []string `json:"models"`
			Tokens       struct {
				Input      int64 `json:"input"`
				Output     int64 `json:"output"`
				CacheRead  int64 `json:"cache_read"`
				CacheWrite int64 `json:"cache_write"`
			} `json:"tokens"`
			CostUSD float64 `json:"cost_usd"`
			Recent  []struct {
				ActionType string `json:"action_type"`
				Target     string `json:"target"`
				Success    bool   `json:"success"`
			} `json:"recent_actions"`
		} `json:"active"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.WindowMinutes != 15 {
		t.Errorf("window_minutes: got %d", got.WindowMinutes)
	}
	if len(got.Active) != 2 {
		t.Fatalf("active: got %d want 2 (%+v)", len(got.Active), got.Active)
	}
	if got.Active[0].SessionID != "live-b" || got.Active[1].SessionID != "live-a" {
		t.Fatalf("order: got %s, %s want live-b, live-a", got.Active[0].SessionID, got.Active[1].SessionID)
	}

	a := got.Active[1]
	if a.Tool != "claude-code" || a.ProjectRoot != "/live-p" {
		t.Errorf("identity: %+v", a)
	}
	if a.ActionsTotal != 5 {
		t.Errorf("actions_total: got %d want 5", a.ActionsTotal)
	}
	// Dedup: proxy turn (1000/200/5000/300) + independent jsonl row
	// (50/10) — the duplicated token_usage row must not count.
	if a.Turns != 2 {
		t.Errorf("turns: got %d want 2 (proxy-dedup)", a.Turns)
	}
	if a.Tokens.Input != 1050 || a.Tokens.Output != 210 ||
		a.Tokens.CacheRead != 5000 || a.Tokens.CacheWrite != 300 {
		t.Errorf("tokens: %+v", a.Tokens)
	}
	// Recorded costs only (0.25 + 0.01); no engine lookup needed.
	if a.CostUSD < 0.259 || a.CostUSD > 0.261 {
		t.Errorf("cost: got %f want 0.26", a.CostUSD)
	}
	if len(a.Models) != 2 {
		t.Errorf("models: %v", a.Models)
	}
	if len(a.Recent) != 5 {
		t.Fatalf("recent: got %d want 5", len(a.Recent))
	}
	// Newest first; seed alternates success 0/1 ending at i=4 (success).
	if a.Recent[0].Target != "cmd-live-a" || a.Recent[0].ActionType != "run_command" {
		t.Errorf("recent[0]: %+v", a.Recent[0])
	}
	if a.Recent[0].Success != false || a.Recent[1].Success != true {
		// i runs 0..4 oldest→newest with success i%2; newest (i=4) is 0.
		t.Errorf("recent success order: %+v", a.Recent[:2])
	}

	// Method guard.
	rr = httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/live", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: got %d want 405", rr.Code)
	}
}
