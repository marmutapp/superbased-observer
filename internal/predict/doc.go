// Package predict is the pure-logic core of the Next-Message Cost &
// Limit Predictor (docs/plans/next-message-cost-predictor-plan-2026-06-18.md).
//
// It answers, for a session the operator is about to send their next
// message to: "roughly how much will that message cost?" — expressed
// as a low / mid / high band. The limit half of the feature (what
// fraction of the 5h / weekly window the message consumes) is captured
// on the proxy path and lives in internal/store + internal/proxy; this
// package owns only the cost-estimate math.
//
// Discipline mirrors internal/cachetrack and internal/routing: NO
// database/sql, NO net/http, NO fsnotify (pinned by imports_test.go).
// Every input is caller-assembled from existing tables at the store
// seam (internal/store/predict.go) and handed in as plain structs;
// rates are resolved via cost.Table.Lookup at that seam so this package
// stays cost-package-free.
//
// The §0 live-data findings (2026-06-19) shape the design:
//   - The substrate is token_usage, not api_turns (api_turns is
//     proxy-only and nearly empty on hook installs).
//   - For cached Claude Code, fresh input per turn ≈ 0 (input ≈
//     cache_read); cost variance lives in output (O) and the
//     turns-per-user-message fan-out (T), with the cache prefix (P)
//     large and re-read every turn.
//   - The user_prompt boundary that yields T is present in only ~32%
//     of sessions, so T degrades through a 3-tier ladder
//     (observed → prior → default), surfaced honestly via TurnsTier.
package predict
