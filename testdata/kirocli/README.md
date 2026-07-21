# testdata/kirocli

Anonymized fixtures for the Kiro CLI adapter (`internal/adapter/kirocli`,
`models.ToolKiroCLI = "kiro-cli"`). All captured live 2026-07-09 on WSL
Ubuntu + Windows 11 and scrubbed before landing here (cwd → `/home/dev/
project`, ids replaced with stable placeholders, one injected secret to
exercise the scrubber).

Kiro CLI has a **mode-dependent dual store**: interactive runs write the
flat-file bundle; `--no-interactive` runs write the SQLite
`conversations_v2` table. The adapter parses both; these fixtures cover
each layout plus the two `.json` state shapes.

## Flat-bundle fixtures (Layout 1 — `~/.kiro/sessions/cli/`)

| File | Shape it exercises |
|---|---|
| `flat-with-metadata.json` | The FINISHED `.json` state — `session_state.conversation_metadata.user_turn_metadatas[]` populated with `input/output_token_count` (0), `context_usage_percentage`, `metering_usage` credits, `end_timestamp`. Drives the per-turn token event. |
| `flat-with-metadata.jsonl` | The append-only message stream: `{"kind":"Prompt"|"AssistantMessage","data":{message_id,content:[{kind:"text",data}],meta:{timestamp}}}`. The Prompt carries an injected `sk-…` secret so the scrub test can assert redaction. |
| `flat-with-metadata.history` | Raw input lines — the adapter never reads `.history`; kept only to mirror the real bundle. |
| `flat-live-shape.json` | The LIVE/killed `.json` shape — `conversation_metadata` present but `user_turn_metadatas` ABSENT. Proves the adapter tolerates a bundle with no turn accounting (emits messages, no token events). |
| `flat-live-shape.jsonl` | Stream paired with the bare state. |
| `flat-malformed.jsonl` | Two valid stream lines around one broken JSON line — asserts the parser warns + advances rather than crashing. Has no `.json` sibling. |

## SQLite fixture (Layout 2 — `conversations_v2`)

| File | Shape it exercises |
|---|---|
| `conversations_v2-value.json` | The `value` column of ONE `conversations_v2` row — the live one-shot `--no-interactive --trust-tools` run that did `fs_write` (create hello.txt) then `execute_bash` (`ls`) then a text Response. `history[]` carries the `user`/`assistant`/`request_metadata` turn shape, `env_context.env_state.current_working_directory`, tool_use + tool_use_result records, and the all-null token fields real captures exhibit. |

The SQLite tests (`statedb_test.go`) build a `data.sqlite3` in-process
(`sql.Open` + `CREATE TABLE conversations_v2` + `INSERT`) and load this
`value` JSON — the same in-test-DB approach clinecli / kilocode use, so
no binary `.db` is committed. Tests also seed the `auth_kv` + `state`
tables with sentinel secrets to prove the adapter NEVER reads them, and
vary the row `key` to a `C:\…` string to exercise Windows-key
crossmount translation.

## Reality-check finds (Phase 0, live 2026-07-09)

1. Even a FINISHED `.json` reports `input_token_count: 0` /
   `output_token_count: 0` — Kiro's local counts are structurally zero
   (accounting is server-side on SigV4 endpoints). Emitted honestly,
   tagged `unreliable`.
2. The SQLite `request_metadata` token fields
   (`total_tokens`/`uncached_input_tokens`/`output_tokens`/
   `cache_read_input_tokens`/`cache_write_input_tokens`) were ALL null
   in every capture → no token event from the SQLite path.
3. `metering_usage` values are Kiro **credits**, not tokens and not USD
   — deliberately NOT stored.
4. A tool_use result content block is `{"Text":"…"}` OR `{"Json":{…}}`
   (execute_bash returns the latter with `exit_status`/`stdout`/
   `stderr`).
5. `model_id` is `"auto"` for auto-mode sessions (no pricing entry →
   cost engine `unknown`, documented gap).
6. The Windows SQLite row `key` is a raw `C:\tmp\sbo-capture\kiro`
   string — the KEY itself needs `crossmount.TranslateForeignPath`.
7. The sqlite `conversations` (v1) + `history` tables are legacy
   shell-history — never chat; `auth_kv` (`kirocli:social:token`) +
   `state` (`telemetry-cognito-credentials`, …) are credential/telemetry
   rows the adapter must never read.
