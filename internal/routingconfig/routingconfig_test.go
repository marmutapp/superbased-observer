package routingconfig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/routing"
)

// TestComposeOrgPolicy_Rows pins the §R19.1 composition semantics.
func TestComposeOrgPolicy_Rows(t *testing.T) {
	t.Parallel()
	local := routing.PolicySpec{
		Policy:       "value",
		RespectCache: true,
		PathClasses:  map[string][]string{"secrets": {"local/**"}},
		PrivacyRules: []routing.PrivacyRuleSpec{{Project: "local-proj", LocalOnly: true}},
		Rules: []routing.RuleSpec{{
			Name: "local_rule", When: routing.WhenSpec{TurnKind: "test_run", TierAtLeast: "sonnet-class"},
			Action: routing.ActionSpec{RouteToTier: "haiku-class", Reason: "overpowered_test_run"},
		}},
	}
	orgBody := `
[routing]
mode = "enforce"   # MUST be ignored — no remote enforce toggle (§R23)
enabled = true     # likewise

[routing.path_classes]
pii = ["**/customers/**"]
secrets = ["org/**"]

[[routing.privacy.rules]]
project = "eu-data"
deny_providers = ["*"]
allow_providers = ["local"]

[[routing.budget.scopes]]
scope = "global"
limit_usd = 100.0
window = "month"

[[routing.rules]]
name = "org_soft_rule"
when = { turn_kind = "edit", tier_at_least = "opus-class" }
action = { route_to_tier = "sonnet-class", reason = "phase_pin" }
`
	composed, err := ComposeOrgPolicy(local, orgBody)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	// Hard constraints: org privacy rules PRESENT (intersection — they
	// cannot be relaxed by their position).
	if len(composed.PrivacyRules) != 2 || composed.PrivacyRules[0].Project != "eu-data" {
		t.Errorf("privacy rules: %+v", composed.PrivacyRules)
	}
	if len(composed.BudgetScopes) != 1 || composed.BudgetScopes[0].LimitUSD != 100 {
		t.Errorf("budget scopes: %+v", composed.BudgetScopes)
	}
	// Path classes merge; LOCAL definition wins the name collision.
	if got := composed.PathClasses["secrets"]; len(got) != 1 || got[0] != "local/**" {
		t.Errorf("secrets class: %v", got)
	}
	if _, ok := composed.PathClasses["pii"]; !ok {
		t.Error("org pii class lost")
	}
	// Soft rules rank UNDER local (appended after).
	if composed.Rules[0].Name != "local_rule" || composed.Rules[1].Name != "org_soft_rule" {
		t.Errorf("rule order: %v, %v", composed.Rules[0].Name, composed.Rules[1].Name)
	}
	// No remote enforce: the compose output carries no mode/enabled
	// concept at all — PolicySpec has no such field, structurally.
	// (This comment is load-bearing: if PolicySpec ever grows a Mode
	// field, this test must assert the org body cannot set it.)

	// Garbage body degrades to the local spec with the error surfaced.
	degraded, err := ComposeOrgPolicy(local, "not toml = = =")
	if err == nil {
		t.Error("garbage body produced no error")
	}
	if len(degraded.Rules) != 1 || degraded.Rules[0].Name != "local_rule" {
		t.Errorf("degraded spec mutated: %+v", degraded.Rules)
	}

	// The composed spec compiles + lints clean.
	p, issues := routing.Compile(composed)
	if routing.LintHasErrors(issues) {
		t.Errorf("composed policy lints dirty: %+v", issues)
	}
	if len(p.PrivacyRules) != 2 {
		t.Errorf("compiled privacy rules: %d", len(p.PrivacyRules))
	}
}

// TestSpecMirrorsEveryGroup pins the boundary copy: each [routing]
// group lands on the PolicySpec.
func TestSpecMirrorsEveryGroup(t *testing.T) {
	t.Parallel()
	yes := true
	rc := config.RoutingConfig{
		Policy:          "frugal",
		Stickiness:      config.RoutingStickinessConfig{RespectCache: true, MinTurnsBetweenSwitches: 7},
		PathClasses:     map[string][]string{"secrets": {"**/.env*"}},
		RateLimitWindow: config.RoutingRateLimitWindowConfig{Enabled: true, HeadroomPct: 20},
		Reliability:     config.RoutingReliabilityConfig{Fallbacks: map[string][]string{"m": {"f1"}}},
		Privacy:         config.RoutingPrivacyConfig{Rules: []config.RoutingPrivacyRuleConfig{{Project: "p", LocalOnly: true}}},
		Budget:          config.RoutingBudgetConfig{Scopes: []config.RoutingBudgetScopeConfig{{Scope: "global", LimitUSD: 5, Window: "day"}}},
		Rules: []config.RoutingRuleConfig{{
			Name: "r1",
			When: config.RoutingRuleWhenConfig{
				TurnKind: "read_only", TurnKinds: []string{"edit"}, Phase: "plan",
				TierAtLeast: "opus-class", Model: "claude-*", Project: "p",
				PathClass: "secrets", SessionAgeTurnsMin: 1, SessionAgeTurnsMax: 9,
				Sidechain: &yes, MaxTools: 3, MinPromptTokens: 10, MaxPromptTokens: 99,
				BudgetBandAtLeast: 0.5, Entitlement: "api_key",
			},
			Action: config.RoutingRuleActionConfig{
				RouteToTier: "haiku-class", RouteToModel: "m2", PinTier: "sonnet-class",
				NoRoute: true, SetEffort: "low", SetFallbackChain: []string{"f"},
				DenyProviders: []string{"x"}, AllowProviders: []string{"y"}, Reason: "custom_rule",
			},
		}},
	}
	spec := Spec(rc)
	if spec.Policy != "frugal" || spec.MinTurnsBetweenSwitches != 7 || !spec.RespectCache ||
		!spec.RateLimitEnabled || spec.RateLimitHeadroomPct != 20 ||
		len(spec.PrivacyRules) != 1 || len(spec.BudgetScopes) != 1 ||
		len(spec.Fallbacks) != 1 || len(spec.Rules) != 1 {
		t.Fatalf("spec top-level: %+v", spec)
	}
	r := spec.Rules[0]
	if r.When.TurnKind != "read_only" || len(r.When.TurnKinds) != 1 || r.When.Phase != "plan" ||
		r.When.TierAtLeast != "opus-class" || r.When.Model != "claude-*" ||
		r.When.PathClass != "secrets" || r.When.SessionAgeTurnsMax != 9 ||
		r.When.Sidechain == nil || r.When.MaxTools != 3 || r.When.MaxPromptTokens != 99 ||
		r.When.BudgetBandAtLeast != 0.5 || r.When.Entitlement != "api_key" {
		t.Errorf("when mirror: %+v", r.When)
	}
	if r.Action.RouteToTier != "haiku-class" || r.Action.RouteToModel != "m2" ||
		r.Action.PinTier != "sonnet-class" || !r.Action.NoRoute || r.Action.SetEffort != "low" ||
		len(r.Action.SetFallbackChain) != 1 || len(r.Action.DenyProviders) != 1 ||
		r.Action.Reason != "custom_rule" {
		t.Errorf("action mirror: %+v", r.Action)
	}
}

// TestResolvedTierOverrides pins the §R7.3 file path: a good file
// merges (explicit wins); a bad file is skipped with an error, never
// a failure.
func TestResolvedTierOverrides(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	good := filepath.Join(dir, "bench.json")
	if err := os.WriteFile(good, []byte(`{"version":"v1","source":"X","scores":[{"model":"m1","coding_score":0.9},{"model":"m2","coding_score":0.7}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rc := config.RoutingConfig{
		BenchmarkFiles: []string{good, filepath.Join(dir, "absent.json")},
		Tiers:          map[string]string{"m1": "haiku-class"},
	}
	overrides, provenance, errs := ResolvedTierOverrides(rc)
	if len(errs) != 1 {
		t.Errorf("errs = %v, want the absent file reported", errs)
	}
	if len(provenance) != 1 {
		t.Errorf("provenance = %v", provenance)
	}
	if overrides["m1"] != routing.TierHaikuClass {
		t.Errorf("explicit override lost: %v", overrides["m1"])
	}
	if overrides["m2"] != routing.TierSonnetClass {
		t.Errorf("benchmark placement lost: %v", overrides["m2"])
	}
}

// TestLocalUpstreamRouting pins the §R11.7 derivation.
func TestLocalUpstreamRouting(t *testing.T) {
	t.Parallel()
	tiers, reps, routes := LocalUpstreamRouting(config.RoutingConfig{
		LocalUpstreams: []config.RoutingLocalUpstreamConfig{{
			Name: "ollama", BaseURL: "http://localhost:11434/v1",
			Models: []string{"ollama/llama4", "ollama/qwen3"},
		}},
	})
	if tiers["ollama/llama4"] != routing.TierLocal || tiers["ollama/qwen3"] != routing.TierLocal {
		t.Errorf("tiers: %v", tiers)
	}
	if reps[routing.ShapeOpenAI] != "ollama/llama4" {
		t.Errorf("rep: %v", reps)
	}
	if routes["ollama/qwen3"] != "http://localhost:11434/v1" {
		t.Errorf("routes: %v", routes)
	}
	if a, b, c := LocalUpstreamRouting(config.RoutingConfig{}); a != nil || b != nil || c != nil {
		t.Error("empty config produced non-nil outputs")
	}
}
