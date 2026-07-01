package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/obs/span"
)

func i64(v int64) *int64     { return &v }
func f64(v float64) *float64 { return &v }
func newDB(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "obs.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	s, err := Open(ctx, conn)
	if err != nil {
		t.Fatalf("obs store Open: %v", err)
	}
	return s
}

// TestMigrateIdempotent confirms the obs applier coexists with the host
// schema (db.Open already migrated the host tables) and is safe to re-run.
func TestMigrateIdempotent(t *testing.T) {
	ctx := context.Background()
	conn, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "obs.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := Open(ctx, conn); err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := Open(ctx, conn); err != nil {
		t.Fatalf("second Open (idempotent): %v", err)
	}
	// obs_schema_meta records the latest version (4 after the span-cost
	// columns, migration 0004), host schema_meta untouched.
	var v string
	if err := conn.QueryRowContext(ctx, `SELECT value FROM obs_schema_meta WHERE key='version'`).Scan(&v); err != nil {
		t.Fatalf("read obs version: %v", err)
	}
	if v != "4" {
		t.Errorf("obs_schema_meta version = %q, want 4", v)
	}
}

// TestUpsertTraceAndSpans exercises the write seam: a trace + its spans, then
// a re-upsert that upgrades approximate tokens to exact (authoritative-on-
// merge) without clobbering known values with NULL.
func TestUpsertTraceAndSpans(t *testing.T) {
	ctx := context.Background()
	s := newDB(t)
	start := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)

	if err := s.UpsertTrace(ctx, span.Trace{
		TraceID: "t1", SessionID: "sess-1", Source: span.SourceOTLPTrace,
		RootSpanID: "root", Status: span.StatusUnset, StartedAt: start,
	}); err != nil {
		t.Fatalf("UpsertTrace: %v", err)
	}

	// First observation: approximate input tokens, no cost yet.
	if err := s.UpsertSpansBatch(ctx, []span.Span{
		{SpanID: "root", TraceID: "t1", Kind: span.KindAgent, Name: "run", StartedAt: start},
		{
			SpanID: "llm1", TraceID: "t1", ParentSpanID: "root", Kind: span.KindLLM, Name: "chat",
			Model: "gpt-4o", Provider: "openai", InputTokens: i64(100), RequestID: "req-1",
			Source: span.SourceOTLPTrace, StartedAt: start,
		},
	}); err != nil {
		t.Fatalf("UpsertSpansBatch #1: %v", err)
	}

	// Second observation of llm1: exact output + cost arrive; input stays;
	// a NULL input must NOT clobber the known 100.
	end := start.Add(2 * time.Second)
	if err := s.UpsertSpansBatch(ctx, []span.Span{
		{
			SpanID: "llm1", TraceID: "t1", Kind: span.KindLLM, Status: span.StatusOK,
			OutputTokens: i64(50), CostUSD: f64(0.01), EndedAt: end,
		},
	}); err != nil {
		t.Fatalf("UpsertSpansBatch #2: %v", err)
	}

	var in, out *int64
	var cost *float64
	var status, parent string
	if err := s.db.QueryRowContext(ctx,
		`SELECT input_tokens, output_tokens, cost_usd, status, parent_span_id FROM obs_spans WHERE span_id='llm1'`).
		Scan(&in, &out, &cost, &status, &parent); err != nil {
		t.Fatalf("read llm1: %v", err)
	}
	if in == nil || *in != 100 {
		t.Errorf("input_tokens = %v, want 100 (must survive a later NULL)", in)
	}
	if out == nil || *out != 50 {
		t.Errorf("output_tokens = %v, want 50", out)
	}
	if cost == nil || *cost != 0.01 {
		t.Errorf("cost_usd = %v, want 0.01", cost)
	}
	if status != "ok" {
		t.Errorf("status = %q, want ok", status)
	}
	if parent != "root" {
		t.Errorf("parent_span_id = %q, want root (must survive the second upsert that omitted it)", parent)
	}

	// Tree shape: two spans under trace t1, one of them the root.
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM obs_spans WHERE trace_id='t1'`).Scan(&n); err != nil {
		t.Fatalf("count spans: %v", err)
	}
	if n != 2 {
		t.Errorf("span count = %d, want 2", n)
	}
}

// TestInsertSpanContent confirms the content seam: a raw body and a gated-off
// (hash-only) body both persist, the hash is always present, re-inserts are
// idempotent on (content_hash,kind,span_id), and GetTrace surfaces them.
func TestInsertSpanContent(t *testing.T) {
	ctx := context.Background()
	s := newDB(t)
	start := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)

	if err := s.UpsertTrace(ctx, span.Trace{
		TraceID: "t1", Source: span.SourceOTLPTrace, RootSpanID: "llm1", StartedAt: start,
	}); err != nil {
		t.Fatalf("UpsertTrace: %v", err)
	}
	if err := s.UpsertSpansBatch(ctx, []span.Span{
		{SpanID: "llm1", TraceID: "t1", Kind: span.KindLLM, Name: "chat", StartedAt: start},
	}); err != nil {
		t.Fatalf("UpsertSpansBatch: %v", err)
	}

	// One raw-allowed prompt, one gated-off response (Raw == "" but hash set).
	items := []span.SpanContent{
		{SpanID: "llm1", TraceID: "t1", Kind: span.ContentPrompt, ContentHash: "h-prompt", Raw: "what is 2+2?", Time: start},
		{SpanID: "llm1", TraceID: "t1", Kind: span.ContentResponse, ContentHash: "h-resp", Raw: "", Time: start},
	}
	if err := s.InsertSpanContent(ctx, items); err != nil {
		t.Fatalf("InsertSpanContent: %v", err)
	}
	// Idempotent re-insert: same keys, no duplicate rows.
	if err := s.InsertSpanContent(ctx, items); err != nil {
		t.Fatalf("InsertSpanContent (re-run): %v", err)
	}

	d, found, err := s.GetTrace(ctx, "t1")
	if err != nil || !found {
		t.Fatalf("GetTrace: found=%v err=%v", found, err)
	}
	if len(d.Content) != 2 {
		t.Fatalf("content rows = %d, want 2 (idempotent)", len(d.Content))
	}
	byKind := map[string]SpanContentRow{}
	for _, c := range d.Content {
		byKind[c.Kind] = c
	}
	if c := byKind["prompt"]; c.Content != "what is 2+2?" || c.ContentHash != "h-prompt" {
		t.Errorf("prompt row = %+v, want raw body + hash", c)
	}
	// Gated-off: hash survives, raw is empty.
	if c := byKind["response"]; c.Content != "" || c.ContentHash != "h-resp" {
		t.Errorf("response row = %+v, want hash-only", c)
	}
}

// TestSpanCostRoundTrip confirms cost_source + the cost_detail JSON breakdown
// persist through UpsertSpansBatch and surface on GetTrace's SpanRow.
func TestSpanCostRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newDB(t)
	start := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)

	if err := s.UpsertTrace(ctx, span.Trace{TraceID: "t1", Source: span.SourceOTLPTrace, RootSpanID: "llm1", StartedAt: start}); err != nil {
		t.Fatalf("UpsertTrace: %v", err)
	}
	if err := s.UpsertSpansBatch(ctx, []span.Span{{
		SpanID: "llm1", TraceID: "t1", Kind: span.KindLLM, Name: "chat", StartedAt: start,
		Model: "gpt-4o", CostUSD: f64(0.05), CostSource: span.CostComputed,
		CostDetail: &span.CostBreakdown{Input: f64(0.03), Output: f64(0.02)},
	}}); err != nil {
		t.Fatalf("UpsertSpansBatch: %v", err)
	}

	d, found, err := s.GetTrace(ctx, "t1")
	if err != nil || !found || len(d.Spans) != 1 {
		t.Fatalf("GetTrace: found=%v err=%v spans=%d", found, err, len(d.Spans))
	}
	r := d.Spans[0]
	if r.CostSource != "computed" {
		t.Errorf("cost_source = %q, want computed", r.CostSource)
	}
	if r.CostUSD == nil || *r.CostUSD != 0.05 {
		t.Errorf("cost_usd = %v, want 0.05", r.CostUSD)
	}
	if string(r.CostDetail) != `{"input":0.03,"output":0.02}` {
		t.Errorf("cost_detail = %s, want input/output breakdown", r.CostDetail)
	}
	// Trace cost SUM reflects the (computed) total → hero is non-zero.
	if d.Trace.CostUSD != 0.05 {
		t.Errorf("trace cost = %v, want 0.05", d.Trace.CostUSD)
	}
}

// TestTraceTokenSums confirms Gap C: per-trace token sums surface on both the
// list and detail, and TotalTokens prefers the reported total but falls back to
// input+output when none is reported.
func TestTraceTokenSums(t *testing.T) {
	ctx := context.Background()
	s := newDB(t)
	start := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)

	if err := s.UpsertTrace(ctx, span.Trace{TraceID: "t1", Source: span.SourceOTLPTrace, RootSpanID: "a", StartedAt: start}); err != nil {
		t.Fatalf("UpsertTrace: %v", err)
	}
	// Two LLM spans; only the second reports total_tokens.
	if err := s.UpsertSpansBatch(ctx, []span.Span{
		{SpanID: "a", TraceID: "t1", Kind: span.KindLLM, StartedAt: start, InputTokens: i64(100), OutputTokens: i64(20), CacheReadTokens: i64(40), ReasoningTokens: i64(10)},
		{SpanID: "b", TraceID: "t1", Kind: span.KindLLM, StartedAt: start, InputTokens: i64(200), OutputTokens: i64(30), TotalTokens: i64(230)},
	}); err != nil {
		t.Fatalf("UpsertSpansBatch: %v", err)
	}

	d, found, err := s.GetTrace(ctx, "t1")
	if err != nil || !found {
		t.Fatalf("GetTrace: found=%v err=%v", found, err)
	}
	tr := d.Trace
	if tr.InputTokens != 300 || tr.OutputTokens != 50 || tr.CacheReadTokens != 40 || tr.ReasoningTokens != 10 {
		t.Errorf("sums = in%d out%d cache%d reason%d, want 300/50/40/10", tr.InputTokens, tr.OutputTokens, tr.CacheReadTokens, tr.ReasoningTokens)
	}
	// reported total (230) present → wins over input+output (350).
	if tr.TotalTokens != 230 {
		t.Errorf("total tokens = %d, want 230 (reported wins)", tr.TotalTokens)
	}

	// List path agrees.
	list, err := s.ListTraces(ctx, 10, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListTraces: n=%d err=%v", len(list), err)
	}
	if list[0].InputTokens != 300 || list[0].TotalTokens != 230 {
		t.Errorf("list sums = in%d total%d, want 300/230", list[0].InputTokens, list[0].TotalTokens)
	}
}

// TestTraceTotalTokensFallback: with no reported total_tokens, TotalTokens
// derives from input+output.
func TestTraceTotalTokensFallback(t *testing.T) {
	ctx := context.Background()
	s := newDB(t)
	start := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	if err := s.UpsertTrace(ctx, span.Trace{TraceID: "t1", Source: span.SourceOTLPTrace, RootSpanID: "a", StartedAt: start}); err != nil {
		t.Fatalf("UpsertTrace: %v", err)
	}
	if err := s.UpsertSpansBatch(ctx, []span.Span{
		{SpanID: "a", TraceID: "t1", Kind: span.KindLLM, StartedAt: start, InputTokens: i64(100), OutputTokens: i64(25)},
	}); err != nil {
		t.Fatalf("UpsertSpansBatch: %v", err)
	}
	d, _, err := s.GetTrace(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if d.Trace.TotalTokens != 125 {
		t.Errorf("total tokens = %d, want 125 (input+output fallback)", d.Trace.TotalTokens)
	}
}
