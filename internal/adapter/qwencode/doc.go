// Package qwencode implements the adapter for Alibaba's Qwen Code CLI
// (npm `@qwen-code/qwen-code`, binary `qwen`), a diverged fork of the
// Gemini CLI.
//
// # Storage shape
//
// Qwen persists one Claude-Code-shaped JSONL transcript per session at
//
//	~/.qwen/projects/<dash-sanitized-cwd>/chats/<uuid>.jsonl
//
// with a companion `<uuid>.runtime.json` (pid / hostname / work_dir /
// qwen_version) written beside it. Each JSONL line is one record carrying
// the Claude-Code envelope fields uuid / parentUuid / sessionId / timestamp
// / type / cwd / version / gitBranch, plus a per-record body. The record
// `type` is one of:
//
//   - user       — a user prompt (message.role=user, message.parts[].text)
//   - assistant   — a model turn (message.role=model; parts carry text and/or
//     functionCall blocks; usageMetadata + contextWindowSize)
//   - tool_result — a tool response (message.parts[].functionResponse +
//     a structured toolCallResult{callId,status,resultDisplay})
//   - system      — an out-of-band record discriminated by `subtype`:
//     attribution_snapshot / file_history_snapshot / slash_command /
//     ui_telemetry. The ui_telemetry records carry a `systemPayload.uiEvent`
//     whose `event.name` is one of qwen-code.api_response (per-API-call token
//     usage), qwen-code.tool_call (tool timing + decision), or
//     qwen-code.api_error (upstream error).
//
// # Token capture
//
// Per-turn token usage is read from the `qwen-code.api_response`
// ui_telemetry records (one per model inference), which mirror the
// aggregate sidecars `~/.qwen/usage_record.jsonl` and
// `~/.qwen/usage/token-usage-YYYY-MM.jsonl`. Those sidecars live OUTSIDE
// the watch root and are intentionally NOT parsed — the in-transcript
// ui_telemetry records are the single token seam, so no double-count is
// possible.
//
// Qwen follows OpenAI's GROSS input convention: `input_token_count`
// INCLUDES `cached_content_token_count`, and
// `total_token_count == input_token_count + output_token_count` (verified
// against live turns 2026-07-09). [models.TokenEvent.InputTokens] must be
// NET, so the adapter subtracts cached; cached lands in CacheReadTokens and
// `thoughts_token_count` in ReasoningTokens. Per-turn model may be non-Qwen
// (openai-compat providers; gpt-4o / GLM observed live) — never hardcoded.
//
// # Project root
//
// The directory name dash-sanitizes the cwd (`c--programsx-regulation`),
// but every record carries the RAW OS path in its `cwd` field. The adapter
// resolves the project root from the record cwd through
// crossmount.TranslateForeignPath (so a Windows `C:\...` session parsed by a
// WSL2 observer is mapped to `/mnt/c/...`) followed by git.Resolve — the
// dir name is never used for path resolution.
//
// # Security
//
// The adapter reads only the `chats/*.jsonl` transcripts. It NEVER reads
// `~/.qwen/settings.json`, which embeds plaintext provider API keys
// (DASHSCOPE / ZAI / DEEPSEEK observed 2026-07-09). All raw text (prompts,
// tool inputs, tool outputs, error bodies) is passed through the injected
// scrub.Scrubber before it leaves the adapter.
package qwencode
