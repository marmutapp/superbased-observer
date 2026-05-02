package metrics

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/pidbridge"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// seedDB opens a fresh DB and inserts one session + one action + one
// api_turn with compression stats. The returned time is the scrape's
// "now" — it sits inside the default 5m window so the cost rollup
// picks up the turn.
func seedDB(t *testing.T) (*sql.DB, string, time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "m.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	now := time.Date(2026, 4, 17, 10, 0, 0, 0, time.UTC)
	base := now.Add(-2 * time.Minute) // inside the 5m window
	root := t.TempDir()

	st := store.New(database)
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{{
		SourceFile: "f", SourceEventID: "e1", SessionID: "sA",
		ProjectRoot: root, Timestamp: base, Tool: models.ToolClaudeCode,
		ActionType: models.ActionReadFile, Target: "a.go", Success: true,
	}}, nil, store.IngestOptions{}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	turn := models.APITurn{
		SessionID:                  "sA",
		Timestamp:                  base,
		Provider:                   models.ProviderAnthropic,
		Model:                      "claude-sonnet-4",
		InputTokens:                100,
		OutputTokens:               50,
		CacheReadTokens:            20,
		CacheCreationTokens:        10,
		CostUSD:                    0.0042,
		CompressionOriginalBytes:   10_000,
		CompressionCompressedBytes: 3_000,
		CompressionCount:           2,
		CompressionDroppedCount:    1,
		CompressionMarkerCount:     1,
	}
	if _, err := st.InsertAPITurn(context.Background(), turn); err != nil {
		t.Fatalf("InsertAPITurn: %v", err)
	}

	bridge := pidbridge.New(database)
	if err := bridge.Write(context.Background(), pidbridge.Entry{
		PID: 1234, SessionID: "sA", Tool: "claude-code", CWD: "/tmp",
	}); err != nil {
		t.Fatalf("bridge.Write: %v", err)
	}
	return database, path, now
}

func TestRender_EmitsCoreFamilies(t *testing.T) {
	database, path, now := seedDB(t)
	var out strings.Builder
	err := Render(context.Background(), &out, Options{
		DB: database, DBPath: path,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := out.String()
	for _, want := range []string{
		"# TYPE observer_db_size_bytes gauge",
		"# TYPE observer_db_schema_version gauge",
		"# TYPE observer_projects_total gauge",
		"# TYPE observer_sessions_total gauge",
		"# TYPE observer_actions_total gauge",
		"# TYPE observer_api_turns_total gauge",
		"# TYPE observer_tool_actions_total gauge",
		"# TYPE observer_pidbridge_entries gauge",
		"# TYPE observer_cost_usd gauge",
		"# TYPE observer_tokens gauge",
		"# TYPE observer_compression_saved_bytes gauge",
		"# TYPE observer_metrics_scrape_ok gauge",
		"# TYPE observer_metrics_scrape_duration_seconds gauge",
		`observer_pidbridge_entries 1`,
		`observer_sessions_total 1`,
		`observer_actions_total 1`,
		`observer_api_turns_total 1`,
		`observer_tool_actions_total{tool="claude-code"} 1`,
		`observer_metrics_scrape_ok 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q; body:\n%s", want, body)
		}
	}
}

func TestRender_CostRolledUpOverWindow(t *testing.T) {
	database, path, now := seedDB(t)
	var out strings.Builder
	if err := Render(context.Background(), &out, Options{
		DB: database, DBPath: path,
		Now: func() time.Time { return now },
	}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := out.String()

	// The api_turn is inside the default 5m window, so we should see its
	// cost and token contribution.
	for _, want := range []string{
		`observer_cost_usd{model="claude-sonnet-4",window="5m"}`,
		`observer_tokens{model="claude-sonnet-4",kind="input",window="5m"} 100`,
		`observer_tokens{model="claude-sonnet-4",kind="output",window="5m"} 50`,
		`observer_tokens{model="claude-sonnet-4",kind="cache_read",window="5m"} 20`,
		`observer_tokens{model="claude-sonnet-4",kind="cache_creation",window="5m"} 10`,
		`observer_compression_original_bytes{window="5m"} 10000`,
		`observer_compression_compressed_bytes{window="5m"} 3000`,
		`observer_compression_saved_bytes{window="5m"} 7000`,
		`observer_compression_compressed_count{window="5m"} 2`,
		`observer_compression_dropped_count{window="5m"} 1`,
		`observer_compression_marker_count{window="5m"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q; body:\n%s", want, body)
		}
	}
}

func TestRender_WindowExcludesStaleTurns(t *testing.T) {
	database, path, now := seedDB(t)
	// Push the scrape forward an hour so the seeded turn falls out of the
	// 5m window. We still expect table-count metrics, but cost/compression
	// should be zero-valued families — the sample is still emitted with
	// value 0, not omitted, because the window=5m label set is always
	// present for those families.
	future := now.Add(time.Hour)
	var out strings.Builder
	if err := Render(context.Background(), &out, Options{
		DB: database, DBPath: path,
		Now: func() time.Time { return future },
	}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := out.String()

	// The per-model cost family should NOT contain claude-sonnet-4 (no
	// rows in window).
	if strings.Contains(body, `observer_cost_usd{model="claude-sonnet-4"`) {
		t.Errorf("stale model leaked into window; body:\n%s", body)
	}
	// But the aggregate window rows should still be present and zero.
	for _, want := range []string{
		`observer_cost_usd_window{window="5m"} 0`,
		`observer_compression_saved_bytes{window="5m"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q; body:\n%s", want, body)
		}
	}
}

func TestRender_CustomWindow(t *testing.T) {
	database, path, now := seedDB(t)
	var out strings.Builder
	if err := Render(context.Background(), &out, Options{
		DB: database, DBPath: path,
		CostWindowMinutes: 10,
		Now:               func() time.Time { return now },
	}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, `window="10m"`) {
		t.Errorf("expected window=10m label, body:\n%s", body)
	}
	if strings.Contains(body, `window="5m"`) {
		t.Errorf("default window leaked when custom set; body:\n%s", body)
	}
}

func TestHandler_MetricsEndpoint(t *testing.T) {
	database, path, now := seedDB(t)
	srv, err := New(Options{
		DB: database, DBPath: path,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("GET /metrics: %d", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type: %q", got)
	}
	body, _ := io.ReadAll(rr.Body)
	if !strings.Contains(string(body), "observer_sessions_total 1") {
		t.Errorf("/metrics body missing session count; body:\n%s", body)
	}

	// HEAD should set headers but not write a body.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodHead, "/metrics", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("HEAD /metrics: %d", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Errorf("HEAD body should be empty, got %d bytes", rr.Body.Len())
	}

	// Non-GET/HEAD method should 405.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/metrics", strings.NewReader(""))
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /metrics: got %d want 405", rr.Code)
	}
}

func TestHandler_RootAndNotFound(t *testing.T) {
	database, path, now := seedDB(t)
	srv, err := New(Options{DB: database, DBPath: path, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	// Root: human-readable usage hint.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("GET /: %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "GET /metrics") {
		t.Errorf("root body missing usage hint: %s", rr.Body.String())
	}
	// Unknown path: 404.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/nope", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("GET /nope: %d want 404", rr.Code)
	}
}

func TestEscapeLabel_CoversSpecials(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{`with "quotes"`, `with \"quotes\"`},
		{`back\slash`, `back\\slash`},
		{"with\nnewline", `with\nnewline`},
		{`all "three"\n`, `all \"three\"\\n`},
	}
	for _, tc := range cases {
		if got := escapeLabel(tc.in); got != tc.want {
			t.Errorf("escapeLabel(%q): got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatLabels_DeterministicKeyOrder(t *testing.T) {
	// formatLabels preserves the slice order exactly — the caller is
	// responsible for picking a stable key order. Samples are sorted by
	// the formatted output, so identical-key sets remain grouped.
	got := formatLabels([][2]string{
		{"model", "x"}, {"kind", "input"}, {"window", "5m"},
	})
	want := `{model="x",kind="input",window="5m"}`
	if got != want {
		t.Errorf("formatLabels: got %q want %q", got, want)
	}
	if formatLabels(nil) != "" {
		t.Error("formatLabels(nil) should be empty")
	}
}
