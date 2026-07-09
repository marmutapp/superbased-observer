package rollup

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestOverviewEnriched_AdminAggregates pins the Phase 1 additive metrics over
// the shared seed (admin scope): token buckets, cache, reliability split,
// tool/model mix, activity grids, and the action error rate.
func TestOverviewEnriched_AdminAggregates(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	got, err := Overview(context.Background(), d, w30, adminScope, fixedNow)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}

	// Token buckets: 5 deduped events × (input 100 / output 50); no cache or
	// reasoning in the fixture.
	if got.Tokens == nil {
		t.Fatal("Tokens is nil")
	}
	if got.Tokens.NetInput != 500 || got.Tokens.Output != 250 ||
		got.Tokens.CacheRead != 0 || got.Tokens.CacheWrite != 0 || got.Tokens.Reasoning != 0 {
		t.Errorf("Tokens = %+v, want input 500 output 250 cache/reasoning 0", *got.Tokens)
	}

	// Cache: no cache activity → zero ratios, input mirrors token buckets.
	if got.Cache == nil || got.Cache.InputTokens != 500 || got.Cache.ReadTokens != 0 ||
		!near(got.Cache.HitRatio, 0) {
		t.Errorf("Cache = %+v, want input 500 read 0 hit 0", got.Cache)
	}

	// Reliability: proxy 0.30 (two api_turns), estimated 0.15 (three jsonl).
	if got.Reliability == nil || !near(got.Reliability.ProxyCostUSD, 0.30) ||
		!near(got.Reliability.EstimatedCostUSD, 0.15) || !near(got.Reliability.ProxyShare, 0.30/0.45) {
		t.Errorf("Reliability = %+v, want proxy 0.30 est 0.15 share 0.667", got.Reliability)
	}

	// Tool mix: claude-code (alice api 0.10 + 3 jsonl 0.15 = 0.25, 600 tok),
	// codex (bob api 0.20, 150 tok via the session-resolved tool).
	tools := toolMap(got.ToolMix)
	if cc := tools["claude-code"]; !near(cc.CostUSD, 0.25) || cc.Tokens != 600 {
		t.Errorf("tool claude-code = %+v, want cost 0.25 tokens 600", cc)
	}
	if cx := tools["codex"]; !near(cx.CostUSD, 0.20) || cx.Tokens != 150 {
		t.Errorf("tool codex = %+v, want cost 0.20 tokens 150", cx)
	}

	// Model mix: claude 0.22 (450 tok), gpt 0.23 (300 tok).
	models := modelMap(got.ModelMix)
	if cl := models["claude"]; !near(cl.CostUSD, 0.22) || cl.Tokens != 450 {
		t.Errorf("model claude = %+v, want cost 0.22 tokens 450", cl)
	}
	if gp := models["gpt"]; !near(gp.CostUSD, 0.23) || gp.Tokens != 300 {
		t.Errorf("model gpt = %+v, want cost 0.23 tokens 300", gp)
	}

	// Activity: actions on 05-20 (1), 05-21 (1), 05-22 (2); all at hour 9.
	days := dayMap(got.ActionsByDay)
	if days["2026-05-22"] != 2 || days["2026-05-20"] != 1 || days["2026-05-21"] != 1 {
		t.Errorf("ActionsByDay = %+v, want 05-20:1 05-21:1 05-22:2", got.ActionsByDay)
	}
	if len(got.HourOfDay) != 1 || got.HourOfDay[0].Hour != 9 || got.HourOfDay[0].Count != 4 {
		t.Errorf("HourOfDay = %+v, want [{9,4}]", got.HourOfDay)
	}

	// Errors: all four actions succeeded; both api_turns are 200.
	if got.Errors == nil || got.Errors.TotalActions != 4 || got.Errors.FailedActions != 0 ||
		!near(got.Errors.ActionErrorRate, 0) || got.Errors.APITurns != 2 || got.Errors.HTTPErrors != 0 {
		t.Errorf("Errors = %+v, want 4 actions 0 failed, 2 turns 0 http-errors", got.Errors)
	}
	if len(got.Errors.ByErrorClass) != 0 {
		t.Errorf("ByErrorClass = %+v, want empty (no error_class in fixture)", got.Errors.ByErrorClass)
	}

	// Latency degrades to nil — the fixture carries no proxy timing.
	if got.Latency != nil {
		t.Errorf("Latency = %+v, want nil (no ttft/total in fixture — honest empty)", got.Latency)
	}

	// Deltas: all fixture activity is in the current window → no prior baseline.
	if got.Deltas == nil || got.Deltas.HasPrior {
		t.Errorf("Deltas = %+v, want HasPrior=false (no prior-window activity)", got.Deltas)
	}
}

// TestOverviewEnriched_LeadScoped confirms the enriched aggregates honor the
// lead scope (alice leads team A = alice + carol; bob's spend is excluded).
func TestOverviewEnriched_LeadScoped(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	got, err := Overview(context.Background(), d, w30, aliceScope, fixedNow)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	// alice api (100/50) + alice jsonl (100/50) + carol jsonl (100/50).
	if got.Tokens == nil || got.Tokens.NetInput != 300 || got.Tokens.Output != 150 {
		t.Errorf("lead Tokens = %+v, want input 300 output 150", got.Tokens)
	}
	// proxy = alice api 0.10; estimated = alice 0.05 + carol 0.07 = 0.12.
	if got.Reliability == nil || !near(got.Reliability.ProxyCostUSD, 0.10) ||
		!near(got.Reliability.EstimatedCostUSD, 0.12) {
		t.Errorf("lead Reliability = %+v, want proxy 0.10 est 0.12", got.Reliability)
	}
	// codex (bob's) must NOT appear in a team-A lead's tool mix.
	if _, ok := toolMap(got.ToolMix)["codex"]; ok {
		t.Errorf("lead tool mix leaked bob's codex spend: %+v", got.ToolMix)
	}
}

// TestOverviewEnriched_LatencyPopulated seeds proxy timing and asserts the
// median/p95 are computed via the count-then-offset percentile path.
func TestOverviewEnriched_LatencyPopulated(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed exec: %v", err)
		}
	}
	add := func(req string, ttft, total int64, ts string) {
		exec(`INSERT INTO api_turns (user_id, session_id, timestamp, provider, model, request_id,
		        input_tokens, output_tokens, cost_usd, http_status, time_to_first_token_ms, total_response_ms,
		        pushed_at, pushed_by_user_id)
		      VALUES ('u-x','s-x',?, 'anthropic','claude',?,100,50,0.10,200,?,?, '2026-05-26T11:00:00Z','u-x')`,
			ts, req, ttft, total)
	}
	add("r1", 100, 1000, "2026-05-20T10:00:00Z")
	add("r2", 200, 2000, "2026-05-21T10:00:00Z")
	add("r3", 300, 3000, "2026-05-22T10:00:00Z")

	got, err := Overview(ctx, d, w30, adminScope, fixedNow)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if got.Latency == nil {
		t.Fatal("Latency is nil, want populated")
	}
	if got.Latency.SampleSize != 3 || got.Latency.MedianTTFTMs != 200 ||
		got.Latency.MedianTotalMs != 2000 || got.Latency.P95TotalMs != 3000 {
		t.Errorf("Latency = %+v, want sample 3 medTTFT 200 medTotal 2000 p95 3000", *got.Latency)
	}
}

// TestOverviewEnriched_ErrorClassAndHTTP seeds an HTTP error + an error_class
// and asserts the proxy error view surfaces them.
func TestOverviewEnriched_ErrorClassAndHTTP(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed exec: %v", err)
		}
	}
	exec(`INSERT INTO api_turns (user_id, session_id, timestamp, provider, model, request_id,
	        input_tokens, output_tokens, cost_usd, http_status, error_class, pushed_at, pushed_by_user_id)
	      VALUES ('u-x','s-x','2026-05-20T10:00:00Z','anthropic','claude','r1',100,50,0.10,529,'overloaded','2026-05-26T11:00:00Z','u-x')`)
	exec(`INSERT INTO api_turns (user_id, session_id, timestamp, provider, model, request_id,
	        input_tokens, output_tokens, cost_usd, http_status, pushed_at, pushed_by_user_id)
	      VALUES ('u-x','s-x','2026-05-21T10:00:00Z','anthropic','claude','r2',100,50,0.10,200,'2026-05-26T11:00:00Z','u-x')`)

	got, err := Overview(ctx, d, w30, adminScope, fixedNow)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if got.Errors == nil || got.Errors.HTTPErrors != 1 {
		t.Errorf("HTTPErrors = %+v, want 1 (the 529)", got.Errors)
	}
	if len(got.Errors.ByErrorClass) != 1 || got.Errors.ByErrorClass[0].Key != "overloaded" ||
		got.Errors.ByErrorClass[0].Count != 1 {
		t.Errorf("ByErrorClass = %+v, want [{overloaded,1}]", got.Errors.ByErrorClass)
	}
}

// TestOverviewEnriched_Deltas seeds prior-window activity and asserts the
// prior-period comparison computes (and HasPrior flips true).
func TestOverviewEnriched_Deltas(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed exec: %v", err)
		}
	}
	// Current-window api_turn (within 30d of fixedNow=2026-05-26).
	exec(`INSERT INTO api_turns (user_id, session_id, timestamp, provider, model, request_id,
	        input_tokens, output_tokens, cost_usd, http_status, pushed_at, pushed_by_user_id)
	      VALUES ('u-x','s-x','2026-05-20T10:00:00Z','anthropic','claude','cur',100,50,0.30,200,'2026-05-26T11:00:00Z','u-x')`)
	// Prior-window api_turn (30–60d before now → window [04-26, 03-27)).
	exec(`INSERT INTO api_turns (user_id, session_id, timestamp, provider, model, request_id,
	        input_tokens, output_tokens, cost_usd, http_status, pushed_at, pushed_by_user_id)
	      VALUES ('u-x','s-x','2026-04-20T10:00:00Z','anthropic','claude','old',100,50,0.10,200,'2026-05-26T11:00:00Z','u-x')`)

	got, err := Overview(ctx, d, w30, adminScope, fixedNow)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if got.Deltas == nil || !got.Deltas.HasPrior {
		t.Fatalf("Deltas = %+v, want HasPrior=true", got.Deltas)
	}
	if !near(got.Deltas.PriorCostUSD, 0.10) {
		t.Errorf("PriorCostUSD = %v, want 0.10", got.Deltas.PriorCostUSD)
	}
	// cost delta = (0.30 - 0.10)/0.10 = +2.0.
	if !near(got.Deltas.CostUSD, 2.0) {
		t.Errorf("Deltas.CostUSD = %v, want 2.0 (+200%%)", got.Deltas.CostUSD)
	}
}

// TestOverviewEnriched_NoSentinelColumns is the privacy guard: the enriched
// union substrate must never read a content-bearing column. It pins the
// CTE-level surface (the one place raw content could leak in) and asserts the
// fully-marshaled enriched Overview carries no raw project path from the seed.
func TestOverviewEnriched_NoSentinelColumns(t *testing.T) {
	// Sentinel content columns (mirror tests/invariant/privacy_test.go's set):
	// none of these may appear in the enriched union substrate.
	forbidden := []string{
		"raw_tool_input", "raw_tool_output", "preceding_reasoning",
		"error_message", "git_remote", "otel_content",
		".target", // actions.target (the raw action target)
	}
	for _, col := range forbidden {
		if strings.Contains(enrichedCTE, col) {
			t.Errorf("enrichedCTE references forbidden content column %q", col)
		}
	}
	// `project_root` (raw) must not be selected — only project_root_hash is
	// content-free. The enriched CTE doesn't carry the project dimension at
	// all, so neither spelling should appear.
	if strings.Contains(enrichedCTE, "project_root") {
		t.Errorf("enrichedCTE references project_root; the enriched substrate carries no project dimension")
	}

	// End-to-end: the marshaled enriched Overview must not leak the seed's raw
	// project path "/repo/" (the v1 fields key on the hash, the enriched ones
	// omit the project dimension).
	d := newDB(t)
	seed(t, d)
	got, err := Overview(context.Background(), d, w30, adminScope, fixedNow)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "/repo/") {
		t.Errorf("enriched Overview JSON leaked a raw project path:\n%s", b)
	}
}

// --- test helpers ---

func toolMap(in []ToolSlice) map[string]ToolSlice {
	m := map[string]ToolSlice{}
	for _, t := range in {
		m[t.Tool] = t
	}
	return m
}

func modelMap(in []ModelSlice) map[string]ModelSlice {
	m := map[string]ModelSlice{}
	for _, x := range in {
		m[x.Model] = x
	}
	return m
}

func dayMap(in []DayCount) map[string]int64 {
	m := map[string]int64{}
	for _, d := range in {
		m[d.Date] = d.Count
	}
	return m
}
