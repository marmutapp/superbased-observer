package rollup

import (
	"context"
	"testing"
)

// TestObsAnalytics_EmptyIsNotConfigured pins the honest empty state.
func TestObsAnalytics_EmptyIsNotConfigured(t *testing.T) {
	d := newDB(t)
	got, err := ObsAnalytics(context.Background(), d, w30, fixedNow)
	if err != nil {
		t.Fatalf("ObsAnalytics: %v", err)
	}
	if got.Configured {
		t.Error("Configured = true on an empty obs_summaries, want false")
	}
}

// TestObsAnalytics_Aggregates seeds obs_summaries and asserts the window
// totals, the by-day trend, the model/project/source leaderboards, error rate,
// avg duration, and out-of-window exclusion.
func TestObsAnalytics_Aggregates(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	ins := func(email, day, model, project, source string, traces, spans, in, out, total, errTraces, dSum, dCount int64, cost float64) {
		t.Helper()
		if _, err := d.ExecContext(ctx,
			`INSERT INTO obs_summaries
			   (org_id, user_email, day, model, provider, project_hash, source,
			    traces, spans, input_tokens, output_tokens, cache_read_tokens,
			    cache_write_tokens, reasoning_tokens, total_tokens, cost_usd,
			    error_traces, duration_ms_sum, duration_ms_count,
			    pushed_at, pushed_by_user_id)
			 VALUES ('org1',?,?,?,'',?,?,?,?,?,?,0,0,0,?,?,?,?,?,'2026-05-26T06:00:00Z','u-alice')`,
			email, day, model, project, source, traces, spans, in, out, total, cost, errTraces, dSum, dCount); err != nil {
			t.Fatalf("seed obs_summary: %v", err)
		}
	}
	// In-window (fixedNow = 2026-05-26 → sinceDay 2026-04-26):
	ins("alice@x", "2026-05-20", "gpt-4o", "ph-a", "otlp_trace", 5, 12, 1000, 200, 1200, 1, 500, 5, 0.10)
	ins("alice@x", "2026-05-20", "claude-opus-4-8", "ph-a", "otlp_trace", 3, 6, 600, 100, 700, 0, 300, 3, 0.06)
	ins("bob@x", "2026-05-21", "gpt-4o", "ph-b", "sdk_otlp", 2, 4, 400, 80, 480, 1, 100, 2, 0.04)
	// Out-of-window — must be excluded.
	ins("alice@x", "2026-04-10", "gpt-4o", "ph-a", "otlp_trace", 99, 99, 9999, 9999, 9999, 99, 9999, 99, 99.0)

	got, err := ObsAnalytics(ctx, d, w30, fixedNow)
	if err != nil {
		t.Fatalf("ObsAnalytics: %v", err)
	}
	if !got.Configured {
		t.Fatal("Configured = false, want true")
	}
	if got.TotalTraces != 10 || got.TotalSpans != 22 || got.TotalTokens != 2380 {
		t.Errorf("totals traces/spans/tokens = %d/%d/%d, want 10/22/2380", got.TotalTraces, got.TotalSpans, got.TotalTokens)
	}
	if !near(got.TotalCostUSD, 0.20) {
		t.Errorf("cost = %v, want 0.20", got.TotalCostUSD)
	}
	if got.ErrorTraces != 2 || !near(got.ErrorRate, 0.2) {
		t.Errorf("errors = %d / rate %v, want 2 / 0.2", got.ErrorTraces, got.ErrorRate)
	}
	// Duration: (500+300+100)/(5+3+2) = 900/10 = 90ms.
	if !near(got.AvgDurationMs, 90) {
		t.Errorf("avg duration = %v ms, want 90", got.AvgDurationMs)
	}
	// By-day trend ascending.
	if len(got.ByDay) != 2 || got.ByDay[0].Date != "2026-05-20" || got.ByDay[0].Traces != 8 {
		t.Errorf("ByDay = %+v, want [2026-05-20 traces=8, 2026-05-21]", got.ByDay)
	}
	// By-model leaderboard: gpt-4o leads on tokens (1200+480=1680 > 700).
	if len(got.ByModel) != 2 || got.ByModel[0].Key != "gpt-4o" || got.ByModel[0].Traces != 7 {
		t.Errorf("ByModel[0] = %+v, want gpt-4o traces=7", got.ByModel)
	}
	if len(got.ByProject) != 2 || len(got.BySource) != 2 {
		t.Errorf("byProject=%d bySource=%d, want 2/2", len(got.ByProject), len(got.BySource))
	}
	// No T2 spans seeded → latency depth is honestly not configured.
	if got.LatencyConfigured {
		t.Error("LatencyConfigured = true with no obs_spans shared")
	}
}

// TestObsAnalytics_LatencyDepth seeds T2 obs_spans and asserts the OP5 latency
// percentiles + per-kind + error-cause breakdown.
func TestObsAnalytics_LatencyDepth(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	ins := func(spanID, kind, name, status string, dur int64) {
		t.Helper()
		if _, err := d.ExecContext(ctx,
			`INSERT INTO obs_spans (org_id, trace_id, span_id, kind, name, started_at, duration_ms, status,
			   input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens, total_tokens,
			   cost_usd, pushed_at, pushed_by_user_id)
			 VALUES ('org1','tr-1',?,?,?,'2026-05-20T10:00:00Z',?,?,0,0,0,0,0,0,0,'2026-05-26T06:00:00Z','u-alice')`,
			spanID, kind, name, dur, status); err != nil {
			t.Fatalf("seed span: %v", err)
		}
	}
	for i, dur := range []int64{10, 20, 30, 40, 100} {
		ins("llm-"+string(rune('a'+i)), "llm", "chat", "ok", dur)
	}
	ins("tool-1", "tool", "get_weather", "error", 5)

	got, err := ObsAnalytics(ctx, d, w30, fixedNow)
	if err != nil {
		t.Fatalf("ObsAnalytics: %v", err)
	}
	if !got.LatencyConfigured {
		t.Fatal("LatencyConfigured = false despite spans")
	}
	// Overall over all 6 spans [5,10,20,30,40,100]: P50 idx int(0.5*5)=2 → 20; P95 idx int(0.95*5)=4 → 40.
	if got.LatencyP50Ms != 20 || got.LatencyP95Ms != 40 {
		t.Errorf("p50/p95 = %d/%d, want 20/40", got.LatencyP50Ms, got.LatencyP95Ms)
	}
	var llmKind *ObsKindLatency
	for i := range got.ByKind {
		if got.ByKind[i].Kind == "llm" {
			llmKind = &got.ByKind[i]
		}
	}
	// llm-only [10,20,30,40,100]: P50 idx int(0.5*4)=2 → 30.
	if llmKind == nil || llmKind.Spans != 5 || llmKind.P50Ms != 30 {
		t.Errorf("llm kind = %+v, want 5 spans / P50 30", llmKind)
	}
	if len(got.ErrorCauses) != 1 || got.ErrorCauses[0].Key != "get_weather" || got.ErrorCauses[0].Traces != 1 {
		t.Errorf("error causes = %+v, want get_weather:1", got.ErrorCauses)
	}
}
