package egress

import (
	"strings"
	"testing"
)

func hasIssue(issues []Issue, substr string, fatal bool) bool {
	for _, i := range issues {
		if strings.Contains(i.Message, substr) && i.Fatal == fatal {
			return true
		}
	}
	return false
}

func TestLintProviderActionCompat(t *testing.T) {
	// set_effort with no provider matcher → fatal (finding 10).
	issues := Lint(PolicyInput{Rules: []RuleInput{
		{Name: "e", When: WhenInput{MinPromptTokens: 100}, SetEffort: "low"},
	}})
	if !hasIssue(issues, "set_effort needs a provider matcher", true) {
		t.Errorf("expected fatal provider/action compat issue, got %+v", issues)
	}
}

func TestLintShapeMismatch(t *testing.T) {
	issues := Lint(PolicyInput{
		Targets: []TargetInput{{ID: "t", URL: "http://x", Shape: "openai"}},
		Rules: []RuleInput{
			{Name: "r", When: WhenInput{Provider: "anthropic"}, RouteToUpstream: "t"},
		},
	})
	if !hasIssue(issues, "cross-shape routing is unsupported", true) {
		t.Errorf("expected fatal shape-mismatch issue, got %+v", issues)
	}
}

func TestLintUndeclaredTargetIsAdvisory(t *testing.T) {
	issues := Lint(PolicyInput{Rules: []RuleInput{
		{Name: "r", When: WhenInput{VerdictAtLeast: "flag"}, RouteToUpstream: "ghost"},
	}})
	if !hasIssue(issues, "references undeclared target", false) {
		t.Errorf("expected advisory undeclared-target issue, got %+v", issues)
	}
	if HasFatal(issues) {
		t.Errorf("undeclared target should be advisory (advise may log-only): %+v", issues)
	}
}

func TestLintNoMatcherWarning(t *testing.T) {
	issues := Lint(PolicyInput{Rules: []RuleInput{
		{Name: "r", RouteToModel: "m"},
	}})
	if !hasIssue(issues, "no matchers", false) {
		t.Errorf("expected no-matcher warning, got %+v", issues)
	}
}

func TestLintEmptyPolicy(t *testing.T) {
	issues := Lint(PolicyInput{})
	if !hasIssue(issues, "no rules", false) {
		t.Errorf("expected empty-policy warning, got %+v", issues)
	}
}

func TestStarterTemplatesCompile(t *testing.T) {
	var rules []RuleInput
	var targets []TargetInput
	for _, tpl := range StarterTemplates() {
		rules = append(rules, tpl.Rule)
		if tpl.NeedsTarget {
			targets = append(targets, TargetInput{ID: tpl.Rule.RouteToUpstream, URL: "http://127.0.0.1:11434", Shape: "openai"})
		}
	}
	// Advise mode: templates must at least compile cleanly.
	if _, err := Compile(PolicyInput{Mode: ModeAdvise, Rules: rules, Targets: targets}); err != nil {
		t.Fatalf("starter templates failed to compile: %v", err)
	}
}
