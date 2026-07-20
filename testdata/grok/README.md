# testdata/grok — Grok Build adapter fixtures

Anonymized fixtures for `internal/adapter/grok`. All usernames, hostnames,
paths, git remotes, key material and the model fingerprint are synthetic;
the JSON **structure** and realistic token magnitudes are preserved from a
live `grok` 0.2.93 capture (WSL Ubuntu, 2026-07-09).

## Layout

The bundle mirrors grok's real on-disk layout so `IsSessionFile` (which
keys on the `/.grok/sessions/` and `/.grok/logs/` path segments) exercises
against the same shape it will see in production:

```
home/.grok/
├── logs/
│   └── unified.jsonl                          global diag log → TokenEvent source
└── sessions/
    └── %2Fhome%2Fdev%2Fdemo/                  percent-encoded cwd dir name
        └── 019f0000-0000-7000-8000-000000000001/
            ├── updates.jsonl                  ACP session-update stream → ToolEvent source
            ├── chat_history.jsonl             conversation → ReadTranscript source
            └── summary.json                   model + git_root_dir + head_branch seam
```

Session id (`sid`): `019f0000-0000-7000-8000-000000000001`.
Synthetic cwd / git root: `/home/dev/demo` (branch `main`, model `grok-4.5`).

## File-by-file

### `sessions/.../updates.jsonl` (8 lines)
The Agent Client Protocol stream — one JSON-RPC `session/update` (or the
`_x.ai/session/update` vendor extension) notification per line. Exercises
every variant the adapter handles:

| line | `sessionUpdate` | what it drives |
|---|---|---|
| 1 | `user_message_chunk` | user prompt + session-start marker; carries a `sk-…` secret to prove scrubbing; `_meta.modelId` |
| 2 | `agent_thought_chunk` | carried as `PrecedingReasoning` onto the next event |
| 3 | `agent_message_chunk` | assistant message |
| 4 | `tool_call` (`read_file`) | tool event; `rawInput.target_file` → Target |
| 5 | `tool_call_update` (`completed`) | stamps success + `rawOutput.Content.content` → ToolOutput |
| 6 | `tool_call` (`run_terminal_command`) | second tool event |
| 7 | `tool_call_update` (`failed`) | stamps failure + ErrorMessage |
| 8 | `turn_completed` | terminal marker (skipped — informational) |

### `sessions/.../chat_history.jsonl` (6 lines)
The OpenAI-Responses-shaped conversation, read by `ReadTranscript` /
`ReadTranscriptFull`. Covers the polymorphic `content` field (bare string
on assistant/system/tool_result, array-of-parts on user), a `reasoning`
record (dropped at read time), and a `synthetic_reason` user record (an
injected system-reminder — skipped so only genuine user turns show).

### `logs/unified.jsonl` (4 lines)
The global diagnostic log. Two `shell.turn.inference_done` lines carry the
per-request token splits + the `sid` correlation key; the other two lines
(`agent initialized`, `turn.complete`) are noise the adapter skips. The
token arithmetic is the load-bearing fixture detail:

| loop | `prompt_tokens` (GROSS) | `cached_prompt_tokens` | net input | `completion` | `reasoning` |
|---|---|---|---|---|---|
| 1 | 12438 | 11136 | **1302** | 96 | 28 |
| 2 | 26346 | 25984 | **362** | 154 | 53 |

`cached_prompt_tokens ⊂ prompt_tokens` (GROSS/OpenAI convention), so the
adapter nets `input = prompt − cached` and carries cached as `CacheRead`.

### `sessions/.../summary.json`
The metadata seam. `current_model_id` supplies the token-event model
(unified.jsonl carries none per-turn), `git_root_dir` the project root,
`head_branch` the branch. `agent_name` is `grok-build-plan` (the read-only
plan agent the live capture used).

## Second bundle — array-content + edit/delete shapes (2026-07-09)

`sessions/%2Fhome%2Fdev%2Fdemo2/019f0000-0000-7000-8000-000000000002/`
(sid `…-002`, cwd/git-root `/home/dev/demo2`, model `grok-4.5`) captures the
two live shapes surfaced by fresh non-plan `grok` captures:

| line | `sessionUpdate` | what it drives |
|---|---|---|
| 1 | `user_message_chunk` | user prompt (single-object content — regression guard) |
| 2 | `tool_call` (`write`) | write_file; `rawInput.file_path` → Target |
| 3 | `tool_call_update` (`completed`) | **ARRAY `content`** with a `diff` block |
| 4 | `tool_call` (`search_replace`) | **edit_file** (was `unknown` before the fix) |
| 5 | `tool_call_update` (`completed`) | array `content` with a `diff` block |
| 6 | `tool_call` (`run_terminal_command`) | run_command — the **delete** (`rm … && test ! -e`) |
| 7 | `tool_call_update` (`completed`) | array `content` text block; `rawOutput.output` a **byte array** (proves content-block text is preferred) |
| 8 | `tool_call` (`list_dir`) | search_files |
| 9 | `tool_call_update` (`completed`) | array `content` text block |
| 10 | `turn_completed` | terminal marker (skipped) |

The `content` field is an ARRAY of blocks on every `tool_call_update` here —
before the shape-tolerant `acpContentField` decoder this aborted each line's
unmarshal with `cannot unmarshal array into … content`. `summary.json`
mirrors the first bundle's seam (agent `grok-build`, the non-plan agent).

## Not included / off-limits
- `auth.json`, `config.toml`, `system_prompt.txt`, `prompt_context.json`,
  `events.jsonl`, `rewind_points.jsonl`, `signals.json` — never parsed by
  the adapter (existence/perms only for credential files), so no fixture.
- `session_search.sqlite` — an FTS search index only (a decoy); never
  parsed.
