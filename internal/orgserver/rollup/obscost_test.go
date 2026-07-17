package rollup

import (
	"context"
	"testing"
)

// TestObsCost_Attribution seeds obs_summaries across two developers / projects /
// models and asserts the cost attribution buckets + shares.
func TestObsCost_Attribution(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	ins := func(email, project, model string, cost float64, tokens, traces int64) {
		t.Helper()
		if _, err := d.ExecContext(ctx,
			`INSERT INTO obs_summaries (org_id, user_email, day, model, provider, project_hash, source,
			   traces, spans, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			   reasoning_tokens, total_tokens, cost_usd, error_traces, duration_ms_sum, duration_ms_count,
			   pushed_at, pushed_by_user_id)
			 VALUES ('org1',?,'2026-05-20',?,'',?,'otlp_trace',?,0,0,0,0,0,0,?,?,0,0,0,'2026-05-26T06:00:00Z','u-alice')`,
			email, model, project, traces, tokens, cost); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	ins("alice@x", "ph-a", "gpt-4o", 0.75, 1000, 5)
	ins("bob@x", "ph-b", "claude-opus-4-8", 0.25, 400, 2)

	res, err := ObsCost(ctx, d, w30, fixedNow)
	if err != nil {
		t.Fatalf("ObsCost: %v", err)
	}
	if !res.Configured || !near(res.TotalCostUSD, 1.0) {
		t.Fatalf("total = %v configured=%v, want 1.0", res.TotalCostUSD, res.Configured)
	}
	if len(res.ByDeveloper) != 2 || res.ByDeveloper[0].Key != "alice@x" || !near(res.ByDeveloper[0].CostShare, 0.75) {
		t.Errorf("by developer = %+v, want alice leads at 0.75 share", res.ByDeveloper)
	}
	if res.ByDeveloper[0].Label != "alice@x" {
		t.Errorf("developer label = %q", res.ByDeveloper[0].Label)
	}
	if len(res.ByModel) != 2 || res.ByModel[0].Key != "gpt-4o" {
		t.Errorf("by model = %+v, want gpt-4o leads", res.ByModel)
	}
	if len(res.ByProject) != 2 {
		t.Errorf("by project = %d, want 2", len(res.ByProject))
	}
}
