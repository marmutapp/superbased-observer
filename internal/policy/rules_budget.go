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

// matchUtilOver builds a matcher comparing a stamped utilization
// (0..1, the provider's own usage-window fraction) against a
// configured threshold. AT-OR-OVER matches (got >= lim) so a window
// exactly at the threshold trips; both sides must be non-zero (an
// unconfigured threshold disables the row, an unstamped event —
// no window observed — never matches). Reason is rendered as a
// percentage, the operator-facing unit.
func matchUtilOver(value func(*Event) float64, limit func(*Config) float64, window, action string) MatchFn {
	return func(ctx *MatchContext) (bool, string) {
		lim := limit(ctx.Cfg)
		got := value(ctx.Event)
		if lim <= 0 || got <= 0 || got < lim {
			return false, ""
		}
		return true, window + " usage window at " +
			strconv.FormatFloat(got*100, 'f', 0, 64) + "% (≥ " +
			strconv.FormatFloat(lim*100, 'f', 0, 64) + "% " + action + " threshold)"
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
		{
			ID: "B-603", Category: CategoryBudget, Severity: SeverityHigh,
			AppliesTo: kinds,
			Match: matchCostOver(
				func(ev *Event) float64 { return ev.MonthlyCostUSD },
				func(cfg *Config) float64 { return cfg.BudgetMonthlyUSD },
				"monthly",
			),
			Observe: DecisionFlag, Enforce: DecisionFlag,
			Doc:    "calendar-month cost (all sessions) exceeded [guard.budget].monthly_usd",
			Advice: "Month-to-date spend crossed the configured ceiling; raise [guard.budget].monthly_usd or pause agent work. hard=true blocks further proxy requests in enforce mode.",
		},
		{
			ID: "B-604", Category: CategoryBudget, Severity: SeverityHigh,
			AppliesTo: kinds,
			Match: matchCostOver(
				func(ev *Event) float64 { return ev.WeeklyCostUSD },
				func(cfg *Config) float64 { return cfg.BudgetWeeklyUSD },
				"weekly",
			),
			Observe: DecisionFlag, Enforce: DecisionFlag,
			Doc:    "rolling-7-day cost (all sessions) exceeded [guard.budget].weekly_usd",
			Advice: "Last-7-days spend crossed the configured ceiling; raise [guard.budget].weekly_usd or pause agent work. hard=true blocks further proxy requests in enforce mode.",
		},
	}
}

// limitRules assembles the §12.1 provider-usage-window rows (B-610..
// B-613). Unlike the $ budget rows these compare a UTILIZATION
// fraction (0..1) read from limit_snapshots against explicit warn/deny
// thresholds — CategoryLimit, so [guard.budget].hard never rewrites
// them (the deny threshold is the block trigger, no hard flag needed).
// Each window is TWO rows (warn flag, deny block); a util at-or-over
// the deny threshold matches both and the engine's stricter-wins
// resolution picks the deny.
func limitRules() []Rule {
	kinds := budgetEventKinds()
	return []Rule{
		{
			ID: "B-610", Category: CategoryLimit, Severity: SeverityWarn,
			AppliesTo: kinds,
			Match: matchUtilOver(
				func(ev *Event) float64 { return ev.Window5hUtil },
				func(cfg *Config) float64 { return cfg.LimitUtil5hWarn },
				"5h", "warn",
			),
			Observe: DecisionFlag, Enforce: DecisionFlag,
			Doc:    "5h usage window utilization reached [guard.budget.window].util_5h_warn",
			Advice: "Nearing the provider's 5-hour quota; pace requests or switch to a lighter model. Raise util_5h_warn to quiet this.",
		},
		{
			ID: "B-611", Category: CategoryLimit, Severity: SeverityHigh,
			AppliesTo: kinds,
			Match: matchUtilOver(
				func(ev *Event) float64 { return ev.Window5hUtil },
				func(cfg *Config) float64 { return cfg.LimitUtil5hDeny },
				"5h", "deny",
			),
			Observe: DecisionFlag, Enforce: DecisionDeny,
			Doc:    "5h usage window utilization reached [guard.budget.window].util_5h_deny",
			Advice: "At the provider's 5-hour quota ceiling; further proxy requests are blocked in enforce mode until the window resets. Raise util_5h_deny to relax.",
		},
		{
			ID: "B-612", Category: CategoryLimit, Severity: SeverityWarn,
			AppliesTo: kinds,
			Match: matchUtilOver(
				func(ev *Event) float64 { return ev.Window7dUtil },
				func(cfg *Config) float64 { return cfg.LimitUtilWeeklyWarn },
				"weekly", "warn",
			),
			Observe: DecisionFlag, Enforce: DecisionFlag,
			Doc:    "weekly usage window utilization reached [guard.budget.window].util_weekly_warn",
			Advice: "Nearing the provider's weekly quota; pace requests or switch to a lighter model. Raise util_weekly_warn to quiet this.",
		},
		{
			ID: "B-613", Category: CategoryLimit, Severity: SeverityHigh,
			AppliesTo: kinds,
			Match: matchUtilOver(
				func(ev *Event) float64 { return ev.Window7dUtil },
				func(cfg *Config) float64 { return cfg.LimitUtilWeeklyDeny },
				"weekly", "deny",
			),
			Observe: DecisionFlag, Enforce: DecisionDeny,
			Doc:    "weekly usage window utilization reached [guard.budget.window].util_weekly_deny",
			Advice: "At the provider's weekly quota ceiling; further proxy requests are blocked in enforce mode until the window resets. Raise util_weekly_deny to relax.",
		},
	}
}
