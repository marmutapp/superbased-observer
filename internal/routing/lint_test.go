package routing

import "testing"

// cleanRule returns a valid downshift rule to mutate per case.
func cleanRule(name string) Rule {
	return Rule{
		Name:   name,
		When:   RuleWhen{TurnKinds: []TurnKind{TurnReadOnly}, TierAtLeast: TierSonnetClass},
		Action: RuleAction{RouteToTier: TierHaikuClass},
		Reason: ReasonOverpoweredRead,
	}
}

// TestLintPolicy_CheckRows exercises the lint table one check row at a
// time (§24.5): each case violates exactly one check and asserts that
// check (and only that check) fires.
func TestLintPolicy_CheckRows(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		policy    Policy
		wantCheck string
		wantSev   LintSeverity
	}{
		{
			name: "row_unique_rule_names",
			policy: Policy{Name: "x", Rules: []Rule{
				cleanRule("dup"),
				func() Rule {
					r := cleanRule("dup")
					// distinct when clause so shadowed_rule stays quiet
					r.When.TurnKinds = []TurnKind{TurnTestRun}
					return r
				}(),
			}},
			wantCheck: "unique_rule_names",
			wantSev:   LintError,
		},
		{
			name: "row_action_exactly_one_neither",
			policy: Policy{Name: "x", Rules: []Rule{func() Rule {
				r := cleanRule("r1")
				r.Action = RuleAction{}
				return r
			}()}},
			wantCheck: "action_exactly_one",
			wantSev:   LintError,
		},
		{
			name: "row_action_exactly_one_both",
			policy: Policy{Name: "x", Rules: []Rule{func() Rule {
				r := cleanRule("r1")
				r.Action = RuleAction{RouteToTier: TierHaikuClass, NoRoute: true}
				return r
			}()}},
			wantCheck: "action_exactly_one",
			wantSev:   LintError,
		},
		{
			name: "row_known_turn_kinds",
			policy: Policy{Name: "x", Rules: []Rule{func() Rule {
				r := cleanRule("r1")
				r.When.TurnKinds = []TurnKind{"made_up_kind"}
				return r
			}()}},
			wantCheck: "known_turn_kinds",
			wantSev:   LintError,
		},
		{
			name: "row_known_reason",
			policy: Policy{Name: "x", Rules: []Rule{func() Rule {
				r := cleanRule("r1")
				r.Reason = "free_text_reason"
				return r
			}()}},
			wantCheck: "known_reason",
			wantSev:   LintError,
		},
		{
			name: "row_targetable_tier",
			policy: Policy{Name: "x", Rules: []Rule{func() Rule {
				r := cleanRule("r1")
				r.Action.RouteToTier = TierUnclassified
				return r
			}()}},
			wantCheck: "targetable_tier",
			wantSev:   LintError,
		},
		{
			name: "row_downshift_only_error",
			policy: Policy{Name: "x", Rules: []Rule{func() Rule {
				r := cleanRule("r1")
				r.Action.RouteToTier = TierOpusClass // above tier_at_least
				return r
			}()}},
			wantCheck: "downshift_only",
			wantSev:   LintError,
		},
		{
			name: "row_downshift_only_warning_unbounded",
			policy: Policy{Name: "x", Rules: []Rule{func() Rule {
				r := cleanRule("r1")
				r.When.TierAtLeast = ""
				return r
			}()}},
			wantCheck: "downshift_only",
			wantSev:   LintWarning,
		},
		{
			name: "row_shadowed_rule",
			policy: Policy{Name: "x", Rules: []Rule{
				func() Rule { // matches ALL read_only at sonnet+
					r := cleanRule("broad")
					return r
				}(),
				func() Rule { // narrower: read_only at opus+ — unreachable
					r := cleanRule("narrow")
					r.When.TierAtLeast = TierOpusClass
					return r
				}(),
			}},
			wantCheck: "shadowed_rule",
			wantSev:   LintError,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			issues := LintPolicy(tc.policy)
			var hit *LintIssue
			for i := range issues {
				if issues[i].Check == tc.wantCheck {
					hit = &issues[i]
				} else {
					t.Errorf("unexpected extra finding: %+v", issues[i])
				}
			}
			if hit == nil {
				t.Fatalf("check %q did not fire; findings: %+v", tc.wantCheck, issues)
			}
			if hit.Severity != tc.wantSev {
				t.Errorf("severity = %s, want %s", hit.Severity, tc.wantSev)
			}
		})
	}
}

// TestLintPolicy_ShippedTemplatesClean pins the gate the CLI relies on:
// every built-in template lints clean.
func TestLintPolicy_ShippedTemplatesClean(t *testing.T) {
	t.Parallel()
	for _, p := range Templates() {
		if issues := LintPolicy(p); len(issues) != 0 {
			t.Errorf("template %q has lint findings: %+v", p.Name, issues)
		}
	}
}

// TestLintHasErrors distinguishes warning-only results from errors.
func TestLintHasErrors(t *testing.T) {
	t.Parallel()
	if LintHasErrors(nil) {
		t.Error("empty findings reported as errors")
	}
	if LintHasErrors([]LintIssue{{Severity: LintWarning}}) {
		t.Error("warning-only findings reported as errors")
	}
	if !LintHasErrors([]LintIssue{{Severity: LintWarning}, {Severity: LintError}}) {
		t.Error("error finding not detected")
	}
}
