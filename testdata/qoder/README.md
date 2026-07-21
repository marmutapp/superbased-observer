# testdata/qoder — Qoder CLI fixtures

**Captured**: 2026-07-09 from a live `qoder` v1.0.40 session on WSL Ubuntu
(`~/.qoder/projects/-tmp-sbo-capture-qoder/` + the run log under
`~/.qoder/logs/sessions/<slug>/<sid>/segments/`). Ground truth documented
in
[`docs/plans/new-adapters-live-capture-2026-07-09.md`](../../docs/plans/new-adapters-live-capture-2026-07-09.md)
(qoder rows, Phase A/C + Windows addendum).

**Anonymisation**: every fixture is REGENERATED from the live shapes, not
copied verbatim. Real prompts kept only as the neutral capture prompt
("Create a file hello.txt … then run ls"); real cwd replaced with
`/home/dev/proj` (a non-git dir so `git.Resolve` returns it unchanged);
UUIDs replaced with readable synthetic ids (`…-000000000001`); upstream
`chatcmpl-…` ids replaced with `chatcmpl-AAA/BBB`; tool-call ids replaced
with `call_write01` / `call_bash01`; segment `request_id`s replaced with
`req-0000-0001/0002`. The machine-id fingerprint, `.auth/user` token
blob, `settings.json`, and the encrypted `state.json` blobs are NOT
copied here — the `state.json` fixture is a structure-only STUB with the
nonce + encrypted `p` blob replaced by `STUB_…_TRUNCATED`.

## What the live capture established (verdicts the fixtures pin)

- **No local tokens.** The transcript (`projects/<slug>/<uuid>.jsonl`)
  carries NO token fields at all. The run-log segments DO carry
  Anthropic-NET token names (`input_tokens` / `output_tokens` /
  `cache_read_input_tokens` / `cache_creation_input_tokens` on
  `model.response.completed`), but every value was **ZERO** in live
  capture — usage is resolved server-side and never written locally. The
  adapter parses the segment tokens with a zero-usage guard so real
  future counts would flow while today nothing lands.
- **No local model string.** `message.model`, `runtime-config.model`,
  and every segment `model` field were EMPTY. The adapter leaves `Model`
  empty rather than fabricating one.
- **Anthropic content-block shape.** `message.content` is a bare STRING
  for a human prompt and an ARRAY of `text` / `tool_use` / `tool_result`
  blocks otherwise (like Claude Code). Tool names are the Claude-Code
  vocabulary verbatim (`Write` / `Bash` / `Read` / …).

## File inventory

| File | Purpose | Use in tests |
|------|---------|--------------|
| `tool-call-session.jsonl` | Full one-shot turn: `runtime-config` → user prompt (bare-string content) → `file-history-snapshot` → assistant text + two `tool_use` blocks (`Write` + `Bash`) → two user `tool_result` records → assistant end-turn text → `last-prompt`. Model empty throughout; `chatcmpl-AAA/BBB` message ids. | `TestParseToolCallSession` (taxonomy, result stamping, ContentBytes, MessageID, empty-model honesty), `TestCursorResumption` |
| `segments-zero.jsonl` | A real-shape run-log segment: `session.config.loaded` (project_root) → `turn.started` → `model.request.started` → two `model.response.completed` (all-zero tokens) → `turn.finished` (zero). | `TestSegmentTokensZeroGuarded` (zero-usage guard → 0 token events) |
| `segments-tokens.jsonl` | Same shape but with SYNTHETIC non-zero token counts on the two `model.response.completed` records (and an aggregate `turn.finished` that must NOT be double-counted). | `TestSegmentTokensNonZeroFlow` (future-proof token flow; session id from path; project root from config.loaded) |
| `malformed-session.jsonl` | 4 physical lines: good user record, a non-JSON garbage line, an empty line, good assistant record. | `TestMalformedToleranceAndOffset` (skip-with-warning + cursor reaches EOF) |
| `state.json` | Structure-only STUB of the encrypted per-session store (`items.<id>.{n,p}` nonce + ciphertext replaced with `STUB_…`). | Reference only — the adapter NEVER reads it (off-limits: encrypted). |

## Off-limits files (never read by the adapter)

- `projects/<slug>/<uuid>/state.json` and
  `projects/<slug>/<uuid>/compression-v2/state.json` — encrypted blobs.
- `~/.qoder/settings.json` — provider config.
- `~/.qoder/.auth/user` — auth token blob.
- `~/.qoder/.auth/machine_id` — telemetry fingerprint.
