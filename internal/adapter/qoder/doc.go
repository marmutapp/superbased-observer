// Package qoder implements the adapter for Qoder CLI (closed source;
// binary `qoder`/`qodercli`, PAT auth against api.qoder.com). The CN
// edition under ~/.qoder-cn/ is a SEPARATE tool and out of scope.
//
// # Storage shape
//
// Qoder persists one Claude-Code-shaped JSONL transcript per session at
//
//	~/.qoder/projects/<dash-sanitized-cwd>/<uuid>.jsonl
//
// with an encrypted per-session store beside it
// (<uuid>/state.json and <uuid>/compression-v2/state.json) and a verbose
// run log at
//
//	~/.qoder/logs/sessions/<dash-sanitized-cwd>/<sid>/segments/<ts>-<rand>-p<pid>.jsonl
//
// Each transcript line is one record carrying the Claude-Code envelope
// (uuid / parentUuid / sessionId / timestamp / cwd / version / gitBranch /
// isSidechain), plus a per-record body. The record `type` is one of:
//
//   - user       — a prompt (message.content is a bare STRING) or tool
//     results (message.content is an ARRAY of tool_result blocks, with a
//     structured toolUseResult sibling)
//   - assistant  — a model turn; message.content is an ARRAY of Anthropic
//     content blocks (text and/or tool_use); message.id is the upstream
//     `chatcmpl-…` id
//   - runtime-config / file-history-snapshot / last-prompt — informational
//     records the adapter skips
//
// Qoder uses the Claude-Code tool vocabulary verbatim (Write / Bash /
// Read / Edit / Grep / Glob …), so the action map mirrors the claudecode
// adapter's.
//
// # Token capture (honest gaps)
//
// There is NO usable local token capture. The transcript records carry NO
// token fields at all. The run-log segments DO carry Anthropic-NET token
// names (input_tokens / output_tokens / cache_read_input_tokens /
// cache_creation_input_tokens on model.response.completed records) — but
// every field was ZERO in live capture (v1.0.40, 2026-07-09): qoder
// resolves usage server-side and never writes real counts locally, and it
// exposes no base-URL knob to route through the observer proxy. The adapter
// parses those segment records so that IF a future build writes non-zero
// counts they flow through as TokenEvents, guarded by a zero-usage check so
// no phantom rows land today. TokenTier is effectively NONE.
//
// # Model
//
// The concrete model string is ALSO server-side only — message.model,
// runtime-config.model, and every segment `model` field were EMPTY in live
// capture. The adapter leaves Model empty rather than fabricating one.
//
// # Project root
//
// The directory name dash-sanitizes the cwd, but every transcript record
// carries the RAW OS path in its `cwd` field (and the run-log's
// session.config.loaded carries `project_root`). The adapter resolves the
// project root from that field through crossmount.TranslateForeignPath (so
// a Windows `C:\...` session parsed by a WSL2 observer maps to `/mnt/c/...`)
// followed by git.Resolve — the lossy dir slug is never used for path
// resolution.
//
// # Sub-agents
//
// The per-record `isSidechain` flag is carried onto every emitted event.
// It was false throughout the live capture and no `subagents/` directory
// was present; sub-agent traces (were they to appear) surface inline in the
// same transcript with isSidechain:true, mirroring the Claude-Code model.
//
// # Security / off-limits
//
// The adapter reads only the `projects/<slug>/<uuid>.jsonl` transcripts and
// the `logs/sessions/.../segments/*.jsonl` run logs. It NEVER reads the
// encrypted per-session `state.json` blobs, `~/.qoder/settings.json`
// (provider config), or `~/.qoder/.auth/` (the `user` token blob and the
// `machine_id` telemetry fingerprint). All raw text (prompts, tool inputs,
// tool outputs, error bodies) passes through the injected scrub.Scrubber
// before it leaves the adapter.
//
// Note: qoder writes a stable machine-id fingerprint at
// ~/.qoder/.auth/machine_id used for its own telemetry; the adapter neither
// reads nor emits it.
package qoder
