# testdata/zcode — zcode (Z.AI) fixtures

**Captured**: 2026-08-25, live-grounding pass against two real installs —
a native WSL Ubuntu install (`~/.zcode/cli/db/db.sqlite`) and a foreign-mount
Windows install reached from WSL at `/mnt/c/Users/<user>/.zcode/cli/db/db.sqlite`.
Both installs ran `zcode-app-cli` 3.8.1-15 / `zcode-runtime` 0.16.3.
**Operator**: Santosh, against `zai/glm-5.2` (main) and `zai/glm-5-turbo`
(lite/title-generation) via the `zai` provider.
**Anonymisation**: unlike the cline-cli fixtures (redacted copies of real
captures), `schema.sql` and `fixture.sql` here are **not** derived by
copying real rows out of either live store. `schema.sql` is a genuine
`.schema <table>` dump (DDL only, zero data) scoped to the six tables the
adapter actually consumes; `fixture.sql` is entirely hand-written synthetic
data whose *shapes* (JSON field names, tool names, part types, and — most
importantly — the `model_usage` netting arithmetic) were checked against
the live-grounding numbers below, but whose actual prompts/paths/ids are
invented. This is stricter than copy-and-redact: nothing from a real
session's content ever touches this directory.

These fixtures back the zcode adapter's package tests
([`docs/zcode-adapter.md`](../../docs/zcode-adapter.md)) and the
2026-08-25 reality-check that corrected the doc's "message.data.tokens is
zeroed" claim (it isn't — see below).

## File inventory

| File | Purpose | Use in tests |
|------|---------|--------------|
| `schema.sql` | `.schema <table>` DDL for the six tables `internal/adapter/zcode/adapter.go` queries: `schema_migration`, `session`, `message`, `part`, `todo`, `model_usage`. Zero data. | Build an in-memory/temp-file SQLite for adapter parser tests |
| `fixture.sql` | Synthetic INSERT statements against `schema.sql`'s schema: 2 sessions (1 parent + 1 subagent child linked via `session.parent_id`), 4 messages, 16 parts (every observed live part type plus the two silently-dropped ones), 3 todos, 3 `model_usage` rows | Adapter re-parse smoke tests; loaded with `schema.sql` first, then `fixture.sql` |

`schema.sql` deliberately omits the rest of the live schema — notably
`session_task_link`, `workflow_run`, and `workflow_activity` (see
"Notes for the implementing session" below) — because the adapter reads
none of it and reproducing untested DDL invites drift nobody would notice.

## What the captured data covers

| zcode feature | Where in `fixture.sql` | Test coverage value |
|----------------|------------------------|----------------------|
| `role:"user"` message + `text` part | `msg_u0001` / `part_text_u0001` | `loadUserPromptEvents` |
| `role:"assistant"` message, `finish:"stop"` | `msg_a0001`, `msg_ac001` | `loadCompletionEvents` (→ `task_complete`) |
| `type:"tool"`, `tool:"Read"` | `part_tool_read_0001`, `part_tool_read_c001` | `mapTool` → `read_file` |
| `type:"tool"`, `tool:"Bash"`, one `exit:1` failure and one `exit:0` success | `part_tool_bash_0001` (fails), `part_tool_bash_c001` (succeeds) | `mapTool` → `run_command`; `Success`/`ErrorMessage` derivation from `state.metadata.exit` |
| `type:"tool"`, `tool:"Edit"` | `part_tool_edit_0001` | `mapTool` → `edit_file` |
| `type:"tool"`, `tool:"Write"` | `part_tool_write_0001` | `mapTool` → `write_file` |
| `type:"tool"`, `tool:"WebSearch"` | `part_tool_websearch_0001` | `mapTool` → `web_search` |
| `type:"tool"`, `tool:"Agent"` (subagent spawn) | `part_tool_agent_0001` | `mapTool` → `spawn_subagent`; the tool's own `output` carries the subagent's report (there is no dedicated `subtask` part live — see notes) |
| `type:"reasoning"` threaded onto its successor | `part_reasoning_0001` → threads onto `part_tool_read_0001` | `loadReasoningIndex` CONSUMED-ONCE/LAST-WINS semantics |
| `type:"step-start"` | `part_stepstart_0001` | Confirms the adapter silently drops this type (no loader queries it) |
| `type:"step-finish"` | `part_stepfinish_0001` | `loadStepFinishEvents` → `harness_call`, observability only, never a `TokenEvent` |
| `type:"timeline"` | `part_timeline_0001` | Confirms the adapter silently drops this type too |
| `type:"text"` (assistant) | `part_text_a0001`, `part_text_ac001` | `loadAssistantTextEvents` → `assistant_message` |
| A genuinely separate child session (`ses_e5f6a7b8c9d0`) with `session.parent_id` set to the parent | `session` table, both rows | Confirms `ParseSessionFile` scans the whole `db.sqlite`, so a spawned subagent's own actions land normally without any spawn↔child cross-link (there isn't one — see notes) |
| `todo` rows | 3 rows, `ses_a1b2c3d4e5f6` | `loadTodoEvents` → `todo_update`, `Target` carries `status` |
| `model_usage` row with `cache_read_input_tokens > 0` | `mu_0001` (`input_tokens=14276`, `cache_read_input_tokens=11712`) | Reproduces the exact 2026-08-25 foreign-mount live netting sample: `netInput = 14276 - 11712 = 2564` |
| A second `model_usage` netting sample, different session/model | `mu_0002` (`input_tokens=12860`, `cache_read_input_tokens=8448`) | Reproduces the exact 2026-08-25 native-WSL live netting sample: `netInput = 12860 - 8448 = 4412` |
| `model_usage` row with `assistant_message_id IS NULL` | `usage_model_session_title_a1b2c3d4` (`query_source='session_title'`) | Proves the strict-superset relationship over `message.data.tokens`: this call has no message row at all, only `model_usage` |

A permanent regression test (not committed — deleted after verification per
this session's throwaway-harness discipline) loaded `schema.sql` +
`fixture.sql` through a real `Adapter.ParseSessionFile` call and confirmed:
18 `ToolEvent`s (2 `read_file`, 2 `run_command`, 1 `edit_file`, 1
`write_file`, 1 `web_search`, 1 `spawn_subagent`, 2 `task_complete`, 2
`assistant_message`, 1 `harness_call`, 2 `user_prompt`, 3 `todo_update`)
and exactly 3 `TokenEvent`s whose `InputTokens` were `2564`, `4412`, and
`340` — matching the netting arithmetic above exactly, byte-for-byte,
straight out of the parser (not hand-verified against the SQL alone).

**Notably absent from this corpus** (deliberate — the live grounding never
observed these, so fabricating them would misrepresent reality rather than
document it): a `type:"subtask"` part (the `Agent`-tool `type:"tool"` shape
above is what zcode actually emits for a subagent spawn; `subtask` parts
are OpenCode's older shape and `loadSubtaskEvents`'s query is kept as a
defensive fallback only); any populated row in `session_task_link` /
`workflow_run` / `workflow_activity` (present in the live schema, zero rows
in both live captures — not in `schema.sql` either, see above); an MCP
tool call (`config.json`'s `mcp.servers` was `{}` on both live installs);
a `provider_metadata_json` / `raw_usage_json` payload on `model_usage`
(both were `NULL` on every live row observed, so this fixture leaves them
`NULL` too rather than inventing a shape nobody has seen).

## Reproducing the dumps

```bash
# In WSL, against a native install:
NATIVE=~/.zcode/cli/db
# ...and a foreign-mount Windows install reached from WSL:
WINDOWS=/mnt/c/Users/<user>/.zcode/cli/db
SNAP=/tmp/zcode-snap

# 1. Snapshot main + WAL + SHM so a live zcode process can't hold an
#    exclusive lock mid-dump, and the copy is transaction-consistent.
mkdir -p "$SNAP/native" "$SNAP/windows"
cp "$NATIVE/db.sqlite" "$NATIVE/db.sqlite-wal" "$NATIVE/db.sqlite-shm" "$SNAP/native/" 2>/dev/null
cp "$WINDOWS/db.sqlite" "$WINDOWS/db.sqlite-wal" "$WINDOWS/db.sqlite-shm" "$SNAP/windows/" 2>/dev/null

# 2. Dump schema for ONLY the six adapter-consumed tables, one at a time
#    (a whole-DB `.schema` dump risks this environment's output-scrubbing
#    layer false-positiving on a long lowercase_underscore index name —
#    see "Notes for the implementing session" below — so scope narrowly).
DEST=/path/to/superbased-observer/testdata/zcode
: > "$DEST/schema.sql"
for t in schema_migration session message part todo model_usage; do
  sqlite3 "$SNAP/native/db.sqlite" ".schema $t" >> "$DEST/schema.sql"
  echo >> "$DEST/schema.sql"
done

# 3. fixture.sql is hand-written (not dumped) — see the file header for
#    why, and the "What the captured data covers" table above for what
#    it's grounded against.
```

## Notes for the implementing session

These are the reality-check findings the 2026-08-25 live re-parse produced,
already folded into [`docs/zcode-adapter.md`](../../docs/zcode-adapter.md)
"Known gaps":

1. **`message.data.tokens` is populated, not zeroed.** An earlier doc draft
   claimed OpenCode's per-message token bundle was zeroed out in zcode.
   Live re-parse of both scratch stores shows it's populated on every
   completed assistant message and matches the corresponding `model_usage`
   row's split exactly. The adapter still sources tokens from
   `model_usage` (not `message.data.tokens`) because `model_usage` is a
   **strict superset**: it also carries usage-only calls with no message
   row at all (the `session_title` shape `mu_0003` reproduces here).

2. **`session_task_link` / `workflow_run` / `workflow_activity` exist but
   are empty.** The live schema carries a table shaped exactly for
   cross-linking an `ActionSpawnSubagent` row to its resulting child
   session (`parent_session_id` / `child_session_id` / `role` /
   `agent_type` / `model` / `status`), plus two workflow tables. Both
   scratch stores had **zero rows** in all three as of this grounding —
   so this fixture doesn't populate them either, and the doc documents
   this as an unconfirmed-populated mechanism rather than something a
   future pass can build on without re-checking first.

3. **Live tool names, exhaustively.** `select distinct json_extract(data,
   '$.type') from part` returned exactly: `text`, `step-start`,
   `reasoning`, `step-finish`, `tool`, `timeline` — never `subtask` — in
   both scratch stores. Distinct tool names inside `type='tool'` parts:
   `Read`, `Bash`, `Write`, `Edit`, `WebSearch`, `Agent`. `fixture.sql`
   exercises every one of these part types and every one of these tool
   names.

4. **A whole-DB `.schema` dump can get silently corrupted by this
   environment's write path.** The first attempt at `schema.sql` (a full
   `.schema` dump of the native scratch DB, piped via shell redirection)
   had two `CREATE INDEX` statement names on the unconsumed
   `session_task_link` table replaced with literal `[REDACTED]` text — a
   false-positive token-like-string scrub, confirmed by reading the
   committed file back byte-for-byte (not just the terminal echo). Fixed
   by narrowing `schema.sql` to only the six adapter-consumed tables
   (regenerated via six separate per-table `.schema` dumps, none of which
   touch `session_task_link`), rather than guessing at and committing an
   unverified reconstruction of the redacted names.

5. **The `netInput = input_tokens - cache_read_input_tokens` convention
   holds exactly**, cross-checked three independent ways: direct
   `sqlite3` queries on the live `model_usage` tables, a from-scratch
   `Adapter.ParseSessionFile` re-parse of both live scratch DBs, and (now)
   a from-scratch re-parse of this fixture. All three land on the same
   arithmetic.

## Off-limits files (NEVER commit, NEVER read)

Per [`docs/zcode-adapter.md`](../../docs/zcode-adapter.md) "Known gaps":

- `~/.zcode/cli/config.json` — carries provider `apiKey` values **in
  plaintext** (confirmed live 2026-08-25: a real, live, still-valid API
  key was incidentally observed while checking the doc's `mcp.servers`
  claim — it is not reproduced anywhere in this repo, including here).
  The adapter never reads this file; `provider_id`/`model_id` come from
  `model_usage` and `message.data.model`.
- `~/.zcode/cli/config.json.backup` — same content class as above.
- `~/.zcode/cli/exec/`, `~/.zcode/cli/rollout/`, `~/.zcode/cli/memories/`,
  `~/.zcode/cli/plugins/`, `~/.zcode/cli/artifacts/` — never inventoried
  byte-for-byte during this grounding pass (out of scope for the
  adapter, which only reads `db/db.sqlite`); treat as off-limits by
  default until a future session has a concrete reason to open one.
