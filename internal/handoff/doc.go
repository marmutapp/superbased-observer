// Package handoff is the pure-logic core of the session-handoff
// (continue-anywhere) feature: priced, cache-aware session portability
// between AI tools (docs/plans/session-handoff-plan-2026-07-03.md).
//
// It turns a pre-loaded SessionExtract into three things:
//
//   - a ForkResolution — where the transcript is cut, resolved through the
//     stable-boundary snap table (§7; default = last message);
//   - a HandoverDoc — the distilled, section-built handover payload (§8),
//     rendered to markdown or JSON;
//   - a MigrationEstimate — the per-carry-mode priced-transaction rows
//     (§9), with pricing injected as a PriceFunc.
//
// Module discipline (CLAUDE.md §1, pinned by imports_test.go): no
// database/sql, no net/http, no fsnotify, no os — transcripts and
// action-derived facts arrive pre-loaded from the store seam
// (internal/store/handoff.go) and the adapters' transcript readers;
// pricing arrives as a plain function. The single boundary caller is
// internal/handoffsvc.
package handoff
