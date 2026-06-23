package policy

import "strconv"

// Budget rules B-601/B-602 (spec §5.7, §12.1) — cost-threshold
// breaches. The rows compare the Event's stamped spend-so-far values
// (SessionCostUSD / DailyCostUSD, computed at the owner: the guard
// layer's TTL-cached budget lookup over proxy turns + token_usage)
// against the [guard.budget] thresholds carried on Config. Both sides
// must be non-zero: an unconfigured threshold disables the row, and
// an unstamped event (hook path, lookup not wired) never matches.
//
// Decisions: flag/flag by default — a budget breach alerts, it does
// not block (D2). [guard.budget].hard upgrades the ENFORCE-mode
// decision to deny at engine construction (Config.BudgetHard): the
// §12.1 "deny-on-proxy" — the proxy is the only stamped channel that
// can block (synthetic 4xx), watcher surfaces record the §6.2
// degradation, hook events are never stamped. Severity high so the
// default [guard.alerts] min_severity surfaces the breach.
//
// Record volume is bounded at the guard layer: flag-class budget
// verdicts dedup once per (session, rule) — cost only grows within a
// session, so re-recording every subsequent action is noise; denies
// always record (each blocked request is its own audit event).

// matchCostOver builds a matcher comparing a stamped cost value
// against a configured threshold, both injected as accessors so the
// two budget rows share one implementation (and one test shape).
func matchCostOver(value func(*Event) float64, limit func(*Config) float64, scope string) MatchFn {
	return func(ctx *MatchContext) (bool, string) {
		lim := limit(ctx.Cfg)
		got := value(ctx.Event)
		if lim <= 0 || got <= lim {
			return false, ""
		}
		return true, scope + " spend $" + strconv.FormatFloat(got, 'f', 2, 64) +
			" exceeds the $" + strconv.FormatFloat(lim, 'f', 2, 64) + " budget"
	}
}

// budgetEventKinds are the kinds the guard layer stamps spend onto:
// every classified watcher kind plus the proxy's api_request.
func budgetEventKinds() []EventKind {
	return []EventKind{
		KindAPIRequest, KindShellExec, KindFileAccess,
		KindMCPCall, KindConfigChange, KindToolCall,
	}
}

// budgetRules assembles the §5.7 budget rows.
func budgetRules() []Rule {
	kinds := budgetEventKinds()
	return []Rule{
		{
			ID: "B-601", Category: CategoryBudget, Severity: SeverityHigh,
			AppliesTo: kinds,
			Match: matchCostOver(
				func(ev *Event) float64 { return ev.SessionCostUSD },
				func(cfg *Config) float64 { return cfg.BudgetSessionUSD },
				"session",
			),
			Observe: DecisionFlag, Enforce: DecisionFlag,
			Doc:    "session cost exceeded [guard.budget].session_usd",
			Advice: "Review what the session is burning tokens on; raise [guard.budget].session_usd, approve B-601 for this session, or stop the run. hard=true blocks further proxy requests in enforce mode.",
		},
		{
			ID: "B-602", Category: CategoryBudget, Severity: SeverityHigh,
			AppliesTo: kinds,
			Match: matchCostOver(
				func(ev *Event) float64 { return ev.DailyCostUSD },
				func(cfg *Config) float64 { return cfg.BudgetDailyUSD },
				"daily",
			),
			Observe: DecisionFlag, Enforce: DecisionFlag,
			Doc:    "daily cost (all sessions) exceeded [guard.budget].daily_usd",
			Advice: "Today's total spend across sessions crossed the configured ceiling; raise [guard.budget].daily_usd or pause agent work. hard=true blocks further proxy requests in enforce mode.",
		},
	}
}
