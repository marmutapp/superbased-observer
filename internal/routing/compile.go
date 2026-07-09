package routing

// Policy compiler (§R6.1, §R6.6): the boundary hands a PolicySpec —
// the plain-Go mirror of the [routing] config section (the PromptUsage
// pattern: this package never imports config) — and Compile expands
// the named template, appends compiled custom rules AFTER the
// expansion (§R6.3), attaches the §R14–R16 groups, and lints the
// result. The compiled Policy's Hash() therefore covers the full
// effective content, not just the template name (§R6.6).

// PolicySpec is the boundary-supplied policy declaration.
type PolicySpec struct {
	// Policy names a shipped template or "custom" (rules only).
	Policy string
	// Bases optionally overrides the basis list (templates carry their
	// own; custom policies default to defaultBases()). Listing "phase"
	// is rule sugar (§R6.2): the standard phase-pin rules are
	// prepended at compile time.
	Bases []string
	// MinTurnsBetweenSwitches overrides the template's coherence floor
	// when > 0; 0 keeps the template default.
	MinTurnsBetweenSwitches int
	// RespectCache gates §R13 cache-priced switching.
	RespectCache bool
	// PathClasses is the [routing.path_classes] vocabulary.
	PathClasses map[string][]string
	// PrivacyRules mirror [[routing.privacy.rules]] (§R16).
	PrivacyRules []PrivacyRuleSpec
	// BudgetScopes mirror [[routing.budget.scopes]] (§R14).
	BudgetScopes []BudgetScopeSpec
	// RateLimitEnabled / RateLimitHeadroomPct mirror
	// [routing.rate_limit_window] (§R15).
	RateLimitEnabled     bool
	RateLimitHeadroomPct int
	// Fallbacks mirror [routing.reliability.fallbacks] (§R12.1).
	Fallbacks map[string][]string
	// Rules are the custom rows, appended after template expansion.
	Rules []RuleSpec
}

// PrivacyRuleSpec mirrors config's RoutingPrivacyRuleConfig.
type PrivacyRuleSpec struct {
	Project        string
	PathClass      string
	LocalOnly      bool
	DenyProviders  []string
	AllowProviders []string
}

// BudgetScopeSpec mirrors config's RoutingBudgetScopeConfig.
type BudgetScopeSpec struct {
	Scope     string
	LimitUSD  float64
	Window    string
	Bands     []float64
	Exhausted string
}

// RuleSpec mirrors config's RoutingRuleConfig: plain strings in, typed
// Rule out.
type RuleSpec struct {
	Name   string
	When   WhenSpec
	Action ActionSpec
}

// WhenSpec mirrors RoutingRuleWhenConfig.
type WhenSpec struct {
	TurnKind           string
	TurnKinds          []string
	Phase              string
	TierAtLeast        string
	Model              string
	Project            string
	PathClass          string
	SessionAgeTurnsMin int
	SessionAgeTurnsMax int
	Sidechain          *bool
	MaxTools           int
	MinPromptTokens    int64
	MaxPromptTokens    int64
	BudgetBandAtLeast  float64
	Entitlement        string
}

// ActionSpec mirrors RoutingRuleActionConfig.
type ActionSpec struct {
	RouteToTier      string
	RouteToModel     string
	PinTier          string
	NoRoute          bool
	SetEffort        string
	SetFallbackChain []string
	DenyProviders    []string
	AllowProviders   []string
	Reason           string
}

// DefaultBudgetBands are the §R14 burn bands applied when a scope
// declares none.
var DefaultBudgetBands = []float64{0.5, 0.8, 0.95}

// Compile expands spec into the effective Policy and lints it (§R6.6).
// Compilation never fails: an unknown template or malformed rule
// becomes a lint finding on an otherwise-degraded policy (empty rule
// table → every decision is the quiet no-change default — fail-open,
// G7). Callers gate promotion on LintHasErrors, not on an error
// return.
func Compile(spec PolicySpec) (Policy, []LintIssue) {
	var issues []LintIssue

	var p Policy
	switch {
	case spec.Policy == "custom":
		p = Policy{
			Name:        "custom",
			Description: "Custom policy compiled from [[routing.rules]].",
			Bases:       defaultBases(),
			Floors:      defaultFloors(),
		}
	default:
		tpl, ok := TemplateByName(spec.Policy)
		if !ok {
			issues = append(issues, LintIssue{
				Check: "known_template", Severity: LintError,
				Message: "unknown policy template " + spec.Policy + " — compiled with an empty rule table (fail-open)",
			})
			p = Policy{Name: spec.Policy, Bases: defaultBases(), Floors: defaultFloors()}
		} else {
			p = tpl
		}
	}

	if spec.MinTurnsBetweenSwitches > 0 {
		p.MinTurnsBetweenSwitches = spec.MinTurnsBetweenSwitches
	}
	p.RespectCache = spec.RespectCache
	p.PathClasses = spec.PathClasses
	for _, pr := range spec.PrivacyRules {
		p.PrivacyRules = append(p.PrivacyRules, PrivacyRule(pr))
	}
	for _, bs := range spec.BudgetScopes {
		scope := BudgetScope(bs)
		if len(scope.Bands) == 0 {
			scope.Bands = DefaultBudgetBands
		}
		if scope.Exhausted == "" {
			scope.Exhausted = BudgetAdviseOnly
		}
		p.BudgetScopes = append(p.BudgetScopes, scope)
	}
	p.RateLimit = RateLimitPolicy{Enabled: spec.RateLimitEnabled, HeadroomPct: spec.RateLimitHeadroomPct}
	p.Fallbacks = spec.Fallbacks

	if len(spec.Bases) > 0 {
		p.Bases = spec.Bases
	}

	// Custom rules compile and append AFTER template expansion (§R6.3):
	// templates stay inspectable; custom rows extend, never splice.
	for _, rs := range spec.Rules {
		p.Rules = append(p.Rules, compileRule(rs))
	}

	expandPhaseBasis(&p)

	issues = append(issues, LintPolicy(p)...)
	return p, issues
}

// expandPhaseBasis is the §R6.2 "phase" rule sugar: a policy listing
// the phase basis gets the standard opusplan-style phase rules
// PREPENDED (phase intents outrank cost rules) — and the sugar name is
// rewritten out of the basis list, since phase has no runtime
// evaluator. Idempotent: an existing plan-pin rule suppresses the
// expansion.
func expandPhaseBasis(p *Policy) {
	idx := -1
	for i, b := range p.Bases {
		if b == BasisPhase {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	p.Bases = append(append([]string{}, p.Bases[:idx]...), p.Bases[idx+1:]...)
	for _, r := range p.Rules {
		if len(r.When.TurnKinds) == 1 && r.When.TurnKinds[0] == TurnPlan {
			return // a plan rule already exists; nothing to expand
		}
	}
	phaseRules := []Rule{{
		Name:   "phase_plan_pin",
		When:   RuleWhen{TurnKinds: []TurnKind{TurnPlan}},
		Action: RuleAction{NoRoute: true},
		Reason: ReasonPhasePin,
	}}
	p.Rules = append(phaseRules, p.Rules...)
}

// compileRule converts one plain-string RuleSpec into a typed Rule.
// String enums convert verbatim — lint validates membership afterwards
// so a typo'd tier or kind is a finding, not silent coercion.
func compileRule(rs RuleSpec) Rule {
	kinds := make([]TurnKind, 0, len(rs.When.TurnKinds)+1)
	if rs.When.TurnKind != "" {
		kinds = append(kinds, TurnKind(rs.When.TurnKind))
	}
	for _, k := range rs.When.TurnKinds {
		kinds = append(kinds, TurnKind(k))
	}
	reason := ReasonCode(rs.Action.Reason)
	if reason == "" {
		reason = ReasonCustomRule
	}
	return Rule{
		Name: rs.Name,
		When: RuleWhen{
			TurnKinds:          kinds,
			Phase:              rs.When.Phase,
			TierAtLeast:        Tier(rs.When.TierAtLeast),
			ModelGlob:          rs.When.Model,
			Project:            rs.When.Project,
			PathClass:          rs.When.PathClass,
			SessionAgeTurnsMin: rs.When.SessionAgeTurnsMin,
			SessionAgeTurnsMax: rs.When.SessionAgeTurnsMax,
			MaxToolUses:        rs.When.MaxTools,
			MinPromptTokens:    rs.When.MinPromptTokens,
			MaxPromptTokens:    rs.When.MaxPromptTokens,
			BudgetBandAtLeast:  rs.When.BudgetBandAtLeast,
			Entitlement:        rs.When.Entitlement,
			Sidechain:          rs.When.Sidechain,
		},
		Action: RuleAction{
			RouteToTier:      Tier(rs.Action.RouteToTier),
			RouteToModel:     rs.Action.RouteToModel,
			PinTier:          Tier(rs.Action.PinTier),
			NoRoute:          rs.Action.NoRoute,
			SetEffort:        rs.Action.SetEffort,
			SetFallbackChain: rs.Action.SetFallbackChain,
			DenyProviders:    rs.Action.DenyProviders,
			AllowProviders:   rs.Action.AllowProviders,
		},
		Reason: reason,
	}
}
