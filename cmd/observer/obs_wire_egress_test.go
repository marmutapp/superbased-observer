//go:build !no_obs

package main

import (
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/obs/egress"
)

// TestEgressPolicyInputTranslation is the P2 golden: [observability.egress]
// config translates faithfully into the pure engine's PolicyInput — including
// target-shape resolution, the pointer-gated budget band, and the cohort map —
// and the result compiles + evaluates.
func TestEgressPolicyInputTranslation(t *testing.T) {
	band := 0.8
	cfg := config.Config{}
	cfg.Observability.Egress = config.ObservabilityEgressConfig{
		Enabled:         true,
		Mode:            "advise",
		CooldownSeconds: 180,
		Cohorts:         map[string]string{"user-123": "beta"},
		Targets: []config.EgressTargetConfig{
			{ID: "ollama-local", URL: "http://127.0.0.1:11434", Shape: "openai"},
		},
		Rules: []config.EgressRuleConfig{
			{
				Name:          "flagged-to-local",
				When:          config.EgressWhenConfig{VerdictAtLeast: "flag"},
				Action:        config.EgressActionConfig{RouteToUpstream: "ollama-local"},
				OnUnavailable: "deny",
				Reason:        "Flagged content is served locally.",
				ReasonCode:    "egress_flagged_local",
			},
			{
				Name:   "budget-band-cheaper",
				When:   config.EgressWhenConfig{BudgetBandAtLeast: &band},
				Action: config.EgressActionConfig{RouteToModel: "claude-3-5-haiku-20241022"},
			},
		},
	}

	in := egressPolicyInput(cfg)
	if in.Mode != "advise" || in.CooldownSeconds != 180 {
		t.Fatalf("mode/cooldown mis-translated: %+v", in)
	}
	if len(in.Targets) != 1 || in.Targets[0].Shape != "openai" {
		t.Fatalf("target mis-translated: %+v", in.Targets)
	}
	if in.Cohorts["user-123"] != "beta" {
		t.Fatalf("cohort map mis-translated: %+v", in.Cohorts)
	}
	// Budget band pointer -> value + Set flag.
	if !in.Rules[1].When.BudgetBandSet || in.Rules[1].When.BudgetBandAtLeast != 0.8 {
		t.Fatalf("budget band pointer mis-translated: %+v", in.Rules[1].When)
	}

	spec, err := egress.Compile(in)
	if err != nil {
		t.Fatalf("Compile translated policy: %v", err)
	}
	// The flagged rule routes to the resolved local target with fail-closed pin.
	d := egress.Evaluate(egress.Input{VerdictDecision: "flag"}, spec)
	if d.Action != egress.ActionRouteUpstream || !d.MustUseTarget || d.TargetShape != egress.ShapeOpenAI {
		t.Fatalf("translated flagged rule evaluated wrong: %+v", d)
	}
	if spec.CohortFor("user-123") != "beta" {
		t.Fatalf("cohort resolution broken: %q", spec.CohortFor("user-123"))
	}
}
