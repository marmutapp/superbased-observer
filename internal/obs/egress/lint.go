package egress

import (
	"fmt"
	"path"
	"strings"
)

// Issue is one policy-lint finding. Fatal issues make a policy invalid; the
// rest are advisory.
type Issue struct {
	RuleName string
	Message  string
	Fatal    bool
}

// Lint statically checks a PolicyInput and returns findings — the mistakes that
// would otherwise surface at request time or silently no-op: an unknown enum, a
// bad model glob, a rule with no or multiple actions, a duplicate name/id, a
// route_to_upstream to an unknown/mismatched-shape target, a set_effort with no
// or an incompatible provider matcher (the provider/action compatibility lint,
// finding 10), and the cross-shape rule (the shape-mismatch lint, finding 9).
// Independent of mode, so `egress lint` reports the same findings advise or
// enforce.
func Lint(in PolicyInput) []Issue {
	var issues []Issue

	if len(in.Rules) == 0 {
		issues = append(issues, Issue{Message: "policy has no rules — no request is ever routed", Fatal: false})
	}

	// Targets: shape + url + duplicate id.
	targets := map[string]Target{}
	seenTarget := map[string]bool{}
	for _, t := range in.Targets {
		id := strings.TrimSpace(t.ID)
		if id == "" {
			issues = append(issues, Issue{Message: "target with empty id", Fatal: true})
			continue
		}
		if seenTarget[id] {
			issues = append(issues, Issue{Message: fmt.Sprintf("duplicate target id %q", id), Fatal: true})
		}
		seenTarget[id] = true
		if !validShape(t.Shape) {
			issues = append(issues, Issue{Message: fmt.Sprintf("target %q: unknown shape %q (want anthropic|openai)", id, t.Shape), Fatal: true})
			continue
		}
		if strings.TrimSpace(t.URL) == "" {
			issues = append(issues, Issue{Message: fmt.Sprintf("target %q: empty url", id), Fatal: true})
			continue
		}
		targets[id] = Target{ID: id, URL: t.URL, Shape: ProviderShape(t.Shape)}
	}

	seenRule := map[string]bool{}
	for _, r := range in.Rules {
		issues = append(issues, lintRule(r, targets, seenRule)...)
	}
	return issues
}

// lintRule checks one rule.
func lintRule(r RuleInput, targets map[string]Target, seenRule map[string]bool) []Issue {
	var issues []Issue
	name := strings.TrimSpace(r.Name)
	if name == "" {
		issues = append(issues, Issue{Message: "rule with empty name", Fatal: true})
	} else if seenRule[name] {
		issues = append(issues, Issue{RuleName: name, Message: "duplicate rule name", Fatal: true})
	}
	seenRule[name] = true

	action, err := resolvePrimaryAction(r)
	if err != nil {
		issues = append(issues, Issue{RuleName: name, Message: err.Error(), Fatal: true})
		return issues
	}

	// Matcher enums + glob.
	w := r.When
	if v := strings.ToLower(strings.TrimSpace(w.VerdictAtLeast)); v != "" {
		if _, ok := decisionRank[v]; !ok {
			issues = append(issues, Issue{RuleName: name, Message: fmt.Sprintf("unknown verdict_at_least %q", v), Fatal: true})
		}
	}
	if s := strings.ToLower(strings.TrimSpace(w.SeverityAtLeast)); s != "" {
		if _, ok := severityRank[s]; !ok {
			issues = append(issues, Issue{RuleName: name, Message: fmt.Sprintf("unknown severity_at_least %q", s), Fatal: true})
		}
	}
	provider := strings.ToLower(strings.TrimSpace(w.Provider))
	if provider != "" && !validShape(provider) {
		issues = append(issues, Issue{RuleName: name, Message: fmt.Sprintf("unknown provider %q (v1: anthropic|openai — Gemini excluded)", provider), Fatal: true})
	}
	if w.ModelGlob != "" {
		if _, err := path.Match(w.ModelGlob, ""); err != nil {
			issues = append(issues, Issue{RuleName: name, Message: fmt.Sprintf("bad model_glob %q: %v", w.ModelGlob, err), Fatal: true})
		}
	}
	if rc := strings.TrimSpace(r.ReasonCode); rc != "" && !validReasonCode(rc) {
		issues = append(issues, Issue{RuleName: name, Message: fmt.Sprintf("unknown reason_code %q", rc), Fatal: true})
	}

	// No-constraint warning.
	if isEmptyWhen(w) && action != ActionNone {
		issues = append(issues, Issue{RuleName: name, Message: "rule has no matchers — it fires on every request", Fatal: false})
	}

	switch action {
	case ActionRouteUpstream:
		id := strings.TrimSpace(r.RouteToUpstream)
		t, known := targets[id]
		if !known {
			issues = append(issues, Issue{RuleName: name, Message: fmt.Sprintf("route_to_upstream references undeclared target %q — enforce requires a [[observability.egress.targets]] entry (advise may log-only)", id), Fatal: false})
		} else if provider != "" && string(t.Shape) != provider {
			// Shape-mismatch lint (finding 9): cross-shape routing is a lint
			// error — a rule whose target shape ≠ its provider matcher.
			issues = append(issues, Issue{RuleName: name, Message: fmt.Sprintf("target %q shape %q ≠ rule provider %q — cross-shape routing is unsupported in v1", id, t.Shape, provider), Fatal: true})
		}
	case ActionSetEffort:
		lvl := strings.ToLower(strings.TrimSpace(r.SetEffort))
		if !validEffort(lvl) {
			issues = append(issues, Issue{RuleName: name, Message: fmt.Sprintf("unknown set_effort level %q (want minimal|low|medium|high)", r.SetEffort), Fatal: true})
		}
		// Provider/action compatibility lint (finding 10): set_effort is
		// replace-only + provider-specific, so it needs a provider matcher.
		if !validShape(provider) {
			issues = append(issues, Issue{RuleName: name, Message: "set_effort needs a provider matcher (anthropic|openai) — the effort field is provider-specific and cannot be uniformly interpreted", Fatal: true})
		}
	}
	return issues
}

// isEmptyWhen reports whether a matcher imposes no constraint at all.
func isEmptyWhen(w WhenInput) bool {
	return strings.TrimSpace(w.VerdictAtLeast) == "" &&
		strings.TrimSpace(w.Criterion) == "" &&
		strings.TrimSpace(w.SeverityAtLeast) == "" &&
		strings.TrimSpace(w.ContentClass) == "" &&
		strings.TrimSpace(w.ModelGlob) == "" &&
		strings.TrimSpace(w.Provider) == "" &&
		strings.TrimSpace(w.User) == "" &&
		strings.TrimSpace(w.UserCohort) == "" &&
		!w.BudgetBandSet &&
		w.MinPromptTokens == 0
}

// HasFatal reports whether any issue is fatal.
func HasFatal(issues []Issue) bool {
	for _, i := range issues {
		if i.Fatal {
			return true
		}
	}
	return false
}
