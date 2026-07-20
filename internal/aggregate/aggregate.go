package aggregate

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/integration"
)

// SchemaVersion is the aggregate wire schema version. The collector validates
// it; a material schema change bumps it and suspends submission until
// re-consent (design §9.1). Bump on any change to the field set of
// Submission / Cell.
const SchemaVersion = 1

// PricingVersion identifies the pricing/catalog snapshot the cost_usd figures
// were computed against (design §3.1, finding #9). It is a coarse date-tagged
// marker of internal/intelligence/cost/pricing.go; bump it when rates change
// materially so the report can compare like-with-like across months.
const PricingVersion = "2026-07"

// costMethodVersionBase is the cost-engine method version. The wire
// cost_method_version folds the model_family map version into it (Open Q12) so
// a single field carries both dimensions that affect longitudinal cost
// comparability.
const costMethodVersionBase = "1"

// CostMethodVersion is the wire value for the envelope's cost_method_version:
// the cost-engine method version with FamilyMapVersion folded in (design §3.3
// / Open Q12). e.g. "1+fam1".
func CostMethodVersion() string {
	return costMethodVersionBase + "+fam" + FamilyMapVersion
}

// Local rare-cell coarsening thresholds (design §3.2, Open Q11). Before a
// submission is built, any (model_family, tool) cell whose combined activity
// falls below BOTH floors has its model_family collapsed to "other" (keeping
// the tool) and merged, so a single sparse cell cannot fingerprint the node.
// These are a v1 heuristic; a documented differential-privacy budget at
// publication is the later, stronger guarantee.
const (
	RareCellMinTurns   = 25
	RareCellMinCostUSD = 0.50
)

// Submission is the ENTIRE payload that leaves a node when the rail is
// enabled (design §3.1). It carries no stable application identifier and no
// content: the allow-listed fields below are the only shape, pinned by
// tests/invariant/aggregate_test.go.
type Submission struct {
	SchemaVersion       int    `json:"schema_version"`
	ObserverVersion     string `json:"observer_version"`      // COARSENED to minor, e.g. "1.20"
	PricingVersion      string `json:"pricing_version"`       // pricing catalog used for cost_usd
	CostMethodVersion   string `json:"cost_method_version"`   // cost-engine method + model_family map version
	ToolRegistryVersion int    `json:"tool_registry_version"` // internal/integration vocabulary version
	SubmissionID        string `json:"submission_id"`         // RANDOM per submission, reused-on-retry; NOT a node id
	Month               string `json:"month"`                 // "2026-06" — the single FINALIZED UTC month
	Cells               []Cell `json:"cells"`
}

// Cell is the unit of aggregation, keyed by (month, model_family, tool). Every
// volume/cost metric is split by provenance class (design §3.2, finding #8):
// the _acc twin holds proxy-accurate turns only; the _est twin holds every
// non-"accurate" reliability class (approximate|unreliable|unknown), so the
// report can build a genuinely proxy-only cut and quantify the estimated
// remainder separately, never blending them.
type Cell struct {
	ModelFamily string `json:"model_family"` // closed vocab (Family); unknown → "other"
	Tool        string `json:"tool"`         // closed vocab from internal/integration; unknown → "other"

	TurnsAcc int64 `json:"turns_acc"`
	TurnsEst int64 `json:"turns_est"`

	InputTokensAcc int64 `json:"input_tokens_acc"`
	InputTokensEst int64 `json:"input_tokens_est"`

	OutputTokensAcc int64 `json:"output_tokens_acc"`
	OutputTokensEst int64 `json:"output_tokens_est"`

	CacheReadTokensAcc int64 `json:"cache_read_tokens_acc"`
	CacheReadTokensEst int64 `json:"cache_read_tokens_est"`

	CacheCreationTokensAcc int64 `json:"cache_creation_tokens_acc"` // 5-minute write tier
	CacheCreationTokensEst int64 `json:"cache_creation_tokens_est"`

	CacheCreation1hTokensAcc int64 `json:"cache_creation_1h_tokens_acc"` // 1-hour write tier
	CacheCreation1hTokensEst int64 `json:"cache_creation_1h_tokens_est"`

	ReasoningTokensAcc int64 `json:"reasoning_tokens_acc"`
	ReasoningTokensEst int64 `json:"reasoning_tokens_est"`

	WebSearchRequestsAcc int64 `json:"web_search_requests_acc"`
	WebSearchRequestsEst int64 `json:"web_search_requests_est"`

	FastTurnsAcc int64 `json:"fast_turns_acc"`
	FastTurnsEst int64 `json:"fast_turns_est"`

	CostUSDAcc float64 `json:"cost_usd_acc"` // rounded to cents
	CostUSDEst float64 `json:"cost_usd_est"`

	CacheObservable bool `json:"cache_observable"` // false if the contributing adapter can't see cache tiers
	FastObservable  bool `json:"fast_observable"`  // false if the adapter can't see the fast/priority tier
}

// ModelToolStat is the aggregate package's OWN input DTO (design §6.1) so the
// pure package need not import internal/intelligence/cost. One stat is one
// (model, tool) group already rolled up at the read seam, tagged with whether
// every contributing turn was proxy-accurate.
type ModelToolStat struct {
	// Model is the RAW model id (normalized to a family by Build).
	Model string
	// Tool is the raw tool string (normalized against the registry by Build).
	Tool string
	// Accurate is true iff every turn contributing to this stat was
	// proxy-accurate (the joint read's weakest-reliability == "accurate").
	// When true the stat's volume lands in the _acc twins; otherwise it lands
	// in the _est twins (design §4.2 / Open Q13 — per-cell rollup).
	Accurate bool

	Turns             int64
	InputTokens       int64
	OutputTokens      int64
	CacheReadTokens   int64
	CacheCreation     int64
	CacheCreation1h   int64
	ReasoningTokens   int64
	WebSearchRequests int64
	FastTurns         int64
	CostUSD           float64

	// CacheObservable / FastObservable report whether the contributing
	// adapter can observe the cache / fast tiers (resolved from the capability
	// registry at the read seam). Merged (OR) into the cell.
	CacheObservable bool
	FastObservable  bool
}

// Meta carries the caller-supplied envelope fields Build cannot derive itself:
// the coarsened observer version, the random submission id, and the finalized
// month. Schema/pricing/cost-method/tool-registry versions are filled from
// package constants + the integration registry so they can never drift.
type Meta struct {
	ObserverVersion string // caller coarsens to minor via CoarsenVersion
	SubmissionID    string // random per submission (reused on retry); NOT a node id
	Month           string // "2026-06"
}

// Build assembles the wire submission from the injected stats: it normalizes
// each stat's model → family and tool → registry vocabulary, folds the
// _acc/_est provenance split, rounds cost to cents, applies local rare-cell
// coarsening, and sorts cells deterministically. It is pure and total — no
// I/O, no error path.
func Build(meta Meta, stats []ModelToolStat) Submission {
	// First pass: fold stats into cells keyed by (family, tool).
	type key struct{ family, tool string }
	cells := map[key]*Cell{}
	for _, s := range stats {
		k := key{family: Family(s.Model), tool: normalizeTool(s.Tool)}
		c := cells[k]
		if c == nil {
			c = &Cell{ModelFamily: k.family, Tool: k.tool}
			cells[k] = c
		}
		addStat(c, s)
	}

	// Second pass: local rare-cell coarsening. A cell below BOTH activity
	// floors has its family collapsed to "other" (keeping tool) and merged.
	coarsened := map[key]*Cell{}
	for k, c := range cells {
		finalFamily := k.family
		if isRareCell(c) && finalFamily != FamilyOther {
			finalFamily = FamilyOther
		}
		fk := key{family: finalFamily, tool: k.tool}
		dst := coarsened[fk]
		if dst == nil {
			dst = &Cell{ModelFamily: fk.family, Tool: fk.tool}
			coarsened[fk] = dst
		}
		mergeCell(dst, c)
	}

	out := make([]Cell, 0, len(coarsened))
	for _, c := range coarsened {
		c.CostUSDAcc = roundCents(c.CostUSDAcc)
		c.CostUSDEst = roundCents(c.CostUSDEst)
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ModelFamily != out[j].ModelFamily {
			return out[i].ModelFamily < out[j].ModelFamily
		}
		return out[i].Tool < out[j].Tool
	})

	return Submission{
		SchemaVersion:       SchemaVersion,
		ObserverVersion:     meta.ObserverVersion,
		PricingVersion:      PricingVersion,
		CostMethodVersion:   CostMethodVersion(),
		ToolRegistryVersion: integration.RegistryVersion,
		SubmissionID:        meta.SubmissionID,
		Month:               meta.Month,
		Cells:               out,
	}
}

// addStat accumulates one stat into a cell's _acc or _est twins by provenance.
func addStat(c *Cell, s ModelToolStat) {
	if s.Accurate {
		c.TurnsAcc += s.Turns
		c.InputTokensAcc += s.InputTokens
		c.OutputTokensAcc += s.OutputTokens
		c.CacheReadTokensAcc += s.CacheReadTokens
		c.CacheCreationTokensAcc += s.CacheCreation
		c.CacheCreation1hTokensAcc += s.CacheCreation1h
		c.ReasoningTokensAcc += s.ReasoningTokens
		c.WebSearchRequestsAcc += s.WebSearchRequests
		c.FastTurnsAcc += s.FastTurns
		c.CostUSDAcc += s.CostUSD
	} else {
		c.TurnsEst += s.Turns
		c.InputTokensEst += s.InputTokens
		c.OutputTokensEst += s.OutputTokens
		c.CacheReadTokensEst += s.CacheReadTokens
		c.CacheCreationTokensEst += s.CacheCreation
		c.CacheCreation1hTokensEst += s.CacheCreation1h
		c.ReasoningTokensEst += s.ReasoningTokens
		c.WebSearchRequestsEst += s.WebSearchRequests
		c.FastTurnsEst += s.FastTurns
		c.CostUSDEst += s.CostUSD
	}
	// Observability is a capability of the contributing adapter; within a cell
	// the tool is constant, so OR-ing yields the tool's coverage.
	c.CacheObservable = c.CacheObservable || s.CacheObservable
	c.FastObservable = c.FastObservable || s.FastObservable
}

// mergeCell folds src into dst (used by the coarsening merge pass).
func mergeCell(dst, src *Cell) {
	dst.TurnsAcc += src.TurnsAcc
	dst.TurnsEst += src.TurnsEst
	dst.InputTokensAcc += src.InputTokensAcc
	dst.InputTokensEst += src.InputTokensEst
	dst.OutputTokensAcc += src.OutputTokensAcc
	dst.OutputTokensEst += src.OutputTokensEst
	dst.CacheReadTokensAcc += src.CacheReadTokensAcc
	dst.CacheReadTokensEst += src.CacheReadTokensEst
	dst.CacheCreationTokensAcc += src.CacheCreationTokensAcc
	dst.CacheCreationTokensEst += src.CacheCreationTokensEst
	dst.CacheCreation1hTokensAcc += src.CacheCreation1hTokensAcc
	dst.CacheCreation1hTokensEst += src.CacheCreation1hTokensEst
	dst.ReasoningTokensAcc += src.ReasoningTokensAcc
	dst.ReasoningTokensEst += src.ReasoningTokensEst
	dst.WebSearchRequestsAcc += src.WebSearchRequestsAcc
	dst.WebSearchRequestsEst += src.WebSearchRequestsEst
	dst.FastTurnsAcc += src.FastTurnsAcc
	dst.FastTurnsEst += src.FastTurnsEst
	dst.CostUSDAcc += src.CostUSDAcc
	dst.CostUSDEst += src.CostUSDEst
	dst.CacheObservable = dst.CacheObservable || src.CacheObservable
	dst.FastObservable = dst.FastObservable || src.FastObservable
}

// isRareCell reports whether a cell falls below BOTH local coarsening floors.
func isRareCell(c *Cell) bool {
	turns := c.TurnsAcc + c.TurnsEst
	cost := c.CostUSDAcc + c.CostUSDEst
	return turns < RareCellMinTurns && cost < RareCellMinCostUSD
}

// normalizeTool maps a raw tool string to the closed registry vocabulary; any
// tool not in internal/integration (including "", "<no-tool>", or a future
// adapter name) collapses to "other" so no exotic tool string reaches the wire.
func normalizeTool(tool string) string {
	if knownTools[tool] {
		return tool
	}
	return FamilyOther
}

// knownTools is the closed tool allow-list sourced from the integration
// registry (design §3.2, finding #24).
var knownTools = func() map[string]bool {
	m := map[string]bool{}
	for _, t := range integration.Tools() {
		m[t] = true
	}
	return m
}()

func roundCents(v float64) float64 {
	return math.Round(v*100) / 100
}

// CoarsenVersion reduces a full semantic version ("1.20.3", "v1.20.3",
// "1.20.3-rc.1") to its major.minor form ("1.20") so the wire never carries a
// rare exact patch that could aid timing/version correlation (design §3.1,
// finding #4). Non-semver inputs (e.g. "dev") are returned unchanged.
func CoarsenVersion(full string) string {
	s := strings.TrimSpace(full)
	s = strings.TrimPrefix(s, "v")
	// Drop any pre-release / build suffix.
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return full // not a dotted version (e.g. "dev") — leave as-is
	}
	if !isNumeric(parts[0]) || !isNumeric(parts[1]) {
		return full
	}
	return parts[0] + "." + parts[1]
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// FinalizedMonth returns the most-recently-FINALIZED UTC calendar month
// relative to now — the previous month, never the current partial one (design
// §3.1). Format "2006-01".
func FinalizedMonth(now time.Time) string {
	first := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	prev := first.AddDate(0, 0, -1) // a day inside the previous month
	return prev.Format("2006-01")
}

// MonthWindowUTC returns the [start, end) UTC instants for a "2006-01" month
// string, suitable for a cost-engine Since/Until window.
func MonthWindowUTC(month string) (start, end time.Time, err error) {
	start, err = time.Parse("2006-01", month)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("aggregate.MonthWindowUTC: %w", err)
	}
	start = start.UTC()
	end = start.AddDate(0, 1, 0)
	return start, end, nil
}

// IsFinalizedMonth reports whether month ("2006-01") is fully elapsed relative
// to now (its end boundary is at or before now). The current or a future month
// is never finalized — the rail submits only fully-elapsed months.
func IsFinalizedMonth(month string, now time.Time) bool {
	_, end, err := MonthWindowUTC(month)
	if err != nil {
		return false
	}
	return !end.After(now.UTC())
}
