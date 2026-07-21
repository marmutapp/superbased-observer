# testdata/qwencode — Qwen Code CLI fixtures

**Captured**: 2026-07-09 from live `qwen` 0.19.8 sessions on WSL Ubuntu
(`~/.qwen/projects/-tmp-sbo-capture-qwen/` + the operator's
`-home-marmutapp-superbased-observer` sessions) and Windows 11
(`C:\Users\<u>\.qwen\projects\c--programsx-regulation\`, accessed via
`/mnt/c`). Ground truth documented in
[`docs/plans/new-adapters-live-capture-2026-07-09.md`](../../docs/plans/new-adapters-live-capture-2026-07-09.md)
(Phase A/B/C qwen rows).

**Anonymisation**: every fixture is REGENERATED from the live shapes,
not copied verbatim. Real prompts replaced with the neutral capture
prompts; real cwds replaced with `/home/dev/proj` (WSL) and
`C:\programsx\regulation` (Windows — a path that intentionally does NOT
exist, exercising the git.Resolve fallback-to-cwd branch after
crossmount translation); real hostname replaced with `DEV-HOST`; UUIDs
replaced with readable synthetic ids (`u0…u10`, `s0…`, `w0…`, session
ids `aaaaaaaa-…`); OpenAI `chatcmpl-…` response ids replaced with
`chatcmpl-AAA/BBB/CCC/DDD`. **Token numbers are the real live values**
(17883/81/0, 18049/41/17920, 20557/29/0 …) because they carry the
gross-vs-net evidence the tests pin. No keys, no emails, no real file
bodies.

## File inventory

| File | Purpose | Use in tests |
|------|---------|--------------|
| `tool-call-session.jsonl` | Full one-shot turn: user prompt → attribution_snapshot → api_response (cached=0) → assistant with two `functionCall` parts (`write_file` + `run_shell_command`) → per-tool `ui_telemetry` tool_call events → paired `tool_result` records (object + string `resultDisplay` variants) → second api_response (cached=17920 ⊂ input=18049) → assistant text | `TestParseToolCallSession` (taxonomy mapping, result stamping, duration enrichment, **gross-vs-net token math**), `TestCursorResumption` |
| `simple-session.jsonl` | Minimal prompt→response session with a NON-Qwen model (`glm-5.2`) and nonzero `thoughts_token_count` (40) | `TestParseSimpleSession` (reasoning mapping, model-per-turn honesty) |
| `windows-session.jsonl` | Windows-captured shape: raw `C:\programsx\regulation` cwd in every record, `slash_command` system record (`/auth`), `qwen-code.api_error` ui_telemetry (401 AuthenticationError, model `GLM-5.2`) | `TestParseWindowsSession` (crossmount translation, ActionAPIError, slash-command capture) |
| `malformed-session.jsonl` | 4 physical lines: good user record, a non-JSON garbage line, an empty line, good assistant record | `TestMalformedToleranceAndOffset` (skip-with-warning + cursor reaches EOF) |
| `session.runtime.json` | The `<uuid>.runtime.json` companion (schema_version/pid/session_id/work_dir/hostname/started_at/qwen_version) | Reference + IsSessionFile rejection shape; **not parsed** by the adapter |
| `token-usage-sidecar.jsonl` | Two lines of `~/.qwen/usage/token-usage-YYYY-MM.jsonl` (schemaVersion/sessionId/model/authType/source/inputTokens/…) | Reference only — the adapter deliberately does NOT parse the sidecars (in-transcript `ui_telemetry` api_response records are the single token seam; parsing both would double-count) |

## Record-shape facts the fixtures pin

- Envelope on every line: `uuid / parentUuid / sessionId / timestamp /
  type / cwd / version / gitBranch` (Claude-Code-shaped; qwen is a
  Gemini-CLI fork so message bodies are Gemini-shaped `role`+`parts`).
- `type` values: `user`, `assistant` (role `model`; `usageMetadata` +
  `contextWindowSize`), `tool_result` (message `functionResponse` parts
  + structured `toolCallResult{callId,status,resultDisplay}`), `system`
  discriminated by `subtype` ∈ {`attribution_snapshot`,
  `file_history_snapshot`, `slash_command`, `ui_telemetry`}.
- `ui_telemetry` → `systemPayload.uiEvent` with DOTTED keys
  (`event.name`, `event.timestamp`); `event.name` ∈
  {`qwen-code.api_response`, `qwen-code.tool_call`,
  `qwen-code.api_error`}.
- **Gross input**: `input_token_count` INCLUDES
  `cached_content_token_count`; `total == input + output` on every
  live record. The fixture's second api_response (18049 gross, 17920
  cached, total 18090 = 18049+41) is the pin.
- `prompt_id` = `<sessionId>########<n>` — used as TokenEvent.TurnID.
- Per-turn `model` is whatever provider the operator configured
  (gpt-4o / glm-5.2 / GLM-5.2 / deepseek-… all observed) — never
  assume a Qwen model.
- Windows records carry raw `C:\` backslash cwds while the project
  DIRECTORY name is dash-sanitized (`c--programsx-regulation`); the
  adapter translates the record cwd, never the dir name.
