package audit

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(dir, "obs.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNoopWriter_Discards(t *testing.T) {
	w := NewNoopWriter()
	w.Record(context.Background(), Row{Tool: "get_file", ResponseOK: true})
	// Pass if no panic. Recorder owns no state to inspect.
}

func TestSQLWriter_RoundTrip(t *testing.T) {
	database := openTestDB(t)
	w := NewSQLWriter(database, discardLogger(), SQLWriterOptions{
		FlushInterval: 10 * time.Millisecond,
		BatchSize:     8,
	})
	t.Cleanup(func() { _ = w.Close() })

	w.Record(context.Background(), Row{
		Tool:          "get_file",
		SessionID:     "sess-abc",
		RequestHash:   "deadbeef",
		PathRequested: "/abs/path/foo.ts",
		ResponseBytes: 4823,
		ResponseOK:    true,
		Duration:      750 * time.Microsecond,
	})
	w.Record(context.Background(), Row{
		Tool:        "get_file",
		RequestHash: "cafe",
		ResponseOK:  false,
		Reason:      "path outside project_root",
		Duration:    120 * time.Microsecond,
	})

	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	rows, err := database.Query(
		`SELECT tool_name, session_id, request_hash, path_requested,
		        response_size_bytes, response_truncated, response_ok,
		        reason, duration_us
		 FROM mcp_audit ORDER BY id`,
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	type seen struct {
		tool, sessionID, hash, path, reason string
		bytes, ok, truncated                int
		duration                            int64
	}
	var got []seen
	for rows.Next() {
		var s seen
		var sessionID, path, reason sql.NullString
		if err := rows.Scan(&s.tool, &sessionID, &s.hash, &path,
			&s.bytes, &s.truncated, &s.ok, &reason, &s.duration); err != nil {
			t.Fatalf("scan: %v", err)
		}
		s.sessionID = sessionID.String
		s.path = path.String
		s.reason = reason.String
		got = append(got, s)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d", len(got))
	}
	if got[0].tool != "get_file" || got[0].sessionID != "sess-abc" ||
		got[0].hash != "deadbeef" || got[0].path != "/abs/path/foo.ts" ||
		got[0].bytes != 4823 || got[0].ok != 1 || got[0].truncated != 0 ||
		got[0].duration != 750 {
		t.Errorf("row 0: got %+v", got[0])
	}
	if got[1].ok != 0 || got[1].reason != "path outside project_root" ||
		got[1].sessionID != "" || got[1].path != "" {
		t.Errorf("row 1: got %+v", got[1])
	}
}

func TestSQLWriter_BufferOverflow(t *testing.T) {
	database := openTestDB(t)
	w := NewSQLWriter(database, discardLogger(), SQLWriterOptions{
		BufferSize:    2, // tiny so we overflow fast
		FlushInterval: time.Hour,
		BatchSize:     1024,
	})
	t.Cleanup(func() { _ = w.Close() })

	for i := 0; i < 100; i++ {
		w.Record(context.Background(), Row{
			Tool:        "get_file",
			RequestHash: "x",
			ResponseOK:  true,
		})
	}
	// Close drains buffered + lets us see what landed.
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM mcp_audit`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	// Buffer is 2; some rows dropped on the floor. The exact count depends
	// on goroutine scheduling — we just assert the writer survived AND
	// some subset landed (drop-oldest didn't deadlock).
	if n == 0 {
		t.Errorf("expected at least one row to land, got 0")
	}
	if n > 100 {
		t.Errorf("expected at most 100 rows, got %d", n)
	}
}

func TestRequestHash_StableAcrossCalls(t *testing.T) {
	args := map[string]any{"path": "src/foo.ts", "start_line": 100, "end_line": 200}
	h1 := RequestHash("get_file", args)
	h2 := RequestHash("get_file", args)
	if h1 != h2 {
		t.Errorf("hash should be stable across calls: %q vs %q", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("expected 64-char sha256-hex, got %d", len(h1))
	}
}

func TestRequestHash_DiffersOnArgs(t *testing.T) {
	a := map[string]any{"path": "a.ts"}
	b := map[string]any{"path": "b.ts"}
	if RequestHash("get_file", a) == RequestHash("get_file", b) {
		t.Errorf("hashes should differ when args differ")
	}
}

func TestRequestHash_DiffersOnTool(t *testing.T) {
	args := map[string]any{"path": "a.ts"}
	if RequestHash("get_file", args) == RequestHash("get_symbols", args) {
		t.Errorf("hashes should differ when tool name differs")
	}
}

func TestSQLWriter_ZeroTsStampedAtEnqueue(t *testing.T) {
	database := openTestDB(t)
	w := NewSQLWriter(database, discardLogger(), SQLWriterOptions{
		FlushInterval: 10 * time.Millisecond,
	})
	t.Cleanup(func() { _ = w.Close() })

	before := time.Now().UTC()
	w.Record(context.Background(), Row{
		Tool:        "get_file",
		RequestHash: "h",
		ResponseOK:  true,
	})
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	after := time.Now().UTC()

	var ts string
	if err := database.QueryRow(`SELECT ts FROM mcp_audit LIMIT 1`).Scan(&ts); err != nil {
		t.Fatal(err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t.Fatalf("parse ts: %v", err)
	}
	if parsed.Before(before) || parsed.After(after) {
		t.Errorf("ts %v should be in [%v, %v]", parsed, before, after)
	}
}

func TestSQLWriter_ExplicitTsPreserved(t *testing.T) {
	database := openTestDB(t)
	w := NewSQLWriter(database, discardLogger(), SQLWriterOptions{
		FlushInterval: 10 * time.Millisecond,
	})
	t.Cleanup(func() { _ = w.Close() })

	want := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	w.Record(context.Background(), Row{
		Ts:          want,
		Tool:        "get_file",
		RequestHash: "h",
		ResponseOK:  true,
	})
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	var ts string
	if err := database.QueryRow(`SELECT ts FROM mcp_audit LIMIT 1`).Scan(&ts); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ts, "2026-05-30T12:00:00") {
		t.Errorf("ts %q should start with explicit Ts", ts)
	}
}

func TestSQLWriter_CloseIsIdempotent(t *testing.T) {
	database := openTestDB(t)
	w := NewSQLWriter(database, discardLogger(), SQLWriterOptions{})
	if err := w.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}
