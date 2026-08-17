package egress

import (
	"strings"
	"testing"
)

func TestEgressDecodeBodyStrict(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		max     int64
		wantErr string
	}{
		{name: "valid", in: `{"mode":"advise"}`, max: 1024},
		{name: "unknown field rejected", in: `{"mode":"advise","bogus":1}`, max: 1024, wantErr: "unknown field"},
		{name: "trailing bytes rejected", in: `{"mode":"advise"}{}`, max: 1024, wantErr: "trailing bytes"},
		{name: "over cap rejected", in: `{"mode":"advise"}`, max: 4, wantErr: "exceeds"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeBody([]byte(tc.in), tc.max)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("DecodeBody(%q) = %v, want nil", tc.in, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("DecodeBody(%q) error = %v, want substring %q", tc.in, err, tc.wantErr)
			}
		})
	}
}

// TestBodyV1NestedActionMapsToFlatRuleInput pins Phase F item 5: the wire
// body nests action fields under "action"; ToPolicyInput flattens them onto
// RuleInput at this one boundary.
func TestBodyV1NestedActionMapsToFlatRuleInput(t *testing.T) {
	raw := `{
		"mode": "enforce",
		"targets": [{"id":"t","url":"http://x","shape":"openai"}],
		"rules": [{
			"name": "r1",
			"when": {"provider": "openai", "budget_band_at_least": 0.5},
			"action": {"route_to_upstream": "t"}
		}]
	}`
	body, err := DecodeBody([]byte(raw), 4096)
	if err != nil {
		t.Fatalf("DecodeBody: %v", err)
	}
	in := body.ToPolicyInput()
	if len(in.Rules) != 1 {
		t.Fatalf("rules = %+v", in.Rules)
	}
	r := in.Rules[0]
	if r.RouteToUpstream != "t" {
		t.Errorf("RouteToUpstream = %q, want %q", r.RouteToUpstream, "t")
	}
	if !r.When.BudgetBandSet || r.When.BudgetBandAtLeast != 0.5 {
		t.Errorf("budget band not carried through: %+v", r.When)
	}
	spec, err := Compile(in)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(spec.Rules) != 1 || spec.Rules[0].Action != ActionRouteUpstream {
		t.Fatalf("compiled rule = %+v", spec.Rules)
	}
}

func TestEgressCompileBodyRoundTrip(t *testing.T) {
	spec, canon, err := CompileBody([]byte(`{"mode":"advise","rules":[{"name":"r","action":{"no_route":true}}]}`), 4096)
	if err != nil {
		t.Fatalf("CompileBody: %v", err)
	}
	if spec.Mode != ModeAdvise || len(spec.Rules) != 1 {
		t.Fatalf("spec = %+v", spec)
	}
	if len(canon) == 0 {
		t.Error("expected non-empty canonical body")
	}
	spec2, _, err := CompileBody(canon, 4096)
	if err != nil {
		t.Fatalf("CompileBody(canon): %v", err)
	}
	if spec.Hash != spec2.Hash {
		t.Error("compiling the canonical body must reproduce the identical hash")
	}
}

func TestEgressBodyV1RoundTripThroughPolicyInput(t *testing.T) {
	band := 0.7
	in := PolicyInput{
		Mode: "enforce",
		Rules: []RuleInput{
			{Name: "r", RouteToModel: "cheap-model", When: WhenInput{BudgetBandAtLeast: band, BudgetBandSet: true}},
		},
	}
	out := BodyV1FromPolicyInput(in).ToPolicyInput()
	if len(out.Rules) != 1 || out.Rules[0].RouteToModel != "cheap-model" {
		t.Fatalf("round trip mismatch: %+v", out)
	}
	if !out.Rules[0].When.BudgetBandSet || out.Rules[0].When.BudgetBandAtLeast != band {
		t.Fatalf("budget band round trip mismatch: %+v", out.Rules[0].When)
	}
}
