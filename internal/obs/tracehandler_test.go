package obs

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/db"
	obsstore "github.com/marmutapp/superbased-observer/internal/obs/store"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

type fakeSink struct{ got []LLMTurnFacts }

func (f *fakeSink) ReconcileLLMSpan(_ context.Context, facts LLMTurnFacts) error {
	f.got = append(f.got, facts)
	return nil
}

// fakePricer records calls and returns a fixed cost for known models.
type fakePricer struct {
	calls []SpanCostFacts
	known map[string]SpanCost
}

func (p *fakePricer) PriceSpan(_ context.Context, facts SpanCostFacts) (SpanCost, error) {
	p.calls = append(p.calls, facts)
	if c, ok := p.known[facts.Model]; ok {
		return c, nil
	}
	return SpanCost{Found: false}, nil
}

func kv(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

// TestIngest_EndToEnd drives a built OTLP trace export through the ingestor:
// spans + the trace persist into the obs_* tables, and exactly the LLM span
// (with a request_id) reconciles through the TurnSink.
func TestIngest_EndToEnd(t *testing.T) {
	ctx := context.Background()
	conn, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "obs.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	st, err := obsstore.Open(ctx, conn)
	if err != nil {
		t.Fatalf("obsstore.Open: %v", err)
	}
	sink := &fakeSink{}
	ing := NewTraceIngestor(st, sink, nil)

	llm := &tracepb.Span{
		TraceId: []byte{0x01}, SpanId: []byte{0x02}, Name: "chat",
		Attributes: []*commonpb.KeyValue{
			kv("openinference.span.kind", "LLM"),
			kv("llm.model_name", "gpt-4o"),
			kv("llm.provider", "openai"),
			kv("request_id", "req-1"),
		},
	}
	tool := &tracepb.Span{
		TraceId: []byte{0x01}, SpanId: []byte{0x03}, ParentSpanId: []byte{0x02}, Name: "search",
		Attributes: []*commonpb.KeyValue{kv("openinference.span.kind", "TOOL")},
	}
	req := &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{llm, tool}}},
		}},
	}

	if err := ing.Ingest(ctx, req); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	var spanCount, traceCount int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM obs_spans`).Scan(&spanCount); err != nil {
		t.Fatalf("count spans: %v", err)
	}
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM obs_traces`).Scan(&traceCount); err != nil {
		t.Fatalf("count traces: %v", err)
	}
	if spanCount != 2 || traceCount != 1 {
		t.Errorf("persisted %d spans / %d traces, want 2/1", spanCount, traceCount)
	}

	// Only the LLM span (with a request_id) reconciles to api_turns.
	if len(sink.got) != 1 {
		t.Fatalf("TurnSink calls = %d, want 1 (only the llm span)", len(sink.got))
	}
	if sink.got[0].RequestID != "req-1" || sink.got[0].Model != "gpt-4o" || sink.got[0].Provider != "openai" {
		t.Errorf("reconciled facts = %+v", sink.got[0])
	}

	// Idempotent re-ingest: still 2 spans (upsert), sink called again (the
	// host's UpsertTurnByRequestID dedups downstream by fidelity).
	if err := ing.Ingest(ctx, req); err != nil {
		t.Fatalf("re-Ingest: %v", err)
	}
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM obs_spans`).Scan(&spanCount); err != nil {
		t.Fatalf("recount spans: %v", err)
	}
	if spanCount != 2 {
		t.Errorf("after re-ingest span count = %d, want 2 (upsert, no dupes)", spanCount)
	}
}

// TestIngest_SpanPriceFallback confirms Gap B: an unpriced LLM span with tokens
// gets a computed cost through the pricer; a span that already carries a
// reported cost is NOT re-priced; an unknown model stays unpriced.
func TestIngest_SpanPriceFallback(t *testing.T) {
	ctx := context.Background()
	conn, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "obs.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	st, err := obsstore.Open(ctx, conn)
	if err != nil {
		t.Fatalf("obsstore.Open: %v", err)
	}
	pricer := &fakePricer{known: map[string]SpanCost{
		"gpt-4o": {Found: true, TotalUSD: 0.05, InputUSD: 0.03, OutputUSD: 0.02},
	}}
	ing := NewTraceIngestor(st, nil, nil)
	ing.SetSpanPricer(pricer)

	// (a) unpriced known model with tokens → computed.
	priced := &tracepb.Span{
		TraceId: []byte{0x01}, SpanId: []byte{0x02}, Name: "chat",
		Attributes: []*commonpb.KeyValue{
			kv("llm.model_name", "gpt-4o"), kv("llm.provider", "openai"),
			{Key: "llm.token_count.prompt", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 1000}}},
		},
	}
	// (b) reported cost already present → must NOT be re-priced.
	reported := &tracepb.Span{
		TraceId: []byte{0x01}, SpanId: []byte{0x03}, Name: "chat",
		Attributes: []*commonpb.KeyValue{
			kv("llm.model_name", "gpt-4o"),
			{Key: "llm.cost.total", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 0.99}}},
		},
	}
	// (c) unknown model → stays unpriced.
	unknown := &tracepb.Span{
		TraceId: []byte{0x01}, SpanId: []byte{0x04}, Name: "chat",
		Attributes: []*commonpb.KeyValue{
			kv("llm.model_name", "mystery-model"),
			{Key: "llm.token_count.prompt", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 50}}},
		},
	}
	req := &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{priced, reported, unknown}}},
	}}}
	if err := ing.Ingest(ctx, req); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	get := func(id string) (cost *float64, source string) {
		if e := conn.QueryRowContext(ctx, `SELECT cost_usd, COALESCE(cost_source,'') FROM obs_spans WHERE span_id=?`, id).
			Scan(&cost, &source); e != nil {
			t.Fatalf("read %s: %v", id, e)
		}
		return
	}
	if c, src := get("02"); c == nil || *c != 0.05 || src != "computed" {
		t.Errorf("priced span = %v/%q, want 0.05/computed", c, src)
	}
	if c, src := get("03"); c == nil || *c != 0.99 || src != "reported" {
		t.Errorf("reported span = %v/%q, want 0.99/reported (not re-priced)", c, src)
	}
	if c, src := get("04"); c != nil || src != "" {
		t.Errorf("unknown-model span = %v/%q, want unpriced", c, src)
	}
	// The pricer is NOT called for the already-reported span.
	for _, f := range pricer.calls {
		if f.Model == "gpt-4o" && f.InputTokens == nil {
			t.Errorf("pricer called for the reported span (should short-circuit): %+v", f)
		}
	}
}
