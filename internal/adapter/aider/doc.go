// Package aider implements a session adapter for Aider (pip
// `aider-chat`, Apache-2.0). Aider is unusual among the supported tools:
// it has NO central session directory and NO global project index. Each
// git repository keeps its own append-only Markdown transcript at the
// repo root:
//
//   - <repo>/.aider.chat.history.md  — the chat transcript (parsed here)
//   - <repo>/.aider.input.history    — user inputs only (NOT claimed; see
//     the package README for the rationale)
//   - <repo>/.aider.tags.cache.v4/   — a repo-map cache (NEVER claimed)
//
// Global ~/.aider/ holds only analytics.json / installs.json / a model-
// price cache — no session index — so there is nothing to enumerate the
// way the crush adapter reads projects.json. Watch-root discovery is
// therefore a bounded, best-effort filesystem walk of the NATIVE home
// looking for `.aider.chat.history.md` files (see discover.go; roots are
// the transcript FILE paths, memoized once per process; foreign
// cross-mount homes are skipped — a DrvFs walk costs minutes), and
// the adapter is scan/backfill-primary: `observer scan`/`observer
// backfill --all` walk the discovered roots and re-ingest. See
// docs/aider-adapter.md for the discovery/dispatch story and its honest
// limitations, plus the recommended shared-seam fix (feeding adapters
// the store's known project roots).
//
// Transcript shape (ground-truthed 2026-07-09 against a live v0.86.2
// session + the aider source, coders/base_coder.py + repo.py):
//
//   - Session delimiter: an H1 line `# aider chat started at
//     YYYY-MM-DD HH:MM:SS`. One file concatenates many sessions.
//   - The session opens with a block of blockquote (`> `) tool-output
//     lines that echo the argv, aider version (`> Aider v0.86.2`), the
//     model (`> Main model: gpt-4o with diff edit format` /
//     `> Using gpt-4o model with API key from environment.`), the git
//     repo, and repo-map notices.
//   - User prompts are H4 lines (`#### <text>`); a multi-line prompt is
//     several consecutive `#### ` lines.
//   - Assistant replies are plain Markdown prose (no prefix).
//   - Tool actions surface as blockquote lines: `> Applied edit to
//     <path>` (edit_file), `> Running <command>` (run_command).
//   - Per-turn usage is prose only: `> Tokens: 11k sent, 21 received.
//     Cost: $0.03 message, $0.03 session.` (optional cache-write /
//     cache-hit segments; Cost omitted when the model has no pricing).
//     `sent`/`received` are PER-MESSAGE and ROUNDED by aider's
//     format_tokens (`<1000` verbatim, `<10000` one-decimal `k`,
//     else rounded `k`), so token counts are reliability=unreliable;
//     `sent` is GROSS (includes the cache hit) and is netted at emit
//     time. `message` cost is per-turn; `session` cost is cumulative.
//
// The Markdown is lossy relative to a JSONL transcript: there are no
// per-turn timestamps (every event in a session carries the session
// start time), and assistant prose that itself contains `> ` blockquotes
// or `#### ` headings can be misclassified. These limitations are
// documented rather than hidden. Because the parser is deterministic and
// every event carries a stable (sessionKey, sequence)-derived
// SourceEventID, ParseSessionFile re-reads the whole file on every call
// (watermarking only on file size) and relies on the store's
// (source_file, source_event_id) dedup for idempotency.
package aider
