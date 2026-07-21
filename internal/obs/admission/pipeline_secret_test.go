package admission

import (
	"context"
	"strings"
	"testing"
)

// fakeSecretDetect reports a certain-secret type when the text contains the
// sentinel — a stand-in for scrub.CertainSecretTypes in the pure package's own
// tests (no scrub import here).
func fakeSecretDetect(s string) []string {
	if strings.Contains(s, "SECRETKEY") {
		return []string{"aws_access_key"}
	}
	return nil
}

// secretGatePolicy compiles a judged policy with a SecretRemoteJudge decision.
func secretGatePolicy(t *testing.T, secretDecision string) PolicySpec {
	t.Helper()
	spec, err := Compile(PolicyInput{
		Mode:              "enforce",
		SecretRemoteJudge: secretDecision,
		Criteria: []CriterionInput{
			{ID: "AD-100", Type: "valid_use_case", Name: "On-scope only", Definition: "Only product support.", Decision: "deny", Severity: "warn"},
		},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return spec
}

// TestSecretGate covers the §3 layer-3 gate: a secret-bearing request headed
// for a REMOTE judge is decided locally per SecretRemoteJudge and NEVER reaches
// the judge; a local judge, a clean message, or an off gate all let the judge
// run.
func TestSecretGate(t *testing.T) {
	ctx := context.Background()
	const secretMsg = "here is my key SECRETKEY please help"

	t.Run("remote judge + secret + deny → local deny, judge not called", func(t *testing.T) {
		j := &fakeJudge{reply: `{"decision":"allow"}`}
		p := secretGatePolicy(t, "deny")
		res := Evaluate(ctx, Request{Text: secretMsg}, p, j, WithSecretGate(fakeSecretDetect, true))
		if res.Decision != DecisionDeny || res.Criterion != "secret.remote_judge" {
			t.Fatalf("got %+v, want deny/secret.remote_judge", res)
		}
		if res.JudgeUsed {
			t.Error("JudgeUsed = true; the request must not reach the remote judge")
		}
		if j.calls != 0 {
			t.Errorf("judge called %d times; want 0 (no egress)", j.calls)
		}
	})

	t.Run("remote judge + secret + ask → local ask", func(t *testing.T) {
		j := &fakeJudge{reply: `{"decision":"allow"}`}
		p := secretGatePolicy(t, "ask")
		res := Evaluate(ctx, Request{Text: secretMsg}, p, j, WithSecretGate(fakeSecretDetect, true))
		if res.Decision != DecisionAsk || j.calls != 0 {
			t.Fatalf("got %+v (calls=%d), want ask + 0 calls", res, j.calls)
		}
	})

	t.Run("LOCAL judge + secret → gate inert, judge runs", func(t *testing.T) {
		j := &fakeJudge{reply: `{"decision":"allow"}`}
		p := secretGatePolicy(t, "deny")
		res := Evaluate(ctx, Request{Text: secretMsg}, p, j, WithSecretGate(fakeSecretDetect, false))
		if !res.JudgeUsed || j.calls != 1 {
			t.Fatalf("got %+v (calls=%d), want judge to run for a local judge", res, j.calls)
		}
	})

	t.Run("no secret → gate inert, judge runs", func(t *testing.T) {
		j := &fakeJudge{reply: `{"decision":"allow"}`}
		p := secretGatePolicy(t, "deny")
		res := Evaluate(ctx, Request{Text: "clean request"}, p, j, WithSecretGate(fakeSecretDetect, true))
		if !res.JudgeUsed || j.calls != 1 {
			t.Fatalf("got %+v (calls=%d), want judge to run for a clean message", res, j.calls)
		}
	})

	t.Run("gate off (allow) → judge runs even with a secret", func(t *testing.T) {
		j := &fakeJudge{reply: `{"decision":"allow"}`}
		p := secretGatePolicy(t, "") // empty = allow = off
		res := Evaluate(ctx, Request{Text: secretMsg}, p, j, WithSecretGate(fakeSecretDetect, true))
		if !res.JudgeUsed || j.calls != 1 {
			t.Fatalf("got %+v (calls=%d), want judge to run when the gate is off", res, j.calls)
		}
	})

	t.Run("nil detector → gate inert", func(t *testing.T) {
		j := &fakeJudge{reply: `{"decision":"allow"}`}
		p := secretGatePolicy(t, "deny")
		res := Evaluate(ctx, Request{Text: secretMsg}, p, j, WithSecretGate(nil, true))
		if !res.JudgeUsed || j.calls != 1 {
			t.Fatalf("got %+v (calls=%d), want judge to run with a nil detector", res, j.calls)
		}
	})
}

// TestSecretRemoteJudgeLintAndCompile pins the config surface: an unknown
// decision is a fatal lint + a compile error; a known one round-trips.
func TestSecretRemoteJudgeLintAndCompile(t *testing.T) {
	bad := PolicyInput{SecretRemoteJudge: "nope", Prefilter: PrefilterInput{Deny: []string{"x"}}}
	if !HasFatal(Lint(bad)) {
		t.Error("unknown secret_remote_judge should lint fatal")
	}
	if _, err := Compile(bad); err == nil {
		t.Error("unknown secret_remote_judge should fail Compile")
	}
	good := PolicyInput{SecretRemoteJudge: "deny", Prefilter: PrefilterInput{Deny: []string{"x"}}}
	if HasFatal(Lint(good)) {
		t.Errorf("valid secret_remote_judge lints fatal: %+v", Lint(good))
	}
	spec, err := Compile(good)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if spec.SecretRemoteJudge != DecisionDeny {
		t.Errorf("SecretRemoteJudge = %v, want deny", spec.SecretRemoteJudge)
	}
}
