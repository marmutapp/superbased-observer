package obs

import (
	"context"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/obs/admission"
	"github.com/marmutapp/superbased-observer/internal/obs/egress"
)

// installEgress compiles + installs an egress policy on a service.
func installEgress(t *testing.T, svc *AdmissionService, in egress.PolicyInput) {
	t.Helper()
	spec, err := egress.Compile(in)
	if err != nil {
		t.Fatalf("egress.Compile: %v", err)
	}
	svc.SetEgressPolicy(spec)
}

// TestEgressAdviseRecordsButNeverBlocks proves the advise-mode composition: a
// flagged request evaluates the egress directive, records it to
// obs_egress_decisions (mode advise, applied=false), and returns it on the
// response — but the request is STILL allowed (advise never applies).
func TestEgressAdviseRecordsButNeverBlocks(t *testing.T) {
	ctx := context.Background()
	svc, st := newAdmissionSvc(t, admission.PolicyInput{
		Mode:      "observe",
		Prefilter: admission.PrefilterInput{Deny: []string{"forbidden"}},
	})
	installEgress(t, svc, egress.PolicyInput{
		Mode:    egress.ModeAdvise,
		Targets: []egress.TargetInput{{ID: "ollama-local", URL: "http://127.0.0.1:11434", Shape: "openai"}},
		Rules: []egress.RuleInput{{
			Name: "flagged-to-local", When: egress.WhenInput{VerdictAtLeast: "deny"},
			RouteToUpstream: "ollama-local", OnUnavailable: egress.OnUnavailableDeny,
			ReasonCode: string(egress.ReasonFlaggedLocal),
		}},
	})

	resp := svc.Check(ctx, AdmissionCheck{
		Text: "please help with forbidden", RequestID: "req-a", Model: "claude-opus-4-8",
		Provider: "anthropic", Persist: true,
	})
	if !resp.Allowed {
		t.Fatalf("advise mode must not block: %+v", resp)
	}
	if resp.Egress == nil || !resp.Egress.Matched || resp.Egress.Action != string(egress.ActionRouteUpstream) {
		t.Fatalf("expected an egress route directive on the response: %+v", resp.Egress)
	}
	if !resp.Egress.MustUseTarget || resp.Egress.TargetShape != "openai" {
		t.Fatalf("locality directive fields wrong: %+v", resp.Egress)
	}
	rows, err := st.ListEgressDecisions(ctx, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].Mode != "advise" || rows[0].Applied {
		t.Fatalf("advise decision not recorded correctly: %+v", rows)
	}
	if rows[0].RequestID != "req-a" || rows[0].ModelFrom != "claude-opus-4-8" {
		t.Fatalf("audit soft-join fields wrong: %+v", rows[0])
	}
	if cr, _ := st.VerifyEgressChain(ctx); !cr.OK || cr.Rows != 1 {
		t.Fatalf("egress chain not intact: %+v", cr)
	}
}

// TestEgressEnforceDenyBlocks proves an enforce-mode egress deny converts to a
// request block with the rule's reason.
func TestEgressEnforceDenyBlocks(t *testing.T) {
	ctx := context.Background()
	svc, st := newAdmissionSvc(t, admission.PolicyInput{Mode: "observe"})
	installEgress(t, svc, egress.PolicyInput{
		Mode: egress.ModeEnforce,
		Rules: []egress.RuleInput{{
			Name: "sensitive", When: egress.WhenInput{Criterion: "secret.remote_judge"},
			Deny: true, Reason: "Sensitive content cannot be served.",
		}},
	})
	// Force the criterion via a matching admission verdict is awkward here;
	// instead drive the egress deny by a criterion match through a prefilter is
	// not possible, so assert the enforce-deny path via a direct model matcher.
	// Use a model_glob deny instead to exercise the block path deterministically.
	installEgress(t, svc, egress.PolicyInput{
		Mode: egress.ModeEnforce,
		Rules: []egress.RuleInput{{
			Name: "deny-opus", When: egress.WhenInput{ModelGlob: "claude-opus-*"},
			Deny: true, Reason: "Blocked by egress policy.",
		}},
	})
	resp := svc.Check(ctx, AdmissionCheck{Text: "hello", Model: "claude-opus-4-8", RequestID: "req-b", Persist: true})
	if resp.Allowed {
		t.Fatalf("enforce egress deny must block: %+v", resp)
	}
	if resp.Reason != "Blocked by egress policy." {
		t.Fatalf("block reason not surfaced: %q", resp.Reason)
	}
	rows, _ := st.ListEgressDecisions(ctx, 10)
	if len(rows) != 1 || rows[0].Action != "deny" {
		t.Fatalf("enforce deny not recorded: %+v", rows)
	}
}

// TestEgressEnforceInvalidTargetBlocks proves a statically-known-invalid target
// (an unknown upstream id that slipped past compile — here injected by
// installing a spec whose rule references an id with no target, which advise
// tolerates but enforce treats as a Block at eval).
func TestEgressEnforceInvalidTargetBlocks(t *testing.T) {
	ctx := context.Background()
	svc, _ := newAdmissionSvc(t, admission.PolicyInput{Mode: "observe"})
	// Enforce Compile would normally reject an unknown id; simulate a stale id
	// slipping through by compiling in ADVISE (lenient) then forcing the mode to
	// enforce on the installed spec.
	spec, err := egress.Compile(egress.PolicyInput{
		Mode: egress.ModeAdvise,
		Rules: []egress.RuleInput{{
			Name: "ghost", When: egress.WhenInput{VerdictAtLeast: "allow"},
			RouteToUpstream: "does-not-exist", OnUnavailable: egress.OnUnavailableDeny,
		}},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	spec.Mode = egress.ModeEnforce
	svc.SetEgressPolicy(spec)

	resp := svc.Check(ctx, AdmissionCheck{Text: "hello", RequestID: "req-c", Persist: true})
	if resp.Allowed {
		t.Fatalf("enforce with statically-invalid target must block: %+v", resp)
	}
	if resp.Egress == nil || !resp.Egress.Block {
		t.Fatalf("expected obs-side Block on invalid target: %+v", resp.Egress)
	}
}
