// Package grok implements the SuperBased Observer adapter for xAI's Grok
// Build terminal agent (binary `grok`, models.ToolGrok) — the closed-
// source, SuperGrok/X-Premium+ (or XAI_API_KEY) gated coding agent xAI
// shipped in 2026. Distinct from the community `grok-cli` repos on
// npm/GitHub, which are NOT xAI.
//
// # Two-source capture model
//
// Grok persists each session as an 8-file bundle under a percent-encoded,
// cwd-keyed directory:
//
//	~/.grok/sessions/<url-encoded-cwd>/<uuid>/
//	  chat_history.jsonl   the conversation (OpenAI-Responses-shaped)
//	  updates.jsonl        the ACP session-update stream  ← ToolEvent source
//	  events.jsonl         lifecycle telemetry (phase/turn markers)
//	  summary.json         model + git_root_dir + head_branch  ← metadata seam
//	  rewind_points.jsonl / prompt_context.json / signals.json / system_prompt.txt
//
// The adapter watches ONE bundle file for ToolEvents — updates.jsonl, the
// Agent Client Protocol (ACP) stream. Every line is a JSON-RPC
// notification (`session/update` or the vendor extension
// `_x.ai/session/update`) whose `params.update.sessionUpdate` discriminates
// the variant: user_message_chunk, agent_thought_chunk (→ carried as
// PrecedingReasoning), agent_message_chunk, tool_call, tool_call_update
// (the terminal status + output), hook_execution and turn_completed. Each
// line carries a stable `_meta.eventId` (`<sessionId>-<n>`) used verbatim
// as the deterministic SourceEventID, plus a millisecond agentTimestampMs.
//
// # Tokens: unified.jsonl, correlated by sid
//
// The session bundle's updates.jsonl carries only a cumulative
// `_meta.totalTokens` watermark — no split, no cache, no reasoning. The
// accurate per-request splits (prompt / cached_prompt / completion /
// reasoning) live in the GLOBAL diagnostic log ~/.grok/logs/unified.jsonl,
// on the `shell.turn.inference_done` lines. Those lines ALSO carry a `sid`
// (session id), so the adapter correlates per-request tokens to their
// session WITHOUT any timestamp heuristic — unified.jsonl wins as the token
// source. The adapter watches unified.jsonl as a second file shape and
// emits TokenEvents keyed by sid; the owning session's model + project
// root + branch are resolved from the sibling summary.json (unified.jsonl
// records no per-turn model). grok's prompt_tokens is GROSS (includes
// cached_prompt_tokens, OpenAI convention — verified live 2026-07-09), so
// the net non-cached input is prompt−cached; reasoning_tokens bill at the
// output rate.
//
// session_search.sqlite under ~/.grok/ is an FTS search index only — a
// decoy, never parsed.
//
// # Project root, model, branch
//
// summary.json is the metadata seam: current_model_id (model), git_root_dir
// (primary project root — already a git worktree root), info.cwd (fallback
// cwd, also decodable from the percent-encoded dir name), and head_branch.
// Windows sessions store raw C:\ paths; foreign-OS paths are translated +
// stat-gated before git.Resolve so the observer's own repo root is never
// prefixed on.
//
// # Handoff + capture gaps
//
// ReadTranscript / ReadTranscriptFull re-read chat_history.jsonl for the
// session-handoff transcript tier (grok has no proxy tier). Grok also has a
// positional seed lane (`grok "<prompt>"` opens a seeded session), an ACP
// hook mechanism, and an MCP client — surfaces the orchestrator wires.
//
// Known limitation: the live capture used grok's default read-only
// grok-build-plan agent, so no tool-execution events were observed; the
// write/edit/run tool vocabulary is mapped defensively from grok's
// documented names and falls through to ActionUnknown honestly. A
// tool-exec capture is still owed (operator, with a non-plan agent).
package grok
