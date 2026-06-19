package policy

import (
	"strings"
	"testing"
	"time"
)

// TestBudgetRules is the §5.7 budget conformance table: per row a
// breach hit, the at-threshold near-miss, the unconfigured-threshold
// off-switch, and the unstamped-event off-switch — flag in both modes
// without hard.
func TestBudgetRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		cfg      Config
		ev       Event
		wantRule string
	}{
		{
			name:     "B-601 hit: session over budget",
			cfg:      Config{BudgetSessionUSD: 5},
			ev:       Event{SessionCostUSD: 5.01},
			wantRule: "B-601",
		},
		{
			name: "B-601 near-miss: exactly at threshold",
			cfg:  Config{BudgetSessionUSD: 5},
			ev:   Event{SessionCostUSD: 5},
		},
		{
			name: "B-601 off: threshold unconfigured",
			cfg:  Config{},
			ev:   Event{SessionCostUSD: 99},
		},
		{
			name: "B-601 off: event unstamped",
			cfg:  Config{BudgetSessionUSD: 5},
			ev:   Event{},
		},
		{
			name:     "B-602 hit: daily over budget",
			cfg:      Config{BudgetDailyUSD: 20},
			ev:       Event{DailyCostUSD: 25},
			wantRule: "B-602",
		},
		{
			name: "B-602 near-miss: at threshold",
			cfg:  Config{BudgetDailyUSD: 20},
			ev:   Event{DailyCostUSD: 20},
		},
		{
			name:     "B-601 wins over B-602 by table order when both breach",
			cfg:      Config{BudgetSessionUSD: 5, BudgetDailyUSD: 20},
			ev:       Event{SessionCostUSD: 6, DailyCostUSD: 25},
			wantRule: "B-601",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev := tc.ev
			ev.Kind = KindAPIRequest
			ev.Target = "anthropic:claude-x"
			ev.Now = time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
			for _, mode := range []Mode{ModeObserve, ModeEnforce} {
				cfg := tc.cfg
				cfg.Mode = mode
				eng, err := New(cfg)
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
				if v.RuleID != tc.wantRule || v.Decision != DecisionFlag || v.Severity != SeverityHigh {
					t.Errorf("%s = %s/%s/%s, want %s/flag/high", mode, v.RuleID, v.Decision, v.Severity, tc.wantRule)
				}
				if v.RuleID == tc.wantRule && !strings.Contains(v.Reason, "$") {
					t.Errorf("reason %q lacks the spend detail", v.Reason)
				}
			}
		})
	}
}

// TestBudgetRules_HardMode pins the §12.1 hard upgrade: enforce-mode
// decision becomes deny while observe stays flag (D2 — nothing blocks
// until the operator flips enforce), and the upgrade shows in the
// effective table.
func TestBudgetRules_HardMode(t *testing.T) {
	t.Parallel()
	ev := Event{
		Kind: KindAPIRequest, Target: "anthropic:claude-x",
		SessionCostUSD: 6, Now: time.Now(),
	}
	for _, tc := range []struct {
		mode Mode
		want Decision
	}{
		{ModeObserve, DecisionFlag},
		{ModeEnforce, DecisionDeny},
	} {
		eng, err := New(Config{Mode: tc.mode, BudgetSessionUSD: 5, BudgetHard: true})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if v := eng.Evaluate(ev); v.RuleID != "B-601" || v.Decision != tc.want {
			t.Errorf("%s = %s/%s, want B-601/%s", tc.mode, v.RuleID, v.Decision, tc.want)
		}
	}
	eng, _ := New(Config{Mode: ModeEnforce, BudgetSessionUSD: 5, BudgetHard: true})
	for _, info := range eng.RuleInfos() {
		if info.Category == CategoryBudget && info.Enforce != DecisionDeny {
			t.Errorf("hard mode: %s effective enforce = %s, want deny", info.ID, info.Enforce)
		}
		if info.Category == CategoryBudget && info.Observe != DecisionFlag {
			t.Errorf("hard mode: %s effective observe = %s, want flag (D2)", info.ID, info.Observe)
		}
	}
}
