package mcp

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/mcp/audit"
	"github.com/marmutapp/superbased-observer/internal/stash"
)

// TestServer_RetrieveStashed_RoundTrip pins the v1.4.41 / Tier 1 / G31
// MCP-side contract: a sha that was Written into the stash retrieves
// the original bytes via tools/call, and the response shape matches
// the documented `{sha, size_bytes, content}` form.
func TestServer_RetrieveStashed_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obs.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	st, err := stash.New(stash.Options{Dir: filepath.Join(dir, "stash")})
	if err != nil {
		t.Fatalf("stash.New: %v", err)
	}
	body := []byte("the original tool_result body that was stashed")
	sha, err := st.Write(body)
	if err != nil {
		t.Fatalf("stash.Write: %v", err)
	}

	s, err := New(Options{DB: database, ServerName: "test", ServerVersion: "0", Stash: st})
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}

	parsed := callTool(t, s, "retrieve_stashed", map[string]any{"sha": sha})
	if parsed["sha"] != sha {
		t.Errorf("sha mismatch: got %v, want %s", parsed["sha"], sha)
	}
	if int(parsed["size_bytes"].(float64)) != len(body) {
		t.Errorf("size_bytes: got %v, want %d", parsed["size_bytes"], len(body))
	}
	if parsed["content"] != string(body) {
		t.Errorf("content mismatch:\n got %v\n want %s", parsed["content"], body)
	}
	if _, ok := parsed["truncated"]; ok {
		t.Errorf("unexpected truncated flag set: %v", parsed)
	}
}

// TestServer_RetrieveStashed_NotFound pins the missing-sha error path:
// the model gets a clear error rather than empty content.
func TestServer_RetrieveStashed_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obs.db")
	database, _ := db.Open(context.Background(), db.Options{Path: path})
	t.Cleanup(func() { database.Close() })

	st, _ := stash.New(stash.Options{Dir: filepath.Join(dir, "stash")})
	s, _ := New(Options{DB: database, ServerName: "test", ServerVersion: "0", Stash: st})

	resp := rpcCall(t, s, "tools/call", 1, map[string]any{
		"name":      "retrieve_stashed",
		"arguments": map[string]any{"sha": "0000000000000000000000000000000000000000000000000000000000000000"},
	})
	result, _ := resp["result"].(map[string]any)
	if isErr, _ := result["isError"].(bool); !isErr {
		t.Errorf("expected isError=true on missing sha, got result=%v", result)
	}
}

// TestServer_RetrieveStashed_MaxBytes pins the optional truncation
// path: a max_bytes argument shorter than the body returns truncated
// content with truncated=true and the full size_bytes still reports
// the original length so the model can decide whether to follow up
// with a wider window.
func TestServer_RetrieveStashed_MaxBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obs.db")
	database, _ := db.Open(context.Background(), db.Options{Path: path})
	t.Cleanup(func() { database.Close() })

	st, _ := stash.New(stash.Options{Dir: filepath.Join(dir, "stash")})
	body := []byte(strings.Repeat("xyz", 200)) // 600 bytes
	sha, _ := st.Write(body)
	s, _ := New(Options{DB: database, ServerName: "test", ServerVersion: "0", Stash: st})

	parsed := callTool(t, s, "retrieve_stashed", map[string]any{"sha": sha, "max_bytes": 30})
	truncated, _ := parsed["truncated"].(bool)
	if !truncated {
		t.Errorf("expected truncated=true, got %v", parsed)
	}
	content, _ := parsed["content"].(string)
	if len(content) != 30 {
		t.Errorf("content length: got %d, want 30", len(content))
	}
	if int(parsed["size_bytes"].(float64)) != len(body) {
		t.Errorf("size_bytes should report full size %d, got %v", len(body), parsed["size_bytes"])
	}
}

// TestServer_RetrieveStashed_NotRegisteredWithoutStash pins that the
// tool is opt-in via Options.Stash — without it, tools/list does NOT
// include retrieve_stashed.
func TestServer_RetrieveStashed_NotRegisteredWithoutStash(t *testing.T) {
	s, _, _ := testServer(t)
	resp := rpcCall(t, s, "tools/list", 1, nil)
	tools := resp["result"].(map[string]any)["tools"].([]any)
	for _, raw := range tools {
		tm := raw.(map[string]any)
		if tm["name"] == "retrieve_stashed" {
			t.Errorf("retrieve_stashed registered without Options.Stash")
		}
	}
}

// TestServer_RetrieveStashed_LogsK43Signal pins the v1.4.43+ / Tier 3 /
// K43 wiring: a successful retrieve_stashed call writes one row to
// retrieval_signals so the learn pattern miner can later surface high-
// retrieval-rate shas.
func TestServer_RetrieveStashed_LogsK43Signal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obs.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	st, _ := stash.New(stash.Options{Dir: filepath.Join(dir, "stash")})
	body := []byte("k43 signal payload")
	sha, _ := st.Write(body)

	s, _ := New(Options{
		DB: database, ServerName: "test", ServerVersion: "0",
		Stash:          st,
		SignalRecorder: stubSignalRecorder{database: database},
	})

	_ = callTool(t, s, "retrieve_stashed", map[string]any{"sha": sha})

	var n int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM retrieval_signals WHERE signal_type = 'retrieve_stashed' AND payload = ?`,
		sha,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 retrieve_stashed signal row for sha %s, got %d", sha, n)
	}
}

// stubSignalRecorder is a minimal SignalRecorder that writes through
// to the test DB. We can't import learn here without an import cycle.
type stubSignalRecorder struct{ database *sql.DB }

func (r stubSignalRecorder) RecordRetrieveStashed(ctx context.Context, sha, sessionID string) error {
	_, err := r.database.ExecContext(ctx,
		`INSERT INTO retrieval_signals (action_id, signal_type, signal_at, session_id, payload)
		 VALUES (NULL, 'retrieve_stashed', ?, ?, ?)`,
		"2026-05-07T12:00:00Z", nullableString(sessionID), sha)
	return err
}

func (r stubSignalRecorder) RecordSearchHit(ctx context.Context, actionID int64, query, sessionID string) error {
	_, err := r.database.ExecContext(ctx,
		`INSERT INTO retrieval_signals (action_id, signal_type, signal_at, session_id, payload)
		 VALUES (?, 'search_hit', ?, ?, ?)`,
		actionID, "2026-05-07T12:00:00Z", nullableString(sessionID), query)
	return err
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// retrieveStashedFixture wires a minimal MCP server with the
// retrieve_stashed tool + a real SQL audit writer (so per-sha audit
// rows can be queried in tests). Mirrors getFileFixture.
type retrieveStashedFixture struct {
	s        *Server
	st       *stash.Stash
	auditDB  *sql.DB
	auditW   *audit.SQLWriter
	stashDir string
}

func newRetrieveStashedFixture(t *testing.T) *retrieveStashedFixture {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(dir, "obs.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	stashDir := filepath.Join(dir, "stash")
	st, err := stash.New(stash.Options{Dir: stashDir})
	if err != nil {
		t.Fatalf("stash.New: %v", err)
	}

	auditW := audit.NewSQLWriter(database, slog.New(slog.NewTextHandler(io.Discard, nil)), audit.SQLWriterOptions{
		FlushInterval: 10 * time.Millisecond,
		BatchSize:     8,
	})
	t.Cleanup(func() { _ = auditW.Close() })

	s, err := New(Options{
		DB:            database,
		ServerName:    "test",
		ServerVersion: "0",
		Stash:         st,
		AuditWriter:   auditW,
	})
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}
	return &retrieveStashedFixture{s: s, st: st, auditDB: database, auditW: auditW, stashDir: stashDir}
}

// TestRetrieveStashed_BackwardsCompat_SingleStringByteIdentical pins
// the V7-16 BC contract — DO NOT modify without a major version bump.
//
// A single-string `sha` request + no range params + no max_bytes must
// produce a response whose inner JSON is byte-identical to the v1.7.10
// shape `{"sha":"…","size_bytes":N,"content":"…"}`. Specifically: no
// `returned`, no `total_lines_in_blob`, no `ok`, no `responses`. Key
// order matches the struct field declaration order.
func TestRetrieveStashed_BackwardsCompat_SingleStringByteIdentical(t *testing.T) {
	f := newRetrieveStashedFixture(t)
	body := []byte("hello world")
	sha, _ := f.st.Write(body)

	resp := rpcCall(t, f.s, "tools/call", 1, map[string]any{
		"name":      "retrieve_stashed",
		"arguments": map[string]any{"sha": sha},
	})
	result := resp["result"].(map[string]any)
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("isError=true: %v", result)
	}
	content := result["content"].([]any)[0].(map[string]any)
	got := content["text"].(string)

	// MCP wraps the tool result via json.MarshalIndent("", "  ") so the
	// over-the-wire bytes are pretty-printed. This is part of the
	// v1.7.10 baseline — the agent's prefix cache hashes these exact
	// bytes. ONLY edit this `want` with a corresponding major version
	// bump (V7-16 contract).
	want := fmt.Sprintf("{\n  \"sha\": \"%s\",\n  \"size_bytes\": %d,\n  \"content\": \"%s\"\n}", sha, len(body), string(body))
	if got != want {
		t.Errorf("BC contract violation:\n  got:  %q\n  want: %q", got, want)
	}
}

// TestRetrieveStashed_BackwardsCompat_MaxBytesNoNewKeys pins that the
// legacy `max_bytes` truncation path still emits the v1.7.10 shape
// plus `truncated:true` — no new fields leaked in.
func TestRetrieveStashed_BackwardsCompat_MaxBytesNoNewKeys(t *testing.T) {
	f := newRetrieveStashedFixture(t)
	body := []byte(strings.Repeat("xyz", 200))
	sha, _ := f.st.Write(body)

	parsed := callTool(t, f.s, "retrieve_stashed", map[string]any{"sha": sha, "max_bytes": 30})
	got := setOfKeys(parsed)
	want := map[string]bool{"sha": true, "size_bytes": true, "content": true, "truncated": true}
	if !sameKeySet(got, want) {
		t.Errorf("legacy max_bytes path leaked new fields: got keys=%v, want %v", got, want)
	}
}

// TestRetrieveStashed_SingleStringWithRange covers Branch B of the
// response-shape rules: single-string sha + range params adds
// `returned` and `total_lines_in_blob` to the legacy shape — extra
// fields don't break callers reading only the legacy keys.
func TestRetrieveStashed_SingleStringWithRange(t *testing.T) {
	f := newRetrieveStashedFixture(t)
	body := []byte("L1\nL2\nL3\nL4\nL5\n")
	sha, _ := f.st.Write(body)

	parsed := callTool(t, f.s, "retrieve_stashed", map[string]any{
		"sha":        sha,
		"start_line": 2,
		"end_line":   4,
	})
	if parsed["sha"] != sha {
		t.Errorf("sha mismatch: got %v, want %s", parsed["sha"], sha)
	}
	if got := parsed["content"]; got != "L2\nL3\nL4\n" {
		t.Errorf("content: got %q, want L2\\nL3\\nL4\\n", got)
	}
	returned, ok := parsed["returned"].(map[string]any)
	if !ok {
		t.Fatalf("returned envelope missing: %v", parsed)
	}
	if int(returned["start"].(float64)) != 2 || int(returned["end"].(float64)) != 4 || int(returned["total"].(float64)) != 5 {
		t.Errorf("returned: got %+v, want start=2 end=4 total=5", returned)
	}
	if int(parsed["total_lines_in_blob"].(float64)) != 5 {
		t.Errorf("total_lines_in_blob: got %v, want 5", parsed["total_lines_in_blob"])
	}
}

// TestRetrieveStashed_ArrayShape_AllOK covers Branch C: an array sha
// switches to the `{ok, responses:[...]}` envelope; per-sha rows carry
// their own `ok`. Input order is preserved (load-bearing for prefix
// cache + agent parsing).
func TestRetrieveStashed_ArrayShape_AllOK(t *testing.T) {
	f := newRetrieveStashedFixture(t)
	shaA, _ := f.st.Write([]byte("alpha"))
	shaB, _ := f.st.Write([]byte("beta"))
	shaC, _ := f.st.Write([]byte("gamma"))

	parsed := callTool(t, f.s, "retrieve_stashed", map[string]any{
		"sha": []string{shaA, shaB, shaC},
	})
	if ok, _ := parsed["ok"].(bool); !ok {
		t.Errorf("envelope ok: got %v, want true", parsed["ok"])
	}
	responses, _ := parsed["responses"].([]any)
	if len(responses) != 3 {
		t.Fatalf("len(responses): got %d, want 3", len(responses))
	}
	wantOrder := []string{shaA, shaB, shaC}
	for i, raw := range responses {
		rm := raw.(map[string]any)
		if rm["sha"] != wantOrder[i] {
			t.Errorf("input-order broken at idx %d: got %v, want %s", i, rm["sha"], wantOrder[i])
		}
		if !rm["ok"].(bool) {
			t.Errorf("ok at idx %d: got false, want true (reason=%v)", i, rm["reason"])
		}
	}
}

// TestRetrieveStashed_ArrayShape_MixedSuccess: an array with mixed
// resolvable and missing shas returns the envelope with per-sha
// success/failure rows. Failed lookups never poison the rest of the
// batch.
func TestRetrieveStashed_ArrayShape_MixedSuccess(t *testing.T) {
	f := newRetrieveStashedFixture(t)
	shaA, _ := f.st.Write([]byte("alpha"))
	missing := strings.Repeat("0", 64)
	shaC, _ := f.st.Write([]byte("gamma"))

	parsed := callTool(t, f.s, "retrieve_stashed", map[string]any{
		"sha": []string{shaA, missing, shaC},
	})
	responses := parsed["responses"].([]any)
	if len(responses) != 3 {
		t.Fatalf("len(responses): got %d, want 3", len(responses))
	}
	r0 := responses[0].(map[string]any)
	if !r0["ok"].(bool) || r0["content"] != "alpha" {
		t.Errorf("response[0]: got ok=%v content=%v", r0["ok"], r0["content"])
	}
	r1 := responses[1].(map[string]any)
	if r1["ok"].(bool) {
		t.Errorf("response[1] should be ok=false for missing sha; got %+v", r1)
	}
	if reason, _ := r1["reason"].(string); !strings.Contains(reason, "sha_not_found") {
		t.Errorf("response[1] reason: got %q, want sha_not_found prefix", reason)
	}
	r2 := responses[2].(map[string]any)
	if !r2["ok"].(bool) || r2["content"] != "gamma" {
		t.Errorf("response[2]: got ok=%v content=%v", r2["ok"], r2["content"])
	}
}

// TestRetrieveStashed_ArraySingleElement pins D-2: array of one
// returns the envelope shape (NOT the legacy single-string shape).
// Array literal == explicit caller intent to use the new wire.
func TestRetrieveStashed_ArraySingleElement(t *testing.T) {
	f := newRetrieveStashedFixture(t)
	sha, _ := f.st.Write([]byte("only one"))

	parsed := callTool(t, f.s, "retrieve_stashed", map[string]any{
		"sha": []string{sha},
	})
	if _, isEnvelope := parsed["responses"]; !isEnvelope {
		t.Errorf("array-of-one should emit envelope; got legacy shape %v", parsed)
	}
	if _, hasContent := parsed["content"]; hasContent {
		t.Errorf("envelope shouldn't have top-level content; got %v", parsed)
	}
}

// TestRetrieveStashed_ArrayCap: 26 shas exceeds the default 25 cap;
// 25 works. Cap rejection surfaces as a top-level error (not a
// per-sha failure).
func TestRetrieveStashed_ArrayCap(t *testing.T) {
	f := newRetrieveStashedFixture(t)
	// All same content → all same sha (dedup), but the cap counts
	// requests not unique shas. Use the same valid sha 26 times.
	sha, _ := f.st.Write([]byte("x"))
	over := make([]string, 26)
	for i := range over {
		over[i] = sha
	}
	resp := rpcCall(t, f.s, "tools/call", 1, map[string]any{
		"name":      "retrieve_stashed",
		"arguments": map[string]any{"sha": over},
	})
	result := resp["result"].(map[string]any)
	if isErr, _ := result["isError"].(bool); !isErr {
		t.Errorf("26 shas should error; got result=%v", result)
	}
	under := make([]string, 25)
	for i := range under {
		under[i] = sha
	}
	_ = callTool(t, f.s, "retrieve_stashed", map[string]any{"sha": under})
}

// TestRetrieveStashed_AuditRowPerSha: N shas in a single array call
// produces N audit rows (success OR denial). Each row encodes the sha
// as `stashed://<sha>` in path_requested.
func TestRetrieveStashed_AuditRowPerSha(t *testing.T) {
	f := newRetrieveStashedFixture(t)
	shaA, _ := f.st.Write([]byte("a"))
	shaB, _ := f.st.Write([]byte("b"))
	missing := strings.Repeat("f", 64)

	_ = callTool(t, f.s, "retrieve_stashed", map[string]any{
		"sha": []string{shaA, shaB, missing},
	})

	got := auditRowCount(t, f.auditDB, "tool_name = 'retrieve_stashed'")
	if got != 3 {
		t.Errorf("audit rows: got %d, want 3 (1 per sha — both successes and the not_found)", got)
	}
	// Two success rows + one denial.
	ok := auditRowCount(t, f.auditDB, "tool_name = 'retrieve_stashed' AND response_ok = 1")
	if ok != 2 {
		t.Errorf("audit ok rows: got %d, want 2", ok)
	}
	notFound := auditRowCount(t, f.auditDB, "tool_name = 'retrieve_stashed' AND response_ok = 0 AND path_requested = ?", "stashed://"+missing)
	if notFound != 1 {
		t.Errorf("audit not_found row for missing sha: got %d, want 1", notFound)
	}
}

// TestRetrieveStashed_BadSha_DoesNotPanic: malformed shas at every
// branch point (string + array, valid + invalid mixed) surface as
// clean per-sha denials, never as panics or path escapes.
func TestRetrieveStashed_BadSha_DoesNotPanic(t *testing.T) {
	f := newRetrieveStashedFixture(t)
	good, _ := f.st.Write([]byte("ok"))

	// Single-string bad sha — top-level error.
	for _, bad := range []string{"", "short", "../etc/passwd", "ABCDEF", "not-hex"} {
		resp := rpcCall(t, f.s, "tools/call", 1, map[string]any{
			"name":      "retrieve_stashed",
			"arguments": map[string]any{"sha": bad},
		})
		result := resp["result"].(map[string]any)
		if isErr, _ := result["isError"].(bool); !isErr {
			t.Errorf("single bad-sha %q should error; result=%v", bad, result)
		}
	}

	// Array with mixed bad/good — bad ones surface as per-sha denials,
	// good ones still resolve.
	parsed := callTool(t, f.s, "retrieve_stashed", map[string]any{
		"sha": []string{good, "../etc/passwd", "not-hex"},
	})
	responses := parsed["responses"].([]any)
	if len(responses) != 3 {
		t.Fatalf("len(responses): got %d, want 3", len(responses))
	}
	if !responses[0].(map[string]any)["ok"].(bool) {
		t.Errorf("good sha at idx 0 failed: %v", responses[0])
	}
	for _, idx := range []int{1, 2} {
		if responses[idx].(map[string]any)["ok"].(bool) {
			t.Errorf("bad sha at idx %d should be ok=false: %v", idx, responses[idx])
		}
	}
}

// TestRetrieveStashed_RangeSliceDeterministic: repeated calls with
// the same (sha, start_line, end_line) return byte-identical content.
// Critical for OpenAI's prefix-hash cache.
func TestRetrieveStashed_RangeSliceDeterministic(t *testing.T) {
	f := newRetrieveStashedFixture(t)
	body := []byte("a\nb\nc\nd\ne\n")
	sha, _ := f.st.Write(body)

	first := callTool(t, f.s, "retrieve_stashed", map[string]any{
		"sha":        sha,
		"start_line": 2,
		"end_line":   4,
	})
	second := callTool(t, f.s, "retrieve_stashed", map[string]any{
		"sha":        sha,
		"start_line": 2,
		"end_line":   4,
	})
	if first["content"] != second["content"] {
		t.Errorf("range slice non-deterministic:\n  first:  %v\n  second: %v", first["content"], second["content"])
	}
}

// TestRetrieveStashed_K43SignalPerSha covers the array case: each
// successful sha resolution fires one K43 signal row. Failed lookups
// don't fire signals.
func TestRetrieveStashed_K43SignalPerSha(t *testing.T) {
	dir := t.TempDir()
	database, _ := db.Open(context.Background(), db.Options{Path: filepath.Join(dir, "obs.db")})
	t.Cleanup(func() { database.Close() })
	st, _ := stash.New(stash.Options{Dir: filepath.Join(dir, "stash")})
	shaA, _ := st.Write([]byte("a"))
	shaB, _ := st.Write([]byte("b"))
	missing := strings.Repeat("0", 64)

	s, _ := New(Options{
		DB:             database,
		ServerName:     "test",
		ServerVersion:  "0",
		Stash:          st,
		SignalRecorder: stubSignalRecorder{database: database},
	})
	_ = callTool(t, s, "retrieve_stashed", map[string]any{
		"sha": []string{shaA, shaB, missing},
	})

	var n int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM retrieval_signals WHERE signal_type = 'retrieve_stashed'`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("K43 signals: got %d, want 2 (one per successful sha; missing doesn't fire)", n)
	}
}

// setOfKeys is a small helper to compare the top-level key set of a
// parsed tool response.
func setOfKeys(m map[string]any) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func sameKeySet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// TestServer_RetrieveStashed_RegisteredWhenStashSet pins the inverse:
// passing Options.Stash adds retrieve_stashed to the tool list.
func TestServer_RetrieveStashed_RegisteredWhenStashSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obs.db")
	database, _ := db.Open(context.Background(), db.Options{Path: path})
	t.Cleanup(func() { database.Close() })

	st, _ := stash.New(stash.Options{Dir: filepath.Join(dir, "stash")})
	s, _ := New(Options{DB: database, ServerName: "test", ServerVersion: "0", Stash: st})

	resp := rpcCall(t, s, "tools/list", 1, nil)
	tools := resp["result"].(map[string]any)["tools"].([]any)
	found := false
	for _, raw := range tools {
		tm := raw.(map[string]any)
		if tm["name"] == "retrieve_stashed" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("retrieve_stashed missing from tools/list when Options.Stash is set")
	}
}
