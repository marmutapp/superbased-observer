package policy

import (
	"testing"
	"time"
)

// TestAnomalyRules_A610 is the §5.7 stuck-loop conformance table:
// the row fires exactly on the crossing event (threshold+1), stays
// silent below, and does NOT re-fire deeper into the same run — one
// finding per episode. Flag in both modes (§12.2: anomaly never
// blocks).
func TestAnomalyRules_A610(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		count int
		hit   bool
	}{
		{"below threshold", repeatLoopThreshold, false},
		{"crossing fires", repeatLoopThreshold + 1, true},
		{"deeper into the run stays silent", repeatLoopThreshold + 2, false},
		{"untracked (hook/proxy path)", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev := Event{
				Kind: KindShellExec, ActionType: "run_command",
				Target: "go test ./...", RepeatCount: tc.count,
				Now: time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC),
			}
			for _, mode := range []Mode{ModeObserve, ModeEnforce} {
				eng, err := New(Config{Mode: mode})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				v := eng.Evaluate(ev)
				if !tc.hit {
					if v.RuleID == "A-610" {
						t.Fatalf("%s: unexpected A-610 at count %d", mode, tc.count)
					}
					continue
				}
				if v.RuleID != "A-610" || v.Decision != DecisionFlag || v.Severity != SeverityWarn {
					t.Errorf("%s = %s/%s/%s, want A-610/flag/warn", mode, v.RuleID, v.Decision, v.Severity)
				}
			}
		})
	}
}
