package advisor

import (
	"time"

	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
)

// Suggestion categories.
const (
	CategoryCost    = "cost"
	CategoryLatency = "latency"
	CategoryQuality = "quality"
	CategoryHygiene = "hygiene"
)

// Suggestion scopes.
const (
	ScopeSession = "session"
	ScopeProject = "project"
	ScopeGlobal  = "global"
)

// Suggestion severities.
const (
	SeverityInfo    = "info"
	SeverityAdvice  = "advice"
	SeverityWarning = "warning"
)

// Suggestion is one prescriptive, evidence-backed recommendation. See the
// plan §1 for the contract; DedupKey is the stable identity that Phase-2
// dismissals/snoozes/cooldowns attach to.
type Suggestion struct {
	DedupKey   string   `json:"dedup_key"`
	Detector   string   `json:"detector"`
	Category   string   `json:"category"`
	Scope      string   `json:"scope"`
	ScopeID    string   `json:"scope_id,omitempty"`
	Severity   string   `json:"severity"`
	Title      string   `json:"title"`
	Nudge      string   `json:"nudge"`
	SavingsUSD float64  `json:"savings_usd,omitempty"`
	SavingsMin float64  `json:"savings_min,omitempty"`
	Confidence float64  `json:"confidence"`
	Evidence   Evidence `json:"evidence"`
	ComputedAt string   `json:"computed_at"`
	WindowDays int      `json:"window_days"`
	// Action, when non-nil, is the machine-actionable remediation
	// (usability arc C3, additive — prose-only suggestions stay
	// valid). One renderer in the dashboard maps it to a button;
	// unknown kinds render nothing (forward compatible).
	Action *Action `json:"action,omitempty"`
}

// Action is a suggestion's one-click remediation pointer. Kind
// vocabulary (table-driven in the dashboard renderer):
//
//	"settings_section" — Target is a Settings section id; the button
//	                     deep-links /settings?section=<target>.
//	"page"             — Target is a dashboard route path.
//
// Writes are NEVER performed by the action itself — every kind lands
// on a surface whose own consent flow (route preview/confirm, wizard
// per-step buttons) does the writing. That keeps the C2/Q4 invariant:
// AI-client config writes happen only behind explicit per-action
// consent, never from a suggestion card.
type Action struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
	Label  string `json:"label"`
}

// Evidence is the bounded, structured backing for a suggestion — counts and
// arithmetic, never raw content (targets/commands arrive already scrubbed
// upstream). Rendered by the dashboard drill-down and `observer advise
// --json`.
type Evidence struct {
	Numbers map[string]float64 `json:"numbers,omitempty"`
	Items   []EvidenceItem     `json:"items,omitempty"`
	Math    string             `json:"math,omitempty"`
}

// EvidenceItem is one named, quantified evidence row (a session, a file, a
// command).
type EvidenceItem struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit,omitempty"`
}

// Report is the engine's output for one run. TotalCount survives digest
// truncation (SaveDigest keeps top-K suggestions but the full count) so
// point-readers like the sidebar badge see the real number.
type Report struct {
	Suggestions     []Suggestion `json:"suggestions"`
	TotalCount      int          `json:"total_count"`
	TotalSavingsUSD float64      `json:"total_savings_usd"`
	TotalSavingsMin float64      `json:"total_savings_min"`
	WindowDays      int          `json:"window_days"`
	GeneratedAt     string       `json:"generated_at"`
	SessionsScanned int          `json:"sessions_scanned"`
}

// Thresholds carries every detector tunable as data (CLAUDE.md boundary #5:
// decision logic is table-driven). Defaults are the Phase-0 calibration
// values (advisor-calibration-2026-06-10.md §8, T1–T11).
type Thresholds struct {
	// T1 (revised 2026-06-10, operator feedback) — fraction of the
	// session's context window at which it counts as ballooned. The
	// context window is inferred per session (model tag + empirical
	// tier snap — see contextWindowOf), so a 1M-context session
	// balloons at ~500K, not at a fixed token count that fires on
	// every long session. Savings stay threshold-insensitive
	// (calibration §2); this governs when the nudge fires.
	BalloonWindowFraction float64 `toml:"balloon_window_fraction"`
	// BalloonFloorTokens is the absolute minimum balloon threshold —
	// guards small/unknown-context models from firing on noise.
	BalloonFloorTokens int64 `toml:"balloon_floor_tokens"`
	// T3 — minimum turn rows after the balloon point before the
	// suggestion fires (~23% of substantial sessions at 20).
	BalloonMinRowsAfter int `toml:"balloon_min_rows_after"`
	// T5 — multiplier applied to the naive counterfactual saving before
	// presentation (the user kept context deliberately; some of it was
	// load-bearing).
	CounterfactualHaircut float64 `toml:"counterfactual_haircut"`
	// T6 — idle gap (minutes) that re-anchors the per-turn baseline and
	// defines an A5 re-cache window (Anthropic 5m TTL).
	IdleGapMinutes float64 `toml:"idle_gap_minutes"`
	// Minimum idle re-cache events before idle_recache fires.
	IdleRecacheMinEvents int `toml:"idle_recache_min_events"`
	// Trivial-session bounds for the routing detector (migrated from the
	// Analysis tab rule): gross prompt + output ceilings.
	TrivialPromptTokens int64 `toml:"trivial_prompt_tokens"`
	TrivialOutputTokens int64 `toml:"trivial_output_tokens"`
	// RoutingTargetModel is the cheaper model trivial sessions are
	// re-priced against.
	RoutingTargetModel string `toml:"routing_target_model"`
	// T10 — cache hit-rate below this is flagged (provisional until the
	// post-soak recalibration; calibration §7).
	HealthyCacheHitRate float64 `toml:"healthy_cache_hit_rate"`
	// T9 — minimum never-succeeded failures before C4 fires (plus the
	// non-trivial-command gate; calibration §5).
	C4MinFails int `toml:"c4_min_fails"`
	// MCP calls per 100 turns below which the schema tax is flagged.
	MCPMinCallsPer100Turns float64 `toml:"mcp_min_calls_per_100_turns"`
	// SpikeMultiple — today's spend at or above this multiple of the
	// trailing daily average fires spend_spike (P6.12).
	SpikeMultiple float64 `toml:"spike_multiple"`
	// SpikeMinPriorDays — minimum prior ACTIVE days (any spend) before
	// the spike baseline is believed.
	SpikeMinPriorDays int `toml:"spike_min_prior_days"`
	// GuardIdleHighSevFloor — minimum high/critical guard verdicts in
	// the window before guard_observe_idle calls the posture "recurring"
	// (X3.1). Below it the guard is observing quietly, which needs no
	// nudge.
	GuardIdleHighSevFloor int `toml:"guard_idle_high_sev_floor"`
}

// DefaultThresholds returns the Phase-0 calibrated defaults.
func DefaultThresholds() Thresholds {
	return Thresholds{
		BalloonWindowFraction:  0.5,
		BalloonFloorTokens:     100_000,
		BalloonMinRowsAfter:    20,
		CounterfactualHaircut:  0.7,
		IdleGapMinutes:         5,
		IdleRecacheMinEvents:   2,
		TrivialPromptTokens:    30_000,
		TrivialOutputTokens:    5_000,
		RoutingTargetModel:     "claude-sonnet-4-6",
		HealthyCacheHitRate:    0.70,
		C4MinFails:             5,
		MCPMinCallsPer100Turns: 3,
		SpikeMultiple:          3,
		SpikeMinPriorDays:      3,
		GuardIdleHighSevFloor:  10,
	}
}

// PriceFn prices a token bundle for a model, returning the total USD and
// whether the model resolved in the pricing table. Backed by
// cost.Engine.ComputeBreakdown in production; tests inject flat rates.
type PriceFn func(model string, b cost.TokenBundle) (float64, bool)

// Detector is one registry entry: a pure decision over Facts.
type Detector struct {
	Key      string
	Category string
	Detect   func(f *Facts, t Thresholds) []Suggestion
}

// stamp fills the engine-owned fields on a detector's suggestions.
func stamp(sugs []Suggestion, f *Facts) []Suggestion {
	ts := f.Now.UTC().Format(time.RFC3339)
	for i := range sugs {
		sugs[i].ComputedAt = ts
		sugs[i].WindowDays = f.WindowDays
	}
	return sugs
}
