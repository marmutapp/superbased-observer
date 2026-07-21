package aggregatesource

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/aggregate"
	"github.com/marmutapp/superbased-observer/internal/integration"
	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
)

// readLimit is a high cap so the joint cut is never truncated — a submission
// must carry every cell for the month, not just the top-N by cost.
const readLimit = 1_000_000

// BuildSubmission reads the joint (model × tool) cost cut for meta.Month and
// returns the pure aggregate submission. The window is the [start, end) UTC
// span of meta.Month; the caller is responsible for passing a FINALIZED month
// (aggregate.IsFinalizedMonth) — this seam does not second-guess it, but it
// does refuse a malformed month string.
func BuildSubmission(ctx context.Context, db *sql.DB, engine *cost.Engine, meta aggregate.Meta) (aggregate.Submission, error) {
	start, end, err := aggregate.MonthWindowUTC(meta.Month)
	if err != nil {
		return aggregate.Submission{}, fmt.Errorf("aggregatesource.BuildSubmission: %w", err)
	}
	sum, err := engine.Summary(ctx, db, cost.Options{
		Since:   start,
		Until:   end,
		GroupBy: cost.GroupByModelTool,
		Source:  cost.SourceAuto,
		Limit:   readLimit,
	})
	if err != nil {
		return aggregate.Submission{}, fmt.Errorf("aggregatesource.BuildSubmission: summary: %w", err)
	}
	stats := mapRows(sum.Rows)
	return aggregate.Build(meta, stats), nil
}

// mapRows converts joint cost rows into aggregate input DTOs. Each row is one
// (model, tool) group already rolled up by the cost engine; its
// weakest-reliability tag decides the provenance class for the whole row
// (design §4.2 / Open Q13: per-cell rollup — accurate iff EVERY contributing
// turn was proxy-accurate, everything else feeds the _est twins).
func mapRows(rows []cost.Row) []aggregate.ModelToolStat {
	out := make([]aggregate.ModelToolStat, 0, len(rows))
	for _, r := range rows {
		model, tool := cost.SplitModelToolKey(r.Key)
		out = append(out, aggregate.ModelToolStat{
			Model:             model,
			Tool:              tool,
			Accurate:          r.Reliability == reliabilityAccurate,
			Turns:             int64(r.TurnCount),
			InputTokens:       r.Tokens.Input,
			OutputTokens:      r.Tokens.Output,
			CacheReadTokens:   r.Tokens.CacheRead,
			CacheCreation:     r.Tokens.CacheCreation,
			CacheCreation1h:   r.Tokens.CacheCreation1h,
			ReasoningTokens:   r.Tokens.Reasoning,
			WebSearchRequests: r.Tokens.WebSearchRequests,
			FastTurns:         int64(r.FastTurnCount),
			CostUSD:           r.CostUSD,
			CacheObservable:   observesCache(tool),
			FastObservable:    observesFast(tool),
		})
	}
	return out
}

const reliabilityAccurate = "accurate"

// observesCache reports whether the tool's best token-capture tier can, in
// principle, observe Anthropic-style cache tiers — so a `0` in a cache field
// means "supported and unused", not "unobservable" (design §3.2 coverage
// booleans; exact policy Open Q11). Grounded in the internal/integration
// capability registry (a capability lookup, not a tool-name branch —
// CLAUDE.md #3): the proxy tier sees the full usage block; a transcript/events
// tier that declares a cache gap in the registry cannot.
func observesCache(tool string) bool {
	c, ok := integration.For(tool)
	if !ok {
		return false
	}
	switch c.TokenTier.Best {
	case "proxy":
		return true
	case "", "none", "browser_extension":
		return false
	}
	return !strings.Contains(strings.ToLower(c.TokenTier.Gap), "cache")
}

// observesFast reports whether the tool can observe the provider's fast /
// priority tier. Only the proxy capture path records the fast flag reliably
// (Anthropic Opus speed:"fast", Codex service_tier:"priority"), so this is a
// v1 capability grounding — refined in a later phase.
func observesFast(tool string) bool {
	c, ok := integration.For(tool)
	if !ok {
		return false
	}
	return c.TokenTier.Best == "proxy"
}
