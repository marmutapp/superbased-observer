// Package muse parses Meta's Muse Code CLI session logs.
//
// # The tool
//
// Muse Code ("muse") is Meta's agentic coding CLI — a statically linked
// Rust binary (`fbcode/musecode`, internal crate prefix `tbh_`) fronted by
// a self-updating Bourne-shell launcher at `~/.local/bin/muse` that execs
// `~/.local/bin/muse-bin-<version>`. Phase-0 grounding ran against
// `Muse Code 0.1.0 (0.1.0-R708.1)` on Linux x86_64 (2026-08-06).
//
// # Storage layout
//
// Everything is XDG-shaped, on every platform Muse ships for
// (aarch64/x86 Linux and macOS — there is no Windows build):
//
//	$XDG_CONFIG_HOME|~/.config/muse/
//	    settings.json          schema_version / provider / model
//	    auth.json              OAuth + Model API credentials  (NEVER READ)
//	    trust.json             per-project trust decisions    (NEVER READ)
//	$XDG_DATA_HOME|~/.local/share/muse/
//	    tui-history.jsonl      raw user input history         (NEVER READ)
//	    plugins/ skills/       bundled plugin + skill caches  (not parsed)
//	    sessions/YYYY/MM/DD/<session-uuid>/
//	        session.jsonl      THE session log — the only file parsed
//	        cron.db            scheduled-task SQLite          (not parsed)
//	        .session.lock      pid lock                       (not parsed)
//	        tool-outputs/      spooled oversized tool bodies  (not parsed)
//	        subagent/<child-uuid>/session.jsonl   child-agent logs
//
// The date-sharded `sessions/YYYY/MM/DD/` tree is the same shape codex
// already uses, so the watcher's existing recursion (addRecursive at start,
// addIfDir on a Create event, plus the periodic full-tree rescan) covers new
// day directories with no adapter-side work.
//
// # Off-limits files
//
// This adapter reads `session.jsonl` and nothing else. In particular it
// NEVER opens:
//
//   - `~/.config/muse/auth.json` — OAuth tokens + the minted Model API key
//     (the clinecli `secrets.json` / commandcode `auth.json` precedent).
//   - `~/.config/muse/trust.json` — the operator's per-project trust grants.
//   - `~/.local/share/muse/tui-history.jsonl` — a flat JSON-string-per-line
//     log of RAW user keystrokes across every project, i.e. exactly the
//     cross-project raw-prompt store clinecli refuses to read
//     (`user_input_history.jsonl`). The prompts this adapter DOES record
//     come from the session log's own `started` events, scoped to the
//     session and scrubbed.
//   - `<session>/cron.db` and `<session>/tool-outputs/` — scheduled-task
//     state and spooled tool bodies; neither is needed and the latter is
//     bulk content by definition.
//
// # Record shape (schema_version 1)
//
// The log is an append-only EVENT-SOURCED stream, not a chat transcript.
// Every line is one record envelope:
//
//	{"schema_version":1,"id":"<uuid>","stream":{"kind":"session","id":"<session-uuid>"},
//	 "sequence":89,"recorded_at":1785962540739784,"record_type":"event",
//	 "durability":"durable","causation_id":null,
//	 "payload_type":"runtime.session","payload_schema_version":1,"payload":{…}}
//
// `recorded_at` is MICROSECONDS since the Unix epoch (16 digits) — not
// millis, not seconds. parseTimestamp is unit-detecting so a future
// schema that switches units cannot silently produce 1970 timestamps.
//
// A second, much rarer line shape carries no payload at all: a
// `retained_marker` tombstone recording that an ephemeral record was
// omitted from the durable log (`{"retained_marker":"omitted_live_only",
// "stream":…,"position":…,"omitted_record":{…}}`, 7 of 448 lines in the
// Phase-0 capture). These are skipped SILENTLY — warning on them would
// flood the watcher log on every session (§4.4e).
//
// `payload_type` discriminates the record. The ones this adapter consumes:
//
//	runtime.session                    the workhorse; payload.event.kind
//	                                   further discriminates (30 kinds seen)
//	runtime.session.metadata           payload.record.workspace_root — the
//	                                   ONLY statement of the project root
//	session.opened.observed            payload.record.session_id / resume
//	session.workspace_branch.observed  payload.record.reference.name (branch)
//	session.end                        payload.record.exit_reason
//	tool_batch.effect.terminal         payload.record.{call_id,outcome.kind}
//	                                   — the per-tool-call SUCCESS verdict
//
// Every other payload_type (reminder.*, run.model.configured, …) is
// informational and skipped silently.
//
// Inside `runtime.session`, `payload.event.kind` is the real discriminator.
// The five kinds that produce rows:
//
//	started                          run-level; carries `prompt` (task-level
//	                                 `started` events carry a task_id and no
//	                                 prompt). In the PARENT log the prompt is
//	                                 what the operator typed → user_prompt;
//	                                 in a CHILD log it is the harness's own
//	                                 sub-agent seed → subagent_start
//	assistant_message_committed      `text` = the assistant's visible reply
//	assistant_tool_calls_committed   `tool_calls[]` = {name, call_id, id,
//	                                 args} where args is a JSON STRING
//	tool_result_batch_committed      `results[]` = {tool_call_id,
//	                                 tool_call_index, text}
//	model_completed                  `model` + `usage` → the token row
//	terminal                         terminal:"cancelled" → turn aborted
//
// # Tokens (Tier 2, jsonl / approximate)
//
// `model_completed.usage` is the only token source; there is no proxy lane
// (see the routability note below). Its six fields, and what they mean:
//
//	input_tokens        GROSS  — INCLUDES cache_read_tokens
//	cache_read_tokens   the cached prefix replayed this turn
//	cached_tokens       a duplicate of cache_read_tokens in every observed
//	                    row; used only as a fallback
//	cache_write_tokens  cache-creation tokens (zero in every observed row)
//	output_tokens       GROSS  — INCLUDES reasoning_tokens
//	reasoning_tokens    the model's internal thinking subset
//
// Both gross fields are netted at emit time, because [models.TokenEvent]'s
// contract (and cost.ComputeBreakdown) bills Input and CacheRead
// separately, and bills Reasoning ADDITIVELY at the output rate on top of
// Output.
//
// The GROSS-input evidence is arithmetic across the nine model_completed
// rows of the Phase-0 session: turn 2 reports input=15924 with
// cache_read=15665 immediately after turn 1's input=15719 / output=101.
// Read as NET, turn 2's prompt would be 15924+15665 = 31589 tokens — an
// impossible ~2× jump from a 101-token reply plus a one-line user message.
// Read as GROSS it is the textbook prompt-cache replay: 15924 ≈ 15719 + 101
// + the new user turn, with 15665 of it served from cache. Every later turn
// repeats the pattern (cache_read of turn N ≈ input of turn N-1).
//
// The reasoning ⊂ output relation follows the OpenAI Responses convention
// this backend visibly speaks (`resp_…` response ids, `rs_…`/`fc_…`/`msg_…`
// item ids, an `encrypted_content` reasoning item) — the same convention the
// codex adapter nets against, for the same reason. It is corroborated here:
// the first turn reports output=101 with reasoning=84 for the visible reply
// "Hi — how can I help?", which is ~7 tokens of prose; disjoint counts would
// make that reply 101 output tokens.
//
// # Model
//
// `model_completed.model` is the per-call model id (`muse-spark-1.2-contributor`
// in the Phase-0 capture; provider_id `meta`). Observer ships NO pricing
// entry for Muse models — Meta publishes no per-token rate card for the Muse
// subscription — so cost rows resolve as `unknown` by design rather than
// being invented. `run.model.configured` / `runtime.model_reconfigure.completed`
// carry the same id and are used only as a fallback for a token row that
// somehow omits it.
//
// # Project root
//
// `runtime.session.metadata.record.workspace_root` (record sequence 1, the
// first line of every session log) is the authoritative absolute cwd. The
// directory name in the path is a session UUID and carries no cwd
// information at all, so the header is re-read on EVERY parse — including a
// resumed one — the same way commandcode re-reads its session header.
//
// # Sub-agent logs
//
// A session's `subagent/<child-uuid>/session.jsonl` files are parsed too:
// they carry their own `model_completed` rows whose tokens appear NOWHERE in
// the parent log (verified across all 15 child logs of the Phase-0 session),
// so skipping them would silently undercount. They are attributed to the
// PARENT session id — one canonical session id per session tree (§4.5a) —
// with [models.ToolEvent.IsSidechain] set, mirroring how claudecode folds
// sidechain lines into the parent session. A child log has no
// `runtime.session.metadata` record of its own, so its project root is read
// from the parent's `session.jsonl` (a derived sibling path, symlinks
// refused, read at most once per parse).
//
// # Two findings only the live re-parse produced
//
// Both were invisible to the fixtures and were caught by the checklist §21
// "sample of population, not sample of shape" re-parse of the whole live
// tree:
//
//  1. `submit_reminder_decision` is a real 15th native tool name — the
//     reminder observer's verdict submission. It appears ONLY in child logs
//     (13 calls across 15) and nowhere in the parent, so a parent-only
//     reading of the tool surface misses it entirely. It writes a decision
//     back into the harness and touches no workspace state, hence
//     ActionHarnessCall.
//  2. A child run's `started` prompt is machine-authored. Typing it as
//     user_prompt inflated the grounding session from 3 real prompts to 18,
//     and would have corrupted every surface that counts user-message
//     BOUNDARIES — internal/predict's turns-per-message ladder above all,
//     whose entire unit is "one user message".
//
// # Known gaps
//
//   - Reasoning TEXT is unavailable: `reasoning_committed.text` is always
//     empty and the content sits in `encrypted_content`, an opaque
//     provider-sealed blob. PrecedingReasoning is therefore never populated;
//     the reasoning TOKEN COUNT is still captured.
//   - No hook receiver. The binary carries a Claude-Code-derived hook
//     vocabulary (user_prompt_submit / pre_tool_use / post_tool_use /
//     pre_llm_call / post_llm_call / pre_compact / post_compact /
//     subagent_start / subagent_stop / permission_request) in
//     `settings.json`, but the exact on-disk matcher schema is not grounded
//     and no receiver is wired, so the integration registry declares
//     HookNone rather than a capability observer cannot deliver.
//   - No proxy lane. Model traffic goes to `https://api.meta.ai/v1`, whose
//     base URL is MINTED BY THE LOGIN FLOW ("unrecognized Model API base URL
//     from login; using the default") and authenticated with a Model API
//     key. `TBH_AUTH_BASE_URL` / `TBH_MINT_BASE_URL` steer only the auth and
//     mint endpoints, not model traffic. A settings.json endpoint/transport
//     block with `proxy` and mTLS fields does exist, but its schema is
//     ungrounded — hence Routability probe_required, Proxy nil.
package muse
