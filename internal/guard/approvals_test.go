package guard

import (
	"os"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/policy"
)

// TestApprovals pins the §6.3 integration: an active grant downgrades
// a blocking verdict to flag with the exception RECORDED
// (degraded_from="approved" + reason suffix); flag verdicts never
// consult the lookup; lookup absence/refusal enforces.
func TestApprovals(t *testing.T) {
	t.Parallel()
	cfg := guardCfg()
	cfg.Mode = "enforce"
	mk := func(lookup ApprovalLookup) *Guard {
		g, err := New(Options{
			Config: cfg, Home: "/home/u",
			ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		})
		if err != nil {
			t.Fatalf("guard.New: %v", err)
		}
		g.SetApprovalLookup(lookup)
		return g
	}
	denyEvent := policy.Event{
		Kind: policy.KindShellExec, ActionType: "run_command",
		Target: "rm -rf ~", SessionID: "sA", ProjectRoot: "/home/u/proj",
		Caps: policy.Capabilities{PreExecution: true, CanBlock: true, CanAsk: true},
	}

	t.Run("grant downgrades and records", func(t *testing.T) {
		t.Parallel()
		var askedRule, askedSession, askedHash string
		g := mk(func(ruleID, sessionID, rootHash string) bool {
			askedRule, askedSession, askedHash = ruleID, sessionID, rootHash
			return true
		})
		v, worthy := g.EvaluateHook(denyEvent)
		if !worthy || v.Verdict.Decision != policy.DecisionFlag {
			t.Fatalf("verdict = %+v, want approved-downgrade to flag", v.Verdict)
		}
		if v.DegradedFrom != "approved" {
			t.Errorf("DegradedFrom = %q, want approved (the §14.4 exception register marker)", v.DegradedFrom)
		}
		if !strings.Contains(v.Verdict.Reason, "approved") {
			t.Errorf("reason lacks the approval note: %q", v.Verdict.Reason)
		}
		if askedRule != "R-101" || askedSession != "sA" || askedHash != HashProjectRoot("/home/u/proj") {
			t.Errorf("lookup args = (%s, %s, %s)", askedRule, askedSession, askedHash)
		}
	})

	t.Run("no grant enforces", func(t *testing.T) {
		t.Parallel()
		g := mk(func(string, string, string) bool { return false })
		if v, _ := g.EvaluateHook(denyEvent); v.Verdict.Decision != policy.DecisionDeny {
			t.Errorf("verdict = %v, want deny when no grant covers it", v.Verdict.Decision)
		}
	})

	t.Run("nil lookup enforces", func(t *testing.T) {
		t.Parallel()
		g := mk(nil)
		if v, _ := g.EvaluateHook(denyEvent); v.Verdict.Decision != policy.DecisionDeny {
			t.Errorf("verdict = %v, want deny with no lookup wired", v.Verdict.Decision)
		}
	})

	t.Run("flag verdicts never consult the lookup", func(t *testing.T) {
		t.Parallel()
		calls := 0
		g := mk(func(string, string, string) bool { calls++; return true })
		flagEvent := denyEvent
		flagEvent.Target = "git push --force origin feature-x" // near-miss: no rule
		_, _ = g.EvaluateHook(flagEvent)
		obs := guardCfg() // observe mode: rm -rf ~ is a flag verdict
		gObs, err := New(Options{
			Config: obs, Home: "/home/u",
			ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		})
		if err != nil {
			t.Fatal(err)
		}
		gObs.SetApprovalLookup(func(string, string, string) bool { calls++; return true })
		_, _ = gObs.EvaluateHook(denyEvent)
		if calls != 0 {
			t.Errorf("lookup ran %d times for non-blocking verdicts — it must only pay on ask/deny", calls)
		}
	})
}
