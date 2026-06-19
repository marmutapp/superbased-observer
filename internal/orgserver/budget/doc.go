// Package budget is the org server's budget evaluator: a background goroutine
// that, every poll interval, computes each budget's trailing rolling-30-day
// spend (via rollup.ScopeSpend — the same definition the dashboard shows) and
// fires a webhook alert when spend crosses a new threshold.
//
// "Once per threshold crossing, not per poll cycle" is enforced by
// budgets.last_fired_threshold: the evaluator fires only for the highest
// threshold strictly above the last-fired high-water mark, then persists it.
// When spend ages out of the 30-day window below a fired threshold the mark is
// lowered (no alert), so a later re-crossing re-alerts.
//
// Webhook delivery is asynchronous with exponential backoff up to one hour,
// then dropped with a logged error — a slow or dead webhook never stalls the
// evaluator or the server. The payload is the OpenAPI-documented BudgetAlert.
package budget
