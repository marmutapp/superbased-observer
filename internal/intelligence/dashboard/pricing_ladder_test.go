package dashboard

import (
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
)

// Shared fixture for the date-effective pricing ladder tests across
// report/budget/statusline/live/experiments/analysis: recorded cost
// wins, otherwise the row prices at the rate in force on its OWN
// timestamp, never the current rate unconditionally.
//
// ladderModel is a synthetic id (never a real SKU — see
// cost/dated_test.go's synth-* convention) whose CURRENT (flat) rate
// and OLD (pre-boundary dated) rate are deliberately different, so a
// test asserting the dated rate can't pass by accident via the
// undated-fallback path.
const ladderModel = "synth-ladder-model"

// Old period: expensive. New/current period (from the test's chosen
// boundary): cheap.
var (
	ladderOldRate = cost.Pricing{Input: 20, Output: 80}
	ladderNewRate = cost.Pricing{Input: 5, Output: 20}
)

// ladderBundle is a fixed token shape (100k in / 10k out) used by every
// ladder test so the expected cost at each rate is a round, reusable
// number: old rate = $2.80, new/current rate = $0.70.
var ladderBundle = cost.TokenBundle{Input: 100_000, Output: 10_000}

const (
	ladderOldCost = 2.8 // 100_000*20/1e6 + 10_000*80/1e6
	ladderNewCost = 0.7 // 100_000*5/1e6 + 10_000*20/1e6
)

// ladderOpusModel is a second synthetic id, deliberately containing
// "opus" (case-insensitive), for the routing-suggestions ladder test —
// that surface only evaluates Opus-shaped, single-model, trivial
// (small prompt/output) sessions, so it needs its own small token
// shape distinct from ladderBundle (which is intentionally too big to
// pass the "trivial" filter).
const ladderOpusModel = "synth-ladder-opus"

// ladderTrivialBundle is a small per-turn shape (5k in / 1k out) sized
// so three turns aggregate under the routing-suggestions "trivial"
// thresholds (30k prompt / 5k output tokens).
var ladderTrivialBundle = cost.TokenBundle{Input: 5_000, Output: 1_000}

const (
	ladderOldTrivialCost = 0.18  // 5_000*20/1e6 + 1_000*80/1e6
	ladderNewTrivialCost = 0.045 // 5_000*5/1e6 + 1_000*20/1e6
)

// newLadderTestEngine builds a cost.Engine whose flat table holds the
// CURRENT (new, cheap) rate for ladderModel and ladderOpusModel, and
// whose dated timeline also carries the OLD (expensive) rate for usage
// before boundary. boundary is caller-supplied (rather than a fixed
// calendar date) so each surface's test can place it inside whatever
// window (month, live-session lookback, etc.) that surface's fixture
// needs — always relative to time.Now(), never a hard-coded date that
// could fall outside a real-time test's query window.
func newLadderTestEngine(t *testing.T, boundary time.Time) *cost.Engine {
	t.Helper()
	engine := cost.NewEngine(config.IntelligenceConfig{})
	engine.Table().Merge(map[string]cost.Pricing{
		ladderModel:     ladderNewRate,
		ladderOpusModel: ladderNewRate,
	})
	engine.Table().MergeDated(map[string][]cost.DatedPricing{
		ladderModel: {
			{EffectiveFrom: time.Time{}, Pricing: ladderOldRate},
			{EffectiveFrom: boundary, Pricing: ladderNewRate},
		},
		ladderOpusModel: {
			{EffectiveFrom: time.Time{}, Pricing: ladderOldRate},
			{EffectiveFrom: boundary, Pricing: ladderNewRate},
		},
	})
	return engine
}
