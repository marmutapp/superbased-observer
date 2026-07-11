package routing

import "fmt"

// SubagentEvidence is the boundary-assembled observed profile of one
// named sub-agent persona (§R10.3): sidechain action mix, volume, and
// the dominant model its turns ran on. No prompts, no targets — counts
// and a persona id only.
type SubagentEvidence struct {
	// Name is the persona id (e.g. "code-reviewer").
	Name string `json:"name"`
	// Model is the dominant model observed for the sessions this
	// persona ran in; empty when unattributable.
	Model string `json:"model"`
	// Sessions counts distinct sessions the persona appeared in.
	Sessions int `json:"sessions"`
	// Actions is the total sidechain actions attributed to the persona;
	// the breakdown follows. Failures counts unsuccessful tool actions.
	Actions   int64 `json:"actions"`
	Reads     int64 `json:"reads"`
	Mutations int64 `json:"mutations"`
	Commands  int64 `json:"commands"`
	Failures  int64 `json:"failures"`
}

// FailureRate returns failures over attributed actions (0 when empty).
func (e SubagentEvidence) FailureRate() float64 {
	if e.Actions == 0 {
		return 0
	}
	return float64(e.Failures) / float64(e.Actions)
}

// SubagentRecommendation is one evidence-backed per-sub-agent model
// suggestion (§R10.3). P0 is read-only: nothing writes agent config —
// the recommendation surfaces through the Model Value Report and the
// dashboard; `observer routing apply` is P2.
type SubagentRecommendation struct {
	Name         string `json:"name"`
	CurrentModel string `json:"current_model,omitempty"`
	CurrentTier  Tier   `json:"current_tier,omitempty"`
	// SuggestedModel/Tier are set only when the evidence supports a
	// move; an empty suggestion means "keep" with the rationale below.
	SuggestedModel string `json:"suggested_model,omitempty"`
	SuggestedTier  Tier   `json:"suggested_tier,omitempty"`
	// Reason is the closed-enum code; Rationale is the human sentence
	// with the evidence inline.
	Reason    ReasonCode       `json:"reason"`
	Rationale string           `json:"rationale"`
	Evidence  SubagentEvidence `json:"evidence"`
}

// subagentRule is one row of the recommendation table, walked top-down;
// first match wins (§24.5). One test case per row.
type subagentRule struct {
	name string
	when func(ev SubagentEvidence, tier Tier, minActions int64) bool
	make func(ev SubagentEvidence, tier Tier, tiers *TierTable) SubagentRecommendation
}

// subagentFailureRateCeiling is the failure rate above which the
// emitter refuses to suggest a downshift — a struggling persona is
// escalation evidence, not downshift evidence (§R7.4).
const subagentFailureRateCeiling = 0.10

var subagentRules = []subagentRule{
	{
		name: "insufficient_evidence",
		when: func(ev SubagentEvidence, _ Tier, minActions int64) bool {
			return ev.Actions < minActions
		},
		make: func(ev SubagentEvidence, tier Tier, _ *TierTable) SubagentRecommendation {
			return SubagentRecommendation{
				Name: ev.Name, CurrentModel: ev.Model, CurrentTier: tier,
				Reason: ReasonInsufficientEvidence,
				Rationale: fmt.Sprintf("only %d sidechain actions observed across %d session(s) — below the evidence floor; no recommendation",
					ev.Actions, ev.Sessions),
				Evidence: ev,
			}
		},
	},
	{
		name: "unplaceable_model",
		when: func(ev SubagentEvidence, tier Tier, _ int64) bool {
			return ev.Model == "" || tier == TierUnclassified
		},
		make: func(ev SubagentEvidence, tier Tier, _ *TierTable) SubagentRecommendation {
			return SubagentRecommendation{
				Name: ev.Name, CurrentModel: ev.Model, CurrentTier: tier,
				Reason:    ReasonUnclassifiedModel,
				Rationale: "the persona's model could not be attributed or placed in a tier — no recommendation",
				Evidence:  ev,
			}
		},
	},
	{
		name: "failure_heavy_hold",
		when: func(ev SubagentEvidence, _ Tier, _ int64) bool {
			return ev.FailureRate() > subagentFailureRateCeiling
		},
		make: func(ev SubagentEvidence, tier Tier, _ *TierTable) SubagentRecommendation {
			return SubagentRecommendation{
				Name: ev.Name, CurrentModel: ev.Model, CurrentTier: tier,
				Reason: ReasonEscalation,
				Rationale: fmt.Sprintf("%.0f%% of its %d actions failed — escalation evidence, not downshift evidence; keep the current model",
					ev.FailureRate()*100, ev.Actions),
				Evidence: ev,
			}
		},
	},
	{
		name: "non_mutating_profile_downshift",
		when: func(ev SubagentEvidence, tier Tier, _ int64) bool {
			return ev.Mutations == 0 && ev.Reads > 0 && tier.Rank() > TierHaikuClass.Rank()
		},
		make: func(ev SubagentEvidence, tier Tier, tiers *TierTable) SubagentRecommendation {
			rec := SubagentRecommendation{
				Name: ev.Name, CurrentModel: ev.Model, CurrentTier: tier,
				Reason:   ReasonOverpoweredSubagent,
				Evidence: ev,
			}
			candidate, ok := tiers.Representative(ShapeForModel(ev.Model), TierHaikuClass)
			if !ok {
				rec.Reason = ReasonNoCandidate
				rec.Rationale = "non-mutating profile would downshift, but no same-shape candidate exists in the target tier"
				return rec
			}
			rec.SuggestedModel = candidate
			rec.SuggestedTier = TierHaikuClass
			rec.Rationale = fmt.Sprintf("ran %d sidechain actions (%d reads, 0 mutations, %d commands, %.0f%% failures) across %d session(s) on %s — a read-shaped profile; %s-class shows no need for a %s-class model here",
				ev.Actions, ev.Reads, ev.Commands, ev.FailureRate()*100, ev.Sessions, ev.Model, TierHaikuClass, tier)
			return rec
		},
	},
	{
		name: "already_at_floor_keep",
		when: func(ev SubagentEvidence, tier Tier, _ int64) bool {
			return ev.Mutations == 0 && ev.Reads > 0 && tier.Rank() <= TierHaikuClass.Rank()
		},
		make: func(ev SubagentEvidence, tier Tier, _ *TierTable) SubagentRecommendation {
			return SubagentRecommendation{
				Name: ev.Name, CurrentModel: ev.Model, CurrentTier: tier,
				Reason:    ReasonNoRoute,
				Rationale: fmt.Sprintf("read-shaped profile already runs at %s — nothing cheaper to suggest", tier),
				Evidence:  ev,
			}
		},
	},
	{
		name: "no_downshift_evidence_keep",
		when: func(SubagentEvidence, Tier, int64) bool { return true },
		make: func(ev SubagentEvidence, tier Tier, _ *TierTable) SubagentRecommendation {
			why := "the profile is not read-shaped"
			if ev.Mutations > 0 {
				why = fmt.Sprintf("the profile includes %d mutation(s)", ev.Mutations)
			}
			return SubagentRecommendation{
				Name: ev.Name, CurrentModel: ev.Model, CurrentTier: tier,
				Reason:    ReasonNoRoute,
				Rationale: why + " — no downshift evidence; keep the current model",
				Evidence:  ev,
			}
		},
	},
}

// RecommendSubagentModels walks each persona's evidence through the
// rule table (§R10.3). minActions ≤ 0 applies the §R7.2 default floor
// of 50. Deterministic; recommendations come back in input order.
func RecommendSubagentModels(tiers *TierTable, evidence []SubagentEvidence, minActions int64) []SubagentRecommendation {
	if minActions <= 0 {
		minActions = 50
	}
	out := make([]SubagentRecommendation, 0, len(evidence))
	for _, ev := range evidence {
		tier, _ := tiers.Lookup(ev.Model)
		for _, r := range subagentRules {
			if r.when(ev, tier, minActions) {
				out = append(out, r.make(ev, tier, tiers))
				break
			}
		}
	}
	return out
}

// SubagentRuleNames lists the emitter's rule rows in walk order, for
// tests and status surfaces.
func SubagentRuleNames() []string {
	out := make([]string, len(subagentRules))
	for i, r := range subagentRules {
		out[i] = r.name
	}
	return out
}
