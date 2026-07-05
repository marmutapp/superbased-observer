package routing

import (
	"fmt"
	"path"
)

// LintSeverity grades a lint finding.
type LintSeverity string

// Severities. Errors mean the policy can produce unsound decisions;
// warnings mean it can silently no-op.
const (
	LintError   LintSeverity = "error"
	LintWarning LintSeverity = "warning"
)

// LintIssue is one lint finding against a policy.
type LintIssue struct {
	Check    string       `json:"check"`
	RuleName string       `json:"rule,omitempty"`
	Severity LintSeverity `json:"severity"`
	Message  string       `json:"message"`
}

// lintCheck is one row of the lint table: a named check over a policy.
type lintCheck struct {
	name string
	run  func(Policy) []LintIssue
}

// lintChecks is the ordered check table (§R6.6 lint stage). One test
// case per row in lint_test.go.
var lintChecks = []lintCheck{
	{name: "unique_rule_names", run: lintUniqueRuleNames},
	{name: "action_exactly_one", run: lintActionExactlyOne},
	{name: "known_turn_kinds", run: lintKnownTurnKinds},
	{name: "known_reason", run: lintKnownReason},
	{name: "targetable_tier", run: lintTargetableTier},
	{name: "downshift_only", run: lintDownshiftOnly},
	{name: "known_when_tier", run: lintKnownWhenTier},
	{name: "model_glob_valid", run: lintModelGlobValid},
	{name: "known_entitlement", run: lintKnownEntitlement},
	{name: "known_effort", run: lintKnownEffort},
	{name: "known_basis", run: lintKnownBasis},
	{name: "path_class_ref", run: lintPathClassRef},
	{name: "floor_tiers_known", run: lintFloorTiersKnown},
	{name: "shadowed_rule", run: lintShadowedRule},
}

// LintPolicy validates a policy's rule table and returns every finding,
// in check-table order. An empty result means the policy is clean.
func LintPolicy(p Policy) []LintIssue {
	var out []LintIssue
	for _, c := range lintChecks {
		out = append(out, c.run(p)...)
	}
	return out
}

// LintHasErrors reports whether any finding is error-severity.
func LintHasErrors(issues []LintIssue) bool {
	for _, i := range issues {
		if i.Severity == LintError {
			return true
		}
	}
	return false
}

func lintUniqueRuleNames(p Policy) []LintIssue {
	var out []LintIssue
	seen := map[string]bool{}
	for _, r := range p.Rules {
		if r.Name == "" {
			out = append(out, LintIssue{
				Check: "unique_rule_names", Severity: LintError,
				Message: "rule with empty name — decision rows cannot attribute it",
			})
			continue
		}
		if seen[r.Name] {
			out = append(out, LintIssue{
				Check: "unique_rule_names", RuleName: r.Name, Severity: LintError,
				Message: fmt.Sprintf("duplicate rule name %q", r.Name),
			})
		}
		seen[r.Name] = true
	}
	return out
}

func lintActionExactlyOne(p Policy) []LintIssue {
	var out []LintIssue
	for _, r := range p.Rules {
		n := 0
		if r.Action.RouteToTier != "" {
			n++
		}
		if r.Action.RouteToModel != "" {
			n++
		}
		if r.Action.PinTier != "" {
			n++
		}
		if r.Action.NoRoute {
			n++
		}
		if r.Action.SetEffort != "" {
			n++
		}
		if len(r.Action.SetFallbackChain) > 0 {
			n++
		}
		if len(r.Action.DenyProviders) > 0 || len(r.Action.AllowProviders) > 0 {
			n++
		}
		if n != 1 {
			out = append(out, LintIssue{
				Check: "action_exactly_one", RuleName: r.Name, Severity: LintError,
				Message: "rule must set exactly one primary action (route_to_tier / route_to_model / pin_tier / no_route / set_effort / set_fallback_chain / provider filter)",
			})
		}
	}
	return out
}

func lintKnownTurnKinds(p Policy) []LintIssue {
	known := map[TurnKind]bool{}
	for _, k := range AllTurnKinds() {
		known[k] = true
	}
	var out []LintIssue
	for _, r := range p.Rules {
		for _, k := range r.When.TurnKinds {
			if !known[k] {
				out = append(out, LintIssue{
					Check: "known_turn_kinds", RuleName: r.Name, Severity: LintError,
					Message: fmt.Sprintf("unknown turn-kind %q in when clause", k),
				})
			}
		}
	}
	return out
}

func lintKnownReason(p Policy) []LintIssue {
	known := map[ReasonCode]bool{}
	for _, rc := range KnownReasonCodes() {
		known[rc] = true
	}
	var out []LintIssue
	for _, r := range p.Rules {
		if !known[r.Reason] {
			out = append(out, LintIssue{
				Check: "known_reason", RuleName: r.Name, Severity: LintError,
				Message: fmt.Sprintf("reason %q is not in the closed enum", r.Reason),
			})
		}
	}
	return out
}

func lintTargetableTier(p Policy) []LintIssue {
	var out []LintIssue
	for _, r := range p.Rules {
		if r.Action.RouteToTier != "" && !r.Action.RouteToTier.DownshiftTargetable() {
			out = append(out, LintIssue{
				Check: "targetable_tier", RuleName: r.Name, Severity: LintError,
				Message: fmt.Sprintf("route_to_tier %q is not a routable destination (§R7.1)", r.Action.RouteToTier),
			})
		}
		if r.Action.PinTier != "" && !r.Action.PinTier.DownshiftTargetable() {
			out = append(out, LintIssue{
				Check: "targetable_tier", RuleName: r.Name, Severity: LintError,
				Message: fmt.Sprintf("pin_tier %q is not a routable destination (§R7.1)", r.Action.PinTier),
			})
		}
	}
	return out
}

func lintKnownWhenTier(p Policy) []LintIssue {
	var out []LintIssue
	for _, r := range p.Rules {
		if r.When.TierAtLeast != "" && !r.When.TierAtLeast.Known() {
			out = append(out, LintIssue{
				Check: "known_when_tier", RuleName: r.Name, Severity: LintError,
				Message: fmt.Sprintf("when.tier_at_least %q is not a known tier", r.When.TierAtLeast),
			})
		}
	}
	return out
}

func lintModelGlobValid(p Policy) []LintIssue {
	var out []LintIssue
	for _, r := range p.Rules {
		if r.When.ModelGlob == "" {
			continue
		}
		if _, err := path.Match(r.When.ModelGlob, "probe"); err != nil {
			out = append(out, LintIssue{
				Check: "model_glob_valid", RuleName: r.Name, Severity: LintError,
				Message: fmt.Sprintf("when.model glob %q is malformed: %v", r.When.ModelGlob, err),
			})
		}
	}
	return out
}

func lintKnownEntitlement(p Policy) []LintIssue {
	var out []LintIssue
	for _, r := range p.Rules {
		switch r.When.Entitlement {
		case "", EntitlementAPIKey, EntitlementSubscription:
		default:
			out = append(out, LintIssue{
				Check: "known_entitlement", RuleName: r.Name, Severity: LintError,
				Message: fmt.Sprintf("when.entitlement %q not in {api_key, subscription}", r.When.Entitlement),
			})
		}
	}
	return out
}

func lintKnownEffort(p Policy) []LintIssue {
	var out []LintIssue
	for _, r := range p.Rules {
		switch r.Action.SetEffort {
		case "", EffortMinimal, EffortLow, EffortMedium, EffortHigh:
		default:
			out = append(out, LintIssue{
				Check: "known_effort", RuleName: r.Name, Severity: LintError,
				Message: fmt.Sprintf("set_effort %q not in {minimal, low, medium, high}", r.Action.SetEffort),
			})
		}
	}
	return out
}

func lintKnownBasis(p Policy) []LintIssue {
	known := map[string]bool{}
	for _, n := range KnownBasisNames() {
		known[n] = true
	}
	var out []LintIssue
	for _, b := range p.Bases {
		if !known[b] {
			out = append(out, LintIssue{
				Check: "known_basis", Severity: LintError,
				Message: fmt.Sprintf("basis %q is not in the shipped vocabulary (§R6.2)", b),
			})
		}
	}
	return out
}

// lintPathClassRef flags rules and privacy rules referencing a
// path-class name absent from [routing.path_classes] — the reference
// would never match (a silent no-op the operator should see).
func lintPathClassRef(p Policy) []LintIssue {
	var out []LintIssue
	for _, r := range p.Rules {
		if r.When.PathClass != "" {
			if _, ok := p.PathClasses[r.When.PathClass]; !ok {
				out = append(out, LintIssue{
					Check: "path_class_ref", RuleName: r.Name, Severity: LintError,
					Message: fmt.Sprintf("when.path_class %q is not defined in [routing.path_classes]", r.When.PathClass),
				})
			}
		}
	}
	for i, pr := range p.PrivacyRules {
		if pr.PathClass != "" {
			if _, ok := p.PathClasses[pr.PathClass]; !ok {
				out = append(out, LintIssue{
					Check: "path_class_ref", Severity: LintError,
					Message: fmt.Sprintf("privacy rule %d references undefined path_class %q", i, pr.PathClass),
				})
			}
		}
	}
	return out
}

func lintFloorTiersKnown(p Policy) []LintIssue {
	var out []LintIssue
	for _, kind := range sortedTurnKindKeys(p.Floors) {
		tier := p.Floors[kind]
		if !tier.Known() || tier == TierUnclassified {
			out = append(out, LintIssue{
				Check: "floor_tiers_known", Severity: LintError,
				Message: fmt.Sprintf("floor for %s names invalid tier %q", kind, tier),
			})
		}
	}
	return out
}

// lintDownshiftOnly flags rules whose target tier does not sit strictly
// below the tiers they can match: with tier_at_least set, the lowest
// matching tier must still rank above the target (else the rule no-ops
// or proposes a sideways/upward move and fails open at runtime). With
// no tier_at_least bound the rule can match any tier, so the same
// hazard exists for every model at or below the target — a warning.
func lintDownshiftOnly(p Policy) []LintIssue {
	var out []LintIssue
	for _, r := range p.Rules {
		if r.Action.RouteToTier == "" {
			continue
		}
		if r.When.TierAtLeast == "" {
			out = append(out, LintIssue{
				Check: "downshift_only", RuleName: r.Name, Severity: LintWarning,
				Message: "route_to_tier without tier_at_least — the rule will no-op (fail open) on models at or below the target tier",
			})
			continue
		}
		if r.Action.RouteToTier.Rank() >= r.When.TierAtLeast.Rank() {
			out = append(out, LintIssue{
				Check: "downshift_only", RuleName: r.Name, Severity: LintError,
				Message: fmt.Sprintf("route_to_tier %q does not sit below tier_at_least %q — not a downshift",
					r.Action.RouteToTier, r.When.TierAtLeast),
			})
		}
	}
	return out
}

// lintShadowedRule flags rules that can never fire because an earlier
// rule's when clause matches a superset of theirs (first match wins).
func lintShadowedRule(p Policy) []LintIssue {
	var out []LintIssue
	for i, later := range p.Rules {
		for _, earlier := range p.Rules[:i] {
			if whenSuperset(earlier.When, later.When) {
				out = append(out, LintIssue{
					Check: "shadowed_rule", RuleName: later.Name, Severity: LintError,
					Message: fmt.Sprintf("unreachable: earlier rule %q matches a superset of this rule's when clause", earlier.Name),
				})
				break
			}
		}
	}
	return out
}

// whenSuperset reports whether clause a matches everything clause b
// matches (conservative: false when uncertain). Split into kind /
// exact-string / bound legs to keep each simple.
func whenSuperset(a, b RuleWhen) bool {
	return supersetKinds(a, b) && supersetExact(a, b) && supersetBounds(a, b)
}

func supersetKinds(a, b RuleWhen) bool {
	if len(a.TurnKinds) == 0 {
		return true
	}
	if len(b.TurnKinds) == 0 {
		return false // b matches kinds a doesn't
	}
	set := map[TurnKind]bool{}
	for _, k := range a.TurnKinds {
		set[k] = true
	}
	for _, k := range b.TurnKinds {
		if !set[k] {
			return false
		}
	}
	return true
}

// supersetExact: a's exact-match string fields must be absent or equal.
func supersetExact(a, b RuleWhen) bool {
	if a.Sidechain != nil && (b.Sidechain == nil || *a.Sidechain != *b.Sidechain) {
		return false
	}
	pairs := [][2]string{
		{a.Phase, b.Phase},
		{a.ModelGlob, b.ModelGlob},
		{a.Project, b.Project},
		{a.PathClass, b.PathClass},
		{a.Entitlement, b.Entitlement},
	}
	for _, p := range pairs {
		if p[0] != "" && p[0] != p[1] {
			return false
		}
	}
	return true
}

// supersetBounds: a's bounds must be absent or at-least-as-loose.
func supersetBounds(a, b RuleWhen) bool {
	// a's tier bound must be at or below b's (a matches at least as much).
	if a.TierAtLeast != "" && (b.TierAtLeast == "" || a.TierAtLeast.Rank() > b.TierAtLeast.Rank()) {
		return false
	}
	if a.MaxToolUses > 0 && (b.MaxToolUses == 0 || a.MaxToolUses < b.MaxToolUses) {
		return false
	}
	if a.SessionAgeTurnsMin > 0 && (b.SessionAgeTurnsMin == 0 || a.SessionAgeTurnsMin > b.SessionAgeTurnsMin) {
		return false
	}
	if a.SessionAgeTurnsMax > 0 && (b.SessionAgeTurnsMax == 0 || a.SessionAgeTurnsMax < b.SessionAgeTurnsMax) {
		return false
	}
	if a.MinPromptTokens > 0 && (b.MinPromptTokens == 0 || a.MinPromptTokens > b.MinPromptTokens) {
		return false
	}
	if a.MaxPromptTokens > 0 && (b.MaxPromptTokens == 0 || a.MaxPromptTokens < b.MaxPromptTokens) {
		return false
	}
	if a.BudgetBandAtLeast > 0 && (b.BudgetBandAtLeast == 0 || a.BudgetBandAtLeast > b.BudgetBandAtLeast) {
		return false
	}
	return true
}
