package store

import (
	"context"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/obs/span"
	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// TestAggregateForOrg seeds two traces (one ok, one error) across two models
// and asserts the T1 aggregate cube: per-(day,model,provider,project,source)
// token/cost/trace/error sums, with project_root hashed (never raw) and the
// duration sum/count folded in.
func TestAggregateForOrg(t *testing.T) {
	ctx := context.Background()
	s := newDB(t)
	start := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	end := start.Add(150 * time.Millisecond)

	// Trace 1: ok, gpt-4o, /proj/a, one llm span.
	if err := s.UpsertTrace(ctx, span.Trace{
		TraceID: "t1", Source: span.SourceOTLPTrace, Status: span.StatusOK,
		ProjectRoot: "/proj/a", RootSpanID: "s1", StartedAt: start, EndedAt: end,
	}); err != nil {
		t.Fatalf("UpsertTrace t1: %v", err)
	}
	if err := s.UpsertSpansBatch(ctx, []span.Span{{
		SpanID: "s1", TraceID: "t1", Kind: span.KindLLM, Name: "chat",
		Model: "gpt-4o", Provider: "openai", Status: span.StatusOK,
		StartedAt: start, EndedAt: end, Source: span.SourceOTLPTrace,
		InputTokens: i64(1000), OutputTokens: i64(200), TotalTokens: i64(1200), CostUSD: f64(0.05),
	}}); err != nil {
		t.Fatalf("UpsertSpansBatch t1: %v", err)
	}

	// Trace 2: error, claude, /proj/a, one llm span.
	if err := s.UpsertTrace(ctx, span.Trace{
		TraceID: "t2", Source: span.SourceOTLPTrace, Status: span.StatusError,
		ProjectRoot: "/proj/a", RootSpanID: "s2", StartedAt: start, EndedAt: end,
	}); err != nil {
		t.Fatalf("UpsertTrace t2: %v", err)
	}
	if err := s.UpsertSpansBatch(ctx, []span.Span{{
		SpanID: "s2", TraceID: "t2", Kind: span.KindLLM, Name: "chat",
		Model: "claude-opus-4-8", Provider: "anthropic", Status: span.StatusError,
		StartedAt: start, EndedAt: end, Source: span.SourceOTLPTrace,
		InputTokens: i64(500), OutputTokens: i64(100), TotalTokens: i64(600), CostUSD: f64(0.03),
	}}); err != nil {
		t.Fatalf("UpsertSpansBatch t2: %v", err)
	}

	rows, err := s.AggregateForOrg(ctx, 7)
	if err != nil {
		t.Fatalf("AggregateForOrg: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d cube rows, want 2 (one per model)", len(rows))
	}
	byModel := map[string]int{}
	for i, r := range rows {
		byModel[r.Model] = i
		if r.Day != "2026-06-29" {
			t.Errorf("row %d day = %q", i, r.Day)
		}
		if r.ProjectHash == "" || r.ProjectHash == "/proj/a" {
			t.Errorf("project must be hashed, got %q", r.ProjectHash)
		}
		if r.DurationMsCount != 1 || r.DurationMsSum < 140 || r.DurationMsSum > 160 {
			t.Errorf("row %d duration sum/count = %d/%d, want ~150/1", i, r.DurationMsSum, r.DurationMsCount)
		}
	}
	gpt := rows[byModel["gpt-4o"]]
	if gpt.Traces != 1 || gpt.InputTokens != 1000 || gpt.CostUSD != 0.05 || gpt.ErrorTraces != 0 {
		t.Errorf("gpt-4o row = %+v", gpt)
	}
	claude := rows[byModel["claude-opus-4-8"]]
	if claude.Traces != 1 || claude.ErrorTraces != 1 || claude.CostUSD != 0.03 {
		t.Errorf("claude row = %+v (want 1 error trace)", claude)
	}
}

// TestSpansForOrg asserts the T2 structure export returns trace + span rows
// (hashes only) within the window with the content-free request_id intact.
func TestSpansForOrg(t *testing.T) {
	ctx := context.Background()
	s := newDB(t)
	start := time.Now().UTC().Add(-time.Hour)
	end := start.Add(50 * time.Millisecond)
	if err := s.UpsertTrace(ctx, span.Trace{
		TraceID: "t1", Source: span.SourceOTLPTrace, Status: span.StatusOK,
		ProjectRoot: "/proj/a", RootSpanID: "s1", StartedAt: start, EndedAt: end,
	}); err != nil {
		t.Fatalf("UpsertTrace: %v", err)
	}
	if err := s.UpsertSpansBatch(ctx, []span.Span{{
		SpanID: "s1", TraceID: "t1", Kind: span.KindLLM, Name: "chat", Model: "gpt-4o",
		StartedAt: start, EndedAt: end, Source: span.SourceOTLPTrace,
		RequestID: "chatcmpl-xyz", InputTokens: i64(10), CostUSD: f64(0.01),
	}}); err != nil {
		t.Fatalf("UpsertSpansBatch: %v", err)
	}
	batch, err := s.SpansForOrg(ctx, orgcontract.ObsCursor{}, 100)
	if err != nil {
		t.Fatalf("SpansForOrg: %v", err)
	}
	if len(batch.Traces) != 1 || batch.Traces[0].ProjectHash == "" || batch.Traces[0].SpanCount != 1 {
		t.Fatalf("traces = %+v", batch.Traces)
	}
	if len(batch.Spans) != 1 || batch.Spans[0].RequestID != "chatcmpl-xyz" || batch.Spans[0].Name != "chat" {
		t.Fatalf("spans = %+v", batch.Spans)
	}
	if batch.Spans[0].DurationMs < 40 || batch.Spans[0].DurationMs > 70 {
		t.Errorf("span duration = %d ms, want ~50", batch.Spans[0].DurationMs)
	}
}

// TestEndUserSpendForOrg confirms the T5 provider groups spend by
// (day, end-user) through obs_traces.user, filters by the window, isolates
// end-users, and excludes unattributed (empty-user) traces.
func TestEndUserSpendForOrg(t *testing.T) {
	ctx := context.Background()
	s := newDB(t)
	now := time.Now().UTC()
	recent := now.Add(-1 * time.Hour)
	old := now.Add(-40 * 24 * time.Hour) // outside a 7-day window

	// alice: recent $0.30 / 100 tokens + an OLD span (excluded by window).
	if err := s.UpsertTrace(ctx, span.Trace{TraceID: "ta1", User: "alice", Source: span.SourceOTLPTrace, StartedAt: recent}); err != nil {
		t.Fatalf("UpsertTrace ta1: %v", err)
	}
	if err := s.UpsertTrace(ctx, span.Trace{TraceID: "ta2", User: "alice", Source: span.SourceOTLPTrace, StartedAt: old}); err != nil {
		t.Fatalf("UpsertTrace ta2: %v", err)
	}
	// bob: recent $0.20 / 50 tokens.
	if err := s.UpsertTrace(ctx, span.Trace{TraceID: "tb1", User: "bob", Source: span.SourceOTLPTrace, StartedAt: recent}); err != nil {
		t.Fatalf("UpsertTrace tb1: %v", err)
	}
	// anonymous (no user): recent $9.99 — must never appear.
	if err := s.UpsertTrace(ctx, span.Trace{TraceID: "tx1", User: "", Source: span.SourceOTLPTrace, StartedAt: recent}); err != nil {
		t.Fatalf("UpsertTrace tx1: %v", err)
	}
	if err := s.UpsertSpansBatch(ctx, []span.Span{
		{SpanID: "sa1", TraceID: "ta1", Kind: span.KindLLM, Name: "chat", StartedAt: recent, CostUSD: f64(0.30), TotalTokens: i64(100)},
		{SpanID: "sa2", TraceID: "ta2", Kind: span.KindLLM, Name: "chat", StartedAt: old, CostUSD: f64(0.50), TotalTokens: i64(999)},
		{SpanID: "sb1", TraceID: "tb1", Kind: span.KindLLM, Name: "chat", StartedAt: recent, CostUSD: f64(0.20), TotalTokens: i64(50)},
		{SpanID: "sx1", TraceID: "tx1", Kind: span.KindLLM, Name: "chat", StartedAt: recent, CostUSD: f64(9.99), TotalTokens: i64(1)},
	}); err != nil {
		t.Fatalf("UpsertSpansBatch: %v", err)
	}

	rows, err := s.EndUserSpendForOrg(ctx, 7)
	if err != nil {
		t.Fatalf("EndUserSpendForOrg: %v", err)
	}
	byUser := map[string]orgcontract.ObsEndUserSpendRow{}
	for _, r := range rows {
		if r.EndUser == "" {
			t.Errorf("unattributed (empty-user) row leaked: %+v", r)
		}
		byUser[r.EndUser] = r
	}
	if len(byUser) != 2 {
		t.Fatalf("distinct end-users = %d, want 2 (alice+bob, anon+old excluded): %+v", len(byUser), rows)
	}
	if a := byUser["alice"]; !approxEq(a.CostUSD, 0.30) || a.TotalTokens != 100 || a.Traces != 1 {
		t.Errorf("alice = %+v, want cost 0.30 / tokens 100 / traces 1", a)
	}
	if b := byUser["bob"]; !approxEq(b.CostUSD, 0.20) || b.TotalTokens != 50 || b.Traces != 1 {
		t.Errorf("bob = %+v, want cost 0.20 / tokens 50 / traces 1", b)
	}
}
