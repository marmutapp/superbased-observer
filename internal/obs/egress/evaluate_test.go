package egress

import "testing"

// compileOrFatal compiles a policy input, failing the test on error.
func compileOrFatal(t *testing.T, in PolicyInput) PolicySpec {
	t.Helper()
	spec, err := Compile(in)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return spec
}

func TestEvaluateMatchers(t *testing.T) {
	targets := []TargetInput{{ID: "ollama-local", URL: "http://127.0.0.1:11434", Shape: "openai"}}
	tests := []struct {
		name       string
		rules      []RuleInput
		in         Input
		wantAction Action
		wantRule   string
		wantReason ReasonCode
	}{
		{
			name: "verdict_at_least flag routes to upstream",
			rules: []RuleInput{{
				Name: "flagged", When: WhenInput{VerdictAtLeast: "flag"},
				RouteToUpstream: "ollama-local", OnUnavailable: OnUnavailableDeny,
				ReasonCode: string(ReasonFlaggedLocal),
			}},
			in:         Input{VerdictDecision: "ask"},
			wantAction: ActionRouteUpstream, wantRule: "flagged", wantReason: ReasonFlaggedLocal,
		},
		{
			name: "verdict below threshold does not fire",
			rules: []RuleInput{{
				Name: "flagged", When: WhenInput{VerdictAtLeast: "deny"},
				RouteToUpstream: "ollama-local", OnUnavailable: OnUnavailableDeny,
			}},
			in:         Input{VerdictDecision: "flag"},
			wantAction: ActionNone,
		},
		{
			name: "criterion exact match",
			rules: []RuleInput{{
				Name: "sensitive", When: WhenInput{Criterion: "secret.remote_judge"},
				Deny: true, Reason: "no",
			}},
			in:         Input{Criterion: "secret.remote_judge"},
			wantAction: ActionDeny, wantRule: "sensitive", wantReason: ReasonDenyUnavailable,
		},
		{
			name: "budget band fires only when known",
			rules: []RuleInput{{
				Name: "budget", When: WhenInput{BudgetBandAtLeast: 0.8, BudgetBandSet: true},
				RouteToModel: "claude-3-5-haiku-20241022",
			}},
			in:         Input{BudgetBurnMax: 0.9, BudgetKnown: true},
			wantAction: ActionRouteModel, wantRule: "budget", wantReason: ReasonBudgetBand,
		},
		{
			name: "budget band does not fire when spend unavailable",
			rules: []RuleInput{{
				Name: "budget", When: WhenInput{BudgetBandAtLeast: 0.8, BudgetBandSet: true},
				RouteToModel: "claude-3-5-haiku-20241022",
			}},
			in:         Input{BudgetBurnMax: 0.9, BudgetKnown: false},
			wantAction: ActionNone,
		},
		{
			name: "model glob",
			rules: []RuleInput{{
				Name: "opus-cohort", When: WhenInput{ModelGlob: "claude-opus-*"},
				RouteToModel: "claude-3-5-haiku-20241022",
			}},
			in:         Input{Model: "claude-opus-4-8"},
			wantAction: ActionRouteModel, wantRule: "opus-cohort",
		},
		{
			name: "cohort + set_effort with provider",
			rules: []RuleInput{{
				Name: "overload", When: WhenInput{MinPromptTokens: 100, Provider: "anthropic"},
				SetEffort: "low",
			}},
			in:         Input{PromptTokensEst: 200, Provider: "anthropic"},
			wantAction: ActionSetEffort, wantRule: "overload", wantReason: ReasonOverloadDegrade,
		},
		{
			name: "content_class composite",
			rules: []RuleInput{{
				Name: "cc", When: WhenInput{ContentClass: "valid_use_case/high"},
				Deny: true,
			}},
			in:         Input{Criterion: "valid_use_case", VerdictSeverity: "high"},
			wantAction: ActionDeny, wantRule: "cc",
		},
		{
			name: "no_route exemption is matched but does nothing",
			rules: []RuleInput{{
				Name: "exempt", When: WhenInput{User: "trusted"}, NoRoute: true,
			}},
			in:         Input{User: "trusted"},
			wantAction: ActionNone, wantRule: "exempt", wantReason: ReasonNoRoute,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := compileOrFatal(t, PolicyInput{Mode: ModeAdvise, Rules: tc.rules, Targets: targets})
			d := Evaluate(tc.in, spec)
			if d.Action != tc.wantAction {
				t.Errorf("Action = %q, want %q", d.Action, tc.wantAction)
			}
			if tc.wantRule != "" && d.RuleName != tc.wantRule {
				t.Errorf("RuleName = %q, want %q", d.RuleName, tc.wantRule)
			}
			if tc.wantReason != "" && d.ReasonCode != tc.wantReason {
				t.Errorf("ReasonCode = %q, want %q", d.ReasonCode, tc.wantReason)
			}
		})
	}
}

func TestEvaluateFirstMatchWins(t *testing.T) {
	spec := compileOrFatal(t, PolicyInput{Mode: ModeAdvise, Rules: []RuleInput{
		{Name: "first", When: WhenInput{VerdictAtLeast: "flag"}, Deny: true},
		{Name: "second", When: WhenInput{VerdictAtLeast: "flag"}, SetEffort: "low", Reason: "x"},
	}})
	d := Evaluate(Input{VerdictDecision: "deny"}, spec)
	if d.RuleName != "first" || d.Action != ActionDeny {
		t.Errorf("first-match-wins broken: %+v", d)
	}
}

func TestEvaluateOffReturnsZero(t *testing.T) {
	spec := compileOrFatal(t, PolicyInput{Mode: "off", Rules: []RuleInput{
		{Name: "r", When: WhenInput{VerdictAtLeast: "allow"}, Deny: true},
	}})
	if d := Evaluate(Input{VerdictDecision: "deny"}, spec); d.Matched {
		t.Errorf("off policy still matched: %+v", d)
	}
}

func TestEvaluateMustUseTargetAndTargetKnown(t *testing.T) {
	spec := compileOrFatal(t, PolicyInput{
		Mode:    ModeAdvise,
		Targets: []TargetInput{{ID: "ollama-local", URL: "http://127.0.0.1:11434", Shape: "openai"}},
		Rules: []RuleInput{
			{Name: "known", When: WhenInput{VerdictAtLeast: "flag"}, RouteToUpstream: "ollama-local", OnUnavailable: OnUnavailableDeny},
			{Name: "unknown", When: WhenInput{Criterion: "x"}, RouteToUpstream: "ghost", OnUnavailable: OnUnavailableFailOpen},
		},
	})
	d := Evaluate(Input{VerdictDecision: "flag"}, spec)
	if !d.MustUseTarget || !d.TargetKnown || d.TargetShape != ShapeOpenAI || d.TargetURL == "" {
		t.Errorf("known locality route wrong: %+v", d)
	}
	d2 := Evaluate(Input{Criterion: "x"}, spec)
	if d2.TargetKnown || d2.MustUseTarget {
		t.Errorf("unknown target should be TargetKnown=false, MustUseTarget=false (fail_open): %+v", d2)
	}
}

func TestEvaluateSessionSwitchHold(t *testing.T) {
	spec := compileOrFatal(t, PolicyInput{Mode: ModeAdvise, Rules: []RuleInput{
		// soft cost switch (budget-driven)
		{Name: "cost", When: WhenInput{BudgetBandAtLeast: 0.8, BudgetBandSet: true}, RouteToModel: "cheap"},
	}})
	// Session already served a different model, cooldown NOT elapsed → held.
	held := Evaluate(Input{BudgetKnown: true, BudgetBurnMax: 0.9, SessionModel: "opus", CooldownElapsed: false}, spec)
	if !held.SwitchHeld || held.Action != ActionNone || held.ReasonCode != ReasonSwitchHeld {
		t.Errorf("expected held switch, got %+v", held)
	}
	// Cooldown elapsed → switch applies.
	applied := Evaluate(Input{BudgetKnown: true, BudgetBurnMax: 0.9, SessionModel: "opus", CooldownElapsed: true}, spec)
	if applied.SwitchHeld || applied.Action != ActionRouteModel || applied.Model != "cheap" {
		t.Errorf("expected applied switch, got %+v", applied)
	}
}

func TestEvaluateHardSwitchNeverHeld(t *testing.T) {
	spec := compileOrFatal(t, PolicyInput{Mode: ModeAdvise, Rules: []RuleInput{
		// hard verdict-driven switch
		{Name: "verdict", When: WhenInput{VerdictAtLeast: "deny"}, RouteToModel: "safe"},
	}})
	d := Evaluate(Input{VerdictDecision: "deny", SessionModel: "opus", CooldownElapsed: false}, spec)
	if d.SwitchHeld || d.Action != ActionRouteModel {
		t.Errorf("hard verdict switch must never hold: %+v", d)
	}
}
