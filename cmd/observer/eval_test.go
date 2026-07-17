//go:build !no_obs

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/obs/span"
	obsstore "github.com/marmutapp/superbased-observer/internal/obs/store"
)

// TestEvalCLI_EndToEnd drives the real `observer eval` cobra commands against a
// seeded db: build a dataset from traces, list it, run code scorers, and prove
// --fail-under both passes and trips. Grounds the P5 CLI through the actual
// command wiring (boundary-exempt as a _test.go file).
func TestEvalCLI_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "observer.db")
	ctx := context.Background()

	// Seed: two LLM spans, one with valid-JSON output content.
	conn, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	st, err := obsstore.Open(ctx, conn)
	if err != nil {
		t.Fatalf("obsstore.Open: %v", err)
	}
	start := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	if err := st.UpsertTrace(ctx, span.Trace{TraceID: "t1", Source: span.SourceOTLPTrace, Status: span.StatusOK, StartedAt: start, EndedAt: start.Add(time.Second)}); err != nil {
		t.Fatalf("UpsertTrace: %v", err)
	}
	in, out := int64(10), int64(5)
	if err := st.UpsertSpansBatch(ctx, []span.Span{
		{SpanID: "llm1", TraceID: "t1", Kind: span.KindLLM, Name: "chat", Status: span.StatusOK, InputTokens: &in, OutputTokens: &out, StartedAt: start, EndedAt: start.Add(500 * time.Millisecond)},
		{SpanID: "llm2", TraceID: "t1", Kind: span.KindLLM, Name: "chat2", Status: span.StatusOK, StartedAt: start, EndedAt: start.Add(time.Second)},
	}); err != nil {
		t.Fatalf("UpsertSpansBatch: %v", err)
	}
	// llm1 has valid-JSON output; llm2 has none.
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO obs_span_content (span_id, kind, content, content_hash, time) VALUES (?,?,?,?,?)`,
		"llm1", "output", `{"ok":true}`, "h1", start.Format(time.RFC3339)); err != nil {
		t.Fatalf("seed content: %v", err)
	}
	_ = conn.Close()

	// Config with observability on + full_content (so content snapshots).
	cfgPath := filepath.Join(dir, "config.toml")
	cfgBody := "[observer]\ndb_path = \"" + dbPath + "\"\n[observability]\nenabled = true\n[org_client.share]\nfull_content = true\n"
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	run := func(args ...string) (string, error) {
		cmd := newEvalCmd()
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs(append(args, "--config", cfgPath))
		err := cmd.Execute()
		return buf.String(), err
	}
	// runBare executes without appending --config (for subcommands without it).
	runBare := func(args ...string) (string, error) {
		cmd := newEvalCmd()
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return buf.String(), err
	}

	// scorers discovery (no --config flag on this subcommand).
	if outStr, err := runBare("scorers"); err != nil || !strings.Contains(outStr, "json_valid") {
		t.Fatalf("scorers = %q, %v", outStr, err)
	}

	// build dataset from traces.
	if outStr, err := run("dataset", "create-from-traces", "demo"); err != nil || !strings.Contains(outStr, "2 new item") {
		t.Fatalf("create-from-traces = %q, %v", outStr, err)
	}

	// list shows the dataset with 2 items.
	if outStr, err := run("dataset", "list"); err != nil || !strings.Contains(outStr, "demo") || !strings.Contains(outStr, "2") {
		t.Fatalf("dataset list = %q, %v", outStr, err)
	}

	// run json_valid: 1/2 pass (50%); --fail-under 0.9 must trip.
	outStr, err := run("run", "demo", "--scorer", "json_valid", "--fail-under", "0.9")
	if err == nil {
		t.Fatalf("expected --fail-under to trip at 50%% pass; out=%q", outStr)
	}
	if !strings.Contains(err.Error(), "regression") {
		t.Errorf("err = %v, want a regression error", err)
	}

	// run status_ok: both spans ok → 100%; --fail-under 0.9 passes.
	if outStr, err := run("run", "demo", "--scorer", "status_ok", "--fail-under", "0.9"); err != nil {
		t.Fatalf("status_ok run should pass the gate, got %q / %v", outStr, err)
	}

	// llm_judge without a wired judge fails to build (deferred host binding).
	if _, err := run("run", "demo", "--scorer", "llm_judge:prompt=rate"); err == nil {
		t.Error("llm_judge should error without a wired judge client")
	}
}
