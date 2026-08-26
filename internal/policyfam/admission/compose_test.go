package admission

import "testing"

// TestComposeOrgSpec_ModeIsLocalOnly is the B-B1 regression: the org body's
// mode must be STRUCTURALLY IGNORED (§R23 mirror), so a published
// {"mode":"off"}/{"mode":"observe"} cannot relax a locally-enforcing node
// and a published {"mode":"enforce"} cannot escalate a locally-observing
// one. The composed CONTENT still comes from the org body.
func TestComposeOrgSpec_ModeIsLocalOnly(t *testing.T) {
	orgIn := PolicyInput{
		Prefilter: PrefilterInput{Deny: []string{"org-forbidden"}, MaxMessageBytes: 4096},
		Criteria: []CriterionInput{{
			ID: "org-1", Type: string(TypeDeniedTopics), Topics: []string{"org-topic"},
			Decision: "deny", Severity: "high",
		}},
	}
	localIn := PolicyInput{
		Prefilter: PrefilterInput{Deny: []string{"local-forbidden"}},
	}

	tests := []struct {
		name      string
		hasLocal  bool
		localMode string
		orgMode   string
		want      Mode
	}{
		{name: "no local layer, org enforce -> off", hasLocal: false, orgMode: "enforce", want: ModeOff},
		{name: "no local layer, org observe -> off", hasLocal: false, orgMode: "observe", want: ModeOff},
		{name: "local off, org enforce -> off (remote cannot enable)", hasLocal: true, localMode: "off", orgMode: "enforce", want: ModeOff},
		{name: "local off, org observe -> off", hasLocal: true, localMode: "off", orgMode: "observe", want: ModeOff},
		{name: "local enforce, org off -> enforce (remote cannot disable)", hasLocal: true, localMode: "enforce", orgMode: "off", want: ModeEnforce},
		{name: "local enforce, org observe -> enforce (remote cannot relax)", hasLocal: true, localMode: "enforce", orgMode: "observe", want: ModeEnforce},
		{name: "local observe, org enforce -> observe (remote cannot escalate)", hasLocal: true, localMode: "observe", orgMode: "enforce", want: ModeObserve},
		{name: "local observe, org off -> observe", hasLocal: true, localMode: "observe", orgMode: "off", want: ModeObserve},
		{name: "local enforce, org enforce -> enforce (agreement is not escalation)", hasLocal: true, localMode: "enforce", orgMode: "enforce", want: ModeEnforce},
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
			got := ComposeOrgSpec(local, org, false)
			if got.Mode != tc.want {
				t.Errorf("composed Mode = %v, want %v (org body mode %q must be ignored)", got.Mode, tc.want, tc.orgMode)
			}

			// (c) Rule/filter CONTENT still comes from the org body.
			if got.Hash != org.Hash {
				t.Errorf("composed Hash = %q, want the org body's %q", got.Hash, org.Hash)
			}
			if len(got.Criteria) != 1 || got.Criteria[0].ID != "org-1" {
				t.Errorf("composed Criteria = %+v, want the org body's criterion", got.Criteria)
			}
			if got.Prefilter.MaxMessageBytes != 4096 || len(got.Prefilter.Deny) != 1 {
				t.Errorf("composed Prefilter = %+v, want the org body's prefilter", got.Prefilter)
			}
			if !got.Prefilter.Deny[0].MatchString("an org-forbidden request") {
				t.Errorf("composed prefilter does not carry the org deny pattern")
			}
			// The inputs must not be mutated by composition.
			if org.Mode != orgModeBefore {
				t.Errorf("org spec mutated: Mode = %v, want %v", org.Mode, orgModeBefore)
			}
			if local != nil && local.Mode != ParseMode(tc.localMode) {
				t.Errorf("local spec mutated: %v", local.Mode)
			}
		})
	}
}

// TestComposeOrgSpec_ManagedEnforceHonorsOrgMode is the Arc 4 P3 §R23 lift: with
// orgEnforce=true (managed tenancy + enforce.admission) the org body's mode is
// honored AS AUTHORED, regardless of the local layer — the org may turn
// enforcement on (mode enforce), and observe/off stays a real opt-out. No
// coercion: the composed mode equals the org body's mode exactly.
func TestComposeOrgSpec_ManagedEnforceHonorsOrgMode(t *testing.T) {
	orgIn := PolicyInput{
		Criteria: []CriterionInput{{
			ID: "org-1", Type: string(TypeDeniedTopics), Topics: []string{"org-topic"},
			Decision: "deny", Severity: "high",
		}},
	}
	cases := []struct {
		orgMode   string
		localMode string
		hasLocal  bool
		want      Mode
	}{
		{orgMode: "enforce", hasLocal: false, want: ModeEnforce},                      // org turns it on with no local layer
		{orgMode: "enforce", localMode: "off", hasLocal: true, want: ModeEnforce},     // org overrides a locally-off node
		{orgMode: "observe", localMode: "enforce", hasLocal: true, want: ModeObserve}, // org opt-out wins over local enforce
		{orgMode: "off", localMode: "enforce", hasLocal: true, want: ModeOff},         // org off is honored (opt-out)
	}
	for _, tc := range cases {
		t.Run(tc.orgMode+"/"+tc.localMode, func(t *testing.T) {
			oi := orgIn
			oi.Mode = tc.orgMode
			org, err := Compile(oi)
			if err != nil {
				t.Fatalf("compile org: %v", err)
			}
			var local *PolicySpec
			if tc.hasLocal {
				ls, err := Compile(PolicyInput{Mode: tc.localMode})
				if err != nil {
					t.Fatalf("compile local: %v", err)
				}
				local = &ls
			}
			got := ComposeOrgSpec(local, org, true)
			if got.Mode != tc.want {
				t.Errorf("managed-enforce composed Mode = %v, want %v (org mode %q honored)", got.Mode, tc.want, tc.orgMode)
			}
			if got.Hash != org.Hash {
				t.Errorf("composed Hash = %q, want the org body's %q", got.Hash, org.Hash)
			}
		})
	}
}
