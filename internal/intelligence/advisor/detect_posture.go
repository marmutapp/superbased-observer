package advisor

import "fmt"

// X3.1 posture detectors (security/routing usability arc): two
// navigate-only nudges that point at a surface owning the full
// evidence and every consent flow. Neither claims time savings, and
// guard_observe_idle claims no dollars — they pass the engine's
// filters on confidence alone (the savings floor suppresses small
// claims, not the absence of one). Both are cooldown-respecting like
// every suggestion: dismissing keeps them quiet for the cooldown.

// detectGuardObserveIdle fires when the guard has been observing with
// a recurring high-severity verdict load and ZERO operator engagement
// (no active approvals) — the "installed it, never looked again"
// posture. The nudge navigates to the Security page, whose readiness
// card / approvals register / enforce runbook own the actual workflow.
// Engagement proxy honesty: active approvals are the one engagement
// signal visible in the DB; policy tuning lives in config/policy files
// the advisor deliberately does not read.
func detectGuardObserveIdle(f *Facts, t Thresholds) []Suggestion {
	if f.GuardMode != "observe" {
		return nil // off = nothing to review; enforce = already promoted
	}
	if f.GuardHighSevEvents < t.GuardIdleHighSevFloor {
		return nil
	}
	if f.GuardActiveApprovals > 0 {
		return nil // the operator IS engaging with the verdict stream
	}
	return []Suggestion{{
		DedupKey: "guard_observe_idle|window",
		Detector: "guard_observe_idle",
		Category: CategoryHygiene,
		Scope:    ScopeGlobal,
		Severity: SeverityAdvice,
		Title:    fmt.Sprintf("Guard logged %d high-severity verdicts — nothing tuned yet", f.GuardHighSevEvents),
		Nudge: fmt.Sprintf(
			"The guard has been observing: %d high/critical verdicts landed in the last %d days with zero approvals granted. "+
				"Worth a look at the Security page — tune the loudest rule (or approve the legitimate cases), and the enforce-readiness card shows what blocking mode would have done.",
			f.GuardHighSevEvents, f.WindowDays,
		),
		Confidence: 0.7,
		Evidence: Evidence{
			Numbers: map[string]float64{
				"high_sev_verdicts": float64(f.GuardHighSevEvents),
				"active_approvals":  0,
			},
			Math: fmt.Sprintf("%d guard_events at severity high/critical in %dd ≥ floor %d, with 0 active approvals",
				f.GuardHighSevEvents, f.WindowDays, t.GuardIdleHighSevFloor),
		},
		Action: &Action{Kind: "page", Target: "/security", Label: "Review Security"},
	}}
}

// detectRoutingEvidenceReady fires when the §R22 advise-shadow gate is
// met but routing still is not enforcing — the evidence the operator
// asked advise mode to gather is in, and only the promotion decision
// remains. The verdict comes verbatim from the injected ShadowSignal
// (store.BuildAdviseShadowReport — one gate owner; this detector never
// re-derives it). Navigate-only: the Shadow card restates the evidence
// and owns the consent-gated promote control.
func detectRoutingEvidenceReady(f *Facts, _ Thresholds) []Suggestion {
	sh := f.RoutingShadow
	if sh == nil || !sh.Ready {
		return nil
	}
	if f.RoutingMode == "enforce" {
		return nil // already promoted — nothing to nudge
	}
	return []Suggestion{{
		DedupKey: "routing_evidence_ready|window",
		Detector: "routing_evidence_ready",
		Category: CategoryCost,
		Scope:    ScopeGlobal,
		Severity: SeverityAdvice,
		Title:    fmt.Sprintf("Routing's shadow evidence is in — $%.2f would have been saved", sh.WouldSaveUSD),
		Nudge: fmt.Sprintf(
			"The advise shadow met the §R22 gate: %d advise decisions (floor %d), %d would-have reroutes saving $%.2f net of cache forfeits, zero quality flags. "+
				"Routing is still %s — review the Shadow card's readiness ladder and promote when YOU are ready; the gate is evidence, not the decision.",
			sh.AdviseDecisions, sh.MinDecisions, sh.WouldReroute, sh.WouldSaveUSD, f.RoutingMode,
		),
		SavingsUSD: round2(sh.WouldSaveUSD),
		Confidence: 0.8,
		Evidence: Evidence{
			Numbers: map[string]float64{
				"advise_decisions": float64(sh.AdviseDecisions),
				"would_reroute":    float64(sh.WouldReroute),
				"quality_flags":    float64(sh.QualityFlags),
			},
			Math: fmt.Sprintf("§R22: %d ≥ %d decisions ∧ %d reroutes ∧ $%.2f > 0 ∧ 0 flags",
				sh.AdviseDecisions, sh.MinDecisions, sh.WouldReroute, sh.WouldSaveUSD),
		},
		Action: &Action{Kind: "page", Target: "/routing", Label: "Review the Shadow card"},
	}}
}
