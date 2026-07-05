package routing

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
)

// RuleWhen is a rule's match clause (§R6.3). Zero-valued fields match
// anything; set fields all must hold (AND semantics). Time-window
// matching is deliberately absent: the engine must stay deterministic
// given (snapshot, input, policy hash) (§R9.3).
type RuleWhen struct {
	// TurnKinds matches when the classified kind is in the set; empty
	// matches every kind.
	TurnKinds []TurnKind
	// Phase matches the client-declared phase verbatim ("plan").
	Phase string
	// TierAtLeast matches when the current model's tier ranks at or
	// above this tier — "the model is at least this big". Zero matches
	// any tier.
	TierAtLeast Tier
	// ModelGlob is a path.Match glob over the original model id.
	ModelGlob string
	// Project matches the boundary-resolved project name exactly.
	Project string
	// PathClass matches when the named path-class is among the turn's
	// boundary-resolved PathClassHits (§R16).
	PathClass string
	// SessionAgeTurnsMin/Max bound the session's turn count; zero
	// disables each bound.
	SessionAgeTurnsMin int
	SessionAgeTurnsMax int
	// MaxToolUses, when positive, matches requests carrying at most
	// this many tool definitions.
	MaxToolUses int
	// MinPromptTokens/MaxPromptTokens band the prompt size; zero
	// disables each bound.
	MinPromptTokens int64
	MaxPromptTokens int64
	// BudgetBandAtLeast matches when the highest burn fraction among
	// the turn's budget scopes is at or above this value (§R14).
	BudgetBandAtLeast float64
	// Entitlement matches the boundary-resolved entitlement class
	// (§R11.3). Empty matches any.
	Entitlement string
	// Sidechain, when non-nil, must equal the session's sidechain flag.
	Sidechain *bool
}

// MatchContext is the per-decision view a when clause evaluates
// against: the classification, the tier placement, the input, and the
// snapshot-derived budget burn for the turn's scopes.
type MatchContext struct {
	Kind TurnKind
	Tier Tier
	In   DecisionInput
	// BudgetBurnMax is the highest burn fraction among the budget
	// scopes matching this turn (0 when none or no snapshot data).
	BudgetBurnMax float64
}

// Matches evaluates the clause against one decision context. Split
// into identity / shape / band sub-checks to keep each leg simple.
func (w RuleWhen) Matches(mc MatchContext) bool {
	return w.matchesKindAndIdentity(mc) && w.matchesShape(mc) && w.matchesBands(mc)
}

func (w RuleWhen) matchesKindAndIdentity(mc MatchContext) bool {
	if len(w.TurnKinds) > 0 {
		found := false
		for _, k := range w.TurnKinds {
			if k == mc.Kind {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if w.Phase != "" && w.Phase != mc.In.Session.ClientPhase {
		return false
	}
	if w.Project != "" && w.Project != mc.In.Project {
		return false
	}
	if w.PathClass != "" && !containsString(mc.In.PathClassHits, w.PathClass) {
		return false
	}
	if w.Entitlement != "" && w.Entitlement != mc.In.Entitlement {
		return false
	}
	if w.Sidechain != nil && *w.Sidechain != mc.In.Session.IsSidechain {
		return false
	}
	return true
}

func (w RuleWhen) matchesShape(mc MatchContext) bool {
	if w.TierAtLeast != "" && mc.Tier.Rank() < w.TierAtLeast.Rank() {
		return false
	}
	if w.ModelGlob != "" {
		ok, err := path.Match(w.ModelGlob, strings.ToLower(mc.In.Shape.Model))
		if err != nil || !ok {
			return false
		}
	}
	if w.MaxToolUses > 0 && mc.In.Shape.ToolUseCount > w.MaxToolUses {
		return false
	}
	return true
}

func (w RuleWhen) matchesBands(mc MatchContext) bool {
	if w.SessionAgeTurnsMin > 0 && mc.In.Session.SessionAgeTurns < w.SessionAgeTurnsMin {
		return false
	}
	if w.SessionAgeTurnsMax > 0 && mc.In.Session.SessionAgeTurns > w.SessionAgeTurnsMax {
		return false
	}
	if w.MinPromptTokens > 0 && mc.In.Shape.PromptTokens < w.MinPromptTokens {
		return false
	}
	if w.MaxPromptTokens > 0 && mc.In.Shape.PromptTokens > w.MaxPromptTokens {
		return false
	}
	if w.BudgetBandAtLeast > 0 && mc.BudgetBurnMax < w.BudgetBandAtLeast {
		return false
	}
	return true
}

func containsString(set []string, want string) bool {
	for _, s := range set {
		if s == want {
			return true
		}
	}
	return false
}

// RuleAction is what a matched rule does (§R6.3). Exactly one primary
// action is set (lint enforces this): route_to_tier, route_to_model,
// pin_tier, no_route, set_effort, set_fallback_chain, or a provider
// filter.
type RuleAction struct {
	// RouteToTier selects the same-shape representative of this tier
	// (downshift only).
	RouteToTier Tier
	// RouteToModel selects a specific model (same-shape, classified).
	RouteToModel string
	// PinTier pins the turn to a tier's representative — both
	// directions (opusplan pins plan turns UP). Pins bypass cache-hold
	// and stickiness: they are explicit quality intents.
	PinTier Tier
	// NoRoute is an explicit exemption: the rule pins the turn to its
	// original model and stops the walk.
	NoRoute bool
	// SetEffort downshifts effort instead of model (§R6.5).
	SetEffort string
	// SetFallbackChain overrides the §R12.1 fallback chain for
	// matching turns.
	SetFallbackChain []string
	// DenyProviders / AllowProviders constrain the candidate provider
	// set for matching turns (§R6.3, §R16). Evaluated by the privacy
	// basis in the decision pipeline.
	DenyProviders  []string
	AllowProviders []string
}

// Rule is one row of a policy's ordered rule table. Rules are walked
// top-down; the first matching rule decides (§24.5).
type Rule struct {
	// Name identifies the rule on decision rows (transparency surface).
	Name   string
	When   RuleWhen
	Action RuleAction
	// Reason is the closed-enum code stamped on decisions this rule
	// produces.
	Reason ReasonCode
}

// PrivacyRule is one compiled §R16 privacy rule: selector (project /
// path-class) plus effect (local-only / provider filters). Hard filter
// — evaluated before ranking.
type PrivacyRule struct {
	Project        string
	PathClass      string
	LocalOnly      bool
	DenyProviders  []string
	AllowProviders []string
}

// AppliesTo reports whether the rule's selectors match the input.
func (r PrivacyRule) AppliesTo(in DecisionInput) bool {
	if r.Project != "" && r.Project != in.Project {
		return false
	}
	if r.PathClass != "" && !containsString(in.PathClassHits, r.PathClass) {
		return false
	}
	return true
}

// BudgetScope is one compiled §R14 budget scope. The limit lives here;
// the window-to-date spend arrives via Snapshot.BudgetBurn.
type BudgetScope struct {
	Scope     string
	LimitUSD  float64
	Window    string
	Bands     []float64
	Exhausted string
}

// Budget exhaustion behaviors (§R14).
const (
	BudgetDegradeAll = "degrade_all"
	BudgetAdviseOnly = "advise_only"
	BudgetHardStop   = "hard_stop"
)

// RateLimitPolicy is the compiled §R15 subscription-window group.
type RateLimitPolicy struct {
	Enabled     bool
	HeadroomPct int
}

// Policy is an ordered composition of bases plus rule tables and the
// compiled §R14–R16 groups — the unit users select, version, and (P2)
// distribute (§R6.1).
type Policy struct {
	Name        string
	Description string
	// MinTurnsBetweenSwitches is the session-coherence floor (§R13): a
	// proposed switch within this many turns of the previous one is
	// held with ReasonStickinessHold. Zero disables the floor.
	MinTurnsBetweenSwitches int
	// RespectCache enables §R13 cache-priced switch economics. When
	// false the engine skips the cache-hold check (forfeit still
	// reported, never acted on).
	RespectCache bool
	// Bases is the ordered basis list (§R6.2). Filters intersect in
	// this order, the first ranker is the primary sort, modifiers
	// adjust. The capability basis is ALWAYS evaluated first whether
	// or not it is listed.
	Bases []string
	// Floors are the quality_floor basis's per-turn-kind minimum tiers
	// (§R6.2). A kind absent from the map floors at haiku-class — the
	// engine never auto-routes below the smallest paid class without
	// an explicit floor saying so.
	Floors map[TurnKind]Tier
	// PathClasses is the compiled [routing.path_classes] vocabulary —
	// referenced by rules and privacy rules; matching happens at the
	// boundary (the engine sees hit flags only, §R16).
	PathClasses map[string][]string
	// PrivacyRules are the compiled §R16 rules (hard filters).
	PrivacyRules []PrivacyRule
	// BudgetScopes are the compiled §R14 scopes.
	BudgetScopes []BudgetScope
	// RateLimit is the compiled §R15 group.
	RateLimit RateLimitPolicy
	// Fallbacks maps a model id or tier name to its ordered §R12.1
	// fallback chain.
	Fallbacks map[string][]string
	Rules     []Rule
}

// Hash returns the content hash of the policy — recorded on every
// decision row so rollbacks stay attributable to the exact compiled
// content that decided (§R6.6). Deterministic over the canonical
// serialization: map fields are serialized in sorted key order.
func (p Policy) Hash() string {
	var b strings.Builder
	fmt.Fprintf(&b, "policy:%s;minturns:%d;respect_cache:%v;bases:%v;",
		p.Name, p.MinTurnsBetweenSwitches, p.RespectCache, p.Bases)
	for _, k := range sortedTurnKindKeys(p.Floors) {
		fmt.Fprintf(&b, "floor:%s=%s;", k, p.Floors[k])
	}
	for _, name := range sortedStringListKeys(p.PathClasses) {
		fmt.Fprintf(&b, "path_class:%s=%v;", name, p.PathClasses[name])
	}
	for _, pr := range p.PrivacyRules {
		fmt.Fprintf(&b, "privacy:%s|%s|%v|%v|%v;", pr.Project, pr.PathClass, pr.LocalOnly, pr.DenyProviders, pr.AllowProviders)
	}
	for _, bs := range p.BudgetScopes {
		fmt.Fprintf(&b, "budget:%s|%.4f|%s|%v|%s;", bs.Scope, bs.LimitUSD, bs.Window, bs.Bands, bs.Exhausted)
	}
	fmt.Fprintf(&b, "rate_limit:%v|%d;", p.RateLimit.Enabled, p.RateLimit.HeadroomPct)
	for _, k := range sortedStringListKeys(p.Fallbacks) {
		fmt.Fprintf(&b, "fallback:%s=%v;", k, p.Fallbacks[k])
	}
	for _, r := range p.Rules {
		fmt.Fprintf(&b, "rule:%s;kinds:%v;phase:%s;tier_at_least:%s;glob:%s;project:%s;path_class:%s;age:%d-%d;max_tools:%d;prompt:%d-%d;band:%.4f;ent:%s;",
			r.Name, r.When.TurnKinds, r.When.Phase, r.When.TierAtLeast, r.When.ModelGlob,
			r.When.Project, r.When.PathClass, r.When.SessionAgeTurnsMin, r.When.SessionAgeTurnsMax,
			r.When.MaxToolUses, r.When.MinPromptTokens, r.When.MaxPromptTokens,
			r.When.BudgetBandAtLeast, r.When.Entitlement)
		if r.When.Sidechain != nil {
			fmt.Fprintf(&b, "sidechain:%v;", *r.When.Sidechain)
		}
		fmt.Fprintf(&b, "route_to:%s;route_to_model:%s;pin:%s;no_route:%v;set_effort:%s;chain:%v;deny:%v;allow:%v;reason:%s;",
			r.Action.RouteToTier, r.Action.RouteToModel, r.Action.PinTier, r.Action.NoRoute,
			r.Action.SetEffort, r.Action.SetFallbackChain, r.Action.DenyProviders,
			r.Action.AllowProviders, r.Reason)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

func sortedTurnKindKeys(m map[TurnKind]Tier) []TurnKind {
	out := make([]TurnKind, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortedStringListKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// defaultBases is the basis set custom policies and the value template
// run (§R6.2): always-on capability first, the hard filters, the cost
// ranker, then the dynamic modifiers.
func defaultBases() []string {
	return []string{
		BasisCapability, BasisQualityFloor, BasisPrivacy,
		BasisCostMinimize, BasisBudget, BasisAvailability, BasisRateLimitWindow,
	}
}

// defaultFloors floors every kind at haiku-class — the engine never
// auto-routes below the smallest paid class unless a template or
// custom floor explicitly allows it.
func defaultFloors() map[TurnKind]Tier {
	floors := make(map[TurnKind]Tier, len(AllTurnKinds()))
	for _, k := range AllTurnKinds() {
		floors[k] = TierHaikuClass
	}
	return floors
}

// Templates returns the built-in policy templates (§R6.4), each an
// inspectable rule table — never a black box:
//
//   - value: quality floors + cost minimization. Plan/edit/long-context
//     untouched; overpowered exploration and housekeeping downshift to
//     haiku-class; flagship sub-agent turns step down one tier.
//   - frugal: budget-first. Every soft turn-kind above haiku-class
//     downshifts to haiku-class; housekeeping/test floors tolerate the
//     free tier (§R6.4 free-tier tolerance).
//   - fast: latency primary — rankers prefer observed-fast models; no
//     downshift rules of its own beyond read-only exploration.
//   - strict-privacy: local/allow-listed only; every turn carries the
//     privacy filter (operators add [routing.privacy.rules] selecting
//     their projects).
//   - plan-exec: opusplan generalized — plan pinned, flagship edit
//     turns execute on sonnet-class, soft kinds downshift.
//   - enterprise-default: value's rules with conservative coherence —
//     the org-distributable starting point (§R6.4).
func Templates() []Policy {
	value := Policy{
		Name:                    "value",
		Description:             "Quality floors + cost minimization: downshift overpowered soft turns, leave plan/edit/long-context untouched.",
		MinTurnsBetweenSwitches: 5,
		RespectCache:            true,
		Bases:                   defaultBases(),
		Floors:                  defaultFloors(),
		Rules: []Rule{
			{
				Name:   "read_only_overpowered",
				When:   RuleWhen{TurnKinds: []TurnKind{TurnReadOnly}, TierAtLeast: TierSonnetClass},
				Action: RuleAction{RouteToTier: TierHaikuClass},
				Reason: ReasonOverpoweredRead,
			},
			{
				Name:   "housekeeping_overpowered",
				When:   RuleWhen{TurnKinds: []TurnKind{TurnHousekeeping}, TierAtLeast: TierSonnetClass},
				Action: RuleAction{RouteToTier: TierHaikuClass},
				Reason: ReasonOverpoweredHousekeeping,
			},
			{
				Name:   "subagent_flagship_stepdown",
				When:   RuleWhen{TurnKinds: []TurnKind{TurnSubagent}, TierAtLeast: TierOpusClass},
				Action: RuleAction{RouteToTier: TierSonnetClass},
				Reason: ReasonOverpoweredSubagent,
			},
		},
	}

	frugalFloors := defaultFloors()
	frugalFloors[TurnHousekeeping] = TierFree
	frugalFloors[TurnTestRun] = TierFree
	frugal := Policy{
		Name:                    "frugal",
		Description:             "Aggressive cost reduction: every soft turn-kind above haiku-class downshifts to haiku-class; housekeeping and test floors tolerate the free tier.",
		MinTurnsBetweenSwitches: 3,
		RespectCache:            true,
		Bases:                   defaultBases(),
		Floors:                  frugalFloors,
		Rules: []Rule{
			{
				Name:   "read_only_to_haiku",
				When:   RuleWhen{TurnKinds: []TurnKind{TurnReadOnly}, TierAtLeast: TierSonnetClass},
				Action: RuleAction{RouteToTier: TierHaikuClass},
				Reason: ReasonOverpoweredRead,
			},
			{
				Name:   "housekeeping_to_haiku",
				When:   RuleWhen{TurnKinds: []TurnKind{TurnHousekeeping}, TierAtLeast: TierSonnetClass},
				Action: RuleAction{RouteToTier: TierHaikuClass},
				Reason: ReasonOverpoweredHousekeeping,
			},
			{
				Name:   "subagent_to_haiku",
				When:   RuleWhen{TurnKinds: []TurnKind{TurnSubagent}, TierAtLeast: TierSonnetClass},
				Action: RuleAction{RouteToTier: TierHaikuClass},
				Reason: ReasonOverpoweredSubagent,
			},
			{
				Name:   "test_run_to_haiku",
				When:   RuleWhen{TurnKinds: []TurnKind{TurnTestRun}, TierAtLeast: TierSonnetClass},
				Action: RuleAction{RouteToTier: TierHaikuClass},
				Reason: ReasonOverpoweredTestRun,
			},
		},
	}

	fast := Policy{
		Name:                    "fast",
		Description:             "Latency primary: observed-fast models preferred for interactive turn-kinds; only read-only exploration downshifts.",
		MinTurnsBetweenSwitches: 5,
		RespectCache:            true,
		Bases: []string{
			BasisCapability, BasisQualityFloor, BasisPrivacy,
			BasisLatency, BasisCostMinimize,
			BasisBudget, BasisAvailability, BasisRateLimitWindow,
		},
		Floors: defaultFloors(),
		Rules: []Rule{
			{
				Name:   "read_only_overpowered",
				When:   RuleWhen{TurnKinds: []TurnKind{TurnReadOnly}, TierAtLeast: TierSonnetClass},
				Action: RuleAction{RouteToTier: TierHaikuClass},
				Reason: ReasonOverpoweredRead,
			},
		},
	}

	strictPrivacy := Policy{
		Name:                    "strict-privacy",
		Description:             "Local / allow-listed destinations only: the privacy filter applies to every turn; operators scope it with [routing.privacy.rules].",
		MinTurnsBetweenSwitches: 5,
		RespectCache:            true,
		Bases: []string{
			BasisCapability, BasisPrivacy, BasisQualityFloor,
			BasisCostMinimize, BasisBudget, BasisAvailability, BasisRateLimitWindow,
		},
		Floors: defaultFloors(),
	}

	planExec := Policy{
		Name:                    "plan-exec",
		Description:             "opusplan generalized: plan pinned to its model, flagship edit turns execute on sonnet-class, soft kinds downshift.",
		MinTurnsBetweenSwitches: 5,
		RespectCache:            true,
		Bases:                   defaultBases(),
		Floors:                  defaultFloors(),
		Rules: []Rule{
			{
				Name:   "plan_pin",
				When:   RuleWhen{TurnKinds: []TurnKind{TurnPlan}},
				Action: RuleAction{NoRoute: true},
				Reason: ReasonPhasePin,
			},
			{
				Name:   "edit_exec_tier",
				When:   RuleWhen{TurnKinds: []TurnKind{TurnEdit}, TierAtLeast: TierOpusClass},
				Action: RuleAction{RouteToTier: TierSonnetClass},
				Reason: ReasonPhasePin,
			},
			{
				Name:   "read_only_overpowered",
				When:   RuleWhen{TurnKinds: []TurnKind{TurnReadOnly}, TierAtLeast: TierSonnetClass},
				Action: RuleAction{RouteToTier: TierHaikuClass},
				Reason: ReasonOverpoweredRead,
			},
			{
				Name:   "housekeeping_overpowered",
				When:   RuleWhen{TurnKinds: []TurnKind{TurnHousekeeping}, TierAtLeast: TierSonnetClass},
				Action: RuleAction{RouteToTier: TierHaikuClass},
				Reason: ReasonOverpoweredHousekeeping,
			},
		},
	}

	enterprise := value
	enterprise.Name = "enterprise-default"
	enterprise.Description = "Org-distributable starting point: the value rule table with conservative coherence (longer switch floor)."
	enterprise.MinTurnsBetweenSwitches = 8
	enterprise.Rules = append([]Rule{}, value.Rules...)

	return []Policy{value, frugal, fast, strictPrivacy, planExec, enterprise}
}

// TemplateByName resolves a built-in template.
func TemplateByName(name string) (Policy, bool) {
	for _, p := range Templates() {
		if p.Name == name {
			return p, true
		}
	}
	return Policy{}, false
}

// TemplateNames lists the built-in templates in registry order.
func TemplateNames() []string {
	ts := Templates()
	out := make([]string, len(ts))
	for i, p := range ts {
		out[i] = p.Name
	}
	return out
}
