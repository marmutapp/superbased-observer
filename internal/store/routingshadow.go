package store

import (
	"context"
	"fmt"
	"time"

	"github.com/marmutapp/superbased-observer/internal/intelligence/modelvalue"
	"github.com/marmutapp/superbased-observer/internal/routing"
)

// Advise-shadow report (§R18.2): advise mode is a permanent shadow A/B
// — decisions logged, outcomes observed on the unrouted reality. This
// report is the PROMOTION SURFACE: "in N days advise-mode would have
// saved $X with K quality-flag events". The §R22 P1→P2 gate (a 30-day
// shadow with savings and zero quality flags) consumes it; the soak
// itself is operator-run.

// AdviseShadowReport is the promotion surface's shape.
type AdviseShadowReport struct {
	WindowDays      int     `json:"window_days"`
	AdviseDecisions int     `json:"advise_decisions"`
	WouldReroute    int     `json:"would_reroute"`
	WouldSaveUSD    float64 `json:"would_save_usd"`
	CacheForfeitUSD float64 `json:"cache_forfeit_usd"`
	// QualityFlags counts would-reroutes whose (turn-kind, target tier)
	// lacks parity evidence in the Model Value Report — the §R18.1
	// quality-risk definition applied to the shadow. Maximum caution:
	// a nil evidence map flags every reroute.
	QualityFlags        int            `json:"quality_flags"`
	QualityByKind       map[string]int `json:"quality_flags_by_turn_kind"`
	EvidenceBackedMoves int            `json:"evidence_backed_moves"`
	// HoldsByReason counts the no-change advise decisions by their
	// first reason code — why the shadow router stayed put.
	HoldsByReason map[string]int `json:"holds_by_reason"`
	// ReadyToPromote: the report's mechanical read of the §R22 gate —
	// enough decisions, real savings, zero quality flags. The operator
	// decides; this is the evidence, not the decision.
	ReadyToPromote bool `json:"ready_to_promote"`
	// MinDecisions is the evidence floor the gate checks
	// AdviseDecisions against — carried in the report so UI ladders
	// explain "N of M decisions" without hardcoding the threshold.
	MinDecisions int    `json:"min_decisions"`
	Note         string `json:"note"`
}

// AdviseShadowNote is the structural caveat on every shadow report.
const AdviseShadowNote = "shadow savings are decision-time estimates over the UNROUTED reality (§R18.2); " +
	"quality flags count proposed moves lacking Model Value parity evidence (§R18.1); " +
	"the §R22 promotion gate is a 30-day shadow with savings and zero flags — operator-run, operator-judged."

// shadowMinDecisions is the evidence floor before ReadyToPromote can
// report true — mirrors the §R7.2 default sample threshold.
const shadowMinDecisions = 50

// BuildAdviseShadowReport aggregates advise-mode decisions against the
// Model Value evidence map. Pure.
func BuildAdviseShadowReport(rows []RouterDecisionDetail, evidence map[routing.EvidenceKey]string, tiers *routing.TierTable, windowDays int) AdviseShadowReport {
	rep := AdviseShadowReport{
		WindowDays:    windowDays,
		QualityByKind: map[string]int{},
		HoldsByReason: map[string]int{},
		MinDecisions:  shadowMinDecisions,
		Note:          AdviseShadowNote,
	}
	for _, d := range rows {
		if d.Mode != "advise" || d.Applied {
			continue
		}
		rep.AdviseDecisions++
		if d.SelectedModel == d.OriginalModel {
			if len(d.ReasonCodes) > 0 {
				rep.HoldsByReason[d.ReasonCodes[0]]++
			}
			continue
		}
		rep.WouldReroute++
		rep.WouldSaveUSD += d.EstSavingsUSD
		rep.CacheForfeitUSD += d.CacheForfeitUSD

		targetTier := routing.TierUnclassified
		if tiers != nil {
			targetTier, _ = tiers.Lookup(d.SelectedModel)
		}
		key := routing.EvidenceKey{Kind: routing.TurnKind(d.TurnKind), Tier: targetTier}
		if v := evidence[key]; v == routing.EvidenceParity || v == routing.EvidenceCandidateBetter {
			rep.EvidenceBackedMoves++
		} else {
			rep.QualityFlags++
			rep.QualityByKind[d.TurnKind]++
		}
	}
	rep.ReadyToPromote = rep.AdviseDecisions >= shadowMinDecisions &&
		rep.WouldReroute > 0 && rep.WouldSaveUSD > 0 && rep.QualityFlags == 0
	return rep
}

// AdviseShadowSignal composes the §R22 gate read for the advisor's
// routing_evidence_ready detector (X3.1): decisions + Model Value
// evidence → BuildAdviseShadowReport, the ONE gate owner — callers
// inject the result instead of re-deriving the gate. A cheap COUNT
// early-out skips the expensive modelvalue load on nodes with no
// advise history in the window (the common case until routing is
// adopted). price may be nil (facts fall back to stored row costs).
func (s *Store) AdviseShadowSignal(ctx context.Context, windowDays int, price routing.PriceFn) (AdviseShadowReport, error) {
	since := time.Now().AddDate(0, 0, -windowDays)
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM router_decisions WHERE ts >= ? AND mode = 'advise' AND applied = 0`,
		timestamp(since)).Scan(&n); err != nil {
		return AdviseShadowReport{}, fmt.Errorf("store.AdviseShadowSignal: count: %w", err)
	}
	tiers := routing.NewTierResolver().Table()
	if n == 0 {
		return BuildAdviseShadowReport(nil, nil, tiers, windowDays), nil
	}
	rows, err := s.SelectRouterDecisions(ctx, since, 0)
	if err != nil {
		return AdviseShadowReport{}, fmt.Errorf("store.AdviseShadowSignal: %w", err)
	}
	facts, err := s.LoadModelValueFacts(ctx, modelvalue.LoadOptions{WindowDays: windowDays})
	if err != nil {
		return AdviseShadowReport{}, fmt.Errorf("store.AdviseShadowSignal: %w", err)
	}
	if price != nil {
		facts.Price = price
	}
	evidence := modelvalue.Build(facts, modelvalue.Options{}).EvidenceByKindTier()
	return BuildAdviseShadowReport(rows, evidence, tiers, windowDays), nil
}
