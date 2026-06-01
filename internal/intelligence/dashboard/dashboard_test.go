package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	// Seed minimal data.
	st := store.New(database)
	root := t.TempDir()
	// Timestamp is relative-to-now so the seeded action always falls
	// within any "last N days" query window. Hardcoded dates here turn
	// into time bombs as the clock advances past the test window — the
	// previous 2026-04-16 seed silently broke TestAPITimeseriesActions
	// and TestAPITools once the test ran past 2026-05-16.
	base := time.Now().UTC().Add(-time.Hour)
	_, err = st.Ingest(context.Background(), []models.ToolEvent{{
		SourceFile: "f", SourceEventID: "e1", SessionID: "sA",
		ProjectRoot: root, Timestamp: base, Tool: models.ToolClaudeCode,
		ActionType: models.ActionReadFile, Target: "a.go", Success: true,
	}}, nil, store.IngestOptions{})
	if err != nil {
		t.Fatal(err)
	}

	s, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	return s, root
}

// TestIndexHTML_Served confirms `/` serves the React SPA shell after
// the Phase 8 cutover. The legacy `static/index.html` is gone; the
// rendered HTML now comes from the Vite build embedded under
// internal/intelligence/dashboard/webapp/dist.
func TestIndexHTML_Served(t *testing.T) {
	s, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("GET /: %d", rr.Code)
	}
	body, _ := io.ReadAll(rr.Body)
	if !strings.Contains(string(body), "SuperBased Observer") {
		t.Errorf("missing title: %s", body)
	}
	if !strings.Contains(string(body), `<div id="root"></div>`) {
		t.Errorf("missing React root marker: %s", body)
	}
}

// TestAPIWatcherHealth_OrphanCursorFiltered pins the v1.4.25 fix: a
// parse_cursors row whose path no current adapter recognises (older
// adapter versions tracked broader patterns) gets tagged
// orphan_unmatched and excluded from the behind_count. Without this
// the user-visible banner would show such rows forever — Rescan
// can't process them, so they'd never close.
func TestAPIWatcherHealth_OrphanCursorFiltered(t *testing.T) {
	s, root := newTestServer(t)

	// Seed two parse_cursors rows. One file lives in t.TempDir() (the
	// "real" file the predicate will accept) and is on-disk so a
	// stat() succeeds; the other is fictional and the predicate
	// rejects it as orphan.
	realPath := filepath.Join(root, "rollout-real.jsonl")
	if err := writeFile(realPath, "x"); err != nil {
		t.Fatal(err)
	}
	orphanPath := filepath.Join(root, "GitHub Copilot Chat.log")
	if err := writeFile(orphanPath, "yyyy"); err != nil {
		t.Fatal(err)
	}

	// Both at offset 0 so file_size > byte_offset → both LOOK behind.
	if _, err := s.opts.DB.ExecContext(
		context.Background(),
		`INSERT INTO parse_cursors (source_file, byte_offset, last_parsed) VALUES (?, 0, ?), (?, 0, ?)`,
		realPath, time.Now().Format(time.RFC3339Nano),
		orphanPath, time.Now().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}

	// Predicate accepts only the rollout pattern.
	s.opts.RecognizesSessionFile = func(p string) bool {
		return strings.HasPrefix(filepath.Base(p), "rollout-")
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/health/watcher", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status: %d", rr.Code)
	}
	var got map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["behind_count"].(float64) != 1 {
		t.Errorf("behind_count: %v want 1 (orphan must NOT be counted)", got["behind_count"])
	}
	if got["orphan_count"].(float64) != 1 {
		t.Errorf("orphan_count: %v want 1", got["orphan_count"])
	}
	files := got["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("files: %d want 2 (both still listed, only counts differ)", len(files))
	}
	// Find the orphan and verify the flag.
	var orphanFlagged bool
	for _, fAny := range files {
		f := fAny.(map[string]any)
		if strings.HasSuffix(f["path"].(string), "GitHub Copilot Chat.log") {
			orphanFlagged = f["orphan_unmatched"] == true
		}
	}
	if !orphanFlagged {
		t.Error("orphan row should carry orphan_unmatched=true")
	}
}

// writeFile is a tiny helper for the orphan test.
func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}

// TestAPIWatcherHealth_SuspectedMisrouted pins the v1.4.51
// suspected_misrouted heuristic: a parse_cursors row whose cursor
// reached EOF on a non-trivial file but the actions table has zero
// rows for that source_file gets flagged as a suspected pre-v1.4.51
// misroute. The operator-driven recovery is
// `observer scan --force --adapter <name>`.
func TestAPIWatcherHealth_SuspectedMisrouted(t *testing.T) {
	s, root := newTestServer(t)

	// A non-trivial file (well above the 1KB threshold) the predicate
	// will accept.
	bigPath := filepath.Join(root, "rollout-suspect.jsonl")
	body := strings.Repeat("padding line\n", 200) // ~2.4 KB
	if err := writeFile(bigPath, body); err != nil {
		t.Fatal(err)
	}
	// Cursor seeded at EOF: byte_offset == file size → caught up.
	if _, err := s.opts.DB.ExecContext(
		context.Background(),
		`INSERT INTO parse_cursors (source_file, byte_offset, last_parsed) VALUES (?, ?, ?)`,
		bigPath, len(body), time.Now().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
	// Crucially: no rows inserted into actions for this source_file.
	// That's the pre-v1.4.51 fingerprint (claude-code "parsed" a
	// Codex file, advanced cursor to EOF, emitted nothing).

	s.opts.RecognizesSessionFile = func(p string) bool {
		return strings.HasPrefix(filepath.Base(p), "rollout-")
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/health/watcher", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status: %d", rr.Code)
	}
	var got map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["suspected_misrouted_count"].(float64) != 1 {
		t.Errorf("suspected_misrouted_count: %v want 1", got["suspected_misrouted_count"])
	}
	files := got["files"].([]any)
	var flagged bool
	for _, fAny := range files {
		f := fAny.(map[string]any)
		if f["suspected_misrouted"] == true {
			flagged = true
			if reason, ok := f["misroute_reason"].(string); !ok || reason == "" {
				t.Errorf("suspected_misrouted row missing misroute_reason: %v", f)
			}
		}
	}
	if !flagged {
		t.Error("expected at least one row with suspected_misrouted=true")
	}
}

// TestAPIWatcherHealth_SmallFileNotFlaggedAsSuspect pins the
// 1KB-min-bytes guard — empty/stub session files (cursor at EOF, no
// actions) must NOT be flagged because real sessions sometimes start
// with a session_meta line and nothing else when the user closes
// the IDE immediately.
func TestAPIWatcherHealth_SmallFileNotFlaggedAsSuspect(t *testing.T) {
	s, root := newTestServer(t)

	smallPath := filepath.Join(root, "rollout-stub.jsonl")
	body := "stub" // 4 bytes
	if err := writeFile(smallPath, body); err != nil {
		t.Fatal(err)
	}
	if _, err := s.opts.DB.ExecContext(
		context.Background(),
		`INSERT INTO parse_cursors (source_file, byte_offset, last_parsed) VALUES (?, ?, ?)`,
		smallPath, len(body), time.Now().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
	s.opts.RecognizesSessionFile = func(p string) bool { return true }

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/health/watcher", nil)
	s.Handler().ServeHTTP(rr, req)
	var got map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["suspected_misrouted_count"].(float64) != 0 {
		t.Errorf("suspected_misrouted_count: %v want 0 (file below 1KB threshold)", got["suspected_misrouted_count"])
	}
}

func TestAPIStatus(t *testing.T) {
	s, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status: %d", rr.Code)
	}
	var got map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	counts, ok := got["counts"].(map[string]any)
	if !ok {
		t.Fatalf("missing counts map: %+v", got)
	}
	if counts["sessions"] == nil || counts["actions"] == nil {
		t.Errorf("missing session/action counts: %+v", counts)
	}
}

func TestAPICodexSupport_ChatGPTJSONLOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)
	now := time.Now().UTC()
	_, err = st.Ingest(context.Background(), []models.ToolEvent{{
		SourceFile: "codex.jsonl", SourceEventID: "e1", SessionID: "codex-s1",
		ProjectRoot: t.TempDir(), Timestamp: now.Add(-5 * time.Minute),
		Tool: models.ToolCodex, ActionType: models.ActionReadFile, Target: "a.go", Success: true,
	}}, []models.TokenEvent{{
		SessionID: "codex-s1", Timestamp: now.Add(-4 * time.Minute), Tool: models.ToolCodex,
		Model: "gpt-5", InputTokens: 100, OutputTokens: 20, Source: models.TokenSourceJSONL,
		Reliability: models.ReliabilityApproximate, SourceFile: "codex.jsonl", SourceEventID: "tok1",
	}}, store.IngestOptions{})
	if err != nil {
		t.Fatal(err)
	}

	codexHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"auth_mode":"chatgpt"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)

	s, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/codex/support?days=30", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["auth_mode"] != "chatgpt" {
		t.Fatalf("auth_mode: %v", got["auth_mode"])
	}
	if got["mode"] != "jsonl_only" {
		t.Fatalf("mode: %v", got["mode"])
	}
	if got["compression_mode"] != "unavailable" {
		t.Fatalf("compression_mode: %v", got["compression_mode"])
	}
	if got["linked_api_turn_count"].(float64) != 0 {
		t.Fatalf("linked_api_turn_count: %v", got["linked_api_turn_count"])
	}
	summary := got["summary"].(string)
	if !strings.Contains(summary, "ChatGPT-auth") {
		t.Fatalf("summary missing ChatGPT explanation: %q", summary)
	}
}

func TestAPICodexSupport_ChatGPTProxyUnlinked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)
	now := time.Now().UTC()
	_, err = st.Ingest(context.Background(), []models.ToolEvent{{
		SourceFile: "codex.jsonl", SourceEventID: "e1", SessionID: "codex-s1",
		ProjectRoot: t.TempDir(), Timestamp: now.Add(-5 * time.Minute),
		Tool: models.ToolCodex, ActionType: models.ActionReadFile, Target: "a.go", Success: true,
	}}, []models.TokenEvent{{
		SessionID: "codex-s1", Timestamp: now.Add(-4 * time.Minute), Tool: models.ToolCodex,
		Model: "gpt-5.5", InputTokens: 100, OutputTokens: 20, Source: models.TokenSourceJSONL,
		Reliability: models.ReliabilityApproximate, SourceFile: "codex.jsonl", SourceEventID: "tok1",
	}}, store.IngestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		Timestamp:                  now.Add(-3 * time.Minute),
		Provider:                   models.ProviderOpenAI,
		Model:                      "gpt-5.5",
		CompressionOriginalBytes:   4000,
		CompressionCompressedBytes: 900,
		CompressionCount:           1,
	}); err != nil {
		t.Fatal(err)
	}

	codexHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"auth_mode":"chatgpt"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)

	s, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/codex/support?days=30", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["mode"] != "proxy_unlinked+jsonl" {
		t.Fatalf("mode: %v", got["mode"])
	}
	if got["compression_mode"] != "live_unlinked" {
		t.Fatalf("compression_mode: %v", got["compression_mode"])
	}
	if got["unlinked_api_turn_count"].(float64) != 1 {
		t.Fatalf("unlinked_api_turn_count: %v", got["unlinked_api_turn_count"])
	}
}

func TestAPICodexSupport_ProxyLinked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)
	root := t.TempDir()
	now := time.Now().UTC()
	_, err = st.Ingest(context.Background(), []models.ToolEvent{{
		SourceFile: "codex.jsonl", SourceEventID: "e1", SessionID: "codex-s1",
		ProjectRoot: root, Timestamp: now.Add(-10 * time.Minute),
		Tool: models.ToolCodex, ActionType: models.ActionReadFile, Target: "a.go", Success: true,
	}}, []models.TokenEvent{{
		SessionID: "codex-s1", Timestamp: now.Add(-9 * time.Minute), Tool: models.ToolCodex,
		Model: "gpt-5", InputTokens: 100, OutputTokens: 20, Source: models.TokenSourceJSONL,
		Reliability: models.ReliabilityApproximate, SourceFile: "codex.jsonl", SourceEventID: "tok1",
	}}, store.IngestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "codex-s1", Timestamp: now.Add(-8 * time.Minute),
		Provider: models.ProviderOpenAI, Model: "gpt-5", InputTokens: 100, OutputTokens: 20,
	}); err != nil {
		t.Fatal(err)
	}

	s, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/codex/support?days=30", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["mode"] != "proxy+jsonl" {
		t.Fatalf("mode: %v", got["mode"])
	}
	if got["compression_mode"] != "live" {
		t.Fatalf("compression_mode: %v", got["compression_mode"])
	}
	if got["linked_api_turn_count"].(float64) != 1 {
		t.Fatalf("linked_api_turn_count: %v", got["linked_api_turn_count"])
	}
}

func TestAPICost_Shape(t *testing.T) {
	s, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cost?days=30&group_by=model", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("cost: %d  body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["group_by"] == nil {
		t.Errorf("missing group_by: %+v", got)
	}
}

// TestAPICost_TotalCompression verifies that when api_turns carries
// compression stats, /api/cost surfaces them in the total_compression
// block and at least one row's compression.original_bytes > 0.
func TestAPICost_TotalCompression(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	st := store.New(database)
	turn := models.APITurn{
		SessionID:                  "sA",
		Timestamp:                  time.Now().UTC().Add(-time.Hour),
		Provider:                   models.ProviderAnthropic,
		Model:                      "claude-sonnet-4",
		InputTokens:                100,
		OutputTokens:               50,
		CompressionOriginalBytes:   10000,
		CompressionCompressedBytes: 3000,
		CompressionCount:           2,
		CompressionDroppedCount:    1,
		CompressionMarkerCount:     1,
	}
	if _, err := st.InsertAPITurn(context.Background(), turn); err != nil {
		t.Fatalf("InsertAPITurn: %v", err)
	}

	srv, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cost?days=30&group_by=model", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("cost: %d  body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	tc, ok := got["total_compression"].(map[string]any)
	if !ok {
		t.Fatalf("missing total_compression: %+v", got)
	}
	orig, _ := tc["original_bytes"].(float64)
	compressed, _ := tc["compressed_bytes"].(float64)
	if int(orig) != 10000 {
		t.Errorf("original_bytes: got %v want 10000", tc["original_bytes"])
	}
	if int(compressed) != 3000 {
		t.Errorf("compressed_bytes: got %v want 3000", tc["compressed_bytes"])
	}
	rows, _ := got["rows"].([]any)
	if len(rows) == 0 {
		t.Fatalf("no rows in cost summary: %+v", got)
	}
	row0, _ := rows[0].(map[string]any)
	rc, ok := row0["compression"].(map[string]any)
	if !ok {
		t.Fatalf("row[0] missing compression: %+v", row0)
	}
	if int(rc["original_bytes"].(float64)) != 10000 {
		t.Errorf("row compression original_bytes: %v", rc["original_bytes"])
	}
}

// TestAPICompressionEvents seeds two compression_events rows and an
// owning api_turn, then verifies /api/compression/events returns them
// with derived saved_bytes and joined model. Pins the contract behind
// the new "Recent compression events" table on the Compression tab.
func TestAPICompressionEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)
	now := time.Now().UTC()
	turn := models.APITurn{
		SessionID: "s1", Timestamp: now.Add(-time.Hour),
		Provider: models.ProviderAnthropic, Model: "claude-sonnet-4",
		InputTokens: 100, OutputTokens: 50,
		CompressionOriginalBytes:   30000,
		CompressionCompressedBytes: 8000,
		CompressionEvents: []models.CompressionEvent{
			{Mechanism: "json", OriginalBytes: 20000, CompressedBytes: 5000, MsgIndex: 3},
			{Mechanism: "drop", OriginalBytes: 10000, CompressedBytes: 0, MsgIndex: 5, ImportanceScore: 0.12},
		},
	}
	for i := range turn.CompressionEvents {
		turn.CompressionEvents[i].Timestamp = turn.Timestamp
	}
	if _, err := st.InsertAPITurn(context.Background(), turn); err != nil {
		t.Fatalf("InsertAPITurn: %v", err)
	}

	srv, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/compression/events?days=30", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("/api/compression/events: %d  body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if int(got["total"].(float64)) != 2 {
		t.Fatalf("total: got %v want 2", got["total"])
	}
	rows, _ := got["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("rows: got %d want 2", len(rows))
	}
	// Rows ordered by timestamp DESC, id DESC — the second event
	// inserted (drop) lands first.
	first := rows[0].(map[string]any)
	if first["mechanism"] != "drop" {
		t.Errorf("first row mechanism: got %v want drop", first["mechanism"])
	}
	if int(first["saved_bytes"].(float64)) != 10000 {
		t.Errorf("first saved_bytes: got %v want 10000", first["saved_bytes"])
	}
	if first["model"] != "claude-sonnet-4" {
		t.Errorf("first model: got %v want claude-sonnet-4", first["model"])
	}
	// $ saved is priced via the cost engine — claude-sonnet-4 is in
	// baked-in pricing at $3 / 1M input. saved_tokens_est for the drop
	// row = 10000 / 4 = 2500 tokens → $0.0075.
	dropUSD, _ := first["saved_usd_est"].(float64)
	wantDropUSD := 2500.0 * 3.0 / 1_000_000
	if !approxEqual(dropUSD, wantDropUSD, 1e-9) {
		t.Errorf("first saved_usd_est: got %v want %v", dropUSD, wantDropUSD)
	}

	// Mechanism filter: only json
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/api/compression/events?days=30&mechanism=json", nil)
	srv.Handler().ServeHTTP(rr2, req2)
	if rr2.Code != 200 {
		t.Fatalf("filtered: %d", rr2.Code)
	}
	var got2 map[string]any
	json.NewDecoder(rr2.Body).Decode(&got2)
	if int(got2["total"].(float64)) != 1 {
		t.Errorf("filtered total: got %v want 1", got2["total"])
	}
}

// TestAPICompressionTimeseries verifies the per-day per-mechanism
// rollup shape — feeds the "Savings by mechanism" stacked-bar chart.
func TestAPICompressionTimeseries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)
	now := time.Now().UTC()
	turn := models.APITurn{
		SessionID: "s1", Timestamp: now.Add(-2 * time.Hour),
		Provider: models.ProviderAnthropic, Model: "claude-sonnet-4",
		CompressionEvents: []models.CompressionEvent{
			{Mechanism: "json", Timestamp: now.Add(-2 * time.Hour), OriginalBytes: 10000, CompressedBytes: 3000},
			{Mechanism: "json", Timestamp: now.Add(-2 * time.Hour), OriginalBytes: 5000, CompressedBytes: 1000},
			{Mechanism: "drop", Timestamp: now.Add(-2 * time.Hour), OriginalBytes: 8000, CompressedBytes: 0},
		},
	}
	if _, err := st.InsertAPITurn(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	srv, _ := New(Options{DB: database, DBPath: path})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/compression/timeseries?bucket=day&days=30", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("/api/compression/timeseries: %d  body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	json.NewDecoder(rr.Body).Decode(&got)
	series, _ := got["series"].([]any)
	if len(series) != 1 {
		t.Fatalf("series: got %d want 1", len(series))
	}
	point := series[0].(map[string]any)
	by, _ := point["by_mechanism"].(map[string]any)
	jsonStats, _ := by["json"].(map[string]any)
	if int(jsonStats["count"].(float64)) != 2 {
		t.Errorf("json count: got %v want 2", jsonStats["count"])
	}
	// 10000+5000-3000-1000 = 11000 saved by json
	if int(jsonStats["saved_bytes"].(float64)) != 11000 {
		t.Errorf("json saved_bytes: got %v want 11000", jsonStats["saved_bytes"])
	}
	dropStats, _ := by["drop"].(map[string]any)
	if int(dropStats["saved_bytes"].(float64)) != 8000 {
		t.Errorf("drop saved_bytes: got %v want 8000", dropStats["saved_bytes"])
	}

	// Per-mechanism $ saved is priced via the cost engine —
	// claude-sonnet-4 is in baked-in pricing at $3 / 1M input tokens
	// (matches TestAPICost_CompressionSavingsDerived). Tokens =
	// saved_bytes / 4; $ = tokens × input_rate / 1M.
	jsonUSD, _ := jsonStats["saved_usd_est"].(float64)
	wantJSONUSD := (11000.0 / 4) * 3.0 / 1_000_000
	if !approxEqual(jsonUSD, wantJSONUSD, 1e-9) {
		t.Errorf("json saved_usd_est: got %v want %v", jsonUSD, wantJSONUSD)
	}
	dropUSD, _ := dropStats["saved_usd_est"].(float64)
	wantDropUSD := (8000.0 / 4) * 3.0 / 1_000_000
	if !approxEqual(dropUSD, wantDropUSD, 1e-9) {
		t.Errorf("drop saved_usd_est: got %v want %v", dropUSD, wantDropUSD)
	}
	totalUSD, _ := point["total_saved_usd_est"].(float64)
	wantTotalUSD := wantJSONUSD + wantDropUSD
	if !approxEqual(totalUSD, wantTotalUSD, 1e-9) {
		t.Errorf("total_saved_usd_est: got %v want %v", totalUSD, wantTotalUSD)
	}
}

func approxEqual(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= eps
}

// TestAPIToolsBreakdown seeds two tools with action-type variety and
// asserts /api/tools/breakdown returns per-tool by_type counts.
// Pins the contract behind the action-type-mix chart on Tools tab.
func TestAPIToolsBreakdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)
	root := t.TempDir()
	now := time.Now().UTC()
	events := []models.ToolEvent{
		// claude-code: 2 read, 1 edit
		{
			SourceFile: "f1", SourceEventID: "1", SessionID: "sA", ProjectRoot: root,
			Timestamp: now.Add(-time.Hour), Tool: models.ToolClaudeCode,
			ActionType: models.ActionReadFile, Target: "a.go", Success: true,
		},
		{
			SourceFile: "f1", SourceEventID: "2", SessionID: "sA", ProjectRoot: root,
			Timestamp: now.Add(-50 * time.Minute), Tool: models.ToolClaudeCode,
			ActionType: models.ActionReadFile, Target: "b.go", Success: true,
		},
		{
			SourceFile: "f1", SourceEventID: "3", SessionID: "sA", ProjectRoot: root,
			Timestamp: now.Add(-40 * time.Minute), Tool: models.ToolClaudeCode,
			ActionType: models.ActionEditFile, Target: "a.go", Success: true,
		},
		// cursor: 2 edit
		{
			SourceFile: "f2", SourceEventID: "4", SessionID: "sB", ProjectRoot: root,
			Timestamp: now.Add(-30 * time.Minute), Tool: models.ToolCursor,
			ActionType: models.ActionEditFile, Target: "c.go", Success: true,
		},
		{
			SourceFile: "f2", SourceEventID: "5", SessionID: "sB", ProjectRoot: root,
			Timestamp: now.Add(-20 * time.Minute), Tool: models.ToolCursor,
			ActionType: models.ActionEditFile, Target: "d.go", Success: true,
		},
	}
	if _, err := st.Ingest(context.Background(), events, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}
	srv, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tools/breakdown?days=30", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("/api/tools/breakdown: %d  body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	rows, _ := got["tools"].([]any)
	if len(rows) != 2 {
		t.Fatalf("expected 2 tools: got %d", len(rows))
	}
	// claude-code should be first (3 actions vs cursor's 2).
	first := rows[0].(map[string]any)
	if first["tool"] != "claude-code" {
		t.Errorf("ordering: got first=%v want claude-code", first["tool"])
	}
	if int(first["total"].(float64)) != 3 {
		t.Errorf("claude-code total: %v want 3", first["total"])
	}
	bt, _ := first["by_type"].(map[string]any)
	if int(bt["read_file"].(float64)) != 2 {
		t.Errorf("claude-code read_file: %v want 2", bt["read_file"])
	}
	if int(bt["edit_file"].(float64)) != 1 {
		t.Errorf("claude-code edit_file: %v want 1", bt["edit_file"])
	}
}

// TestAPICost_CompressionSavingsDerived seeds an api_turn with known
// compression byte deltas and asserts the cost engine derives
// tokens_saved_est (=saved_bytes/4) and cost_saved_usd_est
// (=tokens_saved × model_input_rate) onto the row + total_compression.
// Pins the new headline metrics on the redesigned Compression tab.
func TestAPICost_CompressionSavingsDerived(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)
	// claude-sonnet-4 is in the baked-in pricing table at $3 / 1M input
	// (as of 2026-04). Verify we look up correctly and compute.
	turn := models.APITurn{
		SessionID: "s1", Timestamp: time.Now().UTC().Add(-time.Hour),
		Provider: models.ProviderAnthropic, Model: "claude-sonnet-4",
		InputTokens:                100,
		OutputTokens:               50,
		CompressionOriginalBytes:   40000,
		CompressionCompressedBytes: 8000, // saved 32000 bytes
		CompressionCount:           4,
		CompressionDroppedCount:    2,
		CompressionMarkerCount:     2,
	}
	if _, err := st.InsertAPITurn(context.Background(), turn); err != nil {
		t.Fatalf("InsertAPITurn: %v", err)
	}
	srv, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cost?days=30&group_by=model", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("/api/cost: %d  body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	tc, ok := got["total_compression"].(map[string]any)
	if !ok {
		t.Fatalf("missing total_compression: %+v", got)
	}
	// 32000 saved bytes / 4 = 8000 tokens
	if int(tc["tokens_saved_est"].(float64)) != 8000 {
		t.Errorf("total_compression.tokens_saved_est: got %v want 8000", tc["tokens_saved_est"])
	}
	// 8000 × $3/1M = $0.024 (claude-sonnet-4 input rate)
	usd, _ := tc["cost_saved_usd_est"].(float64)
	if usd <= 0 {
		t.Errorf("total_compression.cost_saved_usd_est: got %v want > 0 (model has known pricing)", usd)
	}
	rows, _ := got["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row: got %d", len(rows))
	}
	row := rows[0].(map[string]any)
	rc, _ := row["compression"].(map[string]any)
	if int(rc["tokens_saved_est"].(float64)) != 8000 {
		t.Errorf("row[0].compression.tokens_saved_est: got %v want 8000", rc["tokens_saved_est"])
	}
}

// TestAPICost_TokenMathReconciles seeds api_turns with known per-bucket
// token counts and asserts the /api/cost summary sums them correctly,
// per-row and in the total_tokens aggregate. Pins the contract behind
// the user-visible "Net In / Cache R / Cache W / Output" cost-table
// columns and the cost-summary line.
func TestAPICost_TokenMathReconciles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)
	now := time.Now().UTC()
	turns := []models.APITurn{
		{
			SessionID: "s1", Timestamp: now.Add(-2 * time.Hour),
			Provider: models.ProviderAnthropic, Model: "claude-sonnet-4",
			InputTokens: 1000, CacheReadTokens: 4000,
			CacheCreationTokens: 200, OutputTokens: 500,
		},
		{
			SessionID: "s1", Timestamp: now.Add(-time.Hour),
			Provider: models.ProviderAnthropic, Model: "claude-sonnet-4",
			InputTokens: 2000, CacheReadTokens: 6000,
			CacheCreationTokens: 100, OutputTokens: 800,
		},
	}
	for _, tu := range turns {
		if _, err := st.InsertAPITurn(context.Background(), tu); err != nil {
			t.Fatalf("InsertAPITurn: %v", err)
		}
	}

	srv, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cost?days=30&group_by=model", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("/api/cost: %d  body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	tt, ok := got["total_tokens"].(map[string]any)
	if !ok {
		t.Fatalf("missing total_tokens: %+v", got)
	}
	// 1000 + 2000 = 3000 net input
	if int(tt["input"].(float64)) != 3000 {
		t.Errorf("total_tokens.input: got %v want 3000", tt["input"])
	}
	// 4000 + 6000 = 10000 cache read
	if int(tt["cache_read"].(float64)) != 10000 {
		t.Errorf("total_tokens.cache_read: got %v want 10000", tt["cache_read"])
	}
	// 200 + 100 = 300 cache creation
	if int(tt["cache_creation"].(float64)) != 300 {
		t.Errorf("total_tokens.cache_creation: got %v want 300", tt["cache_creation"])
	}
	// 500 + 800 = 1300 output
	if int(tt["output"].(float64)) != 1300 {
		t.Errorf("total_tokens.output: got %v want 1300", tt["output"])
	}
	// Per-row tokens should reconcile too — single row since both turns
	// are the same model.
	rows, _ := got["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row (one model): got %d", len(rows))
	}
	row := rows[0].(map[string]any)
	rt := row["tokens"].(map[string]any)
	if int(rt["input"].(float64)) != 3000 ||
		int(rt["cache_read"].(float64)) != 10000 ||
		int(rt["cache_creation"].(float64)) != 300 ||
		int(rt["output"].(float64)) != 1300 {
		t.Errorf("row[0].tokens mismatch: %+v", rt)
	}
	if int(row["turn_count"].(float64)) != 2 {
		t.Errorf("row[0].turn_count: got %v want 2", row["turn_count"])
	}
}

// TestAPIExportXLSX_Sheets seeds minimal data and verifies the export
// endpoint returns a non-empty xlsx with the expected sheet names.
// Catches regressions where a sheet writer panics or excelize fails to
// render the workbook.
func TestAPIExportXLSX_Sheets(t *testing.T) {
	s, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/export.xlsx?days=30", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("/api/export.xlsx: %d  body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); !strings.Contains(got, "spreadsheetml") {
		t.Errorf("Content-Type: got %q want xlsx", got)
	}
	if cd := rr.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Errorf("Content-Disposition: got %q", cd)
	}
	body := rr.Body.Bytes()
	if len(body) < 1024 {
		t.Fatalf("xlsx body suspiciously small: %d bytes", len(body))
	}
	// xlsx is a zip — check the magic.
	if body[0] != 'P' || body[1] != 'K' {
		t.Errorf("body doesn't start with PK zip magic: %q", body[:8])
	}
	// Open the workbook from the response bytes and assert the expected
	// sheet names exist.
	f, err := excelize.OpenReader(strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("excelize.OpenReader: %v", err)
	}
	defer f.Close()
	want := map[string]bool{
		"Overview": false, "Cost": false, "Sessions": false,
		"Actions": false, "Tools": false, "Compression": false,
		"Discovery — stale rereads": false, "Discovery — repeated commands": false,
		"Patterns": false,
	}
	for _, name := range f.GetSheetList() {
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for k, ok := range want {
		if !ok {
			t.Errorf("missing sheet: %q", k)
		}
	}
}

// Phase 8 cutover (2026-05-16) removed the legacy vanilla-JS
// SPA whose static markup these regression guards asserted on:
//
//   - TestIndexHTML_HasSavingsSection (compression anchors / ids)
//   - TestIndexHTML_CostPanelExposesAllTokenBuckets (Net Input /
//     Cache Read / Cache Write / Output literal text and the JS
//     `tokens.cache_read` / `tokens.cache_creation` template
//     literals)
//
// The React build renders all of that client-side from the API,
// so the markers don't appear in server-rendered HTML. The
// data-correctness intent (every billing bucket is exposed
// separately, never collapsed) lives on in the API tests:
// TestAPICost_TokenMathReconciles, TestAPICompressionTimeseries,
// TestAPICost_TotalCompression, TestAPIToolsBreakdown.

func TestAPISessions_ReturnsRows(t *testing.T) {
	s, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/sessions?limit=10", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("sessions: %d", rr.Code)
	}
	var got struct {
		Rows  []map[string]any `json:"rows"`
		Page  int              `json:"page"`
		Limit int              `json:"limit"`
		Total int              `json:"total"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Total != 1 {
		t.Fatalf("expected total=1, got %d", got.Total)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("expected 1 session row, got %d", len(got.Rows))
	}
	if got.Page != 1 || got.Limit != 10 {
		t.Errorf("page/limit: got %d/%d want 1/10", got.Page, got.Limit)
	}
	// Regression guard for A4: total_actions must reflect the count of
	// actions joined live, not the never-written sessions.total_actions
	// stored column. Fixture seeds exactly one ToolEvent for "sA".
	if got.Rows[0]["total_actions"].(float64) != 1 {
		t.Errorf("total_actions: got %v want 1 (seed has one action)",
			got.Rows[0]["total_actions"])
	}
}

// TestAPISessions_AttachesTokensAndCost pins the per-session token/cost
// rollup wired into /api/sessions. When an api_turns row exists for a
// session, total_tokens, cost_usd, and cost_reliability should be
// populated; sessions with no token data render zeroed fields the
// frontend renders as "—".
func TestAPISessions_AttachesTokensAndCost(t *testing.T) {
	s, _ := newTestServer(t)

	// Seed one api_turn for the existing "sA" session. newTestServer
	// already wrote the session via the action; we just attach billing
	// data on top.
	st := store.New(s.opts.DB)
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sA", Provider: models.ProviderAnthropic,
		Model:        "claude-sonnet-4-6",
		InputTokens:  1_000_000,
		OutputTokens: 50_000,
		Timestamp:    time.Date(2026, 4, 16, 10, 0, 5, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/sessions?limit=10", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Rows []struct {
			ID                  string  `json:"id"`
			InputTokens         int64   `json:"input_tokens"`
			OutputTokens        int64   `json:"output_tokens"`
			CacheReadTokens     int64   `json:"cache_read_tokens"`
			CacheCreationTokens int64   `json:"cache_creation_tokens"`
			TotalTokens         int64   `json:"total_tokens"`
			CostUSD             float64 `json:"cost_usd"`
			CostReliability     string  `json:"cost_reliability"`
		} `json:"rows"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("expected 1 session, got %d", len(got.Rows))
	}
	row := got.Rows[0]
	if row.ID != "sA" {
		t.Fatalf("session id = %q want sA", row.ID)
	}
	wantTokens := int64(1_000_000 + 50_000)
	if row.TotalTokens != wantTokens {
		t.Errorf("total_tokens = %d want %d", row.TotalTokens, wantTokens)
	}
	// v1.4.51 — per-bucket token fields. Verify the breakdown matches
	// the InputTokens / OutputTokens we seeded, and that the cache
	// buckets stay zero when no cache traffic was recorded.
	if row.InputTokens != 1_000_000 {
		t.Errorf("input_tokens = %d want 1_000_000", row.InputTokens)
	}
	if row.OutputTokens != 50_000 {
		t.Errorf("output_tokens = %d want 50_000", row.OutputTokens)
	}
	if row.CacheReadTokens != 0 || row.CacheCreationTokens != 0 {
		t.Errorf("cache buckets should be zero (no cache traffic seeded): R=%d W=%d",
			row.CacheReadTokens, row.CacheCreationTokens)
	}
	if row.CostReliability != "accurate" {
		t.Errorf("cost_reliability = %q want accurate (proxy row)", row.CostReliability)
	}
	if row.CostUSD <= 0 {
		t.Errorf("cost_usd = %f, want > 0 (claude-sonnet-4-6 is priced)", row.CostUSD)
	}
}

// TestAPISessions_DurationSecondsFromActions pins the v1.4.51 elapsed
// computation. A session with two actions 10 minutes apart should
// report duration_seconds ≈ 600 even when ended_at is NULL (open
// session — backend uses MAX(actions.timestamp) as the fallback).
func TestAPISessions_DurationSecondsFromActions(t *testing.T) {
	s, _ := newTestServer(t)

	// newTestServer seeded "sA" with one action; add a second action
	// 10 minutes later so duration_seconds resolves to 600.
	st := store.New(s.opts.DB)
	root := s.opts.DBPath // any non-empty stand-in for project root
	startTs := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	endTs := startTs.Add(10 * time.Minute)
	// First action at startTs, second 10 minutes later. The first
	// becomes started_at via UpsertSession's MIN-merge; the second
	// becomes MAX(actions.timestamp).
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{
		{
			SourceFile: "f", SourceEventID: "dur1", SessionID: "sDur",
			ProjectRoot: root, Timestamp: startTs,
			Tool: models.ToolClaudeCode, ActionType: models.ActionReadFile,
			Target: "a.go", Success: true,
		},
		{
			SourceFile: "f", SourceEventID: "dur2", SessionID: "sDur",
			ProjectRoot: root, Timestamp: endTs,
			Tool: models.ToolClaudeCode, ActionType: models.ActionReadFile,
			Target: "b.go", Success: true,
		},
	}, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/sessions?limit=10", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Rows []struct {
			ID              string `json:"id"`
			DurationSeconds int64  `json:"duration_seconds"`
			LastSeenAt      string `json:"last_seen_at"`
		} `json:"rows"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range got.Rows {
		if r.ID != "sDur" {
			continue
		}
		found = true
		if r.LastSeenAt == "" {
			t.Errorf("last_seen_at should fall back to MAX(actions.timestamp), got empty")
		}
		// Allow ±1s for any rounding inside RFC3339 parse.
		if r.DurationSeconds < 599 || r.DurationSeconds > 601 {
			t.Errorf("duration_seconds = %d want ≈ 600", r.DurationSeconds)
		}
	}
	if !found {
		t.Errorf("session sDur not in /api/sessions response")
	}
}

// TestAPISessions_HasProjectField pins the response contract for the
// fix that ties to issue #1 + #2 (project column + project filter):
// handleSessions must return the session's project root_path on every
// row so the frontend can render the column. Pre-fix the field was
// absent and every row in the dashboard table looked identical to the
// user, hiding cross-project sessions.
func TestAPISessions_HasProjectField(t *testing.T) {
	s, root := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/sessions?limit=10", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) == 0 {
		t.Fatal("expected at least one session row")
	}
	proj, ok := got.Rows[0]["project"].(string)
	if !ok {
		t.Fatalf("project field missing or wrong type: %+v", got.Rows[0])
	}
	if proj != root {
		t.Errorf("project: got %q want %q", proj, root)
	}
}

// TestAPISessions_ProjectFilter verifies that ?project=<root> scopes the
// result to a single project root.
func TestAPISessions_ProjectFilter(t *testing.T) {
	s, root := newTestServer(t)

	// Match — filter on the seeded project's root.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/sessions?limit=10&project="+root, nil)
	s.Handler().ServeHTTP(rr, req)
	var got struct {
		Rows  []map[string]any `json:"rows"`
		Total int              `json:"total"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Total != 1 {
		t.Errorf("filtered total: got %d want 1", got.Total)
	}

	// Miss — filter on a non-existent root.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet,
		"/api/sessions?limit=10&project=/does/not/exist", nil)
	s.Handler().ServeHTTP(rr, req)
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Total != 0 {
		t.Errorf("non-matching project filter: got total=%d want 0", got.Total)
	}
}

// TestAPISessions_DaysFilter pins the v1.4.28 fix: the global days
// window now applies to /api/sessions. Pre-fix the Sessions tab
// rendered everything regardless of the dropdown, so 30-day windows
// still showed 60+ day old rows.
func TestAPISessions_DaysFilter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	st := store.New(database)
	root := t.TempDir()
	now := time.Now().UTC()
	recent := now.Add(-3 * 24 * time.Hour)
	old := now.Add(-60 * 24 * time.Hour)

	// Two sessions: one 3 days ago, one 60 days ago. Both same project.
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{
		{
			SourceFile: "f1", SourceEventID: "e1", SessionID: "sRecent",
			ProjectRoot: root, Timestamp: recent, Tool: models.ToolClaudeCode,
			ActionType: models.ActionReadFile, Target: "a.go", Success: true,
		},
		{
			SourceFile: "f2", SourceEventID: "e2", SessionID: "sOld",
			ProjectRoot: root, Timestamp: old, Tool: models.ToolClaudeCode,
			ActionType: models.ActionReadFile, Target: "b.go", Success: true,
		},
	}, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}

	check := func(query string, wantTotal int) {
		t.Helper()
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/sessions?"+query, nil)
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("%s: status %d body=%s", query, rr.Code, rr.Body.String())
		}
		var got struct {
			Total int `json:"total"`
		}
		if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.Total != wantTotal {
			t.Errorf("%s: total=%d want %d", query, got.Total, wantTotal)
		}
	}

	check("limit=10", 2)         // no filter → both sessions visible.
	check("limit=10&days=7", 1)  // 7-day window → only sRecent.
	check("limit=10&days=30", 1) // 30-day window → still only sRecent.
	check("limit=10&days=90", 2) // 90-day window → both back.
}

// TestAPIProjects covers the /api/projects endpoint introduced for the
// project-filter dropdown on the toolbar.
func TestAPIProjects(t *testing.T) {
	s, root := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) == 0 {
		t.Fatal("expected at least one project row")
	}
	first := got.Rows[0]
	if first["root_path"] != root {
		t.Errorf("root_path: got %v want %q", first["root_path"], root)
	}
	if first["session_count"].(float64) != 1 {
		t.Errorf("session_count: got %v want 1", first["session_count"])
	}
	if first["action_count"].(float64) != 1 {
		t.Errorf("action_count: got %v want 1", first["action_count"])
	}
}

// TestAPISessionDetail covers the new /api/session/<id> endpoint.
func TestAPISessionDetail(t *testing.T) {
	s, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/session/sA", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["id"] != "sA" {
		t.Errorf("id: %v", got["id"])
	}
	if got["tool"] != "claude-code" {
		t.Errorf("tool: %v", got["tool"])
	}
	if got["total_actions"].(float64) != 1 {
		t.Errorf("total_actions: %v", got["total_actions"])
	}
	if _, ok := got["tokens"].(map[string]any); !ok {
		t.Errorf("tokens: missing or wrong shape: %v", got["tokens"])
	}
	if _, ok := got["tool_breakdown"].([]any); !ok {
		t.Errorf("tool_breakdown: missing or wrong shape: %v", got["tool_breakdown"])
	}
}

// TestAPISessionDetail_CostSurvivesUnattributedProxy is the regression
// guard for the cost_usd:0 polish item: the per-session cost lookup must
// still surface real cost when proxy api_turns rows for the same logical
// turn landed with NULL session_id (pre-pidbridge era) and would
// otherwise dedup the session-attributed JSONL row out of the rollup.
//
// Setup mirrors the live failure mode: session sA owns a token_usage row
// at minute T with model M and tokens P. An api_turn with session_id=""
// also exists at minute T with the same model+tokens — the cost engine's
// SourceAuto fingerprint dedup matches the JSONL row against the
// NULL-session proxy fingerprint and drops it, leaving sA with no row in
// the GroupBySession rollup. Pre-fix: cost_usd=0. Post-fix: cost is
// computed directly off the session's own token_usage row via
// engine.Compute, so it lands non-zero.
func TestAPISessionDetail_CostSurvivesUnattributedProxy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	st := store.New(database)
	root := t.TempDir()
	ts := time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC)

	// Create session sA via Ingest (also inserts one action).
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{{
		SourceFile: "f", SourceEventID: "e1", SessionID: "sA",
		ProjectRoot: root, Timestamp: ts, Tool: models.ToolClaudeCode,
		ActionType: models.ActionReadFile, Target: "a.go", Success: true,
	}}, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}

	// JSONL token_usage row for sA — model+tokens with known pricing so
	// engine.Compute returns a positive value.
	if _, err := st.InsertTokenEvents(context.Background(), []models.TokenEvent{{
		SourceFile: "tu", SourceEventID: "tu-e1", SessionID: "sA",
		Timestamp: ts, Tool: string(models.ToolClaudeCode),
		Model:               "claude-sonnet-4-6",
		InputTokens:         1000,
		OutputTokens:        2000,
		CacheReadTokens:     500,
		CacheCreationTokens: 250,
		Source:              "jsonl",
		Reliability:         "approximate",
	}}); err != nil {
		t.Fatal(err)
	}

	// Proxy api_turn at the same minute with the SAME tokens but NULL
	// session_id — matches the JSONL fingerprint, would dedup the JSONL
	// row out of the GroupBySession rollup pre-fix.
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID:           "",
		Timestamp:           ts,
		Provider:            models.ProviderAnthropic,
		Model:               "claude-sonnet-4-6",
		InputTokens:         1000,
		OutputTokens:        2000,
		CacheReadTokens:     500,
		CacheCreationTokens: 250,
	}); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/session/sA", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	costUSD, _ := got["cost_usd"].(float64)
	if costUSD <= 0 {
		t.Errorf("cost_usd: got %v, want > 0 (token_usage row for sA "+
			"with priced model claude-sonnet-4-6 should yield positive "+
			"cost even when a NULL-session proxy row shares its fingerprint)",
			got["cost_usd"])
	}
	// Spot-check the math: pricing for claude-sonnet-4-6 is
	// {Input: 3, Output: 15, CacheRead: 0.30, CacheCreation: 3.75} per
	// million tokens. Expected = 1000*3/1e6 + 2000*15/1e6 +
	// 500*0.30/1e6 + 250*3.75/1e6 = 0.003 + 0.030 + 0.00015 + 0.0009375
	// = 0.0341 (approximately).
	want := 0.0341
	if costUSD < want*0.99 || costUSD > want*1.01 {
		t.Errorf("cost_usd: got %v, want ≈ %v (claude-sonnet-4-6 pricing × seeded tokens)",
			costUSD, want)
	}
}

// TestAPISessionMessages_GroupsByMessageID pins the v1.4.17 per-message
// timeline endpoint. Multiple tool calls under the same upstream
// message.id collapse into a single row whose tool_call_count == N
// and tool_calls array carries the N entries; user_prompt actions
// surface as their own message rows.
func TestAPISessionMessages_GroupsByMessageID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	st := store.New(database)
	root := t.TempDir()
	ts := time.Date(2026, 4, 28, 6, 50, 0, 0, time.UTC)

	// One assistant turn (msg_A) emits two tool calls AND has a
	// token_usage row. Plus one user_prompt action with no token data.
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{
		{
			SourceFile: "f", SourceEventID: "u1", SessionID: "sA",
			ProjectRoot: root, Timestamp: ts, Tool: models.ToolClaudeCode,
			ActionType: models.ActionUserPrompt, Target: "do X", Success: true,
			RawToolInput: "do X with a much longer explanation than fits in target",
			MessageID:    "user_evt_1",
		},
		{
			SourceFile: "f", SourceEventID: "toolu_1", SessionID: "sA",
			ProjectRoot: root, Timestamp: ts.Add(time.Second), Tool: models.ToolClaudeCode,
			ActionType: models.ActionReadFile, Target: "main.go", Success: true,
			MessageID: "msg_A",
		},
		{
			SourceFile: "f", SourceEventID: "toolu_2", SessionID: "sA",
			ProjectRoot: root, Timestamp: ts.Add(2 * time.Second), Tool: models.ToolClaudeCode,
			ActionType: models.ActionRunCommand, Target: "ls -la", Success: true,
			RawToolInput: "[\"powershell.exe\",\"-Command\",\"Get-ChildItem -Recurse -Force C:\\\\Users\\\\marmu\\\\.codex\\\\sessions\"]",
			MessageID:    "msg_A",
		},
	}, []models.TokenEvent{
		{
			SourceFile: "f", SourceEventID: "msg_A", SessionID: "sA",
			Timestamp: ts.Add(time.Second), Tool: models.ToolClaudeCode,
			Model: "claude-opus-4-7", InputTokens: 100, OutputTokens: 500,
			MessageID: "msg_A",
			Source:    models.TokenSourceJSONL, Reliability: models.ReliabilityUnreliable,
		},
	}, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}
	var runCmdActionID int64
	if err := database.QueryRowContext(
		context.Background(),
		`SELECT id FROM actions WHERE session_id = ? AND source_event_id = ?`,
		"sA", "toolu_2",
	).Scan(&runCmdActionID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(
		context.Background(),
		`INSERT INTO action_excerpts (action_id, tool_name, target, excerpt, error_message)
		 VALUES (?, ?, ?, ?, ?)`,
		runCmdActionID, "Bash", "ls -la", "total 64\n-rw-r--r-- main.go", "",
	); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/session/sA/messages", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Messages []struct {
			MessageID     string `json:"message_id"`
			Role          string `json:"role"`
			Input         int64  `json:"input"`
			Output        int64  `json:"output"`
			ToolCallCount int    `json:"tool_call_count"`
			ToolCalls     []struct {
				ActionType string `json:"action_type"`
				Target     string `json:"target"`
				FullText   string `json:"full_text"`
				Excerpt    string `json:"excerpt"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages: got %d want 2 (user_evt_1 + msg_A)", len(got.Messages))
	}
	// User prompt row: no tokens, action_type=user_prompt.
	user := got.Messages[0]
	if user.MessageID != "user_evt_1" {
		t.Errorf("first message: got id=%q want user_evt_1", user.MessageID)
	}
	if user.Role != "user" {
		t.Errorf("first message role: got %q want user", user.Role)
	}
	if user.ToolCallCount != 1 || got.Messages[0].ToolCalls[0].ActionType != "user_prompt" {
		t.Errorf("first message tool calls: %+v", user.ToolCalls)
	}
	if got.Messages[0].ToolCalls[0].FullText != "do X with a much longer explanation than fits in target" {
		t.Errorf("user full_text: got %q", got.Messages[0].ToolCalls[0].FullText)
	}
	// Assistant row: tokens populated, two tool calls grouped.
	asst := got.Messages[1]
	if asst.MessageID != "msg_A" {
		t.Errorf("second message: got id=%q want msg_A", asst.MessageID)
	}
	if asst.Role != "assistant" {
		t.Errorf("second message role: got %q want assistant", asst.Role)
	}
	if asst.Input != 100 || asst.Output != 500 {
		t.Errorf("second message tokens: got in=%d out=%d want 100/500", asst.Input, asst.Output)
	}
	if asst.ToolCallCount != 2 {
		t.Errorf("second message tool_call_count: got %d want 2 (read_file + run_command)", asst.ToolCallCount)
	}
	if got.Messages[1].ToolCalls[1].Excerpt != "total 64\n-rw-r--r-- main.go" {
		t.Errorf("run_command excerpt: got %q", got.Messages[1].ToolCalls[1].Excerpt)
	}
	if got.Messages[1].ToolCalls[1].FullText != "powershell.exe -Command Get-ChildItem -Recurse -Force C:\\Users\\marmu\\.codex\\sessions" {
		t.Errorf("run_command full_text: got %q", got.Messages[1].ToolCalls[1].FullText)
	}
}

// TestAPISessionMessages_LongContextPerTurn pins the message-timeline
// pricing bug the user hit locally: message rows may aggregate multiple
// turns, but long-context repricing is per-turn. Summing the tokens
// first and then pricing once would falsely double the displayed cost
// for models like GPT-5.5 whenever the aggregate crosses the LC
// threshold even though each underlying turn stayed below it.
func TestAPISessionMessages_LongContextPerTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	st := store.New(database)
	root := t.TempDir()
	ts := time.Date(2026, 5, 5, 6, 18, 43, 0, time.UTC)

	if _, err := st.Ingest(context.Background(), []models.ToolEvent{{
		SourceFile: "f", SourceEventID: "toolu_1", SessionID: "sLC",
		ProjectRoot: root, Timestamp: ts, Tool: models.ToolCodex,
		ActionType: models.ActionReadFile, Target: "main.go", Success: true,
		MessageID: "msg_lc",
	}}, []models.TokenEvent{
		// Two distinct per-turn delta emissions under the same
		// message_id. Use slightly different token tuples so the v1.6.5
		// tuple-dedup leaves both alive (production-realistic shape:
		// codex delta token_count events never share byte-identical
		// last_token_usage). Sum still = 300_000 input.
		{
			SourceFile: "f", SourceEventID: "tk_1", SessionID: "sLC",
			ProjectRoot: root, Timestamp: ts, Tool: models.ToolCodex,
			Model: "gpt-5.5", InputTokens: 149_999, MessageID: "msg_lc",
			Source: models.TokenSourceJSONL, Reliability: models.ReliabilityApproximate,
		},
		{
			SourceFile: "f", SourceEventID: "tk_2", SessionID: "sLC",
			ProjectRoot: root, Timestamp: ts.Add(20 * time.Second), Tool: models.ToolCodex,
			Model: "gpt-5.5", InputTokens: 150_001, MessageID: "msg_lc",
			Source: models.TokenSourceJSONL, Reliability: models.ReliabilityApproximate,
		},
	}, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/session/sLC/messages", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Messages []struct {
			MessageID string  `json:"message_id"`
			Input     int64   `json:"input"`
			CostUSD   float64 `json:"cost_usd"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages: got %d want 1", len(got.Messages))
	}
	msg := got.Messages[0]
	if msg.MessageID != "msg_lc" {
		t.Fatalf("message_id: got %q want msg_lc", msg.MessageID)
	}
	if msg.Input != 300_000 {
		t.Fatalf("input: got %d want 300000", msg.Input)
	}
	const wantCost = 1.50 // 2 x (150K * $5 / 1M), both turns stay below 272K.
	if diff := msg.CostUSD - wantCost; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("cost_usd: got %v want %v (message rollup must sum per-turn pricing, not LC-price the aggregate)", msg.CostUSD, wantCost)
	}
}

// TestAPISessionMessages_TieBreakUserBeforeAssistant pins the v1.4.28
// ordering rule: when a user_prompt and its assistant turn share an
// identical timestamp (common when the proxy stamps the synthesized
// user row with the same wall-clock as the assistant turn), the user
// row must render first in the timeline.
func TestAPISessionMessages_TieBreakUserBeforeAssistant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	st := store.New(database)
	root := t.TempDir()
	ts := time.Date(2026, 5, 3, 6, 50, 0, 0, time.UTC)

	// User prompt and assistant tool call at the EXACT same timestamp.
	// Note: the user row is inserted SECOND so the natural insertion
	// order in the merged slice puts the assistant first — which is
	// what the bug looks like before the tie-break fix.
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{
		{
			SourceFile: "f", SourceEventID: "toolu_1", SessionID: "sB",
			ProjectRoot: root, Timestamp: ts, Tool: models.ToolClaudeCode,
			ActionType: models.ActionReadFile, Target: "main.go", Success: true,
			MessageID: "msg_B",
		},
		{
			SourceFile: "f", SourceEventID: "u1", SessionID: "sB",
			ProjectRoot: root, Timestamp: ts, Tool: models.ToolClaudeCode,
			ActionType: models.ActionUserPrompt, Target: "do X", Success: true,
			MessageID: "user_evt_1",
		},
	}, []models.TokenEvent{
		{
			SourceFile: "f", SourceEventID: "msg_B", SessionID: "sB",
			Timestamp: ts, Tool: models.ToolClaudeCode,
			Model: "claude-opus-4-7", InputTokens: 100, OutputTokens: 500,
			MessageID: "msg_B",
			Source:    models.TokenSourceJSONL, Reliability: models.ReliabilityUnreliable,
		},
	}, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/session/sB/messages", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages: got %d want 2", len(got.Messages))
	}
	if got.Messages[0].Role != "user" || got.Messages[1].Role != "assistant" {
		t.Errorf("tie-break ordering: got %q,%q want user,assistant",
			got.Messages[0].Role, got.Messages[1].Role)
	}
}

// TestAPISessionMessages_ElapsedMs pins v1.4.28's per-message
// wall-clock duration: each message exposes the gap (ms) to the
// next message in the timeline. The final message has no successor
// and reports null.
func TestAPISessionMessages_ElapsedMs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	st := store.New(database)
	root := t.TempDir()
	t0 := time.Date(2026, 5, 3, 6, 50, 0, 0, time.UTC)

	// User prompt → assistant turn 12s later → user follow-up 47s
	// after that. Three messages, two gaps.
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{
		{
			SourceFile: "f", SourceEventID: "u1", SessionID: "sE",
			ProjectRoot: root, Timestamp: t0, Tool: models.ToolClaudeCode,
			ActionType: models.ActionUserPrompt, Target: "do X", Success: true,
			MessageID: "user_evt_1",
		},
		{
			SourceFile: "f", SourceEventID: "toolu_1", SessionID: "sE",
			ProjectRoot: root, Timestamp: t0.Add(12 * time.Second), Tool: models.ToolClaudeCode,
			ActionType: models.ActionReadFile, Target: "main.go", Success: true,
			MessageID: "msg_E",
		},
		{
			SourceFile: "f", SourceEventID: "u2", SessionID: "sE",
			ProjectRoot: root, Timestamp: t0.Add(59 * time.Second), Tool: models.ToolClaudeCode,
			ActionType: models.ActionUserPrompt, Target: "now Y", Success: true,
			MessageID: "user_evt_2",
		},
	}, []models.TokenEvent{
		{
			SourceFile: "f", SourceEventID: "msg_E", SessionID: "sE",
			Timestamp: t0.Add(12 * time.Second), Tool: models.ToolClaudeCode,
			Model: "claude-opus-4-7", InputTokens: 100, OutputTokens: 500,
			MessageID: "msg_E",
			Source:    models.TokenSourceJSONL, Reliability: models.ReliabilityUnreliable,
		},
	}, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/session/sE/messages", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Messages []struct {
			Role      string `json:"role"`
			ElapsedMs *int64 `json:"elapsed_ms"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("messages: got %d want 3", len(got.Messages))
	}
	// First message → 12000ms gap to assistant turn.
	if got.Messages[0].ElapsedMs == nil || *got.Messages[0].ElapsedMs != 12000 {
		t.Errorf("messages[0].elapsed_ms: got %v want 12000", got.Messages[0].ElapsedMs)
	}
	// Assistant turn → 47000ms gap to next user prompt.
	if got.Messages[1].ElapsedMs == nil || *got.Messages[1].ElapsedMs != 47000 {
		t.Errorf("messages[1].elapsed_ms: got %v want 47000", got.Messages[1].ElapsedMs)
	}
	// Last message has no successor → null.
	if got.Messages[2].ElapsedMs != nil {
		t.Errorf("messages[2].elapsed_ms: got %v want null", *got.Messages[2].ElapsedMs)
	}
}

// TestAPISessionMessages_ToolDurationMs pins v1.4.28's per-message
// tool-execution time: each assistant turn's message row carries the
// sum of its contained tool_calls' duration_ms (separately from the
// wall-clock ElapsedMs). Exposes per-tool-call duration_ms too.
func TestAPISessionMessages_ToolDurationMs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	st := store.New(database)
	root := t.TempDir()
	ts := time.Date(2026, 5, 3, 7, 0, 0, 0, time.UTC)

	// Two tool calls under the same assistant message — durations 250ms
	// and 750ms — should sum to 1000ms on the message row.
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{
		{
			SourceFile: "f", SourceEventID: "tu_1", SessionID: "sT",
			ProjectRoot: root, Timestamp: ts, Tool: models.ToolClaudeCode,
			ActionType: models.ActionReadFile, Target: "a.go", Success: true,
			DurationMs: 250, MessageID: "msg_T",
		},
		{
			SourceFile: "f", SourceEventID: "tu_2", SessionID: "sT",
			ProjectRoot: root, Timestamp: ts.Add(time.Second), Tool: models.ToolClaudeCode,
			ActionType: models.ActionRunCommand, Target: "ls -la", Success: true,
			DurationMs: 750, MessageID: "msg_T",
		},
	}, []models.TokenEvent{
		{
			SourceFile: "f", SourceEventID: "msg_T", SessionID: "sT",
			Timestamp: ts, Tool: models.ToolClaudeCode,
			Model: "claude-opus-4-7", InputTokens: 100, OutputTokens: 500,
			MessageID: "msg_T",
			Source:    models.TokenSourceJSONL, Reliability: models.ReliabilityUnreliable,
		},
	}, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/session/sT/messages", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Messages []struct {
			Role           string `json:"role"`
			ToolDurationMs int64  `json:"tool_duration_ms"`
			ToolCalls      []struct {
				DurationMs int64 `json:"duration_ms"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages: got %d want 1", len(got.Messages))
	}
	m := got.Messages[0]
	if m.ToolDurationMs != 1000 {
		t.Errorf("tool_duration_ms: got %d want 1000 (250+750)", m.ToolDurationMs)
	}
	if len(m.ToolCalls) != 2 {
		t.Fatalf("tool_calls: got %d want 2", len(m.ToolCalls))
	}
	if m.ToolCalls[0].DurationMs != 250 || m.ToolCalls[1].DurationMs != 750 {
		t.Errorf("per-tool durations: got %d/%d want 250/750",
			m.ToolCalls[0].DurationMs, m.ToolCalls[1].DurationMs)
	}
}

// TestAPISessionDetail_PerTurnDedup_GapFillAndPerModel is the regression
// test for the b9bd459d-shape complaint: a session whose proxy intercepted
// SOME turns must have token_usage rows for the OTHER turns folded back
// into the totals (gap-fill), and the per-model breakdown must show every
// model that contributed (haiku + opus), not just the session's
// most-recent model. Pre-2026-04-29 the session detail endpoint had its
// own per-session dedup ("if api_turns has any rows, drop all
// token_usage") that mirrored the cost-engine bug fixed in v1.4.12 — that
// silently dropped 80%+ of tokens when the proxy was off for most of a
// session.
func TestAPISessionDetail_PerTurnDedup_GapFillAndPerModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	st := store.New(database)
	root := t.TempDir()
	ts := time.Date(2026, 4, 28, 6, 50, 0, 0, time.UTC)

	// Create session sA with one action so the row exists.
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{{
		SourceFile: "f", SourceEventID: "e1", SessionID: "sA",
		ProjectRoot: root, Timestamp: ts, Tool: models.ToolClaudeCode,
		ActionType: models.ActionReadFile, Target: "a.go", Success: true,
	}}, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}

	// Two proxy turns the proxy DID intercept — one haiku, one opus —
	// each with a request_id (msg_xxx) that matches a JSONL row. Plus
	// two JSONL-only turns (representing turns the proxy missed and
	// only the JSONL adapter recorded), one per model.
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sA", Provider: models.ProviderAnthropic,
		Model: "claude-haiku-4-5", InputTokens: 100, OutputTokens: 500,
		CacheReadTokens: 50_000, CacheCreationTokens: 10_000,
		Timestamp: ts, RequestID: "msg_haiku_1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sA", Provider: models.ProviderAnthropic,
		Model: "claude-opus-4-7", InputTokens: 50, OutputTokens: 5_000,
		CacheReadTokens: 800_000, CacheCreationTokens: 200_000,
		Timestamp: ts.Add(time.Minute), RequestID: "msg_opus_1",
	}); err != nil {
		t.Fatal(err)
	}

	// Insert four token_usage rows directly (not via the adapter).
	// Two have source_event_id matching the proxy request_ids — those
	// must be deduped out. Two have novel ids — those must survive.
	for _, tu := range []struct {
		eid, model      string
		in, out, cr, cw int64
	}{
		{"msg_haiku_1", "claude-haiku-4-5", 100, 500, 50_000, 10_000},            // dup of proxy
		{"msg_opus_1", "claude-opus-4-7", 50, 5_000, 800_000, 200_000},           // dup of proxy
		{"msg_haiku_gap", "claude-haiku-4-5", 7_200, 28_700, 1_950_000, 313_000}, // gap-fill
		{"msg_opus_gap", "claude-opus-4-7", 97, 78_100, 3_200_000, 800_000},      // gap-fill
	} {
		_, err := database.ExecContext(context.Background(),
			`INSERT INTO token_usage (session_id, source_file, source_event_id, timestamp, tool, model,
				input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
				source, reliability)
			 VALUES ('sA', 'f-tu', ?, ?, 'claude-code', ?, ?, ?, ?, ?, 'jsonl', 'unreliable')`,
			tu.eid, ts.Format(time.RFC3339Nano), tu.model, tu.in, tu.out, tu.cr, tu.cw)
		if err != nil {
			t.Fatal(err)
		}
	}

	srv, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/session/sA", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Tokens   map[string]int64 `json:"tokens"`
		PerModel []struct {
			Model         string  `json:"model"`
			Input         int64   `json:"input"`
			Output        int64   `json:"output"`
			CacheRead     int64   `json:"cache_read"`
			CacheCreation int64   `json:"cache_creation"`
			TurnCount     int64   `json:"turn_count"`
			CostUSD       float64 `json:"cost_usd"`
		} `json:"per_model"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}

	// Expected totals: proxy turns + JSONL gap-fill turns; matching
	// JSONL rows for the proxy turns are deduped out.
	wantTotal := map[string]int64{
		"input":          100 + 50 + 7_200 + 97,
		"output":         500 + 5_000 + 28_700 + 78_100,
		"cache_read":     50_000 + 800_000 + 1_950_000 + 3_200_000,
		"cache_creation": 10_000 + 200_000 + 313_000 + 800_000,
	}
	for k, v := range wantTotal {
		if got.Tokens[k] != v {
			t.Errorf("tokens[%s]: got %d want %d", k, got.Tokens[k], v)
		}
	}

	// Per-model breakdown: must surface BOTH haiku and opus, with
	// per-turn-deduped totals. Pre-fix it would have surfaced only
	// the proxy values (or only the JSONL values, depending on which
	// path triggered) — and never the gap-filled mix.
	if len(got.PerModel) != 2 {
		t.Fatalf("per_model: got %d rows want 2 (haiku + opus)", len(got.PerModel))
	}
	byModel := map[string]int64{} // model → input
	turnsByModel := map[string]int64{}
	for _, m := range got.PerModel {
		byModel[m.Model] = m.Input
		turnsByModel[m.Model] = m.TurnCount
	}
	if byModel["claude-haiku-4-5"] != 100+7_200 {
		t.Errorf("haiku input: got %d want %d", byModel["claude-haiku-4-5"], 100+7_200)
	}
	if byModel["claude-opus-4-7"] != 50+97 {
		t.Errorf("opus input: got %d want %d", byModel["claude-opus-4-7"], 50+97)
	}
	// Each model has 2 surviving turns (one proxy + one JSONL gap-fill).
	if turnsByModel["claude-haiku-4-5"] != 2 {
		t.Errorf("haiku turn count: got %d want 2", turnsByModel["claude-haiku-4-5"])
	}
	if turnsByModel["claude-opus-4-7"] != 2 {
		t.Errorf("opus turn count: got %d want 2", turnsByModel["claude-opus-4-7"])
	}
}

// TestAPISessionDetail_CacheCreation1hTier is the end-to-end check for
// audit item C5: a session whose proxy turns split cache_creation into
// 5m vs 1h tiers must see the 1h portion billed at the 1h rate (2× the
// 5m rate). Pre-tier-aware code summed all cache_creation at the 5m rate
// and silently under-counted whenever 1h-tier traffic appeared.
func TestAPISessionDetail_CacheCreation1hTier(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	st := store.New(database)
	root := t.TempDir()
	ts := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)

	// Create session sA via Ingest.
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{{
		SourceFile: "f", SourceEventID: "e1", SessionID: "sA",
		ProjectRoot: root, Timestamp: ts, Tool: models.ToolClaudeCode,
		ActionType: models.ActionReadFile, Target: "a.go", Success: true,
	}}, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}

	// Proxy turn for sA: 1M cache_creation tokens, 400k of them in the
	// 1h tier. Pricing for claude-sonnet-4-6 per
	// docs/pricing-reference.md:
	//   {Input: 3, Output: 15, CacheRead: 0.30, CacheCreation: 3.75,
	//    CacheCreation1h: 6.00 (= 2 × Input — Anthropic's published ratio)}.
	// Expected cost split:
	//   5m portion: 600_000 × 3.75 / 1e6 = 2.25
	//   1h portion: 400_000 × 6.00 / 1e6 = 2.40
	//   Total cache_creation cost: 4.65
	// (Input/output set to zero so the cache_creation contribution is
	// the entire cost — easier to assert without arithmetic noise.)
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID:             "sA",
		Timestamp:             ts,
		Provider:              models.ProviderAnthropic,
		Model:                 "claude-sonnet-4-6",
		CacheCreationTokens:   1_000_000,
		CacheCreation1hTokens: 400_000,
	}); err != nil {
		t.Fatalf("InsertAPITurn: %v", err)
	}

	srv, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/session/sA", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	costUSD, _ := got["cost_usd"].(float64)
	want := 4.65
	if costUSD < want*0.999 || costUSD > want*1.001 {
		t.Errorf("cost_usd: got %v, want ≈ %v (1h tier billed at 2× input)", costUSD, want)
	}

	// Sanity: a session with the *same* total cache_creation but ZERO
	// 1h tier should bill at the 5m rate only — proves we're not just
	// applying the 1h rate uniformly.
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{{
		SourceFile: "f", SourceEventID: "e2", SessionID: "sB",
		ProjectRoot: root, Timestamp: ts, Tool: models.ToolClaudeCode,
		ActionType: models.ActionReadFile, Target: "b.go", Success: true,
	}}, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sB", Timestamp: ts,
		Provider:            models.ProviderAnthropic,
		Model:               "claude-sonnet-4-6",
		CacheCreationTokens: 1_000_000, // entire bundle 5m-tier
	}); err != nil {
		t.Fatalf("InsertAPITurn sB: %v", err)
	}
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/session/sB", nil)
	srv.Handler().ServeHTTP(rr, req)
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	costUSDB, _ := got["cost_usd"].(float64)
	want = 3.75 // 1M × 3.75 / 1e6
	if costUSDB < want*0.999 || costUSDB > want*1.001 {
		t.Errorf("5m-only cost_usd: got %v, want ≈ %v", costUSDB, want)
	}
}

// TestAPISessionDetail_LongContextPerTurn pins the per-turn pricing
// guarantee in the session-detail endpoint. The cost engine reprices an
// entire turn at the long-context tier when the prompt exceeds a
// threshold (Sonnet 4 / 4.5 at 200K). Pre-LC the endpoint aggregated
// tokens at SQL level and called Compute on the sum — a session with N
// sub-threshold turns whose summed prompt cleared the threshold would
// false-positive LC pricing. The fix pulls individual rows and prices
// per-turn before bucketing.
//
// Setup: three Sonnet 4 turns, each 100K input. Aggregate = 300K (over
// the 200K threshold) but no individual turn is. Expected cost is the
// standard rate: 3 × 100K × $3 / 1M = $0.90. If aggregation-then-pricing
// regressed in, the cost would be 300K × $6 / 1M = $1.80.
func TestAPISessionDetail_LongContextPerTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	st := store.New(database)
	root := t.TempDir()
	ts := time.Date(2026, 4, 28, 9, 0, 0, 0, time.UTC)

	if _, err := st.Ingest(context.Background(), []models.ToolEvent{{
		SourceFile: "f", SourceEventID: "e1", SessionID: "sLC",
		ProjectRoot: root, Timestamp: ts, Tool: models.ToolClaudeCode,
		ActionType: models.ActionReadFile, Target: "a.go", Success: true,
	}}, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
			SessionID:   "sLC",
			Provider:    models.ProviderAnthropic,
			Model:       "claude-sonnet-4-20250514",
			InputTokens: 100_000,
			Timestamp:   ts.Add(time.Duration(i) * time.Minute),
			RequestID:   fmt.Sprintf("msg_lc_%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	srv, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/session/sLC", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		CostUSD  float64 `json:"cost_usd"`
		PerModel []struct {
			Model     string  `json:"model"`
			Input     int64   `json:"input"`
			TurnCount int64   `json:"turn_count"`
			CostUSD   float64 `json:"cost_usd"`
		} `json:"per_model"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	const wantCost = 0.90 // 3 × 100K × $3 / 1M, all turns under threshold
	if diff := got.CostUSD - wantCost; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("cost_usd: got %v want %v (per-turn pricing must NOT trip LC tier)", got.CostUSD, wantCost)
	}
	if len(got.PerModel) != 1 {
		t.Fatalf("per_model: got %d rows want 1", len(got.PerModel))
	}
	if got.PerModel[0].TurnCount != 3 {
		t.Errorf("turn_count: got %d want 3", got.PerModel[0].TurnCount)
	}
	if got.PerModel[0].Input != 300_000 {
		t.Errorf("aggregated input: got %d want 300_000", got.PerModel[0].Input)
	}
	if diff := got.PerModel[0].CostUSD - wantCost; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("per_model cost: got %v want %v", got.PerModel[0].CostUSD, wantCost)
	}

	// Counter-test: a single Sonnet 4 turn that DOES clear the threshold
	// must price at LC rates. Confirms LC dispatch still runs per-turn.
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{{
		SourceFile: "f", SourceEventID: "e2", SessionID: "sLC2",
		ProjectRoot: root, Timestamp: ts, Tool: models.ToolClaudeCode,
		ActionType: models.ActionReadFile, Target: "b.go", Success: true,
	}}, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID:   "sLC2",
		Provider:    models.ProviderAnthropic,
		Model:       "claude-sonnet-4-20250514",
		InputTokens: 250_000, // single turn clears 200K threshold
		Timestamp:   ts,
		RequestID:   "msg_lc_single",
	}); err != nil {
		t.Fatal(err)
	}
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/session/sLC2", nil)
	srv.Handler().ServeHTTP(rr, req)
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	const wantLC = 1.50 // 250K × $6 / 1M (LC input rate)
	if diff := got.CostUSD - wantLC; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("LC turn cost_usd: got %v want %v", got.CostUSD, wantLC)
	}
}

// TestAPITimeseriesCost_Chronological pins the chart-axis fix: the
// day-bucket cost timeseries used to inherit cost.Engine.Summary's
// cost_usd-DESC sort, which left chart x-axes reading newest-on-left
// (high-cost days first). Series must come back oldest-first so the
// chart reads chronologically left-to-right.
func TestAPITimeseriesCost_Chronological(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	st := store.New(database)
	// Three turns on three different days; insert deliberately in
	// non-chronological order with a *higher* cost on the middle day so
	// a cost-DESC sort would put it first. Chronological sort must put
	// the earliest day first regardless.
	turns := []models.APITurn{
		{
			SessionID: "s", Provider: models.ProviderAnthropic,
			Model: "claude-sonnet-4-6", InputTokens: 1_000_000,
			Timestamp: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		},
		{
			SessionID: "s", Provider: models.ProviderAnthropic,
			Model: "claude-sonnet-4-6", InputTokens: 5_000_000, // most expensive day
			Timestamp: time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC),
		},
		{
			SessionID: "s", Provider: models.ProviderAnthropic,
			Model: "claude-sonnet-4-6", InputTokens: 2_000_000,
			Timestamp: time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC),
		},
	}
	for _, turn := range turns {
		if _, err := st.InsertAPITurn(context.Background(), turn); err != nil {
			t.Fatalf("InsertAPITurn: %v", err)
		}
	}

	srv, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/timeseries/cost?days=365&bucket=day", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Series []struct {
			Bucket  string  `json:"bucket"`
			CostUSD float64 `json:"cost_usd"`
		} `json:"series"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Series) != 3 {
		t.Fatalf("series: got %d points want 3", len(got.Series))
	}
	want := []string{"2026-04-10", "2026-04-15", "2026-04-20"}
	for i, w := range want {
		if got.Series[i].Bucket != w {
			t.Errorf("series[%d].bucket: got %q want %q (chronological order)",
				i, got.Series[i].Bucket, w)
		}
	}
}

// TestAPITimeseriesTokensByModel pins the per-model day-split chart
// endpoint: each (day, model) pair becomes one point, points sort
// chronologically then alphabetically by model, and the cost engine's
// dedup applies (proxy preferred over JSONL for the same session).
func TestAPITimeseriesTokensByModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	st := store.New(database)
	// Two days × two models = four expected points. Inserted in a
	// scrambled order to flush out a missing chronological sort.
	turns := []models.APITurn{
		{
			SessionID: "s1", Provider: models.ProviderAnthropic,
			Model: "claude-sonnet-4-6", InputTokens: 1_000_000,
			Timestamp: time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC),
		},
		{
			SessionID: "s2", Provider: models.ProviderAnthropic,
			Model: "claude-haiku-4-5", InputTokens: 500_000,
			Timestamp: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		},
		{
			SessionID: "s3", Provider: models.ProviderAnthropic,
			Model: "claude-sonnet-4-6", InputTokens: 2_000_000,
			Timestamp: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		},
		{
			SessionID: "s4", Provider: models.ProviderAnthropic,
			Model: "claude-haiku-4-5", InputTokens: 800_000,
			Timestamp: time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC),
		},
	}
	for _, turn := range turns {
		if _, err := st.InsertAPITurn(context.Background(), turn); err != nil {
			t.Fatalf("InsertAPITurn: %v", err)
		}
	}

	srv, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/timeseries/tokens-by-model?days=365", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Metric string `json:"metric"`
		Series []struct {
			Bucket      string `json:"bucket"`
			Model       string `json:"model"`
			TotalTokens int64  `json:"total_tokens"`
			Input       int64  `json:"input"`
		} `json:"series"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Metric != "tokens_by_model" {
		t.Errorf("metric = %q want tokens_by_model", got.Metric)
	}
	if len(got.Series) != 4 {
		t.Fatalf("series: got %d points want 4", len(got.Series))
	}
	want := []struct{ bucket, model string }{
		{"2026-04-10", "claude-haiku-4-5"},
		{"2026-04-10", "claude-sonnet-4-6"},
		{"2026-04-20", "claude-haiku-4-5"},
		{"2026-04-20", "claude-sonnet-4-6"},
	}
	for i, w := range want {
		if got.Series[i].Bucket != w.bucket || got.Series[i].Model != w.model {
			t.Errorf("series[%d]: got (%q, %q) want (%q, %q)",
				i, got.Series[i].Bucket, got.Series[i].Model, w.bucket, w.model)
		}
	}
	// Sanity-check token totals on one specific cell so we know the
	// pivot didn't smear values across rows.
	for _, p := range got.Series {
		if p.Bucket == "2026-04-10" && p.Model == "claude-sonnet-4-6" && p.Input != 2_000_000 {
			t.Errorf("(2026-04-10, sonnet) input = %d want 2_000_000", p.Input)
		}
	}
}

// TestAPITimeseriesActions covers the time-series action endpoint.
func TestAPITimeseriesActions(t *testing.T) {
	s, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/timeseries/actions?days=30&bucket=day", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["metric"] != "actions" || got["bucket"] != "day" {
		t.Errorf("metric/bucket: %v / %v", got["metric"], got["bucket"])
	}
	series, ok := got["series"].([]any)
	if !ok {
		t.Fatalf("series missing or wrong shape: %T", got["series"])
	}
	if len(series) < 1 {
		t.Errorf("expected >= 1 bucket from the seeded action, got %d", len(series))
	}
}

// TestAPITools covers the per-tool breakdown endpoint.
func TestAPITools(t *testing.T) {
	s, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tools?days=30", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status: %d", rr.Code)
	}
	var got map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	tools, ok := got["tools"].([]any)
	if !ok {
		t.Fatalf("tools missing or wrong shape: %v", got)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool entry (claude-code), got %d", len(tools))
	}
	first, _ := tools[0].(map[string]any)
	if first["tool"] != "claude-code" {
		t.Errorf("tool name: %v", first["tool"])
	}
	if first["action_count"].(float64) != 1 {
		t.Errorf("action_count: %v", first["action_count"])
	}
}

func TestAPIDiscover(t *testing.T) {
	s, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/discover?limit=5", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("discover: %d", rr.Code)
	}
	var got map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["summary"] == nil {
		t.Errorf("missing summary: %+v", got)
	}
}

// TestAPIActions_MetadataFilters pins the v1.4.48 dashboard filter
// surface: /api/actions accepts effort_level / permission_mode /
// is_interrupt query params that filter on the migration-017
// actions.metadata JSON column via json_extract. Empty / absent
// params skip the corresponding filter so legacy callers' results
// are unchanged.
func TestAPIActions_MetadataFilters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	st := store.New(database)
	root := t.TempDir()
	base := time.Date(2026, 5, 11, 10, 0, 0, 0, time.UTC)

	// Seed 5 rows covering the filter matrix:
	// - row1: plan + high + interrupt
	// - row2: plan + medium + no interrupt
	// - row3: default + high + no interrupt
	// - row4: acceptEdits + xhigh + interrupt
	// - row5: no metadata at all (Claude Code Tier 0 / pre-017)
	events := []models.ToolEvent{
		{
			SourceFile: "f", SourceEventID: "e1", SessionID: "s1",
			ProjectRoot: root, Timestamp: base, Tool: models.ToolClaudeCode,
			ActionType: models.ActionReadFile, Target: "a.go", Success: true,
			Metadata: &models.ActionMetadata{PermissionMode: "plan", EffortLevel: "high", IsInterrupt: true},
		},
		{
			SourceFile: "f", SourceEventID: "e2", SessionID: "s1",
			ProjectRoot: root, Timestamp: base.Add(time.Minute), Tool: models.ToolClaudeCode,
			ActionType: models.ActionReadFile, Target: "b.go", Success: true,
			Metadata: &models.ActionMetadata{PermissionMode: "plan", EffortLevel: "medium"},
		},
		{
			SourceFile: "f", SourceEventID: "e3", SessionID: "s1",
			ProjectRoot: root, Timestamp: base.Add(2 * time.Minute), Tool: models.ToolClaudeCode,
			ActionType: models.ActionReadFile, Target: "c.go", Success: true,
			Metadata: &models.ActionMetadata{PermissionMode: "default", EffortLevel: "high"},
		},
		{
			SourceFile: "f", SourceEventID: "e4", SessionID: "s1",
			ProjectRoot: root, Timestamp: base.Add(3 * time.Minute), Tool: models.ToolClaudeCode,
			ActionType: models.ActionReadFile, Target: "d.go", Success: true,
			Metadata: &models.ActionMetadata{PermissionMode: "acceptEdits", EffortLevel: "xhigh", IsInterrupt: true},
		},
		{
			SourceFile: "f", SourceEventID: "e5", SessionID: "s1",
			ProjectRoot: root, Timestamp: base.Add(4 * time.Minute), Tool: models.ToolClaudeCode,
			ActionType: models.ActionReadFile, Target: "e.go", Success: true,
		},
	}
	if _, err := st.Ingest(context.Background(), events, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}

	check := func(query string, wantTotal int) {
		t.Helper()
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/actions?"+query, nil)
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("%s: status %d body=%s", query, rr.Code, rr.Body.String())
		}
		var got struct {
			Total int `json:"total"`
		}
		if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
			t.Fatalf("%s: decode: %v", query, err)
		}
		if got.Total != wantTotal {
			t.Errorf("%s: total=%d want %d", query, got.Total, wantTotal)
		}
	}

	// Baseline: no filter = all 5 rows.
	check("limit=50", 5)

	// effort_level=high → rows 1 + 3.
	check("effort_level=high", 2)
	// effort_level=xhigh → row 4 only.
	check("effort_level=xhigh", 1)
	// effort_level=low → none.
	check("effort_level=low", 0)

	// permission_mode=plan → rows 1 + 2.
	check("permission_mode=plan", 2)
	// permission_mode=default → row 3 only.
	check("permission_mode=default", 1)
	// permission_mode=acceptEdits → row 4 only.
	check("permission_mode=acceptEdits", 1)

	// is_interrupt=1 → rows 1 + 4. Row 5 with nil metadata must NOT
	// match (json_extract on NULL returns NULL, which fails the = 1
	// comparison).
	check("is_interrupt=1", 2)
	// is_interrupt=0 is NOT a documented filter value — it's
	// intentionally ignored (only "1" triggers the WHERE clause).
	check("is_interrupt=0", 5)

	// Combined: effort_level=high AND permission_mode=plan → row 1
	// only.
	check("effort_level=high&permission_mode=plan", 1)
	// Combined + interrupt → still row 1.
	check("effort_level=high&permission_mode=plan&is_interrupt=1", 1)

	// Empty filters fall through to baseline.
	check("limit=50&effort_level=&permission_mode=&is_interrupt=", 5)
}

// TestAPIActions_AssistantTextFilter pins the v1.4.49 "AI messages only"
// surface: the multi-pattern OR-chain matches all five assistant-text
// RawToolName conventions across the codebase (new <source>.assistant_text
// wirings + the 4 legacy precedents: Antigravity's structured.assistant_text,
// Pi's message.assistant.<stopReason>, OpenClaw's message.assistant.stop,
// Copilot's agent_response).
func TestAPIActions_AssistantTextFilter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	st := store.New(database)
	root := t.TempDir()
	base := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)

	events := []models.ToolEvent{
		// New convention rows — should match.
		{
			SourceFile: "f", SourceEventID: "n1", SessionID: "s", ProjectRoot: root, Timestamp: base, Tool: models.ToolCodex,
			ActionType: models.ActionTaskComplete, Target: "codex msg", Success: true,
			RawToolName: "codex.assistant_text",
		},
		{
			SourceFile: "f", SourceEventID: "n2", SessionID: "s", ProjectRoot: root, Timestamp: base.Add(time.Second), Tool: models.ToolCline,
			ActionType: models.ActionTaskComplete, Target: "cline msg", Success: true,
			RawToolName: "cline.assistant_text",
		},
		{
			SourceFile: "f", SourceEventID: "n3", SessionID: "s", ProjectRoot: root, Timestamp: base.Add(2 * time.Second), Tool: models.ToolClaudeCode,
			ActionType: models.ActionTaskComplete, Target: "cc msg", Success: true,
			RawToolName: "claudecode.assistant_text",
		},
		// Legacy convention rows — should also match.
		{
			SourceFile: "f", SourceEventID: "l1", SessionID: "s", ProjectRoot: root, Timestamp: base.Add(3 * time.Second), Tool: models.ToolAntigravity,
			ActionType: models.ActionTaskComplete, Target: "antigravity msg", Success: true,
			RawToolName: "structured.assistant_text",
		},
		{
			SourceFile: "f", SourceEventID: "l2", SessionID: "s", ProjectRoot: root, Timestamp: base.Add(4 * time.Second), Tool: models.ToolPi,
			ActionType: models.ActionTaskComplete, Target: "pi msg", Success: true,
			RawToolName: "message.assistant.stop",
		},
		{
			SourceFile: "f", SourceEventID: "l3", SessionID: "s", ProjectRoot: root, Timestamp: base.Add(5 * time.Second), Tool: models.ToolCopilot,
			ActionType: models.ActionTaskComplete, Target: "copilot msg", Success: true,
			RawToolName: "agent_response",
		},
		// Non-matching rows — tool calls + other action types.
		{
			SourceFile: "f", SourceEventID: "x1", SessionID: "s", ProjectRoot: root, Timestamp: base.Add(6 * time.Second), Tool: models.ToolClaudeCode,
			ActionType: models.ActionReadFile, Target: "a.go", Success: true,
			RawToolName: "Read",
		},
		{
			SourceFile: "f", SourceEventID: "x2", SessionID: "s", ProjectRoot: root, Timestamp: base.Add(7 * time.Second), Tool: models.ToolClaudeCode,
			ActionType: models.ActionTaskComplete, Target: "stop", Success: true,
			RawToolName: "task_complete",
		},
	}
	if _, err := st.Ingest(context.Background(), events, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}

	check := func(query string, wantTotal int) {
		t.Helper()
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/actions?"+query, nil)
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("%s: status %d body=%s", query, rr.Code, rr.Body.String())
		}
		var got struct {
			Total int `json:"total"`
		}
		if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
			t.Fatalf("%s: decode: %v", query, err)
		}
		if got.Total != wantTotal {
			t.Errorf("%s: total=%d want %d", query, got.Total, wantTotal)
		}
	}

	// Baseline: 8 rows total.
	check("limit=50", 8)
	// assistant_text=1: 6 matching rows (3 new conventions + 3 legacy patterns).
	check("assistant_text=1", 6)
	// Combine with tool filter: assistant_text=1 + tool=codex → 1 row.
	check("assistant_text=1&tool=codex", 1)
	// assistant_text=0 is NOT a documented value — falls through (no filter).
	check("assistant_text=0", 8)
}

// TestAPIPatterns_ToolFilter pins the v1.6.3 tool-scoping behavior on
// /api/patterns. Patterns are project-scoped; the tool filter restricts
// to projects whose actions table has at least one row for the
// requested tool. Seeds two projects with actions for different tools
// and one pattern per project, then asserts every combination of the
// (no filter, project-only, tool-only, both) matrix.
func TestAPIPatterns_ToolFilter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)
	rootA := t.TempDir()
	rootB := t.TempDir()
	now := time.Now().UTC()
	events := []models.ToolEvent{
		{
			SourceFile: "fA", SourceEventID: "1", SessionID: "sA", ProjectRoot: rootA,
			Timestamp: now.Add(-time.Hour), Tool: models.ToolClaudeCode,
			ActionType: models.ActionReadFile, Target: "a.go", Success: true,
		},
		{
			SourceFile: "fB", SourceEventID: "2", SessionID: "sB", ProjectRoot: rootB,
			Timestamp: now.Add(-time.Hour), Tool: models.ToolCopilot,
			ActionType: models.ActionReadFile, Target: "b.go", Success: true,
		},
	}
	if _, err := st.Ingest(context.Background(), events, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}

	// Insert one pattern per project. Resolve project_ids via root_path
	// lookup (Ingest auto-creates projects rows).
	var pidA, pidB int64
	if err := database.QueryRowContext(context.Background(),
		`SELECT id FROM projects WHERE root_path = ?`, rootA).Scan(&pidA); err != nil {
		t.Fatalf("lookup pidA: %v", err)
	}
	if err := database.QueryRowContext(context.Background(),
		`SELECT id FROM projects WHERE root_path = ?`, rootB).Scan(&pidB); err != nil {
		t.Fatalf("lookup pidB: %v", err)
	}
	for _, p := range []struct {
		pid  int64
		kind string
	}{{pidA, "hot_file"}, {pidB, "co_change"}} {
		if _, err := database.ExecContext(context.Background(),
			`INSERT INTO project_patterns
			  (project_id, pattern_type, pattern_data, confidence,
			   last_reinforced_at, decay_half_life_days,
			   observation_count, source_tools, source_session_count)
			 VALUES (?, ?, '{}', 0.9, ?, 30, 5, '[]', 1)`,
			p.pid, p.kind, now.Format(time.RFC3339Nano)); err != nil {
			t.Fatalf("insert pattern: %v", err)
		}
	}

	srv, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	check := func(query string, wantTotal int) {
		t.Helper()
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/patterns?"+query, nil)
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("%s: status %d body=%s", query, rr.Code, rr.Body.String())
		}
		var got struct {
			Total int `json:"total"`
		}
		if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
			t.Fatalf("%s: decode: %v", query, err)
		}
		if got.Total != wantTotal {
			t.Errorf("%s: total=%d want %d", query, got.Total, wantTotal)
		}
	}

	check("limit=20", 2)
	check("limit=20&tool=claude-code", 1)
	check("limit=20&tool=copilot", 1)
	check("limit=20&tool=cursor", 0)
	check("limit=20&project="+rootA, 1)
	check("limit=20&project="+rootA+"&tool=claude-code", 1)
	check("limit=20&project="+rootA+"&tool=copilot", 0)
}

// TestAPISessions_CostReconcilesWithModels pins the v1.6.4 fix that
// aligns /api/sessions' per-session cost rollup window to the `days`
// query param (was hardcoded Days=365). After the fix, summing
// cost_usd across all sessions returned by /api/sessions?days=N
// equals /api/models?days=N total_cost_usd to within rounding —
// closing the cross-page discrepancy operators noticed when
// reconciling the Cost-page headline against the Sessions-page sum.
//
// Setup: one session with two api_turns — one inside the 7-day
// window, one 60 days old (outside). With the old Days=365 default,
// the per-session cost rollup included the old turn; with the fix,
// it doesn't.
func TestAPISessions_CostReconcilesWithModels(t *testing.T) {
	s, _ := newTestServer(t)
	st := store.New(s.opts.DB)
	now := time.Now().UTC()
	recent := now.Add(-2 * 24 * time.Hour) // inside 7-day window
	stale := now.Add(-60 * 24 * time.Hour) // outside 7-day window

	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sA", Provider: models.ProviderAnthropic,
		Model:       "claude-sonnet-4-6",
		InputTokens: 1_000_000, OutputTokens: 50_000,
		Timestamp: recent,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sA", Provider: models.ProviderAnthropic,
		Model:       "claude-sonnet-4-6",
		InputTokens: 5_000_000, OutputTokens: 250_000,
		Timestamp: stale,
	}); err != nil {
		t.Fatal(err)
	}

	// Hit both endpoints with days=7.
	getJSON := func(url string) map[string]any {
		t.Helper()
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, url, nil)
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("%s: status %d body=%s", url, rr.Code, rr.Body.String())
		}
		var got map[string]any
		if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
			t.Fatalf("%s: decode: %v", url, err)
		}
		return got
	}

	sessionsRsp := getJSON("/api/sessions?days=7&limit=10")
	modelsRsp := getJSON("/api/models?days=7")

	rows, _ := sessionsRsp["rows"].([]any)
	var sessionsCostSum float64
	for _, r := range rows {
		row, _ := r.(map[string]any)
		if c, ok := row["cost_usd"].(float64); ok {
			sessionsCostSum += c
		}
	}
	modelsTotal, _ := modelsRsp["total_cost_usd"].(float64)

	if sessionsCostSum == 0 {
		t.Fatalf("sessions cost sum is zero; the seeded recent turn should produce a cost")
	}
	// Allow 0.01% tolerance for any rounding in distributed compute.
	delta := sessionsCostSum - modelsTotal
	if delta < 0 {
		delta = -delta
	}
	if delta > sessionsCostSum*0.0001 {
		t.Errorf("sessions cost sum (%.6f) != /api/models total (%.6f): delta=%.6f",
			sessionsCostSum, modelsTotal, delta)
	}

	// Belt-and-suspenders: the recent-only window cost should be much
	// less than the seeded stale turn (5x bigger). If the fix
	// regressed and Days=365 returned, sessionsCostSum would include
	// the stale turn and balloon.
	staleApprox := 5 * sessionsCostSum
	if sessionsCostSum >= staleApprox/2 {
		t.Errorf("sessions cost sum %.6f is suspiciously high — Days param may not be honored",
			sessionsCostSum)
	}
}

// TestAPICost_HonorsToolFilter pins the v1.6.4 fix wiring the `tool`
// query param through handleCost. Pre-fix the handler only read
// `project` and `source` and ignored `tool`, so callers using the
// CLI's `observer cost --tool=X` (which forwards to this endpoint)
// got unfiltered output.
func TestAPICost_HonorsToolFilter(t *testing.T) {
	s, _ := newTestServer(t)
	st := store.New(s.opts.DB)
	now := time.Now().UTC()

	// Two sessions on different tools, each with a priced api_turn.
	if err := st.UpsertSession(context.Background(), models.Session{
		ID: "sCo", ProjectID: 1, Tool: models.ToolCodex, StartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sA", Provider: models.ProviderAnthropic, Model: "claude-sonnet-4-6",
		InputTokens: 1_000_000, OutputTokens: 50_000, Timestamp: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sCo", Provider: models.ProviderOpenAI, Model: "gpt-5.5",
		InputTokens: 500_000, OutputTokens: 25_000, Timestamp: now,
	}); err != nil {
		t.Fatal(err)
	}

	getJSON := func(url string) map[string]any {
		t.Helper()
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, url, nil)
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("%s: status %d body=%s", url, rr.Code, rr.Body.String())
		}
		var got map[string]any
		if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
			t.Fatalf("%s: decode: %v", url, err)
		}
		return got
	}

	unfiltered := getJSON("/api/cost?days=30")
	claude := getJSON("/api/cost?days=30&tool=claude-code")
	codex := getJSON("/api/cost?days=30&tool=codex")
	missing := getJSON("/api/cost?days=30&tool=nonexistent")

	unfilteredTotal := unfiltered["total_cost_usd"].(float64)
	claudeTotal := claude["total_cost_usd"].(float64)
	codexTotal := codex["total_cost_usd"].(float64)
	missingTotal := missing["total_cost_usd"].(float64)

	if unfilteredTotal <= claudeTotal {
		t.Errorf("unfiltered (%.4f) should exceed claude-only (%.4f)", unfilteredTotal, claudeTotal)
	}
	if claudeTotal == 0 || codexTotal == 0 {
		t.Errorf("tool filter returned zero for a seeded tool: claude=%.4f codex=%.4f",
			claudeTotal, codexTotal)
	}
	// Tool=claude-code + tool=codex should sum to unfiltered (modulo
	// rounding) when no other tools have data.
	delta := claudeTotal + codexTotal - unfilteredTotal
	if delta < 0 {
		delta = -delta
	}
	if delta > unfilteredTotal*0.0001 {
		t.Errorf("claude (%.4f) + codex (%.4f) != unfiltered (%.4f): delta=%.4f",
			claudeTotal, codexTotal, unfilteredTotal, delta)
	}
	if missingTotal != 0 {
		t.Errorf("nonexistent tool should return 0, got %.4f", missingTotal)
	}
}

// TestAPIActionFullText pins the v1.6.29 on-demand full-text endpoint:
// /api/action/<id>/full_text returns the untruncated raw_tool_input +
// raw_tool_output for the requested action, 404s on unknown ids, and
// 400s on a malformed path. Companion of the /messages elision: the
// timeline payload carries a 4 KiB preview; this endpoint serves the
// full body when the operator clicks copy or view-full-text.
func TestAPIActionFullText(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)

	// Seed one action with raw_tool_input AND raw_tool_output set
	// so the endpoint has both fields to return.
	bigInput := strings.Repeat("INPUT-", 800) // ~4.8 KiB, exceeds inline preview cap
	bigOutput := strings.Repeat("OUT-", 1500) // ~6 KiB
	_, err := store.New(s.opts.DB).Ingest(context.Background(), []models.ToolEvent{{
		SourceFile: "ft", SourceEventID: "ft-1", SessionID: "sFT",
		ProjectRoot: t.TempDir(), Timestamp: time.Now().UTC(),
		Tool: models.ToolClaudeCode, ActionType: models.ActionUserPrompt,
		Target: "preview-target", RawToolInput: bigInput, ToolOutput: bigOutput,
		Success: true,
	}}, nil, store.IngestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var actionID int64
	if err := s.opts.DB.QueryRow(
		`SELECT id FROM actions WHERE source_event_id = ?`, "ft-1",
	).Scan(&actionID); err != nil {
		t.Fatal(err)
	}

	// Happy path.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/action/%d/full_text", actionID), nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("GET /api/action/%d/full_text: %d body=%s", actionID, rr.Code, rr.Body.String())
	}
	var got struct {
		ActionID      int64  `json:"action_id"`
		ActionType    string `json:"action_type"`
		Target        string `json:"target"`
		RawToolInput  string `json:"raw_tool_input"`
		RawToolOutput string `json:"raw_tool_output"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if got.ActionID != actionID {
		t.Errorf("action_id: got %d, want %d", got.ActionID, actionID)
	}
	if got.RawToolInput != bigInput {
		t.Errorf("raw_tool_input length: got %d, want %d", len(got.RawToolInput), len(bigInput))
	}
	if got.RawToolOutput != bigOutput {
		t.Errorf("raw_tool_output length: got %d, want %d", len(got.RawToolOutput), len(bigOutput))
	}
	if got.ActionType != models.ActionUserPrompt {
		t.Errorf("action_type: got %q, want %q", got.ActionType, models.ActionUserPrompt)
	}

	// 404: unknown id.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/action/999999/full_text", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 404 {
		t.Errorf("missing id: got %d, want 404", rr.Code)
	}

	// 400: malformed id.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/action/not-a-number/full_text", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Errorf("malformed id: got %d, want 400", rr.Code)
	}

	// 404: missing sub-resource (path without /full_text suffix).
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/action/%d", actionID), nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 404 {
		t.Errorf("unsupported sub-resource: got %d, want 404", rr.Code)
	}
}
