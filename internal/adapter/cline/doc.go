// Package cline implements the Adapter interface for Cline and Roo Code
// (VS Code extensions). Both use the same api_conversation_history.json
// format. See spec §4.4. Implemented in Phase 2.
//
// A `thinking` / `redacted_thinking` block mints no row: it accumulates
// into the per-message reasoning buffer and reaches the timeline as
// PrecedingReasoning on the tool_use rows that follow it in that message
// (FAN-OUT — one thought can precede several calls and each carries it).
// The posture holds for every retag of this parser (cline / roo-code /
// legacy kilo-code). See
// docs/plans/b3-reasoning-convergence-plan-2026-07-31.md §1.
package cline
