package cost

import (
	"context"
	"database/sql"
	"sync/atomic"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// DefaultBlendedInputRate is the fallback $/1M used when the install
// has no usable api_turns history (no proxy traffic yet, or all the
// observed models lack pricing entries). Set to claude-sonnet-4's
// input rate as a reasonable middle-of-the-road default — Anthropic
// pricing as of 2026-04.
const DefaultBlendedInputRate = 3.0

// Engine computes costs from tokens using a Pricing Table plus the spec §24
// reliability matrix. The table is held behind atomic.Pointer so the
// dashboard's Settings page can hot-reload pricing edits without
// restarting the daemon — in-flight Lookup callers keep their snapshot
// of the old table, fresh callers see the new one.
type Engine struct {
	table atomic.Pointer[Table]
}

// NewEngine returns an engine seeded with baked-in defaults + user pricing
// overrides from cfg. Safe to call with a zero config (no overrides).
func NewEngine(cfg config.IntelligenceConfig) *Engine {
	e := &Engine{}
	e.Reload(cfg)
	return e
}

// Reload swaps the engine's pricing table for one built from cfg. Used
// by the Settings page after a PUT /api/config/pricing save and by tests.
// Reads against the engine remain valid throughout — atomic.Pointer
// guarantees readers see either the old or new table, never a torn state.
func (e *Engine) Reload(cfg config.IntelligenceConfig) {
	t := NewTable()
	if cfg.Pricing.Models != nil {
		overrides := map[string]Pricing{}
		for id, mp := range cfg.Pricing.Models {
			overrides[id] = Pricing{
				Input:                      mp.Input,
				Output:                     mp.Output,
				CacheRead:                  mp.CacheRead,
				CacheCreation:              mp.CacheCreation,
				CacheCreation1h:            mp.CacheCreation1h,
				LongContextThreshold:       mp.LongContextThreshold,
				LongContextInput:           mp.LongContextInput,
				LongContextOutput:          mp.LongContextOutput,
				LongContextCacheRead:       mp.LongContextCacheRead,
				LongContextCacheCreation:   mp.LongContextCacheCreation,
				LongContextCacheCreation1h: mp.LongContextCacheCreation1h,
				FastMultiplier:             mp.FastMultiplier,
			}
		}
		t.Merge(overrides)
	}
	e.table.Store(t)
}

// Table returns the active pricing table snapshot. Safe to call from
// any goroutine; the returned pointer remains valid for reads even if
// a concurrent Reload swaps the engine onto a new table.
func (e *Engine) Table() *Table {
	if e == nil {
		return nil
	}
	return e.table.Load()
}

// Lookup is a convenience wrapper that snapshots the active table and
// runs a Lookup against it. Equivalent to e.Table().Lookup(model).
func (e *Engine) Lookup(model string) (Pricing, bool) {
	t := e.Table()
	if t == nil {
		return Pricing{}, false
	}
	return t.Lookup(model)
}

// LookupWithSource mirrors Lookup but propagates the PricingSource so
// callers can flag fallback-rate rows.
func (e *Engine) LookupWithSource(model string) (Pricing, PricingSource, bool) {
	t := e.Table()
	if t == nil {
		return Pricing{}, PricingSourceMiss, false
	}
	return t.LookupWithSource(model)
}

// TokenBundle is the per-turn token shape the engine prices. Zero fields
// contribute nothing to the total. Tokens are absolute counts (not per-million).
//
// CacheCreation is the total cache-write tokens for the turn; CacheCreation1h
// is the subset of those tokens that landed in the 1h ephemeral tier (the
// remainder is implicitly 5m-tier). This split lets Compute apply the 2×
// premium Anthropic charges for 1h-tier writes. Pre-tier-aware data has
// CacheCreation1h == 0 → priced entirely at the 5m rate, matching prior
// behavior.
type TokenBundle struct {
	// Input MUST be NET non-cached input tokens — the count of
	// fresh prompt tokens the model paid full input-rate to process,
	// EXCLUDING anything that hit prefix cache (which lives in
	// CacheRead and bills at the discounted cache_read rate).
	//
	// Anthropic-shape providers report this natively (input_tokens
	// is already net). OpenAI-shape providers (Codex JSONL, OpenAI
	// proxy responses, GPT-* via any path) report `prompt_tokens` /
	// `input_tokens` as the TOTAL prompt INCLUDING cached, with
	// `cached_tokens` as a subset — those adapters MUST pre-subtract
	// at emit time (see internal/adapter/codex/adapter.go and
	// internal/proxy/{provider,streaming}.go). Without the
	// subtraction, the cached portion would be billed at BOTH the
	// full input rate AND the cache_read rate — ~3.4× overbilling on
	// short cached sessions (audit 2026-05-24).
	Input           int64 `json:"input"`
	Output          int64 `json:"output"`
	CacheRead       int64 `json:"cache_read"`
	CacheCreation   int64 `json:"cache_creation"`
	CacheCreation1h int64 `json:"cache_creation_1h"`
	// Reasoning is the model's internal "thinking" token count
	// (OpenAI o1/o3/o4/gpt-5 family `reasoning_output_tokens`;
	// Anthropic extended-thinking `output_tokens` portion when
	// thinking is enabled). Billed at the output rate by both
	// providers, but tracked as a separate column so cost
	// breakdowns can attribute the reasoning portion of total
	// model spend. Add via b.Reasoning × p.Output / 1e6 in
	// Compute().
	Reasoning int64 `json:"reasoning"`
	// WebSearchRequests is the count of server-side web_search invocations
	// billed under Anthropic's $10/1000 flat per-request fee. Charged on
	// top of token costs. Zero for non-Anthropic providers.
	WebSearchRequests int64 `json:"web_search_requests"`
	// Fast indicates the turn was served in the provider's low-latency
	// "fast" tier (e.g. Anthropic Opus 4.8 with speed:"fast" on the
	// Messages API). When true AND Pricing.FastMultiplier > 0, Compute
	// scales the AI token cost (input/output/cache/reasoning) by
	// FastMultiplier; the flat WebSearchPerRequest fee is unaffected.
	// Captured by the proxy when the outbound request body carries
	// `"speed":"fast"`; defaults false everywhere else.
	Fast bool `json:"fast,omitempty"`
}

// Add accumulates b's fields into t. Used by summary aggregation.
func (t *TokenBundle) Add(b TokenBundle) {
	t.Input += b.Input
	t.Output += b.Output
	t.CacheRead += b.CacheRead
	t.CacheCreation += b.CacheCreation
	t.CacheCreation1h += b.CacheCreation1h
	t.Reasoning += b.Reasoning
	t.WebSearchRequests += b.WebSearchRequests
}

// Compute returns (cost_usd, ok). ok is false only when the model is
// unrecognized; zero-token bundles against known models return (0, true).
//
// The formula is straight pricing × tokens / 1e6; there's no cache discount
// because cache-read tokens live in their own column with their own rate.
func (e *Engine) Compute(model string, b TokenBundle) (float64, bool) {
	p, ok := e.Lookup(model)
	if !ok {
		return 0, false
	}
	return Compute(p, b), true
}

// ComputeBreakdown is the AI/tool/total split variant of Compute.
// Returns a zero Breakdown when the model is unrecognized
// (matching Compute's "unknown model → $0" semantics).
func (e *Engine) ComputeBreakdown(model string, b TokenBundle) (Breakdown, bool) {
	p, ok := e.Lookup(model)
	if !ok {
		return Breakdown{}, false
	}
	return ComputeBreakdown(p, b), true
}

// BlendedInputRate returns the user's effective $/1M-prompt-tokens
// rate, computed by weighting each observed model's input rate by
// the prompt-token volume it consumed in the last `days` days.
//
// "Prompt tokens" here means input + cache_read (the portion of every
// request that bills at input-class rates; output is excluded because
// the metrics this rate is used for — wasted-token cost on Discovery —
// are about prompt waste). When a model has no pricing entry, its
// volume contributes to the denominator but not the numerator, which
// produces a slight under-estimate that's preferred over silently
// dropping unknown models from the average.
//
// Returns DefaultBlendedInputRate when the install has no usable
// proxy data — e.g. fresh install where the proxy isn't engaged yet.
func (e *Engine) BlendedInputRate(ctx context.Context, db *sql.DB, days int) (float64, error) {
	if e == nil || e.Table() == nil || db == nil {
		return DefaultBlendedInputRate, nil
	}
	if days <= 0 {
		days = 30
	}
	since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	rows, err := db.QueryContext(ctx,
		`SELECT model,
		        COALESCE(SUM(input_tokens), 0) + COALESCE(SUM(cache_read_tokens), 0) AS prompt_tokens
		 FROM api_turns
		 WHERE timestamp >= ?
		 GROUP BY model`,
		since.Format(time.RFC3339Nano))
	if err != nil {
		return DefaultBlendedInputRate, err
	}
	defer rows.Close()
	var weightedRate float64
	var totalTokens int64
	for rows.Next() {
		var model string
		var promptTok int64
		if err := rows.Scan(&model, &promptTok); err != nil {
			return DefaultBlendedInputRate, err
		}
		if promptTok <= 0 {
			continue
		}
		totalTokens += promptTok
		p, ok := e.Lookup(model)
		if !ok || p.Input <= 0 {
			continue
		}
		weightedRate += float64(promptTok) * p.Input
	}
	if totalTokens == 0 {
		return DefaultBlendedInputRate, nil
	}
	return weightedRate / float64(totalTokens), nil
}

// Compute is the rate × tokens formula, factored out so pricing-table lookup
// and the math are independently testable.
//
// The cache-creation total is split into 5m and 1h tiers: the 1h subset is
// b.CacheCreation1h (clamped to [0, b.CacheCreation]), and the rest is 5m.
// Each tier is billed at its own rate. Pre-tier-aware data has
// CacheCreation1h == 0 → entirely 5m-tier (correct: 1h-tier didn't ship).
//
// Long-context dispatch: when p.LongContextThreshold > 0 and the bundle's
// prompt window (Input + CacheRead + CacheCreation) exceeds that
// threshold, each rate is replaced by its LongContext counterpart before
// the rate × tokens math runs. The threshold check is per-bundle, so
// callers MUST pass a per-turn bundle — passing aggregated tokens across
// many turns would false-positive the LC tier.
func Compute(p Pricing, b TokenBundle) float64 {
	return ComputeBreakdown(p, b).Total
}

// Breakdown splits a TokenBundle's cost into the AI-model portion
// (per-token input/output/cache/reasoning charges) and the tool
// portion (per-call fees for server-side tools like web_search).
// Total is AI + Tool. Used by the dashboard to surface "API cost vs
// tool cost vs total" separately rather than collapsing them into a
// single Cost column — different optimization levers, different
// budget lines.
type Breakdown struct {
	AICost   float64 `json:"ai_cost_usd"`
	ToolCost float64 `json:"tool_cost_usd"`
	Total    float64 `json:"total_cost_usd"`

	// Per-bucket components — the AICost split by the four token-billing
	// buckets we track. Reasoning tokens are billed at the model's output
	// rate, so their cost is folded into OutputCost (matching how
	// AICost itself aggregates). Tool fees are NOT in any bucket — they
	// stay on ToolCost. Invariant: InputCost + OutputCost + CacheReadCost
	// + CacheCreationCost == AICost. Added in v1.6.13 to feed the
	// session-detail Models Used panel's cost-by-bucket stacked bar.
	InputCost         float64 `json:"input_cost_usd,omitempty"`
	OutputCost        float64 `json:"output_cost_usd,omitempty"`
	CacheReadCost     float64 `json:"cache_read_cost_usd,omitempty"`
	CacheCreationCost float64 `json:"cache_creation_cost_usd,omitempty"`
}

// ComputeBreakdown is the canonical pricing-math function. Compute()
// is a thin wrapper that discards the split for callers that only
// need the total.
func ComputeBreakdown(p Pricing, b TokenBundle) Breakdown {
	rates := lcAdjusted(p, b)
	inputCost := float64(b.Input) * rates.Input / 1_000_000
	outputCost := float64(b.Output) * rates.Output / 1_000_000
	cacheReadCost := float64(b.CacheRead) * rates.CacheRead / 1_000_000
	cc1h := b.CacheCreation1h
	if cc1h < 0 {
		cc1h = 0
	}
	if cc1h > b.CacheCreation {
		cc1h = b.CacheCreation
	}
	cc5m := b.CacheCreation - cc1h
	cacheCreationCost := float64(cc5m)*rates.CacheCreation/1_000_000 +
		float64(cc1h)*rates.CacheCreation1h/1_000_000
	// Reasoning tokens (OpenAI o-series/gpt-5 reasoning_output_tokens,
	// Anthropic extended-thinking output portion) are billed at the
	// model's output rate. LC-adjusted along with regular output
	// (reasoning is part of the model's output stream). Folded into
	// OutputCost so the four bucket components still sum to AICost.
	outputCost += float64(b.Reasoning) * rates.Output / 1_000_000

	// Fast-mode premium: scale every per-token bucket by FastMultiplier
	// when the turn was served fast AND the model has a fast tier. A
	// uniform multiplier across input/output/cache (Anthropic Opus 4.8's
	// 2× across every dimension) post-multiplies cleanly here without
	// disturbing the LC dispatch — lcAdjusted already swapped in the
	// LC rates above, and 2× × LC = LC × 2 either way. The four bucket
	// components still sum to AICost (each bucket scaled identically).
	// Web search fees stay flat (set in `tool` below — inference
	// premium, not server-tool premium).
	if b.Fast && p.FastMultiplier > 0 {
		inputCost *= p.FastMultiplier
		outputCost *= p.FastMultiplier
		cacheReadCost *= p.FastMultiplier
		cacheCreationCost *= p.FastMultiplier
	}

	ai := inputCost + outputCost + cacheReadCost + cacheCreationCost

	// Tool fees: flat per-call, not per-token, not subject to LC tier,
	// not subject to FastMultiplier (Anthropic's fast mode is an
	// inference throughput premium, not a server-tool-fee premium).
	// Today only server-side web_search is modeled; future tool fees
	// (MCP-tool charges, code-execution sandboxes, etc.) accumulate
	// here so the AI vs Tool split stays meaningful.
	tool := float64(b.WebSearchRequests) * p.WebSearchPerRequest

	return Breakdown{
		AICost:            ai,
		ToolCost:          tool,
		Total:             ai + tool,
		InputCost:         inputCost,
		OutputCost:        outputCost,
		CacheReadCost:     cacheReadCost,
		CacheCreationCost: cacheCreationCost,
	}
}

// lcAdjusted returns p with each rate swapped for its LongContext
// counterpart when the bundle's prompt window exceeds the threshold. A
// zero LongContext* field means "no override at the LC tier" — the
// standard rate carries through. When the threshold is unset (zero) or
// the prompt is below it, p is returned unchanged.
func lcAdjusted(p Pricing, b TokenBundle) Pricing {
	if p.LongContextThreshold <= 0 {
		return p
	}
	prompt := b.Input + b.CacheRead + b.CacheCreation
	if prompt <= p.LongContextThreshold {
		return p
	}
	if p.LongContextInput > 0 {
		p.Input = p.LongContextInput
	}
	if p.LongContextOutput > 0 {
		p.Output = p.LongContextOutput
	}
	if p.LongContextCacheRead > 0 {
		p.CacheRead = p.LongContextCacheRead
	}
	if p.LongContextCacheCreation > 0 {
		p.CacheCreation = p.LongContextCacheCreation
	}
	if p.LongContextCacheCreation1h > 0 {
		p.CacheCreation1h = p.LongContextCacheCreation1h
	}
	return p
}
