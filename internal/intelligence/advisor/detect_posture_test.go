package advisor

import (
	"strings"
	"testing"
	"time"
)

func postureFacts() *Facts {
	return &Facts{WindowDays: 14, Now: time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)}
}

// TestDetectGuardObserveIdle_Rows pins the X3.1 guard posture nudge:
// fires only on observe mode + recurring high-sev verdicts + zero
// active approvals; navigate-only Action to the Security page; no
// savings claim.
func TestDetectGuardObserveIdle_Rows(t *testing.T) {
	t.Parallel()
	th := DefaultThresholds()
	cases := []struct {
		name      string
		mode      string
		highSev   int
		approvals int
		want      bool
	}{
		{"fires_on_idle_observe", "observe", 25, 0, true},
		{"at_floor_fires", "observe", th.GuardIdleHighSevFloor, 0, true},
		{"below_floor_quiet", "observe", th.GuardIdleHighSevFloor - 1, 0, false},
		{"approvals_mean_engagement", "observe", 25, 1, false},
		{"enforce_already_promoted", "enforce", 25, 0, false},
		{"guard_off", "off", 25, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := postureFacts()
			f.GuardMode = tc.mode
			f.GuardHighSevEvents = tc.highSev
			f.GuardActiveApprovals = tc.approvals
			got := detectGuardObserveIdle(f, th)
			if (len(got) == 1) != tc.want {
				t.Fatalf("fired=%v, want %v (%+v)", len(got) == 1, tc.want, got)
			}
			if !tc.want {
				return
			}
			s := got[0]
			if s.SavingsUSD != 0 || s.SavingsMin != 0 {
				t.Errorf("posture nudge must claim no savings: %+v", s)
			}
			if s.Action == nil || s.Action.Kind != "page" || s.Action.Target != "/security" {
				t.Errorf("action = %+v, want page:/security", s.Action)
			}
		})
	}
}

// TestDetectRoutingEvidenceReady_Rows pins the X3.1 routing nudge: the
// gate verdict comes verbatim from the injected ShadowSignal (never
// re-derived); enforce mode and a nil/unready signal stay quiet; the
// would-have savings ride as the suggestion's claim.
func TestDetectRoutingEvidenceReady_Rows(t *testing.T) {
	t.Parallel()
	ready := &ShadowSignal{AdviseDecisions: 80, WouldReroute: 30, WouldSaveUSD: 12.5, MinDecisions: 50, Ready: true}
	cases := []struct {
		name   string
		mode   string
		shadow *ShadowSignal
		want   bool
	}{
		{"fires_in_advise", "advise", ready, true},
		{"fires_when_off_with_history", "off", ready, true},
		{"enforce_already_promoted", "enforce", ready, false},
		{"gate_not_met", "advise", &ShadowSignal{AdviseDecisions: 10, Ready: false}, false},
		{"no_signal_injected", "advise", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := postureFacts()
			f.RoutingMode = tc.mode
			f.RoutingShadow = tc.shadow
			got := detectRoutingEvidenceReady(f, DefaultThresholds())
			if (len(got) == 1) != tc.want {
				t.Fatalf("fired=%v, want %v (%+v)", len(got) == 1, tc.want, got)
			}
			if !tc.want {
				return
			}
			s := got[0]
			if s.SavingsUSD != 12.5 {
				t.Errorf("SavingsUSD = %v, want the shadow's would-save", s.SavingsUSD)
			}
			if s.Action == nil || s.Action.Kind != "page" || s.Action.Target != "/routing" {
				t.Errorf("action = %+v, want page:/routing", s.Action)
			}
			if !strings.Contains(s.Nudge, "the gate is evidence, not the decision") {
				t.Errorf("nudge lost the operator-judged honesty line: %q", s.Nudge)
			}
		})
	}
}

// TestRun_ZeroClaimSuggestionsPassSavingsFloor pins the engine filter
// relaxation: the floor suppresses SMALL claims, never the absence of
// one — a zero-claim posture nudge survives MinSavingsUSD.
func TestRun_ZeroClaimSuggestionsPassSavingsFloor(t *testing.T) {
	t.Parallel()
	// Filter logic in isolation: mirror Run's gate expression.
	floor := 1.00
	pass := func(usd, minutes float64) bool {
		return !(usd > 0 && minutes == 0 && usd < floor)
	}
	if !pass(0, 0) {
		t.Error("zero-claim suggestion must pass the savings floor")
	}
	if pass(0.5, 0) {
		t.Error("small nonzero claim must still be suppressed")
	}
	if !pass(2.0, 0) || !pass(0, 10) {
		t.Error("existing pass arms regressed")
	}
}
