// Package kimicode implements the adapter for Moonshot AI's kimi-code CLI
// (npm `@moonshot-ai/kimi-code`, binary `kimi`, MIT) — the TypeScript
// successor to the Python kimi-cli. See [models.ToolKimiCode].
//
// # Storage shape
//
// kimi-code persists one directory per session at
//
//	~/.kimi-code/sessions/wd_<slug>_<hash>/session_<uuid>/
//
// (the same `~/.kimi-code` layout on Linux, macOS, and Windows — not
// %APPDATA%). Each session directory holds `state.json` (metadata:
// createdAt/updatedAt/title/workDir/lastPrompt) plus one wire-protocol
// trace per agent at `agents/<name>/wire.jsonl` (the primary agent is
// `main`; sub-agents get their own name — the adapter marks non-main
// traces IsSidechain). A `~/.kimi-code/session_index.jsonl` at the state
// root maps `{sessionId, sessionDir, workDir}` and is NOT watched (the
// per-session state.json is the closer project-root source).
//
// The wire trace is a flat JSONL event stream discriminated by `type`.
// The adapter consumes:
//
//   - metadata      — protocol_version + created_at (session-start marker)
//   - turn.prompt   — a user prompt (`input[].text`, `origin.kind:"user"`;
//     injected `origin.kind:"injection"` reminders are skipped)
//   - llm.request   — carries the clean per-call model id ("gpt-4o")
//   - usage.record  — the per-API-call token envelope (see Token capture)
//   - context.append_loop_event — a wrapper whose `event.type` is one of
//     step.begin / tool.call / tool.result / content.part / step.end. The
//     adapter emits tool events from tool.call (stamped by the paired
//     tool.result) and assistant text from content.part; step.begin/end
//     are structural.
//
// config.update (system-prompt bodies), tools.set_active_tools,
// permission.set_mode, llm.tools_snapshot, and the duplicate
// context.append_message rows are intentionally ignored.
//
// # Token capture
//
// Per-call token usage is read from `usage.record` events (Tier 2 /
// approximate). Each carries {inputOther, output, inputCacheRead,
// inputCacheCreation}. inputOther is the NET non-cached input — it
// EXCLUDES the cache read (verified 2026-07-09: a step whose
// inputCacheRead was 18816 reported inputOther 55, so inputOther cannot
// contain the cached portion). No netting is therefore required to reach
// the cost engine's NET-input contract: inputOther → InputTokens,
// inputCacheRead → CacheReadTokens, inputCacheCreation →
// CacheCreationTokens, output → OutputTokens. The wire format emits NO
// reasoning/thoughts split, so ReasoningTokens stays zero (honest gap);
// live gpt-4o runs carry no reasoning tokens anyway. The identical usage
// object also rides each step.end loop event — tokens are emitted only
// from usage.record so the two sources cannot double-count. A fully-zero
// usage event is skipped rather than persisted as a phantom row.
//
// The per-turn model is resolved live and NEVER hardcoded: llm.request
// carries the clean id ("gpt-4o"), usage.record carries a provider-
// prefixed id ("openai/gpt-4o"). Both are normalized (leading `provider/`
// stripped, trailing `:tag` trimmed) so the emitted id matches the cost
// engine's pricing keys. Live installs resolved to gpt-4o via an
// openai-compat provider configured in `~/.kimi-code/config.toml`.
//
// # Project root
//
// The `wd_<slug>_<hash>` directory name encodes only a slug, not the full
// path, so the adapter reads the authoritative cwd from the session-root
// `state.json.workDir` (falling back to a tool.call display `cwd` hint),
// runs it through crossmount.TranslateForeignPath (so a Windows
// `C:/Users/...` session — kimi-code records FORWARD-slash Windows paths,
// which the translator handles natively — maps to `/mnt/c/...` on a WSL2
// observer), then git.Resolve for the working-tree root + branch.
//
// # Security
//
// The adapter reads only `agents/*/wire.jsonl` traces and the metadata-
// only `state.json`. It NEVER reads `~/.kimi-code/config.toml`, which
// holds a plaintext provider API key (world-readable 0644 observed
// 2026-07-09), nor the `credentials/` / `oauth/` directories. All raw
// text (prompts, tool inputs, tool outputs, error bodies) passes through
// the injected scrub.Scrubber before it leaves the adapter.
package kimicode
