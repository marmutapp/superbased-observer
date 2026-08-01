// Package crush implements a SQLite-store adapter for Charm's Crush TUI
// agent (charmbracelet/crush). Unlike every other SQLite-backed adapter,
// Crush has NO central session directory: each project keeps its own
// store at <project>/.crush/crush.db. The watch roots are therefore
// discovered from the global state file
// ~/.local/share/crush/projects.json (Windows %LOCALAPPDATA%\crush\),
// which maps each known project path to its data dir.
//
// The store shape (live-verified 2026-07-09 on WSL + Windows):
//
//   - sessions(id, prompt_tokens, completion_tokens, cost, updated_at,
//     created_at, …) — session-CUMULATIVE token counts plus a
//     pre-computed dollar `cost` (Crush is the only wave tool that
//     stores its own cost). Timestamps are Unix SECONDS despite the
//     schema comment claiming milliseconds — the update trigger writes
//     strftime('%s','now').
//   - messages(id, session_id, role, parts JSON, model, provider,
//     created_at, updated_at, …) — parts carry
//     text / reasoning / tool_call / tool_result / finish blocks.
//     tool_call and tool_result live in SEPARATE messages (assistant
//     vs. role="tool"), paired by tool_call id.
//
// Reasoning emission (B3, 2026-07-31): a `reasoning` part mints NO
// action row. Crush's thinking text rides the NEXT assistant-text or
// tool-call event as PrecedingReasoning, capped at the same 200-char
// preview the retired `crush.reasoning` row carried and scrubbed at the
// flush site. Consumption is grok-style — consumed-once (the first
// successor clears it), last-wins (a newer reasoning part replaces an
// unconsumed one), discarded at a user-prompt turn boundary. The state
// spans messages within one ParseSessionFile call because Crush writes
// the reasoning part and the tool_call it introduces on different rows.
//
// Token capture is session-level: one TokenEvent per session carrying
// the cumulative prompt/completion counts + Crush's own cost in
// TokenEvent.EstimatedCostUSD, with model+provider resolved from the
// NEWEST assistant message (so a bedrock→openai failover session
// reports the provider that actually finished the turn).
package crush
