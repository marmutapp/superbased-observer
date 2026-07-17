package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	browseringest "github.com/marmutapp/superbased-observer/internal/ingest/browser"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// openTestStore opens a fresh migrated store for the ingest-path tests and
// returns both the store and the raw handle (for assertion queries).
func openTestStore(t *testing.T) (*store.Store, *sql.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "observer.db")
	database, err := db.Open(context.Background(), db.Options{Path: dbPath, SkipIntegrityCheck: true})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return store.New(database), database
}

func chatGPTBody(t *testing.T, gran string) []byte {
	t.Helper()
	b, _ := json.Marshal(map[string]any{
		"schema_version":  1,
		"site":            "chatgpt-web",
		"conversation_id": "conv-x",
		"message_id":      "msg-x",
		"model":           "gpt-4o",
		"prompt_text":     "a prompt with content",
		"response_text":   "a response with content",
		"captured_at":     "2026-07-10T09:30:00Z",
		"granularity":     gran,
	})
	return b
}

// TestIngestBrowserTurnGranularityCeiling proves the daemon ceiling (§5.1):
// a "full" turn stores no content when [browser].granularity_ceiling caps at
// usage_only — the daemon is the final authority on what is STORED.
func TestIngestBrowserTurnGranularityCeiling(t *testing.T) {
	st, database := openTestStore(t)
	bc := config.BrowserConfig{Enabled: true, GranularityCeiling: "usage_only"}
	if err := ingestBrowserTurn(context.Background(), st, chatGPTBody(t, "full"), bc); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	var out string
	if err := database.QueryRowContext(context.Background(),
		`SELECT COALESCE(raw_tool_output, '') FROM actions WHERE tool = 'chatgpt-web'`).Scan(&out); err != nil {
		t.Fatalf("query: %v", err)
	}
	if out != "" {
		t.Errorf("tool_output = %q, want empty (ceiling clamped full → usage_only)", out)
	}
}

// TestIngestBrowserTurnSiteToggle proves the per-site daemon toggle drops a
// disabled site's turns before anything is stored.
func TestIngestBrowserTurnSiteToggle(t *testing.T) {
	st, database := openTestStore(t)
	bc := config.BrowserConfig{Enabled: true, Sites: map[string]bool{"chatgpt-web": false}}
	if err := ingestBrowserTurn(context.Background(), st, chatGPTBody(t, "usage_only"), bc); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	var n int
	if err := database.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM actions WHERE tool = 'chatgpt-web'`).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 0 {
		t.Errorf("actions = %d, want 0 (site toggled off)", n)
	}
}

// TestBrowserLoopbackListenerLandsRow wires the loopback receiver to the SAME
// ingestBrowserTurn one-owner path the hook uses, POSTs a captured turn, and
// asserts the row lands. This is the loopback-graft verification: transport
// (HTTP) is a deployment detail; the normalize→ingest owner is shared.
func TestBrowserLoopbackListenerLandsRow(t *testing.T) {
	st, database := openTestStore(t)
	bc := config.BrowserConfig{Enabled: true, GranularityCeiling: "full"}
	recv, err := browseringest.New(browseringest.Options{
		Addr: "127.0.0.1:0",
		Handler: func(ctx context.Context, body []byte) error {
			return ingestBrowserTurn(ctx, st, body, bc)
		},
	})
	if err != nil {
		t.Fatalf("receiver New: %v", err)
	}
	recv.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = recv.Shutdown(ctx)
	})

	url := "http://" + recv.Addr() + "/v1/browser/capture"
	resp, err := http.Post(url, "application/json", bytes.NewReader(chatGPTBody(t, "full"))) //nolint:noctx // test
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	var n int
	if err := database.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM actions WHERE tool = 'chatgpt-web'`).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 1 {
		t.Errorf("actions(chatgpt-web) via loopback = %d, want 1", n)
	}
}

// TestBrowserHookEndToEnd proves the full server-side path MINUS the live
// browser: a realistic captured-turn JSON payload fed to `observer browser
// hook` (via handleBrowserHook) normalizes through internal/adapter/
// browserchat and lands an action row plus an ESTIMATED token row under
// tool=chatgpt-web via the unchanged store.Ingest seam.
//
// The live authenticated-browser capture (extension → native host → this
// command) is an operator-attended step and is NOT exercised here; this test
// drives everything downstream of the native host.
func TestBrowserHookEndToEnd(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "observer.db")
	configPath := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(configPath, []byte("[observer]\ndb_path = \""+filepath.ToSlash(dbPath)+"\"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	payload := map[string]any{
		"schema_version":      1,
		"site":                "chatgpt-web",
		"conversation_id":     "conv-e2e-1",
		"message_id":          "msg-e2e-1",
		"model":               "gpt-4o",
		"request_url":         "https://chatgpt.com/backend-api/conversation",
		"prompt_text":         "summarize the plot of Hamlet in one sentence",
		"response_text":       "A Danish prince avenges his father's murder and dies in the attempt.",
		"prompt_tokens_est":   11,
		"response_tokens_est": 14,
		"latency_ms":          950,
		"captured_at":         "2026-07-10T09:30:00Z",
		"granularity":         "full",
		"title":               "lit",
	}
	body, _ := json.Marshal(payload)

	// Redirect stdin (payload) and stdout (ack) around the call.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	go func() {
		_, _ = stdinW.Write(body)
		_ = stdinW.Close()
	}()
	oldStdin, oldStdout := os.Stdin, os.Stdout
	os.Stdin = stdinR
	devNull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	os.Stdout = devNull
	t.Cleanup(func() {
		os.Stdin, os.Stdout = oldStdin, oldStdout
		_ = devNull.Close()
	})

	handleBrowserHook(context.Background(), "capture", configPath)
	os.Stdin, os.Stdout = oldStdin, oldStdout

	// Assert the rows landed.
	database, err := db.Open(context.Background(), db.Options{Path: dbPath, SkipIntegrityCheck: true})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	var actionCount int
	if err := database.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM actions WHERE tool = 'chatgpt-web' AND action_type = 'assistant_message'`).
		Scan(&actionCount); err != nil {
		t.Fatalf("query actions: %v", err)
	}
	if actionCount != 1 {
		t.Fatalf("actions(chatgpt-web) = %d, want 1", actionCount)
	}

	var (
		input, output int64
		source        string
	)
	if err := database.QueryRowContext(context.Background(),
		`SELECT input_tokens, output_tokens, source FROM token_usage WHERE tool = 'chatgpt-web'`).
		Scan(&input, &output, &source); err != nil {
		t.Fatalf("query token_usage: %v", err)
	}
	if input != 11 || output != 14 {
		t.Errorf("tokens = (%d, %d), want (11, 14) from the client estimate", input, output)
	}
	if source != "estimated" {
		t.Errorf("token source = %q, want estimated", source)
	}

	// The session must be attributed to the synthetic browser project root.
	var projectRoot string
	if err := database.QueryRowContext(context.Background(),
		`SELECT p.root_path FROM sessions s JOIN projects p ON p.id = s.project_id WHERE s.id = 'conv-e2e-1'`).
		Scan(&projectRoot); err != nil {
		t.Fatalf("query session project: %v", err)
	}
	if projectRoot != "browser://chatgpt.com" {
		t.Errorf("project root = %q, want browser://chatgpt.com", projectRoot)
	}
}

// --- A6: health beacon (recorded + surfaced) --------------------------------

func cfgWithDBIn(dir string) config.Config {
	var c config.Config
	c.Observer.DBPath = filepath.Join(dir, "observer.db")
	c.Browser.Enabled = true
	return c
}

// TestRecordBrowserHealthWritesAndSurfaces is the A6 core: a beacon is
// recorded to the node-local file and read back with the right fields.
func TestRecordBrowserHealthWritesAndSurfaces(t *testing.T) {
	dir := t.TempDir()
	cfg := cfgWithDBIn(dir)

	body := []byte(`{"type":"health","site":"chatgpt-web","status":"degraded","reason":"parser stale","ts":1737000000000}`)
	if err := recordBrowserHealth(cfg, body); err != nil {
		t.Fatalf("recordBrowserHealth: %v", err)
	}
	path := filepath.Join(dir, browserHealthFileName)
	hf := loadBrowserHealthFile(path)
	e, ok := hf.Sites["chatgpt-web"]
	if !ok {
		t.Fatalf("site not recorded; file=%v", hf.Sites)
	}
	if e.Status != "degraded" || e.Reason != "parser stale" || e.TS != 1737000000000 {
		t.Errorf("entry = %+v, want degraded/parser stale/ts", e)
	}
	if e.RecordedAt == 0 {
		t.Errorf("recorded_at should be stamped daemon-side")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("health file mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestRecordBrowserHealthDefaultsAndDropsEmptySite(t *testing.T) {
	dir := t.TempDir()
	cfg := cfgWithDBIn(dir)
	if err := recordBrowserHealth(cfg, []byte(`{"type":"health","site":"claude-web"}`)); err != nil {
		t.Fatalf("record: %v", err)
	}
	hf := loadBrowserHealthFile(filepath.Join(dir, browserHealthFileName))
	if hf.Sites["claude-web"].Status != "ok" {
		t.Errorf("missing status should default ok, got %q", hf.Sites["claude-web"].Status)
	}
	if err := recordBrowserHealth(cfg, []byte(`{"type":"health"}`)); err != nil {
		t.Fatalf("empty-site beacon should be a soft no-op: %v", err)
	}
	if hf2 := loadBrowserHealthFile(filepath.Join(dir, browserHealthFileName)); len(hf2.Sites) != 1 {
		t.Errorf("empty-site beacon should not add an entry, sites=%v", hf2.Sites)
	}
}

func TestEvictOldestHealthBounds(t *testing.T) {
	sites := map[string]browserHealthEntry{}
	for i := 0; i < maxHealthSites+5; i++ {
		sites[string(rune('a'+i))] = browserHealthEntry{RecordedAt: int64(i)}
	}
	evictOldestHealth(sites, maxHealthSites)
	if len(sites) != maxHealthSites {
		t.Fatalf("len = %d, want %d", len(sites), maxHealthSites)
	}
	if _, ok := sites["a"]; ok {
		t.Errorf("oldest entry 'a' should have been evicted")
	}
}

func TestRunBrowserHealthEmptyThenSurfaces(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[observer]\ndb_path = \""+filepath.ToSlash(filepath.Join(dir, "observer.db"))+"\"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	var out strings.Builder
	if err := runBrowserHealth(&out, cfgPath); err != nil {
		t.Fatalf("runBrowserHealth: %v", err)
	}
	if !strings.Contains(out.String(), "no browser health beacons recorded yet") {
		t.Errorf("empty-state message missing:\n%s", out.String())
	}

	cfg := cfgWithDBIn(dir)
	if err := recordBrowserHealth(cfg, []byte(`{"type":"health","site":"gemini-web","status":"degraded","reason":"shape canary"}`)); err != nil {
		t.Fatalf("record: %v", err)
	}
	var out2 strings.Builder
	if err := runBrowserHealth(&out2, cfgPath); err != nil {
		t.Fatalf("runBrowserHealth: %v", err)
	}
	s := out2.String()
	if !strings.Contains(s, "gemini-web") || !strings.Contains(s, "degraded") || !strings.Contains(s, "shape canary") {
		t.Errorf("rendered health missing fields:\n%s", s)
	}
}

// TestHandleBrowserHookRoutesHealthEvent proves the event branch: an event of
// "health" records to the health file and never touches the actions table.
func TestHandleBrowserHookRoutesHealthEvent(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "observer.db")
	configPath := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(configPath, []byte("[observer]\ndb_path = \""+filepath.ToSlash(dbPath)+"\"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	body := []byte(`{"type":"health","site":"perplexity-web","status":"ok"}`)

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	go func() { _, _ = stdinW.Write(body); _ = stdinW.Close() }()
	oldStdin, oldStdout := os.Stdin, os.Stdout
	os.Stdin = stdinR
	devNull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	os.Stdout = devNull
	t.Cleanup(func() { os.Stdin, os.Stdout = oldStdin, oldStdout; _ = devNull.Close() })

	handleBrowserHook(context.Background(), "health", configPath)
	os.Stdin, os.Stdout = oldStdin, oldStdout

	hf := loadBrowserHealthFile(filepath.Join(tmp, browserHealthFileName))
	if hf.Sites["perplexity-web"].Status != "ok" {
		t.Errorf("health event not recorded via handleBrowserHook: %v", hf.Sites)
	}
}

// --- A4: shared loopback-ingress token -------------------------------------

func TestResolveBrowserIngestTokenGeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	cfg := cfgWithDBIn(dir)
	tok, err := resolveBrowserIngestToken(cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(tok) < 32 {
		t.Errorf("generated token too short: %q", tok)
	}
	path := filepath.Join(dir, "browser-ingest-token")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("token not persisted: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("token file mode = %v, want 0600", fi.Mode().Perm())
	}
	tok2, err := resolveBrowserIngestToken(cfg)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if tok2 != tok {
		t.Errorf("token not stable across calls: %q != %q", tok, tok2)
	}
}

func TestResolveBrowserIngestTokenPrefersConfigured(t *testing.T) {
	dir := t.TempDir()
	cfg := cfgWithDBIn(dir)
	pinned := "pin" + "ned-value"
	cfg.Browser.Listener.Token = pinned
	tok, err := resolveBrowserIngestToken(cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tok != pinned {
		t.Errorf("resolved %q, want the configured value %q", tok, pinned)
	}
	if _, err := os.Stat(filepath.Join(dir, "browser-ingest-token")); !os.IsNotExist(err) {
		t.Errorf("configured token should not persist a file (stat err = %v)", err)
	}
}
