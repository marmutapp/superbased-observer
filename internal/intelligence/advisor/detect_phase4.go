package advisor

import (
	"fmt"

	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
)

// SessionScores carries the scoring package's persisted per-session
// metrics the Phase-4 detectors read (one owner: scoring computes, the
// advisor only consumes).
type SessionScores struct {
	SessionID          string
	Project            string
	StartedAt          string
	QualityScore       *float64
	StaleReadsWasteful *int64
	WebSearchRequests  int64
}

// detectCrossSessionStaleReads (C1 on the migration-038 wasteful split):
// only stale reads the scoring engine attributed as WASTEFUL (the prior
// copy was still live in cache) ever nudge — compaction/expiry-forced
// re-reads never do (calibration: a false-positive nudge kills trust).
func detectCrossSessionStaleReads(f *Facts, _ Thresholds) []Suggestion {
	var wasteful int64
	var sessions int
	for _, sc := range f.Scores {
		if sc.StaleReadsWasteful != nil && *sc.StaleReadsWasteful > 0 {
			wasteful += *sc.StaleReadsWasteful
			sessions++
		}
	}
	if sessions < 3 || wasteful < 20 {
		return nil
	}
	// ~1K tokens per redundant read (the discover engine's 512-4K range
	// midpoint), priced at the dominant model's input rate.
	model := ""
	for i := range f.Sessions {
		model = sessionModelOf(&f.Sessions[i])
		if model != "" {
			break
		}
	}
	c, ok := f.Price(model, cost.TokenBundle{Input: wasteful * 1000})
	if !ok {
		return nil
	}
	return []Suggestion{{
		DedupKey: "wasteful_stale_reads|window", Detector: "wasteful_stale_reads",
		Category: CategoryCost, Scope: ScopeGlobal, Severity: SeverityAdvice,
		Title: fmt.Sprintf("%d avoidable re-reads across %d sessions", wasteful, sessions),
		Nudge: fmt.Sprintf(
			"%d file re-reads happened while the prior copy was still warm in cache (the scoring engine excludes compaction-forced ones). The MCP freshness tools (check_file_freshness, get_file) exist precisely for this — roughly $%.2f of repeat reading.",
			wasteful, c,
		),
		SavingsUSD: round2(c), Confidence: 0.6,
		Evidence: Evidence{
			Numbers: map[string]float64{"wasteful_stale_reads": float64(wasteful), "sessions": float64(sessions)},
			Math:    fmt.Sprintf("%d wasteful re-reads × ~1K tokens at input rate = $%.2f", wasteful, c),
		},
	}}
}

// detectQualityRegression (G4): the persisted quality_score trending down
// across the window — first-half mean vs second-half mean.
func detectQualityRegression(f *Facts, _ Thresholds) []Suggestion {
	var scored []SessionScores
	for _, sc := range f.Scores {
		if sc.QualityScore != nil {
			scored = append(scored, sc)
		}
	}
	if len(scored) < 10 {
		return nil
	}
	half := len(scored) / 2
	mean := func(xs []SessionScores) float64 {
		var sum float64
		for _, x := range xs {
			sum += *x.QualityScore
		}
		return sum / float64(len(xs))
	}
	early, late := mean(scored[:half]), mean(scored[half:])
	if early-late < 0.12 {
		return nil
	}
	return []Suggestion{{
		DedupKey: "quality_regression|window", Detector: "quality_regression",
		Category: CategoryQuality, Scope: ScopeGlobal, Severity: SeverityWarning,
		Title: fmt.Sprintf("Session quality slid from %.2f to %.2f", early, late),
		Nudge: fmt.Sprintf(
			"Average session quality dropped from %.2f to %.2f across this window (rising redundancy/error share). Worth checking what changed — model, effort level, or project phase.",
			early, late,
		),
		SavingsMin: 30, Confidence: 0.5,
		Evidence: Evidence{
			Numbers: map[string]float64{"early_mean": early, "late_mean": late, "sessions": float64(len(scored))},
			Math:    fmt.Sprintf("first-half mean %.2f − second-half mean %.2f = %.2f drop", early, late, early-late),
		},
	}}
}

// detectWebSearchSpend (B6): server-side web_search fees ($0.01 flat each)
// accumulating quietly.
func detectWebSearchSpend(f *Facts, _ Thresholds) []Suggestion {
	var n int64
	for _, sc := range f.Scores {
		n += sc.WebSearchRequests
	}
	if n < 200 {
		return nil
	}
	usd := float64(n) * 0.01
	return []Suggestion{{
		DedupKey: "web_search_spend|window", Detector: "web_search_spend",
		Category: CategoryCost, Scope: ScopeGlobal, Severity: SeverityInfo,
		Title: fmt.Sprintf("%d server-side web searches ($%.2f)", n, usd),
		Nudge: fmt.Sprintf(
			"%d web searches ran at the $0.01 flat fee this window ($%.2f). Recurring identical lookups are cacheable — the FTS5 index (search_past_outputs) already holds prior results.",
			n, usd,
		),
		SavingsUSD: round2(usd * 0.5), Confidence: 0.5,
		Evidence: Evidence{
			Numbers: map[string]float64{"searches": float64(n)},
			Math:    fmt.Sprintf("%d × $0.01 × 50%% repeat-lookup share = $%.2f", n, usd*0.5),
		},
	}}
}
