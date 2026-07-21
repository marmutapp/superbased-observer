package egress

// StarterTemplate is a named, ready-to-author egress rule example a setup
// surface can present (parallels admission.StarterTemplates). It stays plain
// data — the boundary renders it into config without importing this package's
// compiled types.
type StarterTemplate struct {
	Key         string
	Title       string
	Description string
	Rule        RuleInput
	// NeedsTarget is true when the rule references an upstream id the operator
	// must declare as a [[observability.egress.targets]] entry.
	NeedsTarget bool
}

// StarterTemplates returns the built-in egress rule starters mirroring the
// design's §1 scenarios. They are intentionally conservative (advise-friendly)
// and reference placeholder ids/models the operator adjusts.
func StarterTemplates() []StarterTemplate {
	return []StarterTemplate{
		{
			Key:         "flagged-to-local",
			Title:       "Route flagged content to a local model",
			Description: "Requests the judge flags (verdict flag or stricter) route to an on-prem upstream. Data-locality intent — fail-closed.",
			Rule: RuleInput{
				Name:            "flagged-to-local",
				When:            WhenInput{VerdictAtLeast: "flag"},
				RouteToUpstream: "ollama-local",
				OnUnavailable:   OnUnavailableDeny,
				Reason:          "Flagged content is served locally.",
				ReasonCode:      string(ReasonFlaggedLocal),
			},
			NeedsTarget: true,
		},
		{
			Key:         "budget-band-cheaper",
			Title:       "Budget-pressured end-user to a cheaper model",
			Description: "When an end-user crosses 80% of a spend cap, swap to a cheaper same-shape model. Cost intent — fail-open, cooldown-respecting.",
			Rule: RuleInput{
				Name:          "budget-band-cheaper",
				When:          WhenInput{BudgetBandAtLeast: 0.8, BudgetBandSet: true},
				RouteToModel:  "claude-3-5-haiku-20241022",
				OnUnavailable: OnUnavailableFailOpen,
				ReasonCode:    string(ReasonBudgetBand),
			},
		},
		{
			Key:         "cohort-upstream",
			Title:       "Route a cohort to a dedicated upstream",
			Description: "End-users in a named cohort route to a dedicated declared upstream.",
			Rule: RuleInput{
				Name:            "beta-cohort-pool",
				When:            WhenInput{UserCohort: "beta"},
				RouteToUpstream: "beta-pool",
				ReasonCode:      string(ReasonCohortUpstream),
			},
			NeedsTarget: true,
		},
		{
			Key:         "overload-degrade",
			Title:       "Degrade effort under a large prompt",
			Description: "Under a large-prompt band, downshift effort to low (Anthropic). Best-effort; provider-compatible only.",
			Rule: RuleInput{
				Name:       "overload-degrade",
				When:       WhenInput{MinPromptTokens: 200000, Provider: "anthropic"},
				SetEffort:  "low",
				ReasonCode: string(ReasonOverloadDegrade),
			},
		},
	}
}
