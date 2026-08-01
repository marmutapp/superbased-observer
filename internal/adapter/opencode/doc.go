// Package opencode implements a snapshot-style adapter for OpenCode desktop
// state persisted in opencode.db. The adapter treats assistant-message
// message.data.tokens as the authoritative token source, because OpenCode's
// step-finish parts duplicate that bundle exactly while older session-level
// aggregate columns can be stale or zero.
//
// # Reasoning
//
// `reasoning` parts are NEVER rows of their own. loadReasoningIndex
// resolves each one onto the successor part it precedes within the same
// message (consumed-once, last-wins, message-partitioned — grok's
// reference semantics), and the body lands on that row's
// PrecedingReasoning, beating the part `title` that used to occupy the
// column. See docs/plans/b3-reasoning-convergence-plan-2026-07-31.md §1.
package opencode
