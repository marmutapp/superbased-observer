package config

import (
	"fmt"
	"strings"
)

// RoutingConfig gates the model-routing layer
// (docs/model-routing-spec.md §R21). OFF by default — routing is an
// opt-in feature (unlike cachetrack's default-ON observation): the
// master gate ships false and nothing under it has any effect until
// the operator flips it. The partial-merge invariant still applies in
// the other direction: an install that sets ONLY `[routing] enabled =
// true` must inherit every other default below (mode=advise,
// policy=value, stickiness, calibration…) from Default(), never
// zero values.
//
// Deep semantic validation (unknown tier names, unreachable rules,
// unknown path-class references) is deliberately NOT here: it lives in
// `observer routing lint` / the policy compiler (§R6.6), which has the
// routing vocabulary. Config validates shape-level constraints only,
// so a typo'd tier name degrades to a lint finding + fail-open rule,
// never a daemon that refuses to start.
type RoutingConfig struct {
	// Enabled is the master gate. Default false (opt-in feature).
	Enabled bool `toml:"enabled"`
	// Mode is off | advise | enforce (§R4). Default "advise" — even
	// after enabling, routing only logs what it would have done until
	// the operator explicitly promotes to enforce (§R6.6 lifecycle).
	Mode string `toml:"mode"`
	// Policy names a shipped template (value | frugal | fast |
	// plan-exec | strict-privacy | enterprise-default) or "custom"
	// ([[routing.rules]] only). Default "value".
	Policy string `toml:"policy"`
	// DecisionLogRetentionDays bounds router_decisions row age; the
	// existing retention pass prunes older rows. Default 180. ≤ 0
	// disables pruning.
	DecisionLogRetentionDays int `toml:"decision_log_retention_days"`

	// Tiers overrides the shipped seed tier table (§R7.1): model or
	// family key → tier name. Applied via TierResolver.Reload at the
	// boundary; unknown tier values are lint findings, not load errors.
	Tiers map[string]string `toml:"tiers"`

	// BenchmarkFiles are versioned LOCAL benchmark result files (§R7.3
	// — RouterBench/RouterEval-format coding scores) whose derived
	// placements refine the seed tier table at load time. No network
	// at decision time; provenance is logged. Explicit [routing.tiers]
	// overrides win over benchmark placements.
	BenchmarkFiles []string `toml:"benchmark_files"`

	Stickiness      RoutingStickinessConfig      `toml:"stickiness"`
	Budget          RoutingBudgetConfig          `toml:"budget"`
	RateLimitWindow RoutingRateLimitWindowConfig `toml:"rate_limit_window"`

	// PathClasses defines named path-glob groups once (§R16); privacy
	// rules and rule when-clauses reference them by name. Shared
	// vocabulary with the guard-layer initiative (§R26.5).
	PathClasses map[string][]string `toml:"path_classes"`

	Privacy     RoutingPrivacyConfig     `toml:"privacy"`
	Reliability RoutingReliabilityConfig `toml:"reliability"`
	Calibration RoutingCalibrationConfig `toml:"calibration"`
	Translation RoutingTranslationConfig `toml:"translation"`

	// KeyPool maps a provider name ("anthropic", "openai") to a
	// rotating API-key ring (§R12.4): on 429s the proxy retries with
	// the next key before model fallbacks engage. LOCAL-ONLY by
	// construction — config.toml is never synced or pushed — and the
	// keys are stored plaintext here; the §R11.5 keychain-backed vault
	// is the P3 upgrade path. Key material is never logged.
	KeyPool map[string][]string `toml:"key_pool"`

	// LocalUpstreams declare Ollama / LM Studio / vLLM endpoints as
	// routable targets (§R11.7): OpenAI wire shape, TierLocal
	// placement, privacy-basis destinations, health-observed like any
	// provider. Local-only config — never pushed anywhere.
	LocalUpstreams []RoutingLocalUpstreamConfig `toml:"local_upstreams"`

	// Rules are custom policy rows (§R6.3), appended AFTER template
	// expansion — they extend the named template; with policy =
	// "custom" they are the whole table.
	Rules []RoutingRuleConfig `toml:"rules"`
}

// RoutingStickinessConfig is the §R13 session-coherence group.
type RoutingStickinessConfig struct {
	// RespectCache enables cache-priced switch economics: a switch
	// whose cache forfeit exceeds its saving is held. Default true.
	RespectCache bool `toml:"respect_cache"`
	// MinTurnsBetweenSwitches is the coherence floor. Default 5.
	MinTurnsBetweenSwitches int `toml:"min_turns_between_switches"`
}

// RoutingBudgetConfig groups §R14 budget scopes.
type RoutingBudgetConfig struct {
	Scopes []RoutingBudgetScopeConfig `toml:"scopes"`
}

// RoutingBudgetScopeConfig is one budget scope (§R14): a spend limit
// over a window with graceful-degradation burn bands.
type RoutingBudgetScopeConfig struct {
	// Scope is "global", "project:<name>", "tool:<name>", or
	// "tier:<tier>".
	Scope string `toml:"scope"`
	// LimitUSD is the budget for the window. Must be > 0.
	LimitUSD float64 `toml:"limit_usd"`
	// Window is day | week | month.
	Window string `toml:"window"`
	// Bands are the burn fractions at which the budget modifier
	// progressively demotes soft turn-kinds. Empty → [0.5, 0.8, 0.95].
	Bands []float64 `toml:"bands"`
	// Exhausted picks the 100% behavior: degrade_all | advise_only |
	// hard_stop. Empty → advise_only (the conservative default).
	Exhausted string `toml:"exhausted"`
}

// RoutingRateLimitWindowConfig is the §R15 subscription-window group.
type RoutingRateLimitWindowConfig struct {
	// Enabled gates the rate_limit_window modifier. Default false —
	// it acts only for operators on subscription-window plans.
	Enabled bool `toml:"enabled"`
	// HeadroomPct is the projected-cap headroom (percent of the
	// window's learned capacity) the modifier preserves for hard
	// turn-kinds. Default 15.
	HeadroomPct int `toml:"headroom_pct"`
}

// RoutingPrivacyConfig groups §R16 privacy rules.
type RoutingPrivacyConfig struct {
	Rules []RoutingPrivacyRuleConfig `toml:"rules"`
}

// RoutingPrivacyRuleConfig is one privacy rule (§R16). At least one
// selector (project / path_class) and at least one effect (local_only /
// deny_providers / allow_providers) must be set.
type RoutingPrivacyRuleConfig struct {
	// Project selects by project name (exact match against the
	// observed project). Empty = any project.
	Project string `toml:"project"`
	// PathClass selects turns whose recent action targets matched the
	// named [routing.path_classes] entry. Empty = any.
	PathClass string `toml:"path_class"`
	// LocalOnly denies every non-local destination.
	LocalOnly bool `toml:"local_only"`
	// DenyProviders / AllowProviders filter candidate providers.
	// "*" in DenyProviders means deny-all-except-allowed.
	DenyProviders  []string `toml:"deny_providers"`
	AllowProviders []string `toml:"allow_providers"`
}

// RoutingReliabilityConfig is the §R12 reliability group.
type RoutingReliabilityConfig struct {
	// MaxRetries bounds same-target retries for idempotent failure
	// classes (§R12.2). Default 2; 0 disables retries. Capped at 10.
	MaxRetries int `toml:"max_retries"`
	// RetryConnectionReset / RetryOverloaded / RetryUnavailable are
	// the per-class toggles (§R12.2): transport resets, 529
	// overloaded, 503-with-retry-after. All default true.
	RetryConnectionReset bool `toml:"retry_connection_reset"`
	RetryOverloaded      bool `toml:"retry_overloaded"`
	RetryUnavailable     bool `toml:"retry_unavailable"`
	// Fallbacks declares ordered fallback chains (§R12.1): key is a
	// model id or tier name, value the ordered same-shape fallback
	// models tried on 429/5xx/timeout.
	Fallbacks map[string][]string `toml:"fallbacks"`
	Probe     RoutingProbeConfig  `toml:"probe"`
}

// RoutingProbeConfig is the §R12.3 optional active prober — OFF by
// default (no gratuitous network; passive health is the default
// mechanism).
type RoutingProbeConfig struct {
	Enabled bool `toml:"enabled"`
	// IntervalSeconds is the probe cadence when enabled. Default 300.
	IntervalSeconds int `toml:"interval_seconds"`
}

// RoutingCalibrationConfig is the §R7.2 / §R18.3 calibration group.
type RoutingCalibrationConfig struct {
	// Enabled gates the watcher-side calibration job. Default true
	// (it only computes; acting on deltas needs the evidence gate).
	Enabled bool `toml:"enabled"`
	// MinSamples is the evidence threshold below which a calibration
	// delta never ACTS (display is always-on). Default 50.
	MinSamples int `toml:"min_samples"`
	// AutoDemote demotes a rule to advise when grading shows a
	// regression (§R18.3). Default true.
	AutoDemote bool `toml:"auto_demote"`
}

// RoutingTranslationConfig is the §R11.4 cross-provider translation
// track. P3: the section parses (so a future config file is
// forward-compatible) but enabling it is rejected at validation until
// the translation track ships.
type RoutingTranslationConfig struct {
	Enabled bool `toml:"enabled"`
}

// RoutingLocalUpstreamConfig is one §R11.7 local inference endpoint.
type RoutingLocalUpstreamConfig struct {
	// Name labels the endpoint ("ollama", "lmstudio", "vllm-box").
	Name string `toml:"name"`
	// BaseURL is the OpenAI-shape endpoint root, e.g.
	// "http://localhost:11434/v1".
	BaseURL string `toml:"base_url"`
	// Models are the model ids this endpoint serves; each places into
	// TierLocal and routes to BaseURL when selected.
	Models []string `toml:"models"`
}

// RoutingRuleConfig is one custom policy rule (§R6.3): a when clause
// plus exactly one action, walked top-down, first match wins.
type RoutingRuleConfig struct {
	// Name attributes decisions to this rule. Required.
	Name   string                  `toml:"name"`
	When   RoutingRuleWhenConfig   `toml:"when"`
	Action RoutingRuleActionConfig `toml:"action"`
}

// RoutingRuleWhenConfig is a rule's match clause (§R6.3). Zero-valued
// fields match anything; set fields AND together. Time-window matching
// is deliberately absent: the engine is deterministic given (snapshot,
// input, policy hash) and a wall-clock matcher would break replay.
type RoutingRuleWhenConfig struct {
	// TurnKind / TurnKinds match the classified kind (single-value
	// sugar and the list form; both set = union).
	TurnKind  string   `toml:"turn_kind"`
	TurnKinds []string `toml:"turn_kinds"`
	// Phase matches the client-declared phase ("plan").
	Phase string `toml:"phase"`
	// TierAtLeast matches models whose tier ranks at or above this.
	TierAtLeast string `toml:"tier_at_least"`
	// Model is a glob over the original model id.
	Model string `toml:"model"`
	// Project matches the observed project name exactly.
	Project string `toml:"project"`
	// PathClass matches turns whose recent action targets hit the
	// named [routing.path_classes] entry.
	PathClass string `toml:"path_class"`
	// SessionAgeTurnsMin/Max bound the session's turn count.
	SessionAgeTurnsMin int `toml:"session_age_turns_min"`
	SessionAgeTurnsMax int `toml:"session_age_turns_max"`
	// Sidechain, when present, must equal the turn's sidechain flag.
	Sidechain *bool `toml:"sidechain"`
	// MaxTools matches requests carrying at most this many tools.
	MaxTools int `toml:"max_tools"`
	// MinPromptTokens/MaxPromptTokens band the prompt size.
	MinPromptTokens int64 `toml:"min_prompt_tokens"`
	MaxPromptTokens int64 `toml:"max_prompt_tokens"`
	// BudgetBandAtLeast matches when the active budget scope's burn
	// fraction is at or above this value (0–1].
	BudgetBandAtLeast float64 `toml:"budget_band_at_least"`
	// Entitlement matches the boundary-resolved entitlement class:
	// api_key | subscription (§R11.3).
	Entitlement string `toml:"entitlement"`
}

// RoutingRuleActionConfig is a rule's action (§R6.3). Exactly one
// primary action (route_to_tier / route_to_model / pin_tier /
// no_route / set_effort / set_fallback_chain / provider filter) —
// enforced by lint, not here.
type RoutingRuleActionConfig struct {
	RouteToTier  string `toml:"route_to_tier"`
	RouteToModel string `toml:"route_to_model"`
	// PinTier holds the turn at its tier (no downshift below it).
	PinTier string `toml:"pin_tier"`
	// NoRoute exempts matching turns from routing entirely.
	NoRoute bool `toml:"no_route"`
	// SetEffort downshifts effort instead of model (§R6.5):
	// minimal | low | medium | high.
	SetEffort string `toml:"set_effort"`
	// SetFallbackChain overrides the §R12.1 chain for matching turns.
	SetFallbackChain []string `toml:"set_fallback_chain"`
	// DenyProviders / AllowProviders filter candidates (§R6.3).
	DenyProviders  []string `toml:"deny_providers"`
	AllowProviders []string `toml:"allow_providers"`
	// Reason is the closed-enum reason code stamped on decisions this
	// rule produces; the compiler validates membership.
	Reason string `toml:"reason"`
}

// defaultRouting returns the §R21 defaults. Master gate OFF; everything
// beneath it defaulted so a bare `[routing] enabled = true` behaves
// sensibly (advise mode, value template).
func defaultRouting() RoutingConfig {
	return RoutingConfig{
		Enabled:                  false,
		Mode:                     "advise",
		Policy:                   "value",
		DecisionLogRetentionDays: 180,
		Stickiness: RoutingStickinessConfig{
			RespectCache:            true,
			MinTurnsBetweenSwitches: 5,
		},
		RateLimitWindow: RoutingRateLimitWindowConfig{
			Enabled:     false,
			HeadroomPct: 15,
		},
		Reliability: RoutingReliabilityConfig{
			MaxRetries:           2,
			RetryConnectionReset: true,
			RetryOverloaded:      true,
			RetryUnavailable:     true,
			Probe: RoutingProbeConfig{
				Enabled:         false,
				IntervalSeconds: 300,
			},
		},
		Calibration: RoutingCalibrationConfig{
			Enabled:    true,
			MinSamples: 50,
			AutoDemote: true,
		},
		Translation: RoutingTranslationConfig{Enabled: false},
	}
}

// routingModes / budget windows / exhausted behaviors / effort levels /
// entitlement classes — the closed config-level vocabularies validation
// checks against.
var (
	routingModes            = map[string]bool{"off": true, "advise": true, "enforce": true}
	routingBudgetWindows    = map[string]bool{"day": true, "week": true, "month": true}
	routingExhausted        = map[string]bool{"": true, "degrade_all": true, "advise_only": true, "hard_stop": true}
	routingEffortLevels     = map[string]bool{"": true, "minimal": true, "low": true, "medium": true, "high": true}
	routingEntitlements     = map[string]bool{"": true, "api_key": true, "subscription": true}
	routingBudgetScopeKinds = []string{"project:", "tool:", "tier:"}
)

// ValidateRouting runs the exact shape-level [routing] checks
// config.Load applies (validateRouting). Exported for the dashboard's
// rules-editor save gate: a fragment the PUT accepts must be a fragment
// the next daemon start loads without error — duplicating the checks
// would let the two drift.
func ValidateRouting(rc RoutingConfig) error {
	return validateRouting(rc)
}

// validateRouting checks the shape-level constraints of [routing]
// (§R21). Semantic policy validation (tier vocabulary, rule
// reachability, path-class references) is `observer routing lint`'s
// job — see the RoutingConfig doc comment.
func validateRouting(rc RoutingConfig) error {
	if !routingModes[rc.Mode] {
		return fmt.Errorf("config: routing.mode %q not in {off, advise, enforce}", rc.Mode)
	}
	if rc.Policy == "" {
		return fmt.Errorf("config: routing.policy must name a template or %q", "custom")
	}
	if rc.Translation.Enabled {
		return fmt.Errorf("config: routing.translation.enabled is a P3 track and cannot be enabled yet (§R11.4); enforce-mode candidate sets are same-provider-shape only")
	}
	if rc.RateLimitWindow.HeadroomPct < 0 || rc.RateLimitWindow.HeadroomPct > 100 {
		return fmt.Errorf("config: routing.rate_limit_window.headroom_pct %d must be in [0, 100]", rc.RateLimitWindow.HeadroomPct)
	}
	if rc.Reliability.MaxRetries < 0 || rc.Reliability.MaxRetries > 10 {
		return fmt.Errorf("config: routing.reliability.max_retries %d must be in [0, 10]", rc.Reliability.MaxRetries)
	}
	if rc.Reliability.Probe.Enabled && rc.Reliability.Probe.IntervalSeconds <= 0 {
		return fmt.Errorf("config: routing.reliability.probe.interval_seconds must be > 0 when the probe is enabled")
	}
	if rc.Calibration.MinSamples < 0 {
		return fmt.Errorf("config: routing.calibration.min_samples must be >= 0")
	}
	for i, sc := range rc.Budget.Scopes {
		if err := validateRoutingBudgetScope(sc); err != nil {
			return fmt.Errorf("config: routing.budget.scopes[%d]: %w", i, err)
		}
	}
	for i, pr := range rc.Privacy.Rules {
		if pr.Project == "" && pr.PathClass == "" {
			return fmt.Errorf("config: routing.privacy.rules[%d]: needs a selector (project or path_class)", i)
		}
		if !pr.LocalOnly && len(pr.DenyProviders) == 0 && len(pr.AllowProviders) == 0 {
			return fmt.Errorf("config: routing.privacy.rules[%d]: needs an effect (local_only, deny_providers, or allow_providers)", i)
		}
	}
	for i, lu := range rc.LocalUpstreams {
		if lu.Name == "" {
			return fmt.Errorf("config: routing.local_upstreams[%d]: name is required", i)
		}
		if !strings.HasPrefix(lu.BaseURL, "http://") && !strings.HasPrefix(lu.BaseURL, "https://") {
			return fmt.Errorf("config: routing.local_upstreams[%d] (%s): base_url must be an absolute http(s) URL", i, lu.Name)
		}
		if len(lu.Models) == 0 {
			return fmt.Errorf("config: routing.local_upstreams[%d] (%s): at least one model is required", i, lu.Name)
		}
	}
	for i, r := range rc.Rules {
		if r.Name == "" {
			return fmt.Errorf("config: routing.rules[%d]: name is required (decision rows attribute by it)", i)
		}
		if !routingEffortLevels[r.Action.SetEffort] {
			return fmt.Errorf("config: routing.rules[%d] (%s): set_effort %q not in {minimal, low, medium, high}", i, r.Name, r.Action.SetEffort)
		}
		if !routingEntitlements[r.When.Entitlement] {
			return fmt.Errorf("config: routing.rules[%d] (%s): when.entitlement %q not in {api_key, subscription}", i, r.Name, r.When.Entitlement)
		}
		if b := r.When.BudgetBandAtLeast; b < 0 || b > 1 {
			return fmt.Errorf("config: routing.rules[%d] (%s): when.budget_band_at_least %.2f must be in [0, 1]", i, r.Name, b)
		}
	}
	return nil
}

func validateRoutingBudgetScope(sc RoutingBudgetScopeConfig) error {
	if !validRoutingScope(sc.Scope) {
		return fmt.Errorf("scope %q must be \"global\" or prefixed project:/tool:/tier:", sc.Scope)
	}
	if sc.LimitUSD <= 0 {
		return fmt.Errorf("limit_usd %.2f must be > 0", sc.LimitUSD)
	}
	if !routingBudgetWindows[sc.Window] {
		return fmt.Errorf("window %q not in {day, week, month}", sc.Window)
	}
	if !routingExhausted[sc.Exhausted] {
		return fmt.Errorf("exhausted %q not in {degrade_all, advise_only, hard_stop}", sc.Exhausted)
	}
	prev := 0.0
	for _, b := range sc.Bands {
		if b <= 0 || b >= 1 {
			return fmt.Errorf("band %.2f must be in (0, 1)", b)
		}
		if b <= prev {
			return fmt.Errorf("bands must be strictly ascending")
		}
		prev = b
	}
	return nil
}

func validRoutingScope(scope string) bool {
	if scope == "global" {
		return true
	}
	for _, prefix := range routingBudgetScopeKinds {
		if strings.HasPrefix(scope, prefix) && len(scope) > len(prefix) {
			return true
		}
	}
	return false
}
