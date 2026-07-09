// Package kirocli implements the SuperBased Observer adapter for AWS's
// Kiro CLI (`kiro-cli`, the rebranded Amazon Q Developer CLI;
// models.ToolKiroCLI).
//
// # Mode-dependent dual store
//
// Kiro CLI persists a session in ONE of two layouts depending on how
// the run was invoked (both live-verified 2026-07-09 on WSL Ubuntu +
// Windows 11). A single package parses both, dispatching on the file
// shape at IsSessionFile / ParseSessionFile time — the same
// one-package/multi-layout pattern antigravity uses for its `.pb`
// desktop store vs its plaintext-protobuf `.db` CLI store.
//
//	Layout 1 — interactive flat bundles.
//	  ~/.kiro/sessions/cli/<uuid>.json   full session state; when the
//	                                     turn accounting has been
//	                                     flushed it carries
//	                                     session_state.conversation_metadata
//	                                     .user_turn_metadatas[] with
//	                                     input/output_token_count,
//	                                     context_usage_percentage and
//	                                     metering_usage credits. A live
//	                                     or killed session's .json may
//	                                     LACK user_turn_metadatas and
//	                                     carry only the envelope keys —
//	                                     both shapes are tolerated.
//	  ~/.kiro/sessions/cli/<uuid>.jsonl  the append-only message stream
//	                                     ({"kind":"Prompt"|"AssistantMessage",
//	                                     "data":{message_id,content:[{kind:
//	                                     "text",data}],meta:{timestamp}}}).
//	  ~/.kiro/sessions/cli/<uuid>.history  raw input lines (ignored).
//	  ~/.kiro/sessions/cli/<uuid>.lock     lock sentinel (ignored).
//	  The .kiro/sessions/cli path is identical on every OS (Windows uses
//	  C:\Users\<u>\.kiro\sessions\cli, NOT %LOCALAPPDATA%).
//
//	Layout 2 — non-interactive SQLite.
//	  ~/.local/share/kiro-cli/data.sqlite3           (Linux/macOS)
//	  %LOCALAPPDATA%\Kiro-Cli\data.sqlite3           (Windows)
//	  table conversations_v2(key TEXT = RAW cwd string,
//	                         conversation_id TEXT,
//	                         value TEXT = full JSON conversation,
//	                         created_at INTEGER ms, updated_at INTEGER ms).
//	  The `value` JSON carries history[] of user/assistant turns plus
//	  env_context.env_state.current_working_directory. On Windows the
//	  `key` is a raw `C:\...` string — the KEY itself is crossmount-
//	  translated. The sqlite `conversations` (v1) and `history` tables
//	  are legacy shell-history — never read for chat.
//
// # Token honesty
//
// Kiro CLI authenticates to SigV4 CodeWhisperer endpoints, so the
// proxy CANNOT intercept its traffic — there is NO Tier-1 capture. The
// flat bundle's user_turn_metadatas carry input/output_token_count but
// they were observed to be 0 for every captured turn; the adapter emits
// those counts honestly (0 included) tagged unreliable rather than
// fabricating a value. The SQLite request_metadata carries token fields
// (total_tokens / uncached_input_tokens / output_tokens /
// cache_read_input_tokens / cache_write_input_tokens) but they were all
// null in every capture — no token event is emitted when they are null.
// The metering_usage "credit" values are kiro credits, NOT tokens and
// NOT US dollars; they are deliberately NOT stored (mapping them onto a
// TokenBundle or a USD cost column would be a fabrication).
//
// # Security
//
// The adapter enumerates ONLY conversations_v2 in the SQLite store. The
// auth_kv table (`kirocli:social:token`), the state table
// (`telemetry-cognito-credentials`, `telemetry-cognito-identity-id`,
// `api.codewhisperer.profile`, …) and the `.history` files are NEVER
// read.
package kirocli
