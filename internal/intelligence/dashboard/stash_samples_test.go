package dashboard

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/stash"
)

// TestLoadStashedSamples proves the SROD "what's getting stashed" preview reads
// a scrubbed, single-line snippet of the stashed body keyed by the stash event's
// body_hash, and flags how many times it was retrieved.
func TestLoadStashedSamples(t *testing.T) {
	t.Parallel()
	database, cleanup := openForecastTestDB(t)
	defer cleanup()
	dir := t.TempDir()

	st, err := stash.New(stash.Options{Dir: dir})
	if err != nil {
		t.Fatalf("stash.New: %v", err)
	}
	body := "GET /api/thing\nAuthorization: Bearer sk-secret-ABCDEF123456\nline three of the output"
	sha, err := st.Write([]byte(body))
	if err != nil {
		t.Fatalf("stash.Write: %v", err)
	}

	ts := time.Now().UTC().Format(time.RFC3339Nano)
	// compression_events.api_turn_id is a NOT NULL FK → seed a parent turn.
	res, err := database.ExecContext(context.Background(),
		`INSERT INTO api_turns (session_id, timestamp, provider, model, input_tokens, output_tokens)
		 VALUES ('sA', ?, 'anthropic', 'claude-opus-4-8', 100, 50)`, ts)
	if err != nil {
		t.Fatal(err)
	}
	turnID, _ := res.LastInsertId()
	if _, err := database.ExecContext(context.Background(),
		`INSERT INTO compression_events (api_turn_id, timestamp, mechanism, original_bytes, compressed_bytes, msg_index, importance_score, body_hash)
		 VALUES (?, ?, 'stash', ?, 200, 0, 0, ?), (?, ?, 'stash', ?, 200, 0, 0, ?)`,
		turnID, ts, len(body), sha, turnID, ts, len(body), sha); err != nil { // stashed twice → count 2
		t.Fatal(err)
	}

	srv := &Server{opts: Options{DB: database, StashDir: dir}}
	got := srv.loadStashedSamples(context.Background(), 7, "", "", nil)
	if len(got) != 1 {
		t.Fatalf("samples = %d, want 1 (distinct body_hash)", len(got))
	}
	s := got[0]
	if s.Sha != sha {
		t.Errorf("sha = %q, want %q", s.Sha, sha)
	}
	if s.Count != 2 {
		t.Errorf("count = %d, want 2 (stashed twice)", s.Count)
	}
	if strings.Contains(s.Snippet, "\n") {
		t.Errorf("snippet should be single-line, got %q", s.Snippet)
	}
	if !strings.Contains(s.Snippet, "GET /api/thing") {
		t.Errorf("snippet should preview the body head, got %q", s.Snippet)
	}
	if strings.Contains(s.Snippet, "sk-secret-ABCDEF123456") {
		t.Errorf("snippet must be scrubbed of the bearer token, got %q", s.Snippet)
	}
}

// TestLoadStashedSamples_NoStashDir: without a stash dir the preview degrades to
// an empty slice (never nil, never an error).
func TestLoadStashedSamples_NoStashDir(t *testing.T) {
	t.Parallel()
	database, cleanup := openForecastTestDB(t)
	defer cleanup()
	srv := &Server{opts: Options{DB: database}} // no StashDir
	got := srv.loadStashedSamples(context.Background(), 7, "", "", nil)
	if got == nil || len(got) != 0 {
		t.Errorf("want empty non-nil slice, got %v", got)
	}
}
