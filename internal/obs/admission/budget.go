package admission

// Per-end-user budget verdict (org-hosted-app model). The guardrail budgeting
// surrounds an LLM application hosted on the org server, with END-USER requests
// routed through it; the budgeted subject is an end-user of that app, not an
// enrolled developer. This is the PURE decision half: given the end-user's
// rolling-7-day and calendar-month spend and the configured caps, it returns a
// Deny verdict on a breach. The spend LOOKUP (obs_spans.cost_usd via
// obs_traces.user) lives in the boundary service, which folds this verdict
// stricter-wins with the text/policy pipeline result — evaluated OUTSIDE the
// text+policy verdict cache, since a budget breach depends on the user's
// accumulated spend, not the message. No store, no config type: pure.

// BudgetCriterion{Monthly,Weekly,FiveHour} name the criterion the budget
// verdict carries, so a recorded event and CLI surfaces can attribute a
// would-block to the per-end-user budget rather than a judged criterion. The
// three windows are a calendar-month budget, a rolling-7-day limit, and a
// rolling-5-hour limit (the short-window rate limit).
const (
	BudgetCriterionMonthly  = "budget.user_monthly"
	BudgetCriterionWeekly   = "budget.user_weekly"
	BudgetCriterionFiveHour = "budget.user_5h"
)

// BudgetVerdict reports whether an end-user's spend breaches a configured cap
// in any of the three windows (calendar-month, rolling-7-day, rolling-5-hour).
// Windows are checked broadest-first (monthly → weekly → 5h) and the first
// breach is a terminal Deny — in observe mode the boundary records it as a
// shadow "would-deny", in enforce it blocks. A 0 cap disables that window;
// fired=false means no breach (the caller keeps the pipeline verdict).
//
// The user-facing Reason is deliberately generic (it may be surfaced to the
// calling app / end-user in enforce mode); the $ detail is left to the recorded
// event + operator surfaces, never leaked in the refusal.
func BudgetVerdict(fiveHour, weekly, monthly, fiveHourCap, weeklyCap, monthlyCap float64) (Result, bool) {
	if monthlyCap > 0 && monthly > monthlyCap {
		return budgetDeny(BudgetCriterionMonthly, "monthly"), true
	}
	if weeklyCap > 0 && weekly > weeklyCap {
		return budgetDeny(BudgetCriterionWeekly, "weekly"), true
	}
	if fiveHourCap > 0 && fiveHour > fiveHourCap {
		return budgetDeny(BudgetCriterionFiveHour, "5-hour"), true
	}
	return Result{}, false
}

// budgetDeny builds the Deny verdict for a breached window.
func budgetDeny(criterion, window string) Result {
	return Result{
		Decision:  DecisionDeny,
		Severity:  SeverityHigh,
		Criterion: criterion,
		Reason:    "Usage budget exceeded for this user (" + window + ").",
		Degraded:  "budget-gate",
	}
}
