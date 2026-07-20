# testdata/devin — Devin CLI fixtures

Fixtures backing the `internal/adapter/devin` table-driven tests
(`adapter_test.go`, `transcript_test.go`).

**Source shape**: Modeled on a live capture 2026-07-09 from Cognition's
Devin CLI build **3000.1.27** on WSL2 + Windows — the SQLite store at
`~/.local/share/devin/cli/sessions.db` (Windows
`%APPDATA%\devin\cli\sessions.db`). See
`docs/plans/new-adapters-live-capture-2026-07-09.md`.

**Anonymisation**: These fixtures are **synthesized**, not a copy of the
operator's live DB. They reproduce the real schema and JSON shapes
(`message_nodes` tree, `chat_message` JSON with
`metadata.metrics.{input_tokens,output_tokens,cache_read_tokens,`
`cache_creation_tokens,ttft_ms}`, tool-role
`metadata.extensions["chisel/tool_result_meta"].success`, adjective-noun
session ids, `main_chain_id` leaf pointer) with generic prompts
(`"hi"`, `"Create a file hello.txt …"`) and paths (`/home/user/project`,
`C:\Users\dev\project`). No real usernames, prompts, or file bodies.

## File inventory

| File | Purpose | Use in tests |
|------|---------|--------------|
| (in-test `sessions.db`) | Synthesized SQLite store with 3 sessions: `cobalt-fruit` (native linux cwd, a **dead regeneration branch** off `main_chain_id`, write+exec tool calls with success results, per-node token metrics), `bird-brick` (raw `C:\…` Windows `working_directory` for the crossmount path), `malformed-test` (a `main_chain_id` leaf whose `chat_message` is invalid JSON). **NOT a tracked file** — the repo tracks no `.db` binaries (tree-wide `*.db` gitignore, SQLite fixtures are synthesized in-test by convention); `TestMain` builds the DB once per test binary from the embedded SQL dump in `internal/adapter/devin/fixture_test.go`. | All parser + transcript tests. Exercises: active-chain walk / regeneration dedup, token capture, tool→action mapping, ContentBytes, foreign-Windows project-root translation, malformed-node skip-don't-crash, watermark idempotency, `ReadTranscript`. |
| `cobalt-fruit.json` | Trimmed, anonymized **ATIF-v1.7** rendered transcript export (as Devin writes to `cli/transcripts/<id>.json`). | Reference only — **not loaded by tests**. The adapter reads the always-present `message_nodes` store, not this convenience export. |

## Regenerating the fixture DB

Edit the `fixtureSQL` dump embedded in
`internal/adapter/devin/fixture_test.go`; the tests assert on the
specific ids
(`u-1`, `a-live1`, `a-final`, `call_write1`, `call_exec1`, `a-dead`,
`call_dead`) so keep them stable.
