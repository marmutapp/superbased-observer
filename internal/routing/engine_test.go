package routing

import (
	"sort"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// testSnapshot returns a snapshot over the seed tier table with a
// simple linear price: opus $10/M-in $50/M-out style ratios collapsed to
// per-token multipliers so savings are easy to assert.
func testSnapshot() *Snapshot {
	prices := map[string]struct{ in, out, cacheWrite float64 }{
		"claude-opus-4-8":   {5, 25, 6.25},
		"claude-sonnet-4-6": {3, 15, 3.75},
		"claude-haiku-4-5":  {1, 5, 1.25},
	}
	price := func(model string, u PromptUsage) (float64, bool) {
		p, ok := prices[model]
		if !ok {
			return 0, false
		}
		return (float64(u.Input)*p.in + float64(u.Output)*p.out + float64(u.CacheCreation)*p.cacheWrite) / 1e6, true
	}
	return &Snapshot{Price: price, Tiers: NewTierResolver().Table()}
}

// readOnlyInput is an opus read_only turn with observed usage.
func readOnlyInput() DecisionInput {
	return DecisionInput{
		Shape: TurnShape{Model: "claude-opus-4-8", MessageCount: 8, ToolUseCount: 4},
		Session: SessionState{
			TurnsSinceSwitch: -1,
			RecentActions: []ActionSignal{
				{Type: models.ActionReadFile, Success: true},
				{Type: models.ActionSearchText, Success: true},
			},
		},
		ObservedUsage: &PromptUsage{Input: 100_000, Output: 10_000},
	}
}

func valuePolicy(t *testing.T) Policy {
	t.Helper()
	p, ok := TemplateByName("value")
	if !ok {
		t.Fatal("value template missing")
	}
	return p
}

// TestDecide_DownshiftHappyPath pins the core decision: an opus
// read_only turn under "value" reroutes to the same-shape haiku
// representative with the rule's reason and net positive savings.
func TestDecide_DownshiftHappyPath(t *testing.T) {
	t.Parallel()
	d := Decide(valuePolicy(t), testSnapshot(), readOnlyInput())
	if !d.Changed || d.SelectedModel != "claude-haiku-4-5" {
		t.Fatalf("decision = %+v, want change to claude-haiku-4-5", d)
	}
	if d.TurnKind != TurnReadOnly || d.RuleName != "read_only_overpowered" {
		t.Errorf("kind/rule = %s/%s", d.TurnKind, d.RuleName)
	}
	if len(d.ReasonCodes) != 1 || d.ReasonCodes[0] != ReasonOverpoweredRead {
		t.Errorf("reasons = %v", d.ReasonCodes)
	}
	// gross = (100k×5 + 10k×25)/1e6 − (100k×1 + 10k×5)/1e6 = 0.75 − 0.15 = 0.60
	if d.EstSavingsUSD < 0.59 || d.EstSavingsUSD > 0.61 {
		t.Errorf("savings = %v, want ≈0.60", d.EstSavingsUSD)
	}
	if d.EstimateVersion != EstimateVersion || d.PolicyName != "value" || d.PolicyHash == "" {
		t.Errorf("attribution fields = %+v", d)
	}
}

// TestDecide_FailOpenGuards walks the fail-open rows: nil snapshot, nil
// tier table, empty model — original model passes through with
// fail_open (§R9.2, G7).
func TestDecide_FailOpenGuards(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		snap *Snapshot
		in   DecisionInput
	}{
		{"nil_snapshot", nil, readOnlyInput()},
		{"nil_tiers", &Snapshot{}, readOnlyInput()},
		{"empty_model", testSnapshot(), DecisionInput{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := Decide(valuePolicy(t), tc.snap, tc.in)
			if d.Changed || d.SelectedModel != d.OriginalModel {
				t.Errorf("fail-open mutated the model: %+v", d)
			}
			if len(d.ReasonCodes) != 1 || d.ReasonCodes[0] != ReasonFailOpen {
				t.Errorf("reasons = %v, want [fail_open]", d.ReasonCodes)
			}
		})
	}
}

// TestDecide_UnknownTurnKindNeverRoutes pins §R8.3: a degraded
// classification yields no change.
func TestDecide_UnknownTurnKindNeverRoutes(t *testing.T) {
	t.Parallel()
	in := readOnlyInput()
	in.Session.RecentActions = nil // empty window → unknown
	d := Decide(valuePolicy(t), testSnapshot(), in)
	if d.Changed {
		t.Fatalf("unknown turn rerouted: %+v", d)
	}
	if d.TurnKind != TurnUnknown || d.ReasonCodes[0] != ReasonUnknownTurnKind {
		t.Errorf("kind/reasons = %s/%v", d.TurnKind, d.ReasonCodes)
	}
}

// TestDecide_UnclassifiedModelRefused pins §R7.1: a model the tier
// table cannot place is never reasoned about.
func TestDecide_UnclassifiedModelRefused(t *testing.T) {
	t.Parallel()
	in := readOnlyInput()
	in.Shape.Model = "mystery-model-9000"
	d := Decide(valuePolicy(t), testSnapshot(), in)
	if d.Changed || d.ReasonCodes[0] != ReasonUnclassifiedModel {
		t.Errorf("decision = %+v, want unclassified_model hold", d)
	}
}

// TestDecide_NoRuleMatch pins the quiet default: a kind no rule targets
// (edit under "value") passes through with no rule and no reasons.
func TestDecide_NoRuleMatch(t *testing.T) {
	t.Parallel()
	in := readOnlyInput()
	in.Session.RecentActions = []ActionSignal{{Type: models.ActionEditFile, Success: true}}
	d := Decide(valuePolicy(t), testSnapshot(), in)
	if d.Changed || d.RuleName != "" || len(d.ReasonCodes) != 0 {
		t.Errorf("decision = %+v, want untouched edit turn", d)
	}
	if d.TurnKind != TurnEdit {
		t.Errorf("kind = %s, want edit", d.TurnKind)
	}
}

// TestDecide_NoRoutePin pins the explicit exemption: plan under
// plan-exec records no_route + phase_pin and never moves.
func TestDecide_NoRoutePin(t *testing.T) {
	t.Parallel()
	p, _ := TemplateByName("plan-exec")
	in := readOnlyInput()
	in.Session.ClientPhase = "plan"
	d := Decide(p, testSnapshot(), in)
	if d.Changed {
		t.Fatalf("pinned plan turn moved: %+v", d)
	}
	if d.RuleName != "plan_pin" || len(d.ReasonCodes) != 2 ||
		d.ReasonCodes[0] != ReasonNoRoute || d.ReasonCodes[1] != ReasonPhasePin {
		t.Errorf("rule/reasons = %s/%v", d.RuleName, d.ReasonCodes)
	}
}

// TestDecide_NoCandidateSameShape pins §R11.4: a model with no
// same-shape representative in the target tier holds with no_candidate.
// grok-4.3 is sonnet-class but ShapeUnknown — no candidate exists.
func TestDecide_NoCandidateSameShape(t *testing.T) {
	t.Parallel()
	in := readOnlyInput()
	in.Shape.Model = "grok-4.3"
	d := Decide(valuePolicy(t), testSnapshot(), in)
	if d.Changed || d.ReasonCodes[0] != ReasonNoCandidate {
		t.Errorf("decision = %+v, want no_candidate hold", d)
	}
}

// TestDecide_StickinessHold pins the §R13 coherence floor: a proposed
// switch within min-turns-between-switches holds.
func TestDecide_StickinessHold(t *testing.T) {
	t.Parallel()
	in := readOnlyInput()
	in.Session.TurnsSinceSwitch = 2 // value template floor is 5
	d := Decide(valuePolicy(t), testSnapshot(), in)
	if d.Changed || d.ReasonCodes[0] != ReasonStickinessHold {
		t.Errorf("decision = %+v, want stickiness hold", d)
	}
}

// TestDecide_CacheHold pins the headline §R13 economics: when the warm
// prefix is worth more than the switch saves, the engine stays put and
// says why, with the negative net on the row.
func TestDecide_CacheHold(t *testing.T) {
	t.Parallel()
	in := readOnlyInput()
	// Tiny turn (gross ≈ $0.0006) behind a huge warm prefix: forfeit =
	// 1M × $1.25/M = $1.25 at the candidate's cache-write rate.
	in.ObservedUsage = &PromptUsage{Input: 100, Output: 10}
	in.Session.PriorCacheReadTokens = 1_000_000
	d := Decide(valuePolicy(t), testSnapshot(), in)
	if d.Changed {
		t.Fatalf("cache-held turn moved: %+v", d)
	}
	if d.ReasonCodes[0] != ReasonCacheHold {
		t.Errorf("reasons = %v, want [cache_hold]", d.ReasonCodes)
	}
	if d.CacheForfeitUSD < 1.24 || d.CacheForfeitUSD > 1.26 {
		t.Errorf("forfeit = %v, want ≈1.25", d.CacheForfeitUSD)
	}
	if d.EstSavingsUSD >= 0 {
		t.Errorf("net savings = %v, want negative (that's why we held)", d.EstSavingsUSD)
	}
}

// TestDecide_NoDollarClaimWithoutUsage pins estimate honesty: without
// observed usage the switch still happens (the rule is sound) but no
// dollar figures are invented — and no cache-hold either, since a hold
// is itself a dollar claim.
func TestDecide_NoDollarClaimWithoutUsage(t *testing.T) {
	t.Parallel()
	in := readOnlyInput()
	in.ObservedUsage = nil
	in.Session.PriorCacheReadTokens = 1_000_000
	d := Decide(valuePolicy(t), testSnapshot(), in)
	if !d.Changed || d.SelectedModel != "claude-haiku-4-5" {
		t.Fatalf("decision = %+v, want downshift", d)
	}
	if d.EstSavingsUSD != 0 || d.CacheForfeitUSD != 0 {
		t.Errorf("invented dollars: savings %v forfeit %v", d.EstSavingsUSD, d.CacheForfeitUSD)
	}
}

// TestDecide_Deterministic pins §R9.3 end to end.
func TestDecide_Deterministic(t *testing.T) {
	t.Parallel()
	p := valuePolicy(t)
	snap := testSnapshot()
	in := readOnlyInput()
	first := Decide(p, snap, in)
	for i := 0; i < 10; i++ {
		got := Decide(p, snap, in)
		if got.SelectedModel != first.SelectedModel || got.EstSavingsUSD != first.EstSavingsUSD ||
			got.PolicyHash != first.PolicyHash || got.RuleName != first.RuleName {
			t.Fatalf("run %d diverged: %+v != %+v", i, got, first)
		}
	}
}

// TestPolicyHash_ContentSensitive pins §R6.6 attribution: the hash
// changes with rule content and is stable across calls.
func TestPolicyHash_ContentSensitive(t *testing.T) {
	t.Parallel()
	p1, _ := TemplateByName("value")
	p2, _ := TemplateByName("value")
	if p1.Hash() != p2.Hash() {
		t.Error("hash not stable across template instantiations")
	}
	p2.Rules[0].When.TierAtLeast = TierOpusClass
	if p1.Hash() == p2.Hash() {
		t.Error("hash blind to rule content change")
	}
	frugal, _ := TemplateByName("frugal")
	if p1.Hash() == frugal.Hash() {
		t.Error("distinct templates share a hash")
	}
}

// TestTemplates_Registry pins the shipped template set and that every
// rule's reason code is in the closed enum.
func TestTemplates_Registry(t *testing.T) {
	t.Parallel()
	names := TemplateNames()
	want := []string{"value", "frugal", "fast", "strict-privacy", "plan-exec", "enterprise-default"}
	if len(names) != len(want) {
		t.Fatalf("templates = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("template[%d] = %q, want %q", i, names[i], want[i])
		}
	}
	known := map[ReasonCode]bool{}
	for _, rc := range KnownReasonCodes() {
		known[rc] = true
	}
	for _, p := range Templates() {
		for _, r := range p.Rules {
			if !known[r.Reason] {
				t.Errorf("policy %s rule %s uses unknown reason %q", p.Name, r.Name, r.Reason)
			}
		}
	}
}

// TestRuleWhen_Matches covers each matcher dimension (one row per
// clause field).
func TestRuleWhen_Matches(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	_ = no
	cases := []struct {
		name string
		when RuleWhen
		kind TurnKind
		tier Tier
		in   DecisionInput
		want bool
	}{
		{"empty_matches_all", RuleWhen{}, TurnEdit, TierOpusClass, DecisionInput{}, true},
		{"kind_in_set", RuleWhen{TurnKinds: []TurnKind{TurnReadOnly, TurnEdit}}, TurnEdit, TierOpusClass, DecisionInput{}, true},
		{"kind_not_in_set", RuleWhen{TurnKinds: []TurnKind{TurnReadOnly}}, TurnEdit, TierOpusClass, DecisionInput{}, false},
		{"tier_at_least_pass", RuleWhen{TierAtLeast: TierSonnetClass}, TurnEdit, TierOpusClass, DecisionInput{}, true},
		{"tier_at_least_fail", RuleWhen{TierAtLeast: TierSonnetClass}, TurnEdit, TierHaikuClass, DecisionInput{}, false},
		{"max_tools_pass", RuleWhen{MaxToolUses: 3}, TurnEdit, TierOpusClass, DecisionInput{Shape: TurnShape{ToolUseCount: 2}}, true},
		{"max_tools_fail", RuleWhen{MaxToolUses: 3}, TurnEdit, TierOpusClass, DecisionInput{Shape: TurnShape{ToolUseCount: 4}}, false},
		{"sidechain_match", RuleWhen{Sidechain: &yes}, TurnSubagent, TierOpusClass, DecisionInput{Session: SessionState{IsSidechain: true}}, true},
		{"sidechain_mismatch", RuleWhen{Sidechain: &yes}, TurnEdit, TierOpusClass, DecisionInput{}, false},
		{"phase_match", RuleWhen{Phase: "plan"}, TurnPlan, TierOpusClass, DecisionInput{Session: SessionState{ClientPhase: "plan"}}, true},
		{"phase_mismatch", RuleWhen{Phase: "plan"}, TurnEdit, TierOpusClass, DecisionInput{}, false},
		{"model_glob_match", RuleWhen{ModelGlob: "claude-opus-*"}, TurnEdit, TierOpusClass, DecisionInput{Shape: TurnShape{Model: "claude-opus-4-8"}}, true},
		{"model_glob_mismatch", RuleWhen{ModelGlob: "gpt-*"}, TurnEdit, TierOpusClass, DecisionInput{Shape: TurnShape{Model: "claude-opus-4-8"}}, false},
		{"project_match", RuleWhen{Project: "acme"}, TurnEdit, TierOpusClass, DecisionInput{Project: "acme"}, true},
		{"project_mismatch", RuleWhen{Project: "acme"}, TurnEdit, TierOpusClass, DecisionInput{Project: "other"}, false},
		{"path_class_hit", RuleWhen{PathClass: "secrets"}, TurnEdit, TierOpusClass, DecisionInput{PathClassHits: []string{"secrets"}}, true},
		{"path_class_miss", RuleWhen{PathClass: "secrets"}, TurnEdit, TierOpusClass, DecisionInput{}, false},
		{"session_age_min_pass", RuleWhen{SessionAgeTurnsMin: 5}, TurnEdit, TierOpusClass, DecisionInput{Session: SessionState{SessionAgeTurns: 6}}, true},
		{"session_age_min_fail", RuleWhen{SessionAgeTurnsMin: 5}, TurnEdit, TierOpusClass, DecisionInput{Session: SessionState{SessionAgeTurns: 4}}, false},
		{"session_age_max_pass", RuleWhen{SessionAgeTurnsMax: 5}, TurnEdit, TierOpusClass, DecisionInput{Session: SessionState{SessionAgeTurns: 5}}, true},
		{"session_age_max_fail", RuleWhen{SessionAgeTurnsMax: 5}, TurnEdit, TierOpusClass, DecisionInput{Session: SessionState{SessionAgeTurns: 6}}, false},
		{"min_prompt_tokens_pass", RuleWhen{MinPromptTokens: 1000}, TurnEdit, TierOpusClass, DecisionInput{Shape: TurnShape{PromptTokens: 2000}}, true},
		{"min_prompt_tokens_fail", RuleWhen{MinPromptTokens: 1000}, TurnEdit, TierOpusClass, DecisionInput{Shape: TurnShape{PromptTokens: 500}}, false},
		{"max_prompt_tokens_pass", RuleWhen{MaxPromptTokens: 1000}, TurnEdit, TierOpusClass, DecisionInput{Shape: TurnShape{PromptTokens: 800}}, true},
		{"max_prompt_tokens_fail", RuleWhen{MaxPromptTokens: 1000}, TurnEdit, TierOpusClass, DecisionInput{Shape: TurnShape{PromptTokens: 1200}}, false},
		{"entitlement_match", RuleWhen{Entitlement: EntitlementSubscription}, TurnEdit, TierOpusClass, DecisionInput{Entitlement: EntitlementSubscription}, true},
		{"entitlement_mismatch", RuleWhen{Entitlement: EntitlementAPIKey}, TurnEdit, TierOpusClass, DecisionInput{Entitlement: EntitlementSubscription}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.when.Matches(MatchContext{Kind: tc.kind, Tier: tc.tier, In: tc.in}); got != tc.want {
				t.Errorf("Matches = %v, want %v", got, tc.want)
			}
		})
	}

	// The budget-band matcher reads the snapshot-derived burn, not the
	// input — covered separately.
	t.Run("budget_band_at_least", func(t *testing.T) {
		t.Parallel()
		w := RuleWhen{BudgetBandAtLeast: 0.8}
		if w.Matches(MatchContext{Kind: TurnEdit, Tier: TierOpusClass, BudgetBurnMax: 0.85}) != true {
			t.Error("burn 0.85 should match band 0.8")
		}
		if w.Matches(MatchContext{Kind: TurnEdit, Tier: TierOpusClass, BudgetBurnMax: 0.5}) != false {
			t.Error("burn 0.5 should not match band 0.8")
		}
	})
}

// TestDecideHotPathBudget pins the §R9.2 / §R25 hot-path contract:
// p99 decision latency < 5 ms. Decide is pure in-memory work
// (classify + tier-place + pipeline + rule walk), so this passes with
// orders-of-magnitude headroom — the test exists to catch an accidental
// I/O or quadratic-blowup regression on the path, not to be tight.
func TestDecideHotPathBudget(t *testing.T) {
	p := valuePolicy(t)
	snap := testSnapshot()
	snap.BudgetBurn = []BudgetBurnState{{Scope: "global", LimitUSD: 100, SpentUSD: 40, Window: "week", Bands: DefaultBudgetBands}}
	snap.Health = map[string]HealthState{"claude-sonnet-4-6": HealthDegraded}
	in := readOnlyInput()

	const n = 5000
	durations := make([]time.Duration, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		_ = Decide(p, snap, in)
		durations[i] = time.Since(start)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p99 := durations[n*99/100]
	if p99 >= 5*time.Millisecond {
		t.Fatalf("Decide p99 = %v, budget is < 5ms (§R25)", p99)
	}
}

// TestDecide_CacheHoldSuggestsEffort pins the §R6.5 borderline-case
// default: a cache-held switch carries the zero-cache-loss effort
// downshift suggestion.
func TestDecide_CacheHoldSuggestsEffort(t *testing.T) {
	t.Parallel()
	in := readOnlyInput()
	in.ObservedUsage = &PromptUsage{Input: 100, Output: 10}
	in.Session.PriorCacheReadTokens = 1_000_000
	d := Decide(valuePolicy(t), testSnapshot(), in)
	if d.Changed {
		t.Fatalf("cache-held turn moved: %+v", d)
	}
	if d.SetEffort != EffortLow {
		t.Errorf("SetEffort = %q, want %q (the borderline suggestion)", d.SetEffort, EffortLow)
	}
	if !hasReason(d.ReasonCodes, ReasonCacheHold) || !hasReason(d.ReasonCodes, ReasonEffortDownshift) {
		t.Errorf("reasons = %v, want cache_hold + effort_downshift", d.ReasonCodes)
	}
}

// TestDecide_EffortRuleAction pins the set_effort rule action: the
// model is untouched, the decision carries the effort level.
func TestDecide_EffortRuleAction(t *testing.T) {
	t.Parallel()
	p, issues := Compile(PolicySpec{
		Policy: "custom",
		Rules: []RuleSpec{{
			Name:   "effort_reads",
			When:   WhenSpec{TurnKind: "read_only", TierAtLeast: "sonnet-class"},
			Action: ActionSpec{SetEffort: "low", Reason: "overpowered_read"},
		}},
	})
	if LintHasErrors(issues) {
		t.Fatalf("lint: %+v", issues)
	}
	d := Decide(p, testSnapshot(), readOnlyInput())
	if d.Changed || d.SelectedModel != d.OriginalModel {
		t.Fatalf("effort rule changed the model: %+v", d)
	}
	if d.SetEffort != "low" || !hasReason(d.ReasonCodes, ReasonEffortDownshift) {
		t.Errorf("decision = %+v, want effort-only", d)
	}
}

// TestDecide_EscalatedKindHolds pins §R7.4 at the engine: an escalated
// turn-kind never downshifts — the original model passes through with
// reason=escalation, even though a rule matches.
func TestDecide_EscalatedKindHolds(t *testing.T) {
	t.Parallel()
	in := readOnlyInput()
	in.Session.EscalatedKinds = []TurnKind{TurnReadOnly}
	d := Decide(valuePolicy(t), testSnapshot(), in)
	if d.Changed {
		t.Fatalf("escalated kind downshifted: %+v", d)
	}
	if len(d.ReasonCodes) != 1 || d.ReasonCodes[0] != ReasonEscalation {
		t.Errorf("reasons = %v, want [escalation]", d.ReasonCodes)
	}
	// A DIFFERENT kind under escalation does not hold this one.
	in.Session.EscalatedKinds = []TurnKind{TurnHousekeeping}
	d = Decide(valuePolicy(t), testSnapshot(), in)
	if !d.Changed {
		t.Errorf("unrelated escalation held the turn: %+v", d)
	}
}
