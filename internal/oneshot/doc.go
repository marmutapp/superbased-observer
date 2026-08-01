// Package oneshot implements the pure window-parsing, honesty-note, and
// rendering logic behind `observer usage` — the zero-config, zero-network,
// one-shot cost report described in
// docs/plans/npx-one-shot-report-plan-2026-07-30.md.
//
// This package is PURE (CLAUDE.md "Module Boundaries" #1): it holds no
// database/sql, net/http, os/exec, or github.com/fsnotify/fsnotify import,
// and it does not import internal/store, internal/db, or
// internal/intelligence/cost — imports_test.go pins the forbidden set. All
// I/O (opening a scratch database, walking the filesystem, running the
// cost-engine rollup) happens at the cmd/observer/usage.go boundary; that
// seam maps a cost.Summary into the plain Table / Row / Note value types
// defined here (table.go) — the single place a cost.* type is translated
// away (CLAUDE.md #2, one seam, no type leakage). oneshot DOES import
// internal/integration: a pure data package by design (Capability rows),
// exactly the dependency Notes needs.
//
// The three entry points:
//
//   - Window (window.go) parses a --since spec ("7d"/"30d"/"90d"/"all"/
//     RFC3339, "" defaulting to 30d) into a since time.Time and a
//     human-readable window label.
//   - Notes (notes.go) derives the honesty footer lines — no local token
//     source, a tool's known capture gap, unpriced models, a
//     budget-truncated scan, an empty corpus — strictly from
//     internal/integration.Capability data plus the scan/cost-engine facts
//     passed in as State. Nothing here is fabricated per tool name
//     (CLAUDE.md #3): a capability-derived note only exists when the
//     matching Capability value carries it.
//   - Render (render.go) turns a Table into the exact terminal design in
//     the plan's §1.4: an aligned tool×model table, a TOTAL line, and a
//     fixed honesty footer (reliability/tier, "estimated list price, not
//     invoiced", the proxy upsell, and the conditional Notes) — or, for an
//     empty corpus, just the friendly empty-state line.
//
// Every dollar this package renders carries PriceBasis ("estimated list
// price, not invoiced"); the renderer never shows a compression/savings
// percentage of any kind (the retracted-claim class the accuracy-check CI
// gate forbids — see docs/security.md and CLAUDE.md's honesty rules).
package oneshot
