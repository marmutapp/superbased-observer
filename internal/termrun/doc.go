// Package termrun is the pure-logic core of the terminal-run identity &
// correlation model (docs/plans/terminal-product-exploitation-plan-2026-07-12.md
// §2.1a / §7, Phase 0 item S0b).
//
// An ephemeral PTY handle is not an Observer session id. A fresh (non-handoff)
// launch has no session id until the target tool creates one after startup, and
// hooks/transcript/proxy turns then arrive asynchronously and may disagree. This
// package mints a durable terminal-run identity at launch and scores the
// zero-or-more correlations from that run to the agent sessions it is later
// observed to have produced, so downstream features (cost, status, decorations,
// actions) attach to a run only once a correlation is established — never to a
// raw handle→session guess.
//
// Identity invariant (plan §2.1a): the source handoff session and any target
// session a run spawns are DISTINCT and must never be conflated. A Run carries
// the source session separately (SourceSessionID, handoff kind only); the
// correlated target sessions are Correlation rows.
//
// Module discipline (CLAUDE.md): this is pure logic — no database/sql, no
// net/http, no fsnotify (pinned by imports_test.go). The SQL seam lives at
// internal/store/termrun.go (the one owner of the two node-local tables); the
// application-service orchestration that mints runs at launch and feeds
// correlation observations lives in cmd's terminal service (Phase 1, F1). This
// package only knows how to mint ids, hash inputs with domain separation, and
// score a correlation from a set of observations — the decision logic is a
// data table (CLAUDE.md rule 5), not an if/else ladder.
package termrun
