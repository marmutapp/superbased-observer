package admission

import "testing"

func TestCompileHashStableAndOrderInsensitiveByID(t *testing.T) {
	in := PolicyInput{
		Mode: "enforce",
		Criteria: []CriterionInput{
			{ID: "AD-100", Type: "denied_topics", Decision: "deny", Severity: "warn", Topics: []string{"competitor:AcmeCorp"}},
		},
	}
	a, err := Compile(in)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	b, err := Compile(in)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if a.Hash != b.Hash {
		t.Error("hash must be stable for the identical input")
	}
	if len(a.Criteria) != 1 || a.Criteria[0].Topics[0] != "acmecorp" {
		t.Fatalf("normalized topic mismatch: %+v", a.Criteria)
	}
	if !a.Criteria[0].Type.Judged() && a.Criteria[0].Type == TypeValidUseCase {
		t.Error("valid_use_case must be judged")
	}
	if TypeDeniedTopics.Judged() {
		t.Error("denied_topics is a deterministic prefilter type, not judged")
	}
}

func TestCompileRejectsUnknownDecision(t *testing.T) {
	_, err := Compile(PolicyInput{Criteria: []CriterionInput{
		{ID: "x", Type: "jailbreak", Decision: "explode"},
	}})
	if err == nil {
		t.Fatal("expected an error for an unknown decision")
	}
}

func TestRequiresJudgeAndValidateRuntimeCaps(t *testing.T) {
	judged, err := Compile(PolicyInput{Criteria: []CriterionInput{
		{ID: "x", Type: "valid_use_case", Definition: "on-scope only", Decision: "deny"},
	}})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !RequiresJudge(judged) {
		t.Error("a valid_use_case criterion must require a judge")
	}
	if err := ValidateRuntimeCaps(judged, false); err == nil {
		t.Error("expected capability_mismatch-class error with no judge configured")
	}
	if err := ValidateRuntimeCaps(judged, true); err != nil {
		t.Errorf("a configured judge must satisfy the requirement: %v", err)
	}

	deterministic, err := Compile(PolicyInput{Criteria: []CriterionInput{
		{ID: "y", Type: "jailbreak", Decision: "deny"},
	}})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if RequiresJudge(deterministic) {
		t.Error("a jailbreak-only policy must not require a judge")
	}
	if err := ValidateRuntimeCaps(deterministic, false); err != nil {
		t.Errorf("no judge needed: %v", err)
	}
}
