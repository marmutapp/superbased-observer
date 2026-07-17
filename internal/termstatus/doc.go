// Package termstatus is the pure agent-status classifier (terminal-product-
// exploitation plan F4 — the flagship gap-closer for claude-code #36885/#13024,
// "which of my running agents is blocked on input?").
//
// It FUSES independent signals into one honest status:
//   - trusted lifecycle (process exited, out-of-band launcher signals, hook
//     events) — the anchor;
//   - untrusted PTY hints (OSC 133/633 prompt marks, BEL, output recency) — the
//     early-warning layer, weighted below trusted evidence (§2.1b: screen /
//     OSC-derived state never authorizes and never outranks a trusted signal).
//
// Classification is a table walked top-down, one row per case (CLAUDE.md rule
// 5), ordered by confidence. It NEVER emits a wrong label: when evidence is
// weak or contradictory it returns StatusUnknown, and every Result carries the
// evidence basis + its age so the UI presents a hint as a hint, never a fact.
//
// Pure: no SQL, no HTTP, no fsnotify (pinned by imports_test.go). The signals
// are collected at the boundary (the terminal application service) and passed
// in; this package only decides.
package termstatus
