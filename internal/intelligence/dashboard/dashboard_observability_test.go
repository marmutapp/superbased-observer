package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestAPICompressionRetrieval_HappyPath pins the K43 retrieve-rate
// endpoint shape: stash_retrievals + search_hits counted, retrieve_rate
// computed against compression_events with mechanism='stash', top
// shas + actions surfaced.
func TestAPICompressionRetrieval_HappyPath(t *testing.T) {
	s, _ := newTestServer(t)

	now := time.Now().UTC()
	// Seed a parent api_turn (compression_events.api_turn_id is FK).
	res, err := s.opts.DB.Exec(
		`INSERT INTO api_turns (session_id, timestamp, provider, model, input_tokens, output_tokens)
		 VALUES ('sA', ?, 'anthropic', 'claude-opus-4-7', 100, 50)`,
		now.Format(time.RFC3339Nano),
	)
	if err != nil {
		t.Fatalf("seed api_turn: %v", err)
	}
	turnID, _ := res.LastInsertId()
	// Seed a compression_events stash row.
	_, err = s.opts.DB.Exec(
		`INSERT INTO compression_events (api_turn_id, timestamp, mechanism, original_bytes, compressed_bytes, msg_index, importance_score)
		 VALUES (?, ?, 'stash', 50000, 100, 0, 0)`,
		turnID, now.Format(time.RFC3339Nano),
	)
	if err != nil {
		t.Fatalf("seed stash event: %v", err)
	}
	// Seed retrieval signals: 2× retrieve_stashed (sha-A, sha-A) + 1× search_hit.
	for i := 0; i < 2; i++ {
		_, err = s.opts.DB.Exec(
			`INSERT INTO retrieval_signals (action_id, signal_type, signal_at, payload)
			 VALUES (NULL, 'retrieve_stashed', ?, 'sha-A')`,
			now.Format(time.RFC3339Nano),
		)
		if err != nil {
			t.Fatalf("seed retrieve: %v", err)
		}
	}
	// search_hit needs a real action_id (FK).
	var aid int64
	if err := s.opts.DB.QueryRow(`SELECT id FROM actions LIMIT 1`).Scan(&aid); err != nil {
		t.Fatalf("locate seeded action: %v", err)
	}
	_, err = s.opts.DB.Exec(
		`INSERT INTO retrieval_signals (action_id, signal_type, signal_at, payload)
		 VALUES (?, 'search_hit', ?, 'FAIL')`,
		aid, now.Format(time.RFC3339Nano),
	)
	if err != nil {
		t.Fatalf("seed search hit: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/compression/retrieval?days=7", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	body, _ := io.ReadAll(rr.Body)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if int(got["stash_retrievals"].(float64)) != 2 {
		t.Errorf("stash_retrievals: %v", got["stash_retrievals"])
	}
	if int(got["search_hits"].(float64)) != 1 {
		t.Errorf("search_hits: %v", got["search_hits"])
	}
	if int(got["total_stashes"].(float64)) != 1 {
		t.Errorf("total_stashes: %v", got["total_stashes"])
	}
	if got["retrieve_rate"].(float64) != 2.0 {
		t.Errorf("retrieve_rate: %v, want 2.0 (2 retrieves / 1 stash)", got["retrieve_rate"])
	}
	shas := got["top_retrieved_shas"].([]any)
	if len(shas) != 1 || shas[0].(map[string]any)["sha"] != "sha-A" {
		t.Errorf("top_retrieved_shas: %v", shas)
	}
}

// TestAPICompressionRetrieval_EmptyWindowNoZeroDiv pins safety: zero
// stashes → retrieve_rate = 0 (not NaN, not error).
func TestAPICompressionRetrieval_EmptyWindowNoZeroDiv(t *testing.T) {
	s, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/compression/retrieval?days=7", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d", rr.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["retrieve_rate"].(float64) != 0 {
		t.Errorf("expected retrieve_rate=0 on empty window, got %v", got["retrieve_rate"])
	}
}

// TestAPICompactionEvents_HappyPath pins the D23 visibility endpoint:
// counts events, lists rows, parses ghost-files JSON.
func TestAPICompactionEvents_HappyPath(t *testing.T) {
	s, _ := newTestServer(t)
	now := time.Now().UTC()
	// Need a project + session for FK on compaction_events.
	var pid int64
	_ = s.opts.DB.QueryRow(`SELECT id FROM projects LIMIT 1`).Scan(&pid)
	_, err := s.opts.DB.Exec(
		`INSERT INTO compaction_events (session_id, project_id, timestamp, tool, pre_action_count, file_state_snapshot, ghost_files_after, injected_at)
		 VALUES ('sA', ?, ?, 'claude-code', 42, '{"file_count":3,"files":{}}', '["a.go","b.go"]', ?)`,
		pid, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano),
	)
	if err != nil {
		t.Fatalf("seed compaction: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/compaction/events?days=7", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if int(got["count"].(float64)) != 1 {
		t.Errorf("count: %v", got["count"])
	}
	if int(got["injections_fired"].(float64)) != 1 {
		t.Errorf("injections_fired: %v (want 1)", got["injections_fired"])
	}
	events := got["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("events: %v", events)
	}
	ev := events[0].(map[string]any)
	if int(ev["ghost_files_after_count"].(float64)) != 2 {
		t.Errorf("ghost_files_after_count: %v (want 2)", ev["ghost_files_after_count"])
	}
	if int(ev["file_snapshot_count"].(float64)) != 3 {
		t.Errorf("file_snapshot_count: %v (want 3)", ev["file_snapshot_count"])
	}
	if ev["injected_at"] == "" {
		t.Errorf("injected_at should be populated, got empty")
	}
}

// TestAPICompactionEvents_EmptyWindow pins the empty-state shape.
func TestAPICompactionEvents_EmptyWindow(t *testing.T) {
	s, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/compaction/events?days=7", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d", rr.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if int(got["count"].(float64)) != 0 {
		t.Errorf("count: %v", got["count"])
	}
	events := got["events"].([]any)
	if len(events) != 0 {
		t.Errorf("events should be empty: %v", events)
	}
}

// TestAPICompressionRollingCost_HappyPath pins the D20 cost-net
// endpoint: summary_calls aggregated, rolling_summary savings joined,
// net delta computed against cache_creation rate.
func TestAPICompressionRollingCost_HappyPath(t *testing.T) {
	s, _ := newTestServer(t)
	now := time.Now().UTC()

	// Seed two summary_calls rows.
	for i := 0; i < 2; i++ {
		_, err := s.opts.DB.Exec(
			`INSERT INTO summary_calls (session_id, timestamp, model, input_tokens, output_tokens, cost_usd)
			 VALUES ('sA', ?, 'claude-haiku-4-5', 1000, 200, 0.0042)`,
			now.Format(time.RFC3339Nano),
		)
		if err != nil {
			t.Fatalf("seed summary_calls: %v", err)
		}
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/compression/rolling-cost?days=7", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if int(got["summary_calls"].(float64)) != 2 {
		t.Errorf("summary_calls: %v", got["summary_calls"])
	}
	if int(got["summary_input_tokens"].(float64)) != 2000 {
		t.Errorf("summary_input_tokens: %v", got["summary_input_tokens"])
	}
	if got["summary_cost_usd"].(float64) < 0.008 || got["summary_cost_usd"].(float64) > 0.009 {
		t.Errorf("summary_cost_usd: %v (want ~0.0084)", got["summary_cost_usd"])
	}
	// No rolling_summary events seeded → savings 0, net = -summary_cost.
	if got["rolling_savings_bytes"].(float64) != 0 {
		t.Errorf("rolling_savings_bytes should be 0 without events, got %v", got["rolling_savings_bytes"])
	}
	if got["net_delta_usd"].(float64) >= 0 {
		t.Errorf("net should be negative when no savings, got %v", got["net_delta_usd"])
	}
}
