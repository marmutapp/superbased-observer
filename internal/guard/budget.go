package guard

import (
	"time"

	"github.com/marmutapp/superbased-observer/internal/policy"
)

// Budget & stuck-loop integration (guard spec §12 / G12). The guard
// layer owns three pieces of session state here, all per the
// one-owner rule (§17.4):
//
//   - a TTL-cached spend lookup (SetBudgetLookup, injected at cmd
//     composition — guard never imports store) whose values stamp
//     Event.SessionCostUSD / DailyCostUSD on the watcher-ingest and
//     proxy-request boundaries. Hook processes never stamp: the
//     lookup lives in the daemon, and a per-tool-call DB query would
//     blow the §6.4 latency budget.
//   - a per-session record-dedup for the flag-class B-601/B-602
//     verdicts: cost only grows within a session, so a breach is
//     recorded ONCE per (session, rule) — without this every action
//     past the threshold would re-record. Denies (hard mode, proxy)
//     always record: each blocked request is its own audit event.
//   - the A-610 repeat tracker: consecutive-identical-action run
//     lengths per session, stamped as Event.RepeatCount on the
//     ingest path (watcher only — hook guards are process-local and
//     see one event; documented in the conformance notes).
//
// A-611 (MAD rate baselines) and A-612 (novelty) defer with reason —
// they need the per-project baseline plumbing in internal/intelligence
// (spec §22 tracker).

// BudgetLookup returns the session-so-far and calendar-day-so-far
// spend in USD. ok=false means the data is unavailable (query error);
// the guard treats that as zero stamps — budget rules fail toward
// silence, never toward a spurious breach.
type BudgetLookup func(sessionID string) (sessionUSD, dailyUSD float64, ok bool)

// budgetCacheTTL bounds how stale a stamped spend value may be; a
// breach is detected at most one TTL after it happens, and the
// underlying store query runs at most once per session per TTL.
const budgetCacheTTL = 30 * time.Second

// maxBudgetSessions bounds the cache + dedup maps (the proxySeen
// bound shape).
const maxBudgetSessions = 256

// budgetEntry is one cached lookup result.
type budgetEntry struct {
	session, daily float64
	at             time.Time
}

// SetBudgetLookup wires the spend lookup. Nil keeps budget stamping
// off (events carry zero → B-601/B-602 and cost user-matchers are
// inert). Set once at composition.
func (g *Guard) SetBudgetLookup(fn BudgetLookup) {
	g.budgetLookup = fn
}

// stampBudget fills the Event's spend fields from the cached lookup.
// No-op without a wired lookup or a session ID.
func (g *Guard) stampBudget(ev *policy.Event) {
	if g.budgetLookup == nil || ev.SessionID == "" {
		return
	}
	now := ev.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	g.budgetMu.Lock()
	e, ok := g.budgetCache[ev.SessionID]
	g.budgetMu.Unlock()
	if !ok || now.Sub(e.at) > budgetCacheTTL || now.Before(e.at) {
		sess, daily, lok := g.budgetLookup(ev.SessionID)
		if !lok {
			return
		}
		e = budgetEntry{session: sess, daily: daily, at: now}
		g.budgetMu.Lock()
		if g.budgetCache == nil {
			g.budgetCache = make(map[string]budgetEntry)
		}
		if len(g.budgetCache) >= maxBudgetSessions {
			evictOldestBudget(g.budgetCache)
		}
		g.budgetCache[ev.SessionID] = e
		g.budgetMu.Unlock()
	}
	ev.SessionCostUSD = e.session
	ev.DailyCostUSD = e.daily
}

// evictOldestBudget drops the stalest cache entry (called locked).
func evictOldestBudget(m map[string]budgetEntry) {
	var oldest string
	var at time.Time
	first := true
	for k, e := range m {
		if first || e.at.Before(at) {
			oldest, at, first = k, e.at, false
		}
	}
	delete(m, oldest)
}

// budgetRuleIDs marks the rows the flag-dedup applies to.
func isBudgetRuleID(id string) bool { return id == "B-601" || id == "B-602" }

// budgetAlreadyRecorded reports (and marks) whether a flag-class
// budget verdict was already recorded for this session+rule. Denies
// never consult this — they always record.
func (g *Guard) budgetAlreadyRecorded(sessionID, ruleID string) bool {
	if sessionID == "" {
		return false
	}
	g.budgetMu.Lock()
	defer g.budgetMu.Unlock()
	if g.budgetRecorded == nil {
		g.budgetRecorded = make(map[string]map[string]bool)
	}
	rules := g.budgetRecorded[sessionID]
	if rules == nil {
		if len(g.budgetRecorded) >= maxBudgetSessions {
			for k := range g.budgetRecorded {
				delete(g.budgetRecorded, k)
				break
			}
		}
		rules = make(map[string]bool)
		g.budgetRecorded[sessionID] = rules
	}
	if rules[ruleID] {
		return true
	}
	rules[ruleID] = true
	return false
}

// scanBudget is the §12.1 proxy-request budget check: one stamped
// api_request event through the real engine BEFORE the egress scan
// (cheapest check first; a hard-mode deny skips the rest of the
// pipeline — the request never reaches the provider). Soft breaches
// flag once per session per rule.
func (g *Guard) scanBudget(res *ProxyRequestResult, sessionID, target string, now time.Time) {
	ev := policy.Event{
		Kind:      policy.KindAPIRequest,
		Target:    target,
		SessionID: sessionID,
		Caps:      proxyRequestCaps,
		Now:       now,
	}
	g.stampBudget(&ev)
	if ev.SessionCostUSD == 0 && ev.DailyCostUSD == 0 {
		return
	}
	verdict, guardErr := g.Evaluate(ev)
	if verdict.Decision < policy.DecisionFlag && guardErr == nil {
		return
	}
	if !isBudgetRuleID(verdict.RuleID) && guardErr == nil {
		// Some other api_request row won (it will get its own pass on
		// the egress event); the budget seam only owns B-6xx records.
		return
	}
	verdict, approved := g.applyApprovals(verdict, &ev)

	av := ActionVerdict{
		Input: ActionInput{
			SessionID: sessionID,
			Target:    target,
			Timestamp: now,
		},
		Kind:       policy.KindAPIRequest,
		Category:   g.CategoryFor(verdict.RuleID),
		Verdict:    verdict,
		GuardError: guardErr != nil,
	}
	if approved {
		av.DegradedFrom = "approved"
	}
	em := ResolveEmission(verdict, proxyRequestCaps)
	if em.Permission == "deny" {
		av.Enforced = true
		res.Deny = true
		res.DenyRuleID = verdict.RuleID
		res.DenyReason = verdict.Reason
		res.Verdicts = append(res.Verdicts, av)
		return
	}
	if g.budgetAlreadyRecorded(sessionID, verdict.RuleID) {
		return
	}
	res.Verdicts = append(res.Verdicts, av)
}
