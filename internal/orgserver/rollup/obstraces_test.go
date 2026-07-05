package rollup

import (
	"context"
	"testing"
)

// TestObsTrajectories_ListAndWedge seeds a trace whose LLM span shares a
// request_id with an api_turn, and asserts the list row + the trace detail span
// tree + the proxy-exact enrichment (the wedge).
func TestObsTrajectories_ListAndWedge(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()

	// Trace + root agent span + one llm span (request_id = "rid-1").
	if _, err := d.ExecContext(ctx,
		`INSERT INTO obs_traces (org_id, trace_id, user_email, source, status, started_at, ended_at,
		   project_hash, root_span_id, span_count, total_tokens, cost_usd, pushed_at, pushed_by_user_id)
		 VALUES ('org1','tr-1','alice@x','otlp_trace','ok','2026-05-20T10:00:00Z','2026-05-20T10:00:00.200Z',
		   'ph-a','sp-root',2,1200,0.05,'2026-05-26T06:00:00Z','u-alice')`); err != nil {
		t.Fatalf("seed trace: %v", err)
	}
	ins := func(spanID, parent, kind, name, model, reqID string, in, out, total int64, cost float64) {
		t.Helper()
		if _, err := d.ExecContext(ctx,
			`INSERT INTO obs_spans (org_id, trace_id, span_id, user_email, parent_span_id, kind, name,
			   started_at, ended_at, duration_ms, status, model, provider,
			   input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens,
			   total_tokens, cost_usd, cost_source, request_id, tool_call_id, pushed_at, pushed_by_user_id)
			 VALUES ('org1','tr-1',?,'alice@x',?,?,?,'2026-05-20T10:00:00Z','2026-05-20T10:00:00.200Z',200,'ok',?,'openai',
			   ?,?,0,0,0,?,?,'reported',?, '', '2026-05-26T06:00:00Z','u-alice')`,
			spanID, parent, kind, name, model, in, out, total, cost, reqID); err != nil {
			t.Fatalf("seed span: %v", err)
		}
	}
	ins("sp-root", "", "agent", "run", "", "", 0, 0, 0, 0)
	ins("sp-llm", "sp-root", "llm", "chat", "gpt-4o", "rid-1", 1000, 200, 1200, 0.05)

	// A matching api_turn (the proxy-verified cost the node pushed).
	if _, err := d.ExecContext(ctx,
		`INSERT INTO api_turns (user_id, org_id, request_id, provider, model,
		   input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, cost_usd,
		   pushed_at, pushed_by_user_id)
		 VALUES ('u-alice','org1','rid-1','openai','gpt-4o',950,190,600,80,0.0488,'2026-05-26T06:00:00Z','u-alice')`); err != nil {
		t.Fatalf("seed api_turn: %v", err)
	}

	// List (admin).
	list, err := ObsTrajectories(ctx, d, w30, Scope{Admin: true}, "", 100, fixedNow)
	if err != nil {
		t.Fatalf("ObsTrajectories: %v", err)
	}
	if !list.Configured || len(list.Traces) != 1 {
		t.Fatalf("list = %+v, want 1 configured trace", list)
	}
	row := list.Traces[0]
	if row.TraceID != "tr-1" || row.RootName != "run" || row.SpanCount != 2 || row.DurationMs != 200 {
		t.Errorf("list row = %+v", row)
	}

	// Detail + wedge.
	det, found, err := ObsTraceDetail(ctx, d, "tr-1", Scope{Admin: true}, "", fixedNow)
	if err != nil || !found {
		t.Fatalf("ObsTraceDetail: found=%v err=%v", found, err)
	}
	if len(det.Spans) != 2 {
		t.Fatalf("spans = %d, want 2", len(det.Spans))
	}
	var llm *ObsSpanDetail
	for i := range det.Spans {
		if det.Spans[i].SpanID == "sp-llm" {
			llm = &det.Spans[i]
		}
	}
	if llm == nil {
		t.Fatal("llm span missing")
	}
	if llm.Enrichment == nil || !llm.Enrichment.Found {
		t.Fatalf("wedge enrichment missing on the request_id span: %+v", llm)
	}
	// Proxy-exact values from api_turns, NOT the span-reported ones.
	if llm.Enrichment.CostUSD != 0.0488 || llm.Enrichment.InputTokens != 950 || llm.Enrichment.CacheReadTokens != 600 {
		t.Errorf("wedge = %+v, want proxy-exact 0.0488 / 950 / 600", llm.Enrichment)
	}

	// Unknown trace → 404 (found=false).
	if _, found, _ := ObsTraceDetail(ctx, d, "nope", Scope{Admin: true}, "", fixedNow); found {
		t.Error("unknown trace returned found=true")
	}
}
