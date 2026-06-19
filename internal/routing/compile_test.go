package routing

import "testing"

// TestCompile_TemplateExpansionPlusAppend pins §R6.3: custom rules
// compile AFTER template expansion and append — never splice.
func TestCompile_TemplateExpansionPlusAppend(t *testing.T) {
	t.Parallel()
	p, issues := Compile(PolicySpec{
		Policy:       "value",
		RespectCache: true,
		Rules: []RuleSpec{{
			Name:   "my_custom",
			When:   WhenSpec{TurnKind: "test_run", TierAtLeast: "sonnet-class"},
			Action: ActionSpec{RouteToTier: "haiku-class", Reason: "overpowered_test_run"},
		}},
	})
	if LintHasErrors(issues) {
		t.Fatalf("unexpected lint errors: %+v", issues)
	}
	tpl, _ := TemplateByName("value")
	if len(p.Rules) != len(tpl.Rules)+1 {
		t.Fatalf("rules = %d, want template %d + 1 appended", len(p.Rules), len(tpl.Rules))
	}
	last := p.Rules[len(p.Rules)-1]
	if last.Name != "my_custom" || last.When.TurnKinds[0] != TurnTestRun ||
		last.Action.RouteToTier != TierHaikuClass || last.Reason != ReasonOverpoweredTestRun {
		t.Errorf("appended rule mis-compiled: %+v", last)
	}
	// Hash covers the compiled output: appending a rule changes it.
	if p.Hash() == tpl.Hash() {
		t.Error("policy hash blind to appended custom rule (§R6.6)")
	}
}

// TestCompile_CustomPolicy pins the rules-only path: defaults bases +
// floors, no template rules.
func TestCompile_CustomPolicy(t *testing.T) {
	t.Parallel()
	p, issues := Compile(PolicySpec{
		Policy:                  "custom",
		MinTurnsBetweenSwitches: 7,
		RespectCache:            true,
		Rules: []RuleSpec{{
			Name:   "only_rule",
			When:   WhenSpec{TurnKind: "read_only", TierAtLeast: "opus-class"},
			Action: ActionSpec{RouteToTier: "haiku-class", Reason: "overpowered_read"},
		}},
	})
	if LintHasErrors(issues) {
		t.Fatalf("unexpected lint errors: %+v", issues)
	}
	if p.Name != "custom" || len(p.Rules) != 1 || p.MinTurnsBetweenSwitches != 7 {
		t.Errorf("custom policy mis-compiled: %+v", p)
	}
	if len(p.Bases) == 0 || p.Floors[TurnReadOnly] != TierHaikuClass {
		t.Errorf("custom policy missing default bases/floors: bases=%v floors=%v", p.Bases, p.Floors)
	}
}

// TestCompile_UnknownTemplateFailsOpen pins the degraded path: a lint
// error plus an EMPTY rule table — every decision becomes the quiet
// no-change default (G7), never an error.
func TestCompile_UnknownTemplateFailsOpen(t *testing.T) {
	t.Parallel()
	p, issues := Compile(PolicySpec{Policy: "yolo"})
	if !LintHasErrors(issues) {
		t.Fatal("unknown template produced no lint error")
	}
	if len(p.Rules) != 0 {
		t.Errorf("unknown template compiled %d rules, want 0 (fail-open)", len(p.Rules))
	}
	// The degraded policy still decides without routing anything.
	d := Decide(p, testSnapshot(), readOnlyInput())
	if d.Changed {
		t.Error("degraded policy rerouted a turn")
	}
}

// TestCompile_GroupsAttach pins the §R14–R16 group compilation:
// budget defaults (bands, exhausted), privacy rules, path classes,
// rate-limit, fallbacks — all content-hashed.
func TestCompile_GroupsAttach(t *testing.T) {
	t.Parallel()
	spec := PolicySpec{
		Policy:               "value",
		RespectCache:         true,
		PathClasses:          map[string][]string{"secrets": {"**/.env*"}},
		PrivacyRules:         []PrivacyRuleSpec{{PathClass: "secrets", LocalOnly: true}},
		BudgetScopes:         []BudgetScopeSpec{{Scope: "global", LimitUSD: 50, Window: "week"}},
		RateLimitEnabled:     true,
		RateLimitHeadroomPct: 15,
		Fallbacks:            map[string][]string{"claude-opus-4-8": {"claude-sonnet-4-6"}},
	}
	p, issues := Compile(spec)
	if LintHasErrors(issues) {
		t.Fatalf("unexpected lint errors: %+v", issues)
	}
	if len(p.BudgetScopes) != 1 {
		t.Fatal("budget scope missing")
	}
	bs := p.BudgetScopes[0]
	if len(bs.Bands) != 3 || bs.Bands[0] != 0.5 || bs.Exhausted != BudgetAdviseOnly {
		t.Errorf("budget defaults not applied: %+v", bs)
	}
	if len(p.PrivacyRules) != 1 || !p.PrivacyRules[0].LocalOnly {
		t.Errorf("privacy rules: %+v", p.PrivacyRules)
	}
	if !p.RateLimit.Enabled || p.RateLimit.HeadroomPct != 15 {
		t.Errorf("rate limit: %+v", p.RateLimit)
	}
	base, _ := Compile(PolicySpec{Policy: "value", RespectCache: true})
	if p.Hash() == base.Hash() {
		t.Error("hash blind to §R14–R16 groups (§R6.6: hash covers compiled output)")
	}
}

// TestCompile_EmptyReasonDefaultsToCustomRule pins reason defaulting.
func TestCompile_EmptyReasonDefaultsToCustomRule(t *testing.T) {
	t.Parallel()
	p, issues := Compile(PolicySpec{
		Policy: "custom",
		Rules: []RuleSpec{{
			Name:   "r",
			Action: ActionSpec{NoRoute: true},
		}},
	})
	if LintHasErrors(issues) {
		t.Fatalf("unexpected lint errors: %+v", issues)
	}
	if p.Rules[0].Reason != ReasonCustomRule {
		t.Errorf("reason = %q, want %q", p.Rules[0].Reason, ReasonCustomRule)
	}
}

// TestCompile_LintCatchesBadCustomRules pins that compile surfaces
// custom-rule defects as findings (not silent coercion): unknown kind,
// unknown reason, unknown when-tier, malformed glob, unknown
// path-class reference, two primary actions.
func TestCompile_LintCatchesBadCustomRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		rule      RuleSpec
		wantCheck string
	}{
		{
			name:      "unknown_kind",
			rule:      RuleSpec{Name: "r", When: WhenSpec{TurnKind: "vibing"}, Action: ActionSpec{NoRoute: true}},
			wantCheck: "known_turn_kinds",
		},
		{
			name:      "unknown_reason",
			rule:      RuleSpec{Name: "r", Action: ActionSpec{NoRoute: true, Reason: "because"}},
			wantCheck: "known_reason",
		},
		{
			name:      "unknown_when_tier",
			rule:      RuleSpec{Name: "r", When: WhenSpec{TierAtLeast: "mega-class"}, Action: ActionSpec{NoRoute: true}},
			wantCheck: "known_when_tier",
		},
		{
			name:      "bad_glob",
			rule:      RuleSpec{Name: "r", When: WhenSpec{Model: "[oops"}, Action: ActionSpec{NoRoute: true}},
			wantCheck: "model_glob_valid",
		},
		{
			name:      "undefined_path_class",
			rule:      RuleSpec{Name: "r", When: WhenSpec{PathClass: "ghost"}, Action: ActionSpec{NoRoute: true}},
			wantCheck: "path_class_ref",
		},
		{
			name:      "two_primary_actions",
			rule:      RuleSpec{Name: "r", Action: ActionSpec{NoRoute: true, SetEffort: "low"}},
			wantCheck: "action_exactly_one",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, issues := Compile(PolicySpec{Policy: "custom", Rules: []RuleSpec{tc.rule}})
			found := false
			for _, i := range issues {
				if i.Check == tc.wantCheck && i.Severity == LintError {
					found = true
				}
			}
			if !found {
				t.Errorf("no %q error; issues: %+v", tc.wantCheck, issues)
			}
		})
	}
}

// TestTemplates_AllCompileClean pins that every shipped template +
// every shipped basis list passes the extended lint (§R6.4 templates
// stay inspectable AND clean).
func TestTemplates_AllCompileClean(t *testing.T) {
	t.Parallel()
	for _, name := range TemplateNames() {
		p, issues := Compile(PolicySpec{Policy: name, RespectCache: true})
		if LintHasErrors(issues) {
			t.Errorf("template %s lints dirty: %+v", name, issues)
		}
		if p.Name != name {
			t.Errorf("compiled name %q, want %q", p.Name, name)
		}
	}
}
