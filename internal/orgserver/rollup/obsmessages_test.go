package rollup

import (
	"context"
	"testing"
)

// TestObsTraceContent_AuditedView seeds a trace + span + two content rows (one
// raw, one hash-only) and asserts the viewer returns them with has_raw set
// correctly, and that an unknown trace is found=false (→ 404).
func TestObsTraceContent_AuditedView(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	if _, err := d.ExecContext(ctx,
		`INSERT INTO obs_traces (org_id, trace_id, source, status, started_at, root_span_id,
		   span_count, total_tokens, cost_usd, pushed_at, pushed_by_user_id)
		 VALUES ('org1','tr-1','otlp_trace','ok','2026-05-20T10:00:00Z','sp-llm',1,1200,0.05,'2026-05-26T06:00:00Z','u-alice')`); err != nil {
		t.Fatalf("seed trace: %v", err)
	}
	if _, err := d.ExecContext(ctx,
		`INSERT INTO obs_spans (org_id, trace_id, span_id, kind, name, started_at, duration_ms, status,
		   input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens, total_tokens,
		   cost_usd, pushed_at, pushed_by_user_id)
		 VALUES ('org1','tr-1','sp-llm','llm','chat','2026-05-20T10:00:00Z',200,'ok',0,0,0,0,0,0,0,'2026-05-26T06:00:00Z','u-alice')`); err != nil {
		t.Fatalf("seed span: %v", err)
	}
	insC := func(kind, hash, content string) {
		t.Helper()
		var c any
		if content == "" {
			c = nil
		} else {
			c = content
		}
		if _, err := d.ExecContext(ctx,
			`INSERT INTO obs_content (org_id, content_hash, kind, span_id, trace_id, content, timestamp, pushed_at, pushed_by_user_id)
			 VALUES ('org1',?,?,'sp-llm','tr-1',?,'2026-05-20T10:00:00Z','2026-05-26T06:00:00Z','u-alice')`,
			hash, kind, c); err != nil {
			t.Fatalf("seed content: %v", err)
		}
	}
	insC("prompt", "h-prompt", "what is 2+2?")
	insC("response", "h-resp", "") // hash-only (node didn't share full content)

	res, found, err := ObsTraceContent(ctx, d, "tr-1", Scope{Admin: true}, "")
	if err != nil || !found {
		t.Fatalf("ObsTraceContent: found=%v err=%v", found, err)
	}
	if len(res.Entries) != 2 || !res.AnyRaw {
		t.Fatalf("entries = %+v anyRaw=%v", res.Entries, res.AnyRaw)
	}
	byKind := map[string]ObsContentEntry{}
	for _, e := range res.Entries {
		byKind[e.Kind] = e
	}
	if p := byKind["prompt"]; !p.HasRaw || p.Content != "what is 2+2?" || p.ContentHash != "h-prompt" {
		t.Errorf("prompt entry = %+v", p)
	}
	if r := byKind["response"]; r.HasRaw || r.Content != "" || r.ContentHash != "h-resp" {
		t.Errorf("response entry should be hash-only = %+v", r)
	}

	if _, found, _ := ObsTraceContent(ctx, d, "nope", Scope{Admin: true}, ""); found {
		t.Error("unknown trace returned found=true")
	}
}
