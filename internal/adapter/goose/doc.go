// Package goose implements a SQLite-store adapter for Block's goose agent
// (binary `goose`, Apache-2.0 — the CLI and the desktop app share one
// store). Live-verified against goose 1.41.0 on WSL 2026-07-09.
//
// # Storage
//
// Since goose v1.10 both the CLI and the desktop persist every session in
// a single WAL SQLite store:
//
//   - Linux/macOS: ~/.local/share/goose/sessions/sessions.db
//   - Windows:     %APPDATA%\Block\goose\data\sessions\sessions.db
//
// The watch root is the enclosing `sessions` directory, discovered across
// every cross-mount-resolved home so a WSL2 observer reaches a
// Windows-side store (and vice-versa). The WAL siblings
// (sessions.db-wal / -shm) are watched too and mapped back to the main
// .db; a foreign-mount source is mirrored locally before open because
// modernc.org/sqlite hits SQLITE_IOERR against a DrvFs path goose is
// actively writing.
//
// # Store shape (live-verified 2026-07-09)
//
//   - sessions(id, name, description, session_type, working_dir,
//     created_at, updated_at, total/input/output/cache_read/
//     cache_write_tokens, accumulated_total/input/output/cache_read/
//     cache_write_tokens, accumulated_cost, provider_name,
//     model_config_json, goose_mode, …). `id` is a `YYYYMMDD_seq` slug
//     (e.g. `20260709_4`), NOT a UUID. `working_dir` is the RAW OS path
//     (a `C:\…` string on Windows-side sessions). The plain token
//     columns are LAST-TURN values; the accumulated_* columns are SESSION
//     SUMS (monotonic) and are what this adapter emits.
//     `model_config_json` is a JSON object whose `model_name` is the
//     turn's model; `accumulated_cost` is goose's own dollar calc.
//   - messages(id INTEGER PK AUTOINCREMENT, message_id, session_id, role
//     (user|assistant), content_json, created_timestamp (epoch SECONDS),
//     timestamp, tokens, metadata_json). `id` is the monotonic
//     incremental watermark.
//
// # Session identity (store-scoped)
//
// goose session ids are date+sequence slugs generated INDEPENDENTLY per
// store, so two stores on one machine (a WSL store plus a Windows store
// reached via the foreign mount — the live-verified case) both contain a
// `20260708_1`. Emitted SessionIDs are therefore STORE-SCOPED:
// `<goose id>@<sha256(store path)[:8hex]>` (scopedSessionID), applied
// uniformly to native and foreign stores so a crossmount-classification
// change can never re-key sessions. Strip the `@…` suffix to recover
// goose's own id (e.g. for `goose session -r`).
//
// Each messages.content_json is a JSON ARRAY of MCP-shaped blocks:
//   - {"type":"text","text":"…"} — a user prompt (role=user) or the
//     assistant's narration (role=assistant).
//   - {"type":"toolRequest","id":"call_…","toolCall":{"status":…,
//     "value":{"name":…,"arguments":{…}}}} — an assistant tool call.
//   - {"type":"toolResponse","id":"call_…","toolResult":{"status":…,
//     "value":{"content":[{"type":"text","text":…}],"structuredContent":
//     {"stdout":…,"stderr":…,"exit_code":…},"isError":…}}} — the paired
//     tool RESULT, carried on a role=user message. Consumed into the
//     tool-call it answers, never emitted on its own.
//
// The grounded developer-extension tool names are `write` and `shell`;
// other names (text_editor, read, …) are mapped best-effort and fall
// back to ActionUnknown with the raw name preserved in RawToolName.
//
// # Token capture
//
// messages.tokens was NULL in EVERY 1.41.0 capture (even keyed runs), so
// token attribution is SESSION-LEVEL only — this adapter emits ONE
// approximate TokenEvent per session from the accumulated_* sums. goose's
// input_tokens is GROSS (it INCLUDES cache_read: a single-turn capture
// read input=3062 with cache_read=2944), so the net non-cached input is
// accumulated_input − accumulated_cache_read (clamped ≥0), matching the
// cost engine's NET-input contract. cache_write_tokens was NULL in every
// capture (OpenAI implicit cache reports no writes) and is tolerated
// everywhere. accumulated_cost is threaded as EstimatedCostUSD (goose
// computes it itself). Sessions persist token-EMPTY when the provider
// errors (all token columns NULL); those emit no TokenEvent (the user
// prompt is still recorded).
//
// # Not captured
//
// goose exposes no proxy tier this adapter drives (its base-URL knob is
// OPENAI_HOST, not OPENAI_BASE_URL — a launcher, not this package). It
// ships no hook receiver and no MCP writer here (its extensions ARE MCP
// servers, an orthogonal concept). ~/.config/goose/secrets.yaml (provider
// keys) and ~/.config/goose/config.yaml are NEVER read — everything the
// adapter needs is in sessions.db. reasoning tokens are not separately
// reported (goose folds them into output), so ReasoningTokens stays 0.
package goose
