package advisor

import (
	"fmt"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
)

// detectTrivialModelOverprovisioning migrates the Analysis tab's one
// existing prescriptive rule (trivial Opus-only sessions → Sonnet) into the
// advisor framework. Trivial = gross prompt under TrivialPromptTokens AND
// output under TrivialOutputTokens for the whole session. Eligible sessions
// aggregate into ONE project-scoped suggestion (matching the legacy rule's
// shape) so the per-suggestion savings floor applies to the total, not each
// small session. The legacy /api/analysis/routing-suggestions endpoint
// stays serving until Phase 2 consolidates it onto this detector.
func detectTrivialModelOverprovisioning(f *Facts, t Thresholds) []Suggestion {
	count := 0
	var totalSave float64
	var items []EvidenceItem
	for i := range f.Sessions {
		s := &f.Sessions[i]
		if !sessionAllOpus(s) {
			continue
		}
		var in, out, cr, cc, cc1 int64
		anyFast := false
		for _, r := range s.Rows {
			in += r.Input
			out += r.Output
			cr += r.CacheRead
			cc += r.CacheCreation
			cc1 += r.CacheCreation1h
			anyFast = anyFast || r.Fast
		}
		if in+cc >= t.TrivialPromptTokens || out >= t.TrivialOutputTokens {
			continue
		}
		bundle := cost.TokenBundle{Input: in, Output: out, CacheRead: cr, CacheCreation: cc, CacheCreation1h: min64(cc1, cc), Fast: anyFast}
		actual, ok1 := f.Price(sessionModelOf(s), bundle)
		cheaper, ok2 := f.Price(t.RoutingTargetModel, bundle)
		if !ok1 || !ok2 || actual <= cheaper {
			continue
		}
		count++
		totalSave += actual - cheaper
		if len(items) < 10 {
			items = append(items, EvidenceItem{Label: s.ID, Value: round2(actual - cheaper), Unit: "usd"})
		}
	}
	if count == 0 || totalSave <= 0 {
		return nil
	}
	return []Suggestion{{
		DedupKey: "trivial_model_overprovisioning|window",
		Detector: "trivial_model_overprovisioning",
		Category: CategoryCost,
		Scope:    ScopeGlobal,
		Severity: SeverityInfo,
		Title:    fmt.Sprintf("%d small sessions ran on Opus", count),
		Nudge: fmt.Sprintf(
			"%d sessions in this window stayed under %dK prompt / %dK output tokens but ran entirely on Opus. Routing sessions this small to %s would have saved about $%.2f.",
			count, t.TrivialPromptTokens/1000, t.TrivialOutputTokens/1000, t.RoutingTargetModel, totalSave,
		),
		SavingsUSD: round2(totalSave),
		Confidence: 0.7,
		Evidence: Evidence{
			Numbers: map[string]float64{"sessions": float64(count)},
			Items:   items,
			Math:    fmt.Sprintf("Σ(opus price − %s price) over %d trivial sessions = $%.2f", t.RoutingTargetModel, count, totalSave),
		},
	}}
}

// sessionAllOpus reports whether every model attribution in the session is
// an Opus-family model (the expensive tier the legacy rule targeted).
func sessionAllOpus(s *SessionFacts) bool {
	seen := false
	for _, r := range s.Rows {
		m := rowModel(s, r)
		if m == "" {
			continue
		}
		seen = true
		if !strings.Contains(m, "opus") {
			return false
		}
	}
	return seen
}

func sessionModelOf(s *SessionFacts) string {
	for _, r := range s.Rows {
		if m := rowModel(s, r); m != "" {
			return m
		}
	}
	return s.Model
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
