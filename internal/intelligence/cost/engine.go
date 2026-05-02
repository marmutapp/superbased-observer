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
	Input           int64 `json:"input"`
	Output          int64 `json:"output"`
	CacheRead       int64 `json:"cache_read"`
	CacheCreation   int64 `json:"cache_creation"`
	CacheCreation1h int64 `json:"cache_creation_1h"`
}

// Add accumulates b's fields into t. Used by summary aggregation.
func (t *TokenBundle) Add(b TokenBundle) {
	t.Input += b.Input
	t.Output += b.Output
	t.CacheRead += b.CacheRead
	t.CacheCreation += b.CacheCreation
	t.CacheCreation1h += b.CacheCreation1h
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
	rates := lcAdjusted(p, b)
	total := 0.0
	total += float64(b.Input) * rates.Input / 1_000_000
	total += float64(b.Output) * rates.Output / 1_000_000
	total += float64(b.CacheRead) * rates.CacheRead / 1_000_000
	cc1h := b.CacheCreation1h
	if cc1h < 0 {
		cc1h = 0
	}
	if cc1h > b.CacheCreation {
		cc1h = b.CacheCreation
	}
	cc5m := b.CacheCreation - cc1h
	total += float64(cc5m) * rates.CacheCreation / 1_000_000
	total += float64(cc1h) * rates.CacheCreation1h / 1_000_000
	return total
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
