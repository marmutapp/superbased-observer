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
// Project root resolution falls back through:
//   - tool-call cwd from any captured turn (most reliable)
//   - shadow-git config at ~/.gemini/history/<hash>/.git/config
//   - synthetic key "[gemini-cli:<hash>]" (promoted later via
//     ON CONFLICT DO UPDATE on sessions.project_id when a future scan
//     surfaces a real cwd for the same hash)
//
// Subagent nested sessions (spec extension to gemini-cli's session
// management) are explicitly rejected by IsSessionFile and warned —
// they're a deferred feature, not a silent skip.
package gemini
