package policy

import (
	"testing"
	"time"
)

// TestPostureRules_DialectDrift is the §5.4 conformance table for the
// finding-driven posture rows (R-204 dialect drift, R-205 bundle
// integrity): hit per kind, unknown-kind near-miss, and the
// no-findings baseline, in both modes (flag in both — posture scans
// are post-hoc; remediation is `observer guard compile` / re-enrol,
// not a block).
func TestPostureRules_DialectDrift(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		finding  string
		wantRule string
	}{
		{name: "R-204 hit: dialect drift", finding: PostureFindingDialectDrift, wantRule: "R-204"},
		{name: "R-205 hit: bundle signature rejection", finding: PostureFindingBundleSignature, wantRule: "R-205"},
		{name: "near-miss: unknown finding kind", finding: "not_a_kind"},
		{name: "near-miss: no findings", finding: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Target stays EMPTY (the EvaluatePostureFindings emission
			// shape): a config-change event targeting the settings path
			// itself would hit R-160's earlier path row — the drift
			// scan is not an agent write, so the path travels in the
			// finding, not the event target.
			ev := Event{
				Kind: KindConfigChange,
				Tool: "claude-code",
				Now:  time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC),
			}
			if tc.finding != "" {
				ev.PostureFindings = []PostureFinding{{
					Kind: tc.finding, Client: "claude-code",
					Target: "~/.claude/settings.json", Detail: "2 compiled entries missing",
				}}
			}
			for _, mode := range []Mode{ModeObserve, ModeEnforce} {
				eng, err := New(Config{Mode: mode, Home: "/home/u"})
				if err != nil {
					t.Fatalf("New %s: %v", mode, err)
				}
				v := eng.Evaluate(ev)
				if tc.wantRule == "" {
					if v.RuleID != "" {
						t.Fatalf("%s: want no hit, got %+v", mode, v)
					}
					continue
				}
				if v.RuleID != tc.wantRule || v.Decision != DecisionFlag {
					t.Errorf("%s = %s/%s, want %s/flag (reason %q)", mode, v.RuleID, v.Decision, tc.wantRule, v.Reason)
				}
				if v.RuleID == tc.wantRule && v.Severity != SeverityHigh {
					t.Errorf("%s severity = %s, want high", mode, v.Severity)
				}
			}
		})
	}
}

// TestPostureRules_FindingDetailInReason pins the reason shape: the
// verdict must name the client, the native config path and the detail
// so the audit row is self-explanatory without file access.
func TestPostureRules_FindingDetailInReason(t *testing.T) {
	t.Parallel()
	eng, err := New(Config{Mode: ModeObserve, Home: "/home/u"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	v := eng.Evaluate(Event{
		Kind: KindConfigChange,
		PostureFindings: []PostureFinding{{
			Kind: PostureFindingDialectDrift, Client: "opencode",
			Target: "~/.config/opencode/opencode.json", Detail: "1 compiled entry missing",
		}},
		Now: time.Now(),
	})
	for _, want := range []string{"opencode", "~/.config/opencode/opencode.json", "1 compiled entry missing"} {
		if !contains(v.Reason, want) {
			t.Errorf("reason %q missing %q", v.Reason, want)
		}
	}
}
