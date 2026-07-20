package egress

import (
	"strings"
	"testing"
)

func TestCompileErrors(t *testing.T) {
	tests := []struct {
		name    string
		in      PolicyInput
		wantErr string
	}{
		{
			name: "enforce unknown target id",
			in: PolicyInput{Mode: ModeEnforce, Rules: []RuleInput{
				{Name: "r", When: WhenInput{VerdictAtLeast: "flag"}, RouteToUpstream: "ghost", OnUnavailable: OnUnavailableDeny},
			}},
			wantErr: "unknown target",
		},
		{
			name: "enforce shape mismatch",
			in: PolicyInput{
				Mode:    ModeEnforce,
				Targets: []TargetInput{{ID: "t", URL: "http://x", Shape: "openai"}},
				Rules: []RuleInput{
					{Name: "r", When: WhenInput{Provider: "anthropic"}, RouteToUpstream: "t"},
				},
			},
			wantErr: "shape",
		},
		{
			name: "enforce set_effort without provider",
			in: PolicyInput{Mode: ModeEnforce, Rules: []RuleInput{
				{Name: "r", When: WhenInput{MinPromptTokens: 100}, SetEffort: "low"},
			}},
			wantErr: "requires a provider matcher",
		},
		{
			name: "multiple actions",
			in: PolicyInput{Mode: ModeAdvise, Rules: []RuleInput{
				{Name: "r", When: WhenInput{VerdictAtLeast: "flag"}, RouteToModel: "m", Deny: true},
			}},
			wantErr: "multiple actions",
		},
		{
			name: "no action",
			in: PolicyInput{Mode: ModeAdvise, Rules: []RuleInput{
				{Name: "r", When: WhenInput{VerdictAtLeast: "flag"}},
			}},
			wantErr: "no action",
		},
		{
			name: "duplicate rule name",
			in: PolicyInput{Mode: ModeAdvise, Rules: []RuleInput{
				{Name: "r", When: WhenInput{VerdictAtLeast: "flag"}, Deny: true},
				{Name: "r", When: WhenInput{VerdictAtLeast: "ask"}, Deny: true},
			}},
			wantErr: "duplicate rule name",
		},
		{
			name:    "unknown target shape",
			in:      PolicyInput{Mode: ModeAdvise, Targets: []TargetInput{{ID: "t", URL: "http://x", Shape: "google"}}},
			wantErr: "unknown shape",
		},
		{
			name: "unknown set_effort level",
			in: PolicyInput{Mode: ModeAdvise, Rules: []RuleInput{
				{Name: "r", When: WhenInput{Provider: "anthropic"}, SetEffort: "extreme"},
			}},
			wantErr: "unknown set_effort",
		},
		{
			name: "bad model glob",
			in: PolicyInput{Mode: ModeAdvise, Rules: []RuleInput{
				{Name: "r", When: WhenInput{ModelGlob: "[bad"}, Deny: true},
			}},
			wantErr: "bad model_glob",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(tc.in)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestCompileAdviseLenientOnBareUpstream(t *testing.T) {
	// Advise mode may reference a bare upstream id (log-only) with no declared
	// target — Compile must NOT error.
	spec, err := Compile(PolicyInput{Mode: ModeAdvise, Rules: []RuleInput{
		{Name: "r", When: WhenInput{VerdictAtLeast: "flag"}, RouteToUpstream: "bare-id"},
	}})
	if err != nil {
		t.Fatalf("advise mode should tolerate a bare upstream id: %v", err)
	}
	d := Evaluate(Input{VerdictDecision: "flag"}, spec)
	if d.TargetKnown {
		t.Errorf("bare-id has no target, TargetKnown must be false: %+v", d)
	}
}

func TestCompileHashStableAndOrderSensitive(t *testing.T) {
	a := PolicyInput{Mode: ModeAdvise, Rules: []RuleInput{
		{Name: "x", When: WhenInput{VerdictAtLeast: "flag"}, Deny: true},
		{Name: "y", When: WhenInput{VerdictAtLeast: "ask"}, Deny: true},
	}}
	b := PolicyInput{Mode: ModeAdvise, Rules: []RuleInput{
		{Name: "y", When: WhenInput{VerdictAtLeast: "ask"}, Deny: true},
		{Name: "x", When: WhenInput{VerdictAtLeast: "flag"}, Deny: true},
	}}
	sa := compileOrFatal(t, a)
	sa2 := compileOrFatal(t, a)
	sb := compileOrFatal(t, b)
	if sa.Hash != sa2.Hash {
		t.Error("hash not stable for identical policy")
	}
	if sa.Hash == sb.Hash {
		t.Error("hash must be order-sensitive (first-match-wins depends on rule order)")
	}
}
