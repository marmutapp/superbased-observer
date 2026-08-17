# DeepSeek Harness test fixtures

Two synthetic, path-scrubbed `session.jsonl.zstd` fixtures for
`internal/adapter/deepseek`. Neither is a raw capture — both are
hand-authored JSONL trimmed to the shapes confirmed against a real 178-line
decompressed capture, then zstd-compressed with a throwaway helper using the
already-vendored `github.com/klauspost/compress/zstd` encoder (no `zstd` CLI
is available on the dev box these were built on, so `zstd.NewWriter(nil)` +
`EncodeAll` stood in for it). All paths use the placeholder project root
`/work/project` instead of the real captured path.

## `session.jsonl.zstd`

A single-turn, three-step session exercising the full happy path plus two
honest-failure branches:

- Header line (`type:"session"`) with `cwd:"/work/project"` — the project
  root.
- A genuine user prompt (`user/message`, `data.source.kind:"user"`).
- A harness-injected pseudo-user message (`user/message`,
  `data.source.kind:"plugin"`, plugin id
  `"@deepseek-ai/dsh-system-prompt"`) — must be SKIPPED, not captured as
  `ActionUserPrompt`. This is the exact shape observed in live capture.
- Step 1: `assistant/message` with a `glob` tool-call block + usage with NO
  `cacheReadTokens` (`inputTokens:7749, outputTokens:194`), followed by
  `tool/call` and a successful `tool/result` (with the `meta.{shape,paths,
  truncated,total}` glob sidecar, informational only).
- Step 2: `assistant/message` with a `write` tool-call block + usage WITH
  `cacheReadTokens` (`inputTokens:4011, outputTokens:96,
  cacheReadTokens:7680` — the row proving `inputTokens` is already NET of
  cache, since 4011 < 7680), followed by a successful `tool/result`.
- Step 3: `assistant/message` calling an unrecognised tool name
  (`totally_unknown_tool`) — exercises the `ActionUnknown` fallback + the
  "unrecognised tool name" warning path — followed by a `tool/result` with
  `isError:true`, exercising the error-stamping path
  (`ToolEvent.Success=false` + `ErrorMessage` set).
- `turn/end` with `reason.kind:"completed"` — produces no event (the normal
  path; the assistant message and token rows already describe the turn).

Also includes `step/start`/`step/end` lines (informational, skipped) to
confirm the parser tolerates and ignores them.

## `session-aborted.jsonl.zstd`

A short session ending in `turn/end` with `reason.kind:"cancelled"` (a
non-`"completed"` reason never actually observed live, but defensively
handled per the package doc) — exercises `ActionTurnAborted` emission. Uses
model `deepseek/deepseek-v4-pro` (vs. `-flash` in the other fixture) so a
test can assert both cost-engine pricing slugs resolve.

## Off-limits, not represented here

`~/.dsh/.credentials.yaml` and `~/.dsh/settings.yaml` hold API keys / harness
settings — this package never reads them, and no fixture models their
content.
