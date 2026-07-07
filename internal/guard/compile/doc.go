// Package compile translates the guard's effective policy into each
// AI client's NATIVE permission-rule dialect (guard spec §13.2 — the
// moat feature): one policy definition becomes a native rule the
// client enforces even when the observer daemon is down, plus the
// hook verdict, plus the audit row.
//
// # Purity contract (spec §17 / CLAUDE.md module rule 1)
//
// This package performs NO I/O: translators consume the effective
// rule table ([]policy.RuleInfo) and existing config bytes, and
// return updated bytes. Reading and writing the native config files,
// pinning compiled artifacts (store guard_pins kind=native_dialect)
// and emitting R-204 drift events all live at the cmd layer
// (cmd/observer/guardcompile.go).
//
// # Translation discipline (table-driven, spec §13.2 / module rule 5)
//
// Per implemented dialect, a data table records ONE row per built-in
// catalog rule ID: compilable yes (FidelityExact) / as an
// approximation (FidelityApprox, Note documents exactly how the
// native entry's scope differs) / not at all (FidelityNone, Note
// documents why). Lossy translations are never silent — the Note is
// part of the row, and `observer guard compile --diff` prints it.
//
// Two safety rules bound every approximation:
//
//   - A native entry may only land under the "deny" action when it
//     matches a SUBSET (or the exact set) of what the policy rule
//     denies. An entry broader than the rule (e.g. Bash(rm -rf:*) for
//     R-101's outside-project-only trigger) is demoted to "ask" —
//     native over-blocking would break workflows the policy
//     deliberately allows.
//   - A rule's effective enforce decision caps its entries: an
//     operator override deny→ask caps deny entries to ask, and a rule
//     whose effective decision is flag/allow emits nothing (native
//     dialects have no record-but-allow action). Entries are never
//     escalated above their table action.
//
// # Ownership of managed entries
//
// Compiled entries are recognised by VALUE: an entry present in the
// dialect's translation-table universe is treated as observer-managed
// (the register.go content-heuristic precedent). Apply adds wanted
// entries, removes universe entries the current policy no longer
// wants, and never touches anything else — user-authored entries
// survive every pass (the never-clobber hygiene). Corollary,
// documented: a user-authored entry that exactly matches a table
// value is indistinguishable from ours and will be managed.
//
// # Drift semantics (R-204)
//
// Drift means the native config is MISSING entries the current policy
// compiles to — whether because the file was edited away from policy
// or because policy changed after the last compile. Stale managed
// entries (present but no longer wanted) make the native layer
// STRICTER than policy; they are reported by --diff but do not fire
// R-204. The check verifies entry presence, not surrounding
// precedence — a user entry that out-precedences a managed entry in
// an order-sensitive dialect is not detected (documented
// approximation). Unparseable config files surface as issues, never
// as R-204 events (a JSONC-styled or hand-broken file must not alarm
// forever).
//
// # v1 dialect scope (operator decision Q4: never assume semantics)
//
// Implemented: Claude Code (permissions.deny/ask arrays in
// ~/.claude/settings.json — deny-first aligns natively) and OpenCode
// (permission.bash pattern map in ~/.config/opencode/opencode.json —
// LAST-match-wins, so managed entries are emitted AFTER user entries;
// this inversion vs. every first-match dialect is exactly why the
// compiler is valuable). The other five spec §13.2 targets (Cursor,
// Windsurf, Amp, Gemini, Codex) are declared in the target table as
// deferred, each with its reason recorded as data — semantics are
// researched before they are promised, never assumed.
package compile
