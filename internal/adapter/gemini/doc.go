// Package gemini parses session logs written by Google's Gemini CLI
// (`@google/gemini-cli`), the Node.js terminal AI agent. It is unrelated
// to Google Antigravity (the IDE) despite the shared `~/.gemini/`
// parent directory — different binary, different storage, different
// data format. See internal/adapter/antigravity for that one.
//
// Sessions live under ~/.gemini/tmp/<project_hash>/chats/ and use one
// of two formats:
//
//  1. Legacy single-object JSON: `session-YYYY-MM-DDTHH-mm-<id>.json`
//     with the entire conversation rewritten on every turn append.
//     Token-count updates can change digits in-place without changing
//     file size, so cursor logic uses size+mtime+content-hash rather
//     than byte offset.
//  2. Append-only JSONL (gemini-cli issue #15292, expected ≥0.10):
//     `session-...jsonl` with one event record per line. Cursor uses
//     byte offset.
//
// The adapter dispatches on extension. Both produce normalized
// ToolEvent / TokenEvent records under Tool=gemini-cli.
//
// # Message shapes
//
// Orthogonal to the file format, an assistant message encodes its tool
// calls and reasoning in one of TWO shapes, and the adapter reads both
// (normalizedCall is the boundary type they resolve to):
//
//  1. content-parts (legacy): `functionCall` and `thought` entries live
//     inside the message's `content` array; the tool RESULT arrives
//     later as a `functionResponse` part, joined by call id.
//  2. top-level arrays (LIVE — every build observed since 2026-06):
//     `content` is the empty string and the message carries sibling
//     `toolCalls[]` / `thoughts[]` arrays. Each toolCalls entry embeds
//     its own result, status, and timestamp, so the row lands complete.
//
// Reading only shape 1 was WP-T6 finding G1: gemini-cli had never
// recorded a single tool call or reasoning row.
//
// The live CLI also APPENDS the same assistant message twice — once
// when its text/thoughts land, again once its toolCalls resolve — so
// per-message SourceEventIDs key on the message id, never on the line
// number (see messageKey), or every assistant message doubles.
//
// # Incremental-parse contract
//
// The JSONL path is fed a byte offset by the watcher and returns the
// offset to persist, so it must be correct across a parse that lands
// mid-append. Three rules hold it together:
//
//   - Only a '\n'-TERMINATED record may move the cursor. gemini-cli
//     writes one terminated JSON object per record (verified across the
//     whole live corpus), so an unterminated trailing record is by
//     construction an unfinished append: it is deferred WHOLE and
//     re-read next pass. JSON validity is NOT the discriminator — a
//     partial record whose prefix happens to parse is the more
//     dangerous face of the same bug. A terminated-but-corrupt interior
//     record IS skipped, so one bad line can't wedge the file.
//   - Tool RESULTS may cross a parse window. In the live shape they
//     cannot be lost — the `toolCalls` array is written only once the
//     calls RESOLVE and every entry embeds its own result, making the
//     follow-up user-role `functionResponse` record redundant (17/17
//     calls across the live corpus). In the legacy content-parts shape
//     the result lives on a SEPARATE later record, so when no in-batch
//     call matches, joinResponse emits an ActionOutcomeUpdate keyed by
//     the call's own (SourceFile, SourceEventID) instead of dropping
//     the output.
//   - A call's Success is only as good as its `status`. Terminal
//     failures carry an ErrorMessage — the store's success 1 → 0
//     self-heal is gated on that evidence, so without it a wrong row
//     could never be corrected. Non-terminal statuses (gemini-cli's
//     validating / scheduled / executing / awaiting_approval) set
//     OutcomePending so an optimistic Success=true is never filed as a
//     measured outcome.
//   - The LEGACY shape carries no `status` at all, so its ONLY failure
//     signal is the `error` key inside the functionResponse body. Both
//     join branches read it through the shared responseErrorText helper
//     and, when a meaningful error is present, record the failure —
//     in-batch by mutating Success/ErrorMessage on the row, cross-batch
//     by setting SuccessKnown+Success+ErrorMessage on the outcome
//     update. A response body with no `error` key leaves SuccessKnown
//     false and touches nothing but the output: a record that reports
//     no verdict must not manufacture one.
//
// # Reasoning
//
// The model's thought/thoughts content is carried ONLY as
// PrecedingReasoning on the tool calls of its own assistant message
// (fan-out, 200-char preview). It is never a row of its own — a
// reasoning block is not an action (docs/plans/
// b3-reasoning-convergence-plan-2026-07-31.md §1).
//
// Project root resolution falls back through:
//   - tool-call cwd from any captured turn (most reliable; the live
//     JSONL carries no cwd at all, so this tier rarely fires)
//   - the CLI's own `.project_root` sidecar under
//     ~/.gemini/tmp/<key>/ or ~/.gemini/history/<key>/
//   - ~/.gemini/projects.json, inverted from {root: key} (skipped when
//     two roots share a key)
//   - shadow-git config at ~/.gemini/history/<hash>/.git/config
//   - synthetic key "[gemini-cli:<hash>]" (promoted later via
//     ON CONFLICT DO UPDATE on sessions.project_id when a future scan
//     surfaces a real cwd for the same hash)
//
// Subagent nested sessions (spec extension to gemini-cli's session
// management) are explicitly rejected by IsSessionFile and warned —
// they're a deferred feature, not a silent skip.
package gemini
