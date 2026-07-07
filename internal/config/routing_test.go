package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadRoutingTOML writes body as a global config file and loads it.
func loadRoutingTOML(t *testing.T, body string) (Config, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return Load(LoadOptions{GlobalPath: path, Env: func(string) string { return "" }})
}

func TestRoutingDefaults(t *testing.T) {
	rc := Default().Routing
	if rc.Enabled {
		t.Fatal("routing must be OFF by default (opt-in feature, §R21)")
	}
	if rc.Mode != "advise" {
		t.Errorf("default mode = %q, want advise", rc.Mode)
	}
	if rc.Policy != "value" {
		t.Errorf("default policy = %q, want value", rc.Policy)
	}
	if rc.DecisionLogRetentionDays != 180 {
		t.Errorf("default retention = %d, want 180", rc.DecisionLogRetentionDays)
	}
	if !rc.Stickiness.RespectCache || rc.Stickiness.MinTurnsBetweenSwitches != 5 {
		t.Errorf("stickiness defaults = %+v, want respect_cache=true min_turns=5", rc.Stickiness)
	}
	if rc.RateLimitWindow.Enabled || rc.RateLimitWindow.HeadroomPct != 15 {
		t.Errorf("rate_limit_window defaults = %+v, want disabled headroom=15", rc.RateLimitWindow)
	}
	rel := rc.Reliability
	if rel.MaxRetries != 2 || !rel.RetryConnectionReset || !rel.RetryOverloaded || !rel.RetryUnavailable {
		t.Errorf("reliability defaults = %+v", rel)
	}
	if rel.Probe.Enabled || rel.Probe.IntervalSeconds != 300 {
		t.Errorf("probe defaults = %+v, want disabled interval=300", rel.Probe)
	}
	if !rc.Calibration.Enabled || rc.Calibration.MinSamples != 50 || !rc.Calibration.AutoDemote {
		t.Errorf("calibration defaults = %+v", rc.Calibration)
	}
	if rc.Translation.Enabled {
		t.Error("translation must default disabled (P3 track)")
	}
}

// TestRoutingPartialMergeKeepsDefaults pins the partial-merge invariant:
// a [routing] section that sets ONLY the master gate inherits every
// other default — never zero values.
func TestRoutingPartialMergeKeepsDefaults(t *testing.T) {
	cfg, err := loadRoutingTOML(t, "[routing]\nenabled = true\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rc := cfg.Routing
	if !rc.Enabled {
		t.Fatal("enabled not applied")
	}
	if rc.Mode != "advise" || rc.Policy != "value" || rc.DecisionLogRetentionDays != 180 {
		t.Errorf("partial merge lost top-level defaults: %+v", rc)
	}
	if rc.Stickiness.MinTurnsBetweenSwitches != 5 || !rc.Stickiness.RespectCache {
		t.Errorf("partial merge lost stickiness defaults: %+v", rc.Stickiness)
	}
	if !rc.Calibration.Enabled || rc.Calibration.MinSamples != 50 {
		t.Errorf("partial merge lost calibration defaults: %+v", rc.Calibration)
	}
	if rc.Reliability.MaxRetries != 2 {
		t.Errorf("partial merge lost reliability defaults: %+v", rc.Reliability)
	}
}

// TestRoutingFullSectionParses exercises every §R21 field group with
// the spec's consolidated example (translation left disabled — see the
// reject test).
func TestRoutingFullSectionParses(t *testing.T) {
	cfg, err := loadRoutingTOML(t, `
[routing]
enabled = true
mode = "enforce"
policy = "frugal"
decision_log_retention_days = 90

[routing.tiers]
"my-finetune-v2" = "sonnet-class"

[routing.stickiness]
respect_cache = false
min_turns_between_switches = 3

[[routing.budget.scopes]]
scope = "project:acme-api"
limit_usd = 40.0
window = "week"
bands = [0.5, 0.8, 0.95]
exhausted = "advise_only"

[routing.rate_limit_window]
enabled = true
headroom_pct = 20

[routing.path_classes]
secrets = ["**/.env*", "infra/**"]

[[routing.privacy.rules]]
project = "internal-ml"
local_only = true

[[routing.privacy.rules]]
path_class = "secrets"
deny_providers = ["*"]
allow_providers = ["local"]

[routing.reliability]
max_retries = 3
retry_overloaded = false
[routing.reliability.fallbacks]
"claude-opus-4-8" = ["claude-sonnet-4-6", "claude-haiku-4-5"]
[routing.reliability.probe]
enabled = true
interval_seconds = 120

[routing.calibration]
enabled = true
min_samples = 100
auto_demote = false

[[routing.rules]]
name = "cheap_reads"
when = { turn_kind = "read_only", max_tools = 2, tier_at_least = "opus-class" }
action = { route_to_tier = "haiku-class", reason = "overpowered_read" }

[[routing.rules]]
name = "plan_pin"
when = { phase = "plan" }
action = { no_route = true, reason = "phase_pin" }

[[routing.rules]]
name = "effort_downshift"
when = { turn_kinds = ["test_run", "housekeeping"], entitlement = "subscription" }
action = { set_effort = "low", reason = "overpowered_test_run" }
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rc := cfg.Routing
	if rc.Mode != "enforce" || rc.Policy != "frugal" || rc.DecisionLogRetentionDays != 90 {
		t.Errorf("top-level fields: %+v", rc)
	}
	if got := rc.Tiers["my-finetune-v2"]; got != "sonnet-class" {
		t.Errorf("tiers override = %q", got)
	}
	if rc.Stickiness.RespectCache || rc.Stickiness.MinTurnsBetweenSwitches != 3 {
		t.Errorf("stickiness: %+v", rc.Stickiness)
	}
	if len(rc.Budget.Scopes) != 1 {
		t.Fatalf("budget scopes = %d, want 1", len(rc.Budget.Scopes))
	}
	sc := rc.Budget.Scopes[0]
	if sc.Scope != "project:acme-api" || sc.LimitUSD != 40.0 || sc.Window != "week" ||
		len(sc.Bands) != 3 || sc.Exhausted != "advise_only" {
		t.Errorf("budget scope: %+v", sc)
	}
	if !rc.RateLimitWindow.Enabled || rc.RateLimitWindow.HeadroomPct != 20 {
		t.Errorf("rate_limit_window: %+v", rc.RateLimitWindow)
	}
	if got := rc.PathClasses["secrets"]; len(got) != 2 || got[0] != "**/.env*" {
		t.Errorf("path_classes: %v", rc.PathClasses)
	}
	if len(rc.Privacy.Rules) != 2 || !rc.Privacy.Rules[0].LocalOnly || rc.Privacy.Rules[1].PathClass != "secrets" {
		t.Errorf("privacy rules: %+v", rc.Privacy.Rules)
	}
	rel := rc.Reliability
	if rel.MaxRetries != 3 || rel.RetryOverloaded || !rel.RetryConnectionReset {
		t.Errorf("reliability: %+v", rel)
	}
	if chain := rel.Fallbacks["claude-opus-4-8"]; len(chain) != 2 || chain[0] != "claude-sonnet-4-6" {
		t.Errorf("fallbacks: %v", rel.Fallbacks)
	}
	if !rel.Probe.Enabled || rel.Probe.IntervalSeconds != 120 {
		t.Errorf("probe: %+v", rel.Probe)
	}
	if rc.Calibration.MinSamples != 100 || rc.Calibration.AutoDemote {
		t.Errorf("calibration: %+v", rc.Calibration)
	}
	if len(rc.Rules) != 3 {
		t.Fatalf("rules = %d, want 3", len(rc.Rules))
	}
	r0 := rc.Rules[0]
	if r0.Name != "cheap_reads" || r0.When.TurnKind != "read_only" || r0.When.MaxTools != 2 ||
		r0.When.TierAtLeast != "opus-class" || r0.Action.RouteToTier != "haiku-class" || r0.Action.Reason != "overpowered_read" {
		t.Errorf("rule 0: %+v", r0)
	}
	if !rc.Rules[1].Action.NoRoute || rc.Rules[1].When.Phase != "plan" {
		t.Errorf("rule 1: %+v", rc.Rules[1])
	}
	r2 := rc.Rules[2]
	if len(r2.When.TurnKinds) != 2 || r2.When.Entitlement != "subscription" || r2.Action.SetEffort != "low" {
		t.Errorf("rule 2: %+v", r2)
	}
}

func TestRoutingValidateRejects(t *testing.T) {
	cases := []struct {
		name string
		toml string
		want string
	}{
		{
			name: "bad_mode",
			toml: "[routing]\nmode = \"shadow\"\n",
			want: "routing.mode",
		},
		{
			name: "empty_policy",
			toml: "[routing]\npolicy = \"\"\n",
			want: "routing.policy",
		},
		{
			name: "translation_enable_rejected",
			toml: "[routing.translation]\nenabled = true\n",
			want: "P3 track",
		},
		{
			name: "budget_bad_window",
			toml: "[[routing.budget.scopes]]\nscope = \"global\"\nlimit_usd = 10.0\nwindow = \"year\"\n",
			want: "window",
		},
		{
			name: "budget_bad_scope",
			toml: "[[routing.budget.scopes]]\nscope = \"repo:x\"\nlimit_usd = 10.0\nwindow = \"day\"\n",
			want: "scope",
		},
		{
			name: "budget_zero_limit",
			toml: "[[routing.budget.scopes]]\nscope = \"global\"\nlimit_usd = 0.0\nwindow = \"day\"\n",
			want: "limit_usd",
		},
		{
			name: "budget_bands_not_ascending",
			toml: "[[routing.budget.scopes]]\nscope = \"global\"\nlimit_usd = 10.0\nwindow = \"day\"\nbands = [0.8, 0.5]\n",
			want: "ascending",
		},
		{
			name: "budget_band_out_of_range",
			toml: "[[routing.budget.scopes]]\nscope = \"global\"\nlimit_usd = 10.0\nwindow = \"day\"\nbands = [1.5]\n",
			want: "band",
		},
		{
			name: "budget_bad_exhausted",
			toml: "[[routing.budget.scopes]]\nscope = \"global\"\nlimit_usd = 10.0\nwindow = \"day\"\nexhausted = \"explode\"\n",
			want: "exhausted",
		},
		{
			name: "privacy_rule_no_selector",
			toml: "[[routing.privacy.rules]]\nlocal_only = true\n",
			want: "selector",
		},
		{
			name: "privacy_rule_no_effect",
			toml: "[[routing.privacy.rules]]\nproject = \"x\"\n",
			want: "effect",
		},
		{
			name: "headroom_out_of_range",
			toml: "[routing.rate_limit_window]\nheadroom_pct = 101\n",
			want: "headroom_pct",
		},
		{
			name: "retries_out_of_range",
			toml: "[routing.reliability]\nmax_retries = 11\n",
			want: "max_retries",
		},
		{
			name: "probe_enabled_bad_interval",
			toml: "[routing.reliability.probe]\nenabled = true\ninterval_seconds = 0\n",
			want: "probe",
		},
		{
			name: "rule_missing_name",
			toml: "[[routing.rules]]\nwhen = { turn_kind = \"read_only\" }\naction = { no_route = true }\n",
			want: "name",
		},
		{
			name: "rule_bad_effort",
			toml: "[[routing.rules]]\nname = \"x\"\naction = { set_effort = \"max\" }\n",
			want: "set_effort",
		},
		{
			name: "rule_bad_entitlement",
			toml: "[[routing.rules]]\nname = \"x\"\nwhen = { entitlement = \"oauth\" }\naction = { no_route = true }\n",
			want: "entitlement",
		},
		{
			name: "rule_bad_budget_band",
			toml: "[[routing.rules]]\nname = \"x\"\nwhen = { budget_band_at_least = 1.5 }\naction = { no_route = true }\n",
			want: "budget_band_at_least",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadRoutingTOML(t, tc.toml)
			if err == nil {
				t.Fatalf("Load accepted invalid config:\n%s", tc.toml)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestRoutingEnvOverrides(t *testing.T) {
	env := map[string]string{
		"OBSERVER_ROUTING_ENABLED":                               "true",
		"OBSERVER_ROUTING_MODE":                                  "off",
		"OBSERVER_ROUTING_STICKINESS_MIN_TURNS_BETWEEN_SWITCHES": "9",
	}
	cfg, err := Load(LoadOptions{
		GlobalPath: filepath.Join(t.TempDir(), "absent.toml"),
		Env:        func(k string) string { return env[k] },
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Routing.Enabled || cfg.Routing.Mode != "off" {
		t.Errorf("env overrides not applied: %+v", cfg.Routing)
	}
	if cfg.Routing.Stickiness.MinTurnsBetweenSwitches != 9 {
		t.Errorf("nested env override not applied: %+v", cfg.Routing.Stickiness)
	}
}
