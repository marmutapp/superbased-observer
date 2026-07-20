package egress

import "path"

// decisionRank orders the admission decision vocabulary so verdict_at_least can
// match "this decision or stricter". An unknown/empty decision ranks as allow.
var decisionRank = map[string]int{
	"allow": 0,
	"flag":  1,
	"ask":   2,
	"deny":  3,
}

// severityRank orders the admission severity vocabulary for severity_at_least.
var severityRank = map[string]int{
	"info":     0,
	"warn":     1,
	"high":     2,
	"critical": 3,
}

// Evaluate walks the compiled rules top-down and returns the first-match
// directive, or the zero directive (no action) when the policy is off or no
// rule matches. It is a pure, deterministic table walk with no I/O — safe to
// run every request (design §3.2: egress is never cached under the verdict
// cache key).
//
// A matched route_to_model directive is subject to the session pin/cooldown
// (finding 11): a soft (cost/cohort/size) switch that would move off the model
// already served this session is HELD unless the cooldown has elapsed; a hard
// (verdict/criterion/severity) switch is never held.
func Evaluate(in Input, spec PolicySpec) Directive {
	if spec.Mode == ModeOff || spec.Mode == "" {
		return Directive{}
	}
	for i := range spec.Rules {
		r := spec.Rules[i]
		if !matches(in, r.When) {
			continue
		}
		return buildDirective(in, r, spec)
	}
	return Directive{}
}

// buildDirective realizes a matched rule into a directive, resolving typed
// targets, the pin/cooldown hold, and the fail posture.
func buildDirective(in Input, r CompiledRule, spec PolicySpec) Directive {
	d := Directive{
		Matched:       true,
		Action:        r.Action,
		Reason:        r.Reason,
		ReasonCode:    r.ReasonCode,
		RuleName:      r.Name,
		PolicyHash:    spec.Hash,
		OnUnavailable: r.OnUnavailable,
	}
	switch r.Action {
	case ActionRouteUpstream:
		d.UpstreamID = r.UpstreamID
		if t, ok := spec.Targets[r.UpstreamID]; ok {
			d.TargetURL = t.URL
			d.TargetShape = t.Shape
			d.TargetKnown = true
		}
		// A data-locality/sensitive route pins the target: the proxy must fail
		// closed if it is unavailable at runtime (§3.6). MustUseTarget always
		// accompanies on_unavailable=deny.
		d.MustUseTarget = r.OnUnavailable == OnUnavailableDeny
	case ActionRouteModel:
		d.Model = r.Model
		if held := holdSwitch(in, r); held {
			d.Action = ActionNone
			d.Model = ""
			d.SwitchHeld = true
			d.ReasonCode = ReasonSwitchHeld
			d.Degraded = "switch-held-cooldown"
		}
	case ActionSetEffort:
		d.Effort = r.Effort
	case ActionNone:
		// no_route exemption — matched, but no directive applied.
	}
	return d
}

// holdSwitch reports whether a route_to_model directive must be held by the
// session pin/cooldown: a SOFT switch (no verdict/criterion/severity matcher —
// i.e. a cost/cohort/size switch) that would move off the model already served
// this session is held until the cooldown elapses. A hard governance switch is
// never held (finding 11).
func holdSwitch(in Input, r CompiledRule) bool {
	if in.SessionModel == "" || in.SessionModel == r.Model {
		return false // no prior model, or no actual change
	}
	if r.isHardSwitch() {
		return false // verdict/locality switches never hold
	}
	return !in.CooldownElapsed
}

// isHardSwitch reports whether a rule is a hard governance rule (verdict /
// criterion / severity driven) whose model switch must not be held.
func (r CompiledRule) isHardSwitch() bool {
	return r.When.VerdictAtLeast != "" || r.When.Criterion != "" || r.When.SeverityAtLeast != ""
}

// matches reports whether every SET constraint in the matcher holds (AND). A
// matcher with no constraints matches everything (lint warns).
func matches(in Input, w CompiledWhen) bool {
	if w.VerdictAtLeast != "" && decisionRank[in.VerdictDecision] < decisionRank[w.VerdictAtLeast] {
		return false
	}
	if w.SeverityAtLeast != "" && severityRank[in.VerdictSeverity] < severityRank[w.SeverityAtLeast] {
		return false
	}
	if w.Criterion != "" && in.Criterion != w.Criterion {
		return false
	}
	if w.ContentClass != "" && !contentClassMatches(w.ContentClass, in.Criterion, in.VerdictSeverity) {
		return false
	}
	if w.ModelGlob != "" {
		if in.Model == "" {
			return false
		}
		if ok, _ := path.Match(w.ModelGlob, in.Model); !ok {
			return false
		}
	}
	if w.Provider != "" && in.Provider != w.Provider {
		return false
	}
	if w.User != "" && in.User != w.User {
		return false
	}
	if w.UserCohort != "" && in.Cohort != w.UserCohort {
		return false
	}
	if w.BudgetBandSet {
		// A budget matcher fires ONLY when spend was actually resolved —
		// spend-unavailable never satisfies a budget band (finding 12).
		if !in.BudgetKnown || in.BudgetBurnMax < w.BudgetBandAtLeast {
			return false
		}
	}
	if w.MinPromptTokens > 0 && in.PromptTokensEst < w.MinPromptTokens {
		return false
	}
	return true
}

// contentClassMatches implements the v1 content_class = criterion+severity
// semantics (design §4 / finding 9): the class value matches the fired
// criterion id, the severity token, or the composite "<criterion>/<severity>".
// It reflects only the last user turn — the extraction-scope limitation.
func contentClassMatches(class, criterion, severity string) bool {
	if class == criterion && criterion != "" {
		return true
	}
	if class == severity && severity != "" {
		return true
	}
	return class == criterion+"/"+severity
}
