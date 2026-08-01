// Package cursor implements the Adapter interface for Cursor (hook-based
// capture; no native structured logs). See spec §4.3. Implemented in Phase 3.
//
// Because each hook event arrives in its own short-lived process, the
// afterAgentThought reasoning is carried to its successor through a tiny
// on-disk stash rather than in-memory parser state — see pending.go. It
// never becomes a row of its own. See
// docs/plans/b3-reasoning-convergence-plan-2026-07-31.md §1.
package cursor
