package admission

import "testing"

func TestBudgetVerdict(t *testing.T) {
	tests := []struct {
		name                               string
		fiveHour, weekly, monthly          float64
		fiveHourCap, weeklyCap, monthlyCap float64
		wantFired                          bool
		wantDecision                       Decision
		wantCriterion                      string
	}{
		{name: "under all caps", fiveHour: 1, weekly: 5, monthly: 20, fiveHourCap: 3, weeklyCap: 10, monthlyCap: 50, wantFired: false},
		{name: "over monthly", weekly: 5, monthly: 60, weeklyCap: 10, monthlyCap: 50, wantFired: true, wantDecision: DecisionDeny, wantCriterion: BudgetCriterionMonthly},
		{name: "over weekly only", weekly: 12, monthly: 20, weeklyCap: 10, monthlyCap: 50, wantFired: true, wantDecision: DecisionDeny, wantCriterion: BudgetCriterionWeekly},
		{name: "over 5h only", fiveHour: 4, weekly: 5, monthly: 20, fiveHourCap: 3, weeklyCap: 10, monthlyCap: 50, wantFired: true, wantDecision: DecisionDeny, wantCriterion: BudgetCriterionFiveHour},
		{name: "monthly checked before weekly and 5h", fiveHour: 4, weekly: 12, monthly: 60, fiveHourCap: 3, weeklyCap: 10, monthlyCap: 50, wantFired: true, wantDecision: DecisionDeny, wantCriterion: BudgetCriterionMonthly},
		{name: "zero caps disable", fiveHour: 100, weekly: 100, monthly: 100, wantFired: false},
		{name: "exactly at cap does not breach", fiveHour: 3, weekly: 10, monthly: 50, fiveHourCap: 3, weeklyCap: 10, monthlyCap: 50, wantFired: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, fired := BudgetVerdict(tc.fiveHour, tc.weekly, tc.monthly, tc.fiveHourCap, tc.weeklyCap, tc.monthlyCap)
			if fired != tc.wantFired {
				t.Fatalf("fired = %v, want %v", fired, tc.wantFired)
			}
			if !fired {
				return
			}
			if res.Decision != tc.wantDecision {
				t.Errorf("decision = %v, want %v", res.Decision, tc.wantDecision)
			}
			if res.Criterion != tc.wantCriterion {
				t.Errorf("criterion = %q, want %q", res.Criterion, tc.wantCriterion)
			}
			if res.Reason == "" {
				t.Error("breach verdict has an empty reason")
			}
		})
	}
}
