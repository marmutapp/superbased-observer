package egress

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
)

// Mode is the egress enforcement posture.
const (
	ModeOff     = "off"
	ModeAdvise  = "advise"
	ModeEnforce = "enforce"
)

// normalizeMode maps a token to a mode; empty/unknown → off (safe default).
func normalizeMode(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "advise":
		return ModeAdvise
	case "enforce":
		return ModeEnforce
	default:
		return ModeOff
	}
}

// PolicySpec is the compiled, ready-to-evaluate egress policy.
type PolicySpec struct {
	Mode    string
	Rules   []CompiledRule
	Targets map[string]Target
	// CooldownSeconds is the per-policy hold window for budget-band model
	// switches (§3.6 / finding 11). 0 = the boundary default.
	CooldownSeconds int
	// Cohorts maps an end-user id to a cohort label. The boundary resolves
	// Input.Cohort from this before Evaluate (kept here so one atomic
	// policy pointer carries everything a request needs).
	Cohorts map[string]string
	// Hash is the content address of the policy (attribution + audit).
	Hash string
}

// CohortFor resolves an end-user's cohort from the policy's cohort map ("" when
// the user is anonymous or unmapped).
func (s PolicySpec) CohortFor(user string) string {
	if user == "" || s.Cohorts == nil {
		return ""
	}
	return s.Cohorts[user]
}

// CompiledRule is one resolved rule (first-match-wins).
type CompiledRule struct {
	Name          string
	When          CompiledWhen
	Action        Action
	UpstreamID    string // route_upstream
	Model         string // route_model
	Effort        Effort // set_effort
	Reason        string
	ReasonCode    ReasonCode
	OnUnavailable string
}

// CompiledWhen holds the resolved matcher fields. Unset fields impose no
// constraint (a rule with no constraints matches everything — lint warns).
type CompiledWhen struct {
	VerdictAtLeast    string
	Criterion         string
	SeverityAtLeast   string
	ContentClass      string
	ModelGlob         string
	Provider          string
	User              string
	UserCohort        string
	BudgetBandAtLeast float64
	BudgetBandSet     bool
	MinPromptTokens   int
}

// PolicyInput is the plain, pre-compile policy translated from config AT THE
// BOUNDARY (obs_wire), so this pure package never imports internal/config.
type PolicyInput struct {
	Mode            string
	CooldownSeconds int
	Rules           []RuleInput
	Targets         []TargetInput
	// Cohorts maps an end-user id to a cohort label (the optional local
	// user→cohort map). Translated by the boundary; not compiled here.
	Cohorts map[string]string
}

// RuleInput is one uncompiled egress rule.
type RuleInput struct {
	Name string
	When WhenInput
	// Action fields — exactly one primary must be set (lint-enforced).
	RouteToUpstream string
	RouteToModel    string
	SetEffort       string
	Deny            bool
	NoRoute         bool
	Reason          string
	ReasonCode      string
	OnUnavailable   string
}

// WhenInput is one uncompiled matcher set.
type WhenInput struct {
	VerdictAtLeast    string
	Criterion         string
	SeverityAtLeast   string
	ContentClass      string
	ModelGlob         string
	Provider          string
	User              string
	UserCohort        string
	BudgetBandAtLeast float64
	BudgetBandSet     bool
	MinPromptTokens   int
}

// TargetInput is one uncompiled typed target.
type TargetInput struct {
	ID    string
	URL   string
	Shape string
}

// Compile validates a PolicyInput and produces a ready PolicySpec. It
// resolves the mode, targets, action vocabulary, and matcher enums, compiles
// model globs, and computes a deterministic content Hash. Enforce-critical
// mistakes (unknown target shape, a route_to_upstream target whose shape ≠ the
// rule provider, a set_effort with no/incompatible provider, an unknown
// upstream id) are HARD ERRORS in enforce mode so a bad enforce policy fails to
// install (disabling egress) rather than mis-routing; advise mode is lenient
// (a bare upstream id is log-only). Lint reports the same findings for any
// mode.
func Compile(in PolicyInput) (PolicySpec, error) {
	spec := PolicySpec{
		Mode:            normalizeMode(in.Mode),
		CooldownSeconds: in.CooldownSeconds,
		Targets:         map[string]Target{},
		Cohorts:         in.Cohorts,
	}

	for _, ti := range in.Targets {
		id := strings.TrimSpace(ti.ID)
		if id == "" {
			return PolicySpec{}, fmt.Errorf("egress.Compile: target with empty id")
		}
		if _, dup := spec.Targets[id]; dup {
			return PolicySpec{}, fmt.Errorf("egress.Compile: duplicate target id %q", id)
		}
		if !validShape(ti.Shape) {
			return PolicySpec{}, fmt.Errorf("egress.Compile: target %q: unknown shape %q (want anthropic|openai)", id, ti.Shape)
		}
		if strings.TrimSpace(ti.URL) == "" {
			return PolicySpec{}, fmt.Errorf("egress.Compile: target %q: empty url", id)
		}
		spec.Targets[id] = Target{ID: id, URL: strings.TrimSpace(ti.URL), Shape: ProviderShape(ti.Shape)}
	}

	enforce := spec.Mode == ModeEnforce
	seen := map[string]bool{}
	for _, ri := range in.Rules {
		cr, err := compileRule(ri, spec.Targets, enforce, seen)
		if err != nil {
			return PolicySpec{}, err
		}
		spec.Rules = append(spec.Rules, cr)
	}

	spec.Hash = hashPolicy(in)
	return spec, nil
}

// compileRule resolves one rule, validating the action vocabulary, matchers,
// and (in enforce mode) the provider/shape compatibility.
func compileRule(ri RuleInput, targets map[string]Target, enforce bool, seen map[string]bool) (CompiledRule, error) {
	name := strings.TrimSpace(ri.Name)
	if name == "" {
		return CompiledRule{}, fmt.Errorf("egress.Compile: rule with empty name")
	}
	if seen[name] {
		return CompiledRule{}, fmt.Errorf("egress.Compile: duplicate rule name %q", name)
	}
	seen[name] = true

	action, err := resolvePrimaryAction(ri)
	if err != nil {
		return CompiledRule{}, err
	}

	when, err := compileWhen(ri.When)
	if err != nil {
		return CompiledRule{}, fmt.Errorf("egress.Compile: rule %q: %w", name, err)
	}

	cr := CompiledRule{
		Name:          name,
		When:          when,
		Action:        action,
		Reason:        ri.Reason,
		OnUnavailable: normalizeOnUnavailable(ri.OnUnavailable),
	}

	switch action {
	case ActionRouteUpstream:
		cr.UpstreamID = strings.TrimSpace(ri.RouteToUpstream)
		// A data-locality/sensitive route (on_unavailable=deny) is pinned; the
		// proxy fails closed if the target is unavailable at runtime.
		t, known := targets[cr.UpstreamID]
		if enforce {
			if !known {
				return CompiledRule{}, fmt.Errorf("egress.Compile: rule %q: route_to_upstream references unknown target %q (enforce requires a declared [[observability.egress.targets]] entry)", name, cr.UpstreamID)
			}
			if when.Provider != "" && string(t.Shape) != when.Provider {
				return CompiledRule{}, fmt.Errorf("egress.Compile: rule %q: target %q shape %q ≠ rule provider %q (cross-shape routing is not supported in v1)", name, cr.UpstreamID, t.Shape, when.Provider)
			}
		}
	case ActionRouteModel:
		cr.Model = strings.TrimSpace(ri.RouteToModel)
	case ActionSetEffort:
		cr.Effort = Effort(strings.ToLower(strings.TrimSpace(ri.SetEffort)))
		if !validEffort(string(cr.Effort)) {
			return CompiledRule{}, fmt.Errorf("egress.Compile: rule %q: unknown set_effort level %q (want minimal|low|medium|high)", name, ri.SetEffort)
		}
		// set_effort is replace-only and provider-specific (Anthropic
		// thinking.budget_tokens vs OpenAI reasoning_effort); it cannot be
		// uniformly interpreted without a provider matcher (finding 10).
		if enforce && !validShape(when.Provider) {
			return CompiledRule{}, fmt.Errorf("egress.Compile: rule %q: set_effort requires a provider matcher (anthropic|openai) — the effort field is provider-specific", name)
		}
	}

	cr.ReasonCode, err = resolveReasonCode(ri.ReasonCode, action, cr.OnUnavailable)
	if err != nil {
		return CompiledRule{}, fmt.Errorf("egress.Compile: rule %q: %w", name, err)
	}
	return cr, nil
}

// resolvePrimaryAction enforces "exactly one primary action per rule".
func resolvePrimaryAction(ri RuleInput) (Action, error) {
	var set []Action
	if strings.TrimSpace(ri.RouteToUpstream) != "" {
		set = append(set, ActionRouteUpstream)
	}
	if strings.TrimSpace(ri.RouteToModel) != "" {
		set = append(set, ActionRouteModel)
	}
	if strings.TrimSpace(ri.SetEffort) != "" {
		set = append(set, ActionSetEffort)
	}
	if ri.Deny {
		set = append(set, ActionDeny)
	}
	if ri.NoRoute {
		set = append(set, ActionNone)
	}
	switch len(set) {
	case 0:
		return ActionNone, fmt.Errorf("egress.Compile: rule %q: no action (want one of route_to_upstream|route_to_model|set_effort|deny|no_route)", ri.Name)
	case 1:
		return set[0], nil
	default:
		return ActionNone, fmt.Errorf("egress.Compile: rule %q: multiple actions set — exactly one primary action is allowed", ri.Name)
	}
}

// compileWhen resolves and validates the matcher set.
func compileWhen(w WhenInput) (CompiledWhen, error) {
	out := CompiledWhen{
		VerdictAtLeast:    strings.ToLower(strings.TrimSpace(w.VerdictAtLeast)),
		Criterion:         strings.TrimSpace(w.Criterion),
		SeverityAtLeast:   strings.ToLower(strings.TrimSpace(w.SeverityAtLeast)),
		ContentClass:      strings.TrimSpace(w.ContentClass),
		ModelGlob:         strings.TrimSpace(w.ModelGlob),
		Provider:          strings.ToLower(strings.TrimSpace(w.Provider)),
		User:              strings.TrimSpace(w.User),
		UserCohort:        strings.TrimSpace(w.UserCohort),
		BudgetBandAtLeast: w.BudgetBandAtLeast,
		BudgetBandSet:     w.BudgetBandSet,
		MinPromptTokens:   w.MinPromptTokens,
	}
	if out.VerdictAtLeast != "" {
		if _, ok := decisionRank[out.VerdictAtLeast]; !ok {
			return CompiledWhen{}, fmt.Errorf("unknown verdict_at_least %q (want allow|flag|ask|deny)", out.VerdictAtLeast)
		}
	}
	if out.SeverityAtLeast != "" {
		if _, ok := severityRank[out.SeverityAtLeast]; !ok {
			return CompiledWhen{}, fmt.Errorf("unknown severity_at_least %q (want info|warn|high|critical)", out.SeverityAtLeast)
		}
	}
	if out.Provider != "" && !validShape(out.Provider) {
		return CompiledWhen{}, fmt.Errorf("unknown provider %q (v1 supports anthropic|openai only — Gemini is excluded)", out.Provider)
	}
	if out.ModelGlob != "" {
		if _, err := path.Match(out.ModelGlob, ""); err != nil {
			return CompiledWhen{}, fmt.Errorf("bad model_glob %q: %w", out.ModelGlob, err)
		}
	}
	return out, nil
}

// normalizeOnUnavailable maps a token to a fail posture; empty/unknown →
// fail_open (the safe default for cost/cohort convenience routing).
func normalizeOnUnavailable(s string) string {
	if strings.EqualFold(strings.TrimSpace(s), OnUnavailableDeny) {
		return OnUnavailableDeny
	}
	return OnUnavailableFailOpen
}

// resolveReasonCode validates an explicit reason_code or derives a default from
// the action + fail posture (still a closed-enum value).
func resolveReasonCode(explicit string, action Action, onUnavailable string) (ReasonCode, error) {
	if e := strings.TrimSpace(explicit); e != "" {
		if !validReasonCode(e) {
			return "", fmt.Errorf("unknown reason_code %q", e)
		}
		return ReasonCode(e), nil
	}
	switch action {
	case ActionRouteUpstream:
		if onUnavailable == OnUnavailableDeny {
			return ReasonSensitiveLocalOnly, nil
		}
		return ReasonCohortUpstream, nil
	case ActionRouteModel:
		return ReasonBudgetBand, nil
	case ActionSetEffort:
		return ReasonOverloadDegrade, nil
	case ActionDeny:
		return ReasonDenyUnavailable, nil
	default:
		return ReasonNoRoute, nil
	}
}

// hashPolicy computes a stable content hash over the semantic policy fields so
// the same policy always yields the same Hash (attribution + audit). Field
// order is fixed; rules keep author order (first-match-wins is order-sensitive)
// and targets are sorted by id.
func hashPolicy(in PolicyInput) string {
	h := sha256.New()
	w := func(parts ...string) {
		for _, p := range parts {
			_, _ = h.Write([]byte(p))
			_, _ = h.Write([]byte{0x1e})
		}
	}
	w("mode", normalizeMode(in.Mode))
	w("cooldown", itoa(in.CooldownSeconds))
	tgs := append([]TargetInput(nil), in.Targets...)
	sort.Slice(tgs, func(i, j int) bool { return tgs[i].ID < tgs[j].ID })
	for _, t := range tgs {
		w("target", t.ID, t.URL, t.Shape)
	}
	for _, r := range in.Rules {
		w("rule", r.Name, r.RouteToUpstream, r.RouteToModel, r.SetEffort,
			boolStr(r.Deny), boolStr(r.NoRoute), r.OnUnavailable, r.ReasonCode)
		w("when", r.When.VerdictAtLeast, r.When.Criterion, r.When.SeverityAtLeast,
			r.When.ContentClass, r.When.ModelGlob, r.When.Provider, r.When.User,
			r.When.UserCohort, ftoa(r.When.BudgetBandAtLeast), boolStr(r.When.BudgetBandSet),
			itoa(r.When.MinPromptTokens))
	}
	// The cohort map is content-addressed sorted so ordering is stable.
	keys := make([]string, 0, len(in.Cohorts))
	for k := range in.Cohorts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w("cohort", k, in.Cohorts[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func itoa(i int) string     { return fmt.Sprintf("%d", i) }
func ftoa(f float64) string { return fmt.Sprintf("%g", f) }
