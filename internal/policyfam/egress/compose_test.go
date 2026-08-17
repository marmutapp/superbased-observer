package egress

import "testing"

// TestComposeOrgSpec_ModeIsLocalOnly is the egress half of the B-B1
// regression: the org body's mode is structurally ignored, the local
// layer's posture governs, and the org body's rules/targets/cohorts still
// compose (§R23 mirror).
func TestComposeOrgSpec_ModeIsLocalOnly(t *testing.T) {
	orgIn := PolicyInput{
		CooldownSeconds: 42,
		Cohorts:         map[string]string{"u@example.com": "vip"},
		Targets:         []TargetInput{{ID: "local-llm", URL: "http://127.0.0.1:11434", Shape: "openai"}},
		Rules: []RuleInput{{
			Name: "org-deny", When: WhenInput{VerdictAtLeast: "deny"}, Deny: true,
			Reason: "org policy", ReasonCode: string(ReasonFlaggedLocal),
		}},
	}
	localIn := PolicyInput{
		Rules: []RuleInput{{
			Name: "local-noroute", When: WhenInput{VerdictAtLeast: "allow"}, NoRoute: true,
		}},
	}

	tests := []struct {
		name      string
		hasLocal  bool
		localMode string
		orgMode   string
		want      string
	}{
		{name: "no local layer, org enforce -> off", hasLocal: false, orgMode: ModeEnforce, want: ModeOff},
		{name: "no local layer, org advise -> off", hasLocal: false, orgMode: ModeAdvise, want: ModeOff},
		{name: "local off, org enforce -> off (remote cannot enable)", hasLocal: true, localMode: ModeOff, orgMode: ModeEnforce, want: ModeOff},
		{name: "local enforce, org off -> enforce (remote cannot disable)", hasLocal: true, localMode: ModeEnforce, orgMode: ModeOff, want: ModeEnforce},
		{name: "local enforce, org advise -> enforce (remote cannot relax)", hasLocal: true, localMode: ModeEnforce, orgMode: ModeAdvise, want: ModeEnforce},
		{name: "local advise, org enforce -> advise (remote cannot escalate)", hasLocal: true, localMode: ModeAdvise, orgMode: ModeEnforce, want: ModeAdvise},
		{name: "local advise, org off -> advise", hasLocal: true, localMode: ModeAdvise, orgMode: ModeOff, want: ModeAdvise},
		{name: "unset local mode -> off (normalized)", hasLocal: true, localMode: "", orgMode: ModeEnforce, want: ModeOff},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			oi := orgIn
			oi.Mode = tc.orgMode
			org, err := Compile(oi)
			if err != nil {
				t.Fatalf("compile org: %v", err)
			}
			var local *PolicySpec
			if tc.hasLocal {
				li := localIn
				li.Mode = tc.localMode
				ls, err := Compile(li)
				if err != nil {
					t.Fatalf("compile local: %v", err)
				}
				local = &ls
			}

			orgModeBefore := org.Mode
			got := ComposeOrgSpec(local, org)
			if got.Mode != tc.want {
				t.Errorf("composed Mode = %q, want %q (org body mode %q must be ignored)", got.Mode, tc.want, tc.orgMode)
			}

			// (c) Rule/target/cohort CONTENT still comes from the org body.
			if got.Hash != org.Hash {
				t.Errorf("composed Hash = %q, want the org body's %q", got.Hash, org.Hash)
			}
			if len(got.Rules) != 1 || got.Rules[0].Name != "org-deny" {
				t.Errorf("composed Rules = %+v, want the org body's rule", got.Rules)
			}
			if _, ok := got.Targets["local-llm"]; !ok {
				t.Errorf("composed Targets = %+v, want the org body's target", got.Targets)
			}
			if got.CooldownSeconds != 42 || got.CohortFor("u@example.com") != "vip" {
				t.Errorf("composed cooldown/cohorts lost: %d / %q", got.CooldownSeconds, got.CohortFor("u@example.com"))
			}
			if org.Mode != orgModeBefore {
				t.Errorf("org spec mutated: Mode = %q, want %q", org.Mode, orgModeBefore)
			}
		})
	}
}
