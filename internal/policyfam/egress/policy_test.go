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
			name: "duplicate rule name",
			in: PolicyInput{Mode: ModeAdvise, Rules: []RuleInput{
				{Name: "r", When: WhenInput{VerdictAtLeast: "flag"}, Deny: true},
				{Name: "r", When: WhenInput{VerdictAtLeast: "ask"}, Deny: true},
			}},
			wantErr: "duplicate rule name",
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

func TestCompileHashStableAndOrderSensitive(t *testing.T) {
	a := PolicyInput{Mode: ModeAdvise, Rules: []RuleInput{
		{Name: "x", When: WhenInput{VerdictAtLeast: "flag"}, Deny: true},
		{Name: "y", When: WhenInput{VerdictAtLeast: "ask"}, Deny: true},
	}}
	b := PolicyInput{Mode: ModeAdvise, Rules: []RuleInput{
		{Name: "y", When: WhenInput{VerdictAtLeast: "ask"}, Deny: true},
		{Name: "x", When: WhenInput{VerdictAtLeast: "flag"}, Deny: true},
	}}
	sa, err := Compile(a)
	if err != nil {
		t.Fatalf("Compile a: %v", err)
	}
	sa2, err := Compile(a)
	if err != nil {
		t.Fatalf("Compile a again: %v", err)
	}
	sb, err := Compile(b)
	if err != nil {
		t.Fatalf("Compile b: %v", err)
	}
	if sa.Hash != sa2.Hash {
		t.Error("hash not stable for identical policy")
	}
	if sa.Hash == sb.Hash {
		t.Error("hash must be order-sensitive (first-match-wins depends on rule order)")
	}
}

func TestIsHardSwitch(t *testing.T) {
	hard := CompiledRule{When: CompiledWhen{VerdictAtLeast: "deny"}}
	if !hard.IsHardSwitch() {
		t.Error("a verdict-gated rule must be a hard switch")
	}
	soft := CompiledRule{When: CompiledWhen{ModelGlob: "claude-*"}}
	if soft.IsHardSwitch() {
		t.Error("a glob-only rule must not be a hard switch")
	}
}
