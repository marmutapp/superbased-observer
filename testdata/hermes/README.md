# testdata/hermes — Hermes Agent fixtures

**Captured**: 2026-06-05 from a fresh Hermes Agent install in WSL2 (Ubuntu)
running schema **v14**.
**Operator**: Santosh, against `nvidia/nemotron-3-ultra:free` via OpenRouter.
**Anonymisation**: All `/home/marmu` paths in committed `.sql` files were
replaced with `/home/dev`. API keys never appear in `state.db` (Hermes
stores them in `~/.hermes/.env`).

These fixtures back the hermes adapter's table-driven tests (Commit 11 of
`docs/plans/hermes-adapter-implementation-plan-2026-06-05.md`) and were
also the basis of the §0.5 reality-check that landed in
`docs/hermes-adapter-plan.md`.

## File inventory

| File | Purpose | Use in tests |
|------|---------|--------------|
| `sessions.sql` | `.dump sessions` — 9 real rows, full schema-v14 column set | Build in-memory SQLite for `statedb_test.go` / `parse_test.go` |
| `messages.sql` | `.dump messages` — 62 real rows including `role='assistant'` tool_calls + `role='tool'` results | Same |
| `schema-sessions.sql` | Just the `CREATE TABLE sessions` DDL | Schema-shape pinning test |
| `schema-messages.sql` | `CREATE TABLE messages` + 6 triggers (FTS5 word + trigram on insert/update/delete) | Same |
| `schema-schema_version.sql` | `CREATE TABLE schema_version` | Adapter open-time version-check test |
| `schema-state_meta.sql` | `CREATE TABLE state_meta` (key/value strings) | Not consumed today — kept for reference |
| `schema-compression_locks.sql` | `CREATE TABLE compression_locks` | Not consumed today — Hermes-internal compression bookkeeping |
| `reality-check.txt` | Output of the §0.4 inspection queries — schema_version, sessions summary, distinct models, distinct tool_names, sample tool_calls JSON, sample tool-result content, state_meta/compression_locks contents | Reference for the plan reality-check section; **not loaded by tests** |
| `plugin-reality-check.txt` | Plugin loader source-file discovery results | Reference — confirms `hermes_cli/plugins.py` exists and which hook names appear |
| `plugin-api-source.txt` | First 200 lines of `plugins.py` + first 300 lines of `hooks.py` + the `langfuse` sample plugin manifest + code | Reference for our `__init__.py` template (Commit 8) |
| `plugin-context-api.txt` | `class PluginContext`, `def register_hook`, `def invoke_hook` source extracts | The exact API contract our Python bridge consumes |
| `tool-calls-sample.txt` | Tool-call JSON samples (first 200 chars × 20 rows) — separate from `reality-check.txt` to keep that file lean | Reference only |
| `sessions-summary.txt` | Session-table summary (id / source / model / counts) | Reference only |

## What the captured data covers

The 9 sessions exercise the following Hermes tools (every entry below has
at least one matching `messages.tool_calls` row):

| Hermes tool | Sessions | Test coverage value |
|-------------|---------:|---------------------|
| `write_file` | 3 | Action normalization → `write_file`, file_path target extraction |
| `read_file` | 1 | → `read_file` |
| `patch` | 1 | → `edit_file`; `{path, old_string, new_string}` argument shape |
| `terminal` | 8 | → `run_command`; `{command}` argument shape; structured result `{output, exit_code, error}` |
| `search_files` | 1 | → `search_files`; `{pattern, target, path}` argument shape |
| `web_search` | 1 | → `web_search`; `{query, limit}` |
| `web_extract` | 1 | → `web_fetch`; `{urls: [...]}` (note: array, not single URL) |

Notably absent from this corpus (deliberate — would need more elaborate test
prompts): `delegate_task`, `mixture_of_agents`, `todo`, `clarify`,
`browser_*`, `execute_code`, `memory`, `vision_analyze`,
`image_generate`, `cronjob`, and any `mcp_<server>_<tool>` calls. The
normalizer should handle those via the prefix rules in
`normalize.go`; targeted unit tests in `normalize_test.go` cover them
synthetically.

## Reproducing the dumps

```bash
# In WSL Ubuntu
DEST=/mnt/d/programsx/superbased-observer/testdata/hermes
DB=~/.hermes/state.db

mkdir -p "$DEST"
sqlite3 "$DB" ".dump sessions"          > "$DEST/sessions.sql"
sqlite3 "$DB" ".dump messages"          > "$DEST/messages.sql"
sqlite3 "$DB" ".schema sessions"        > "$DEST/schema-sessions.sql"
sqlite3 "$DB" ".schema messages"        > "$DEST/schema-messages.sql"

# Anonymise the operator's home path before committing
sed -i 's#/home/marmu#/home/dev#g' "$DEST"/*.sql "$DEST"/*.txt
```

## Notes for the implementing session

1. **Filter `messages.active = 1`.** Hermes adds an `active INTEGER NOT NULL
   DEFAULT 1` column on the messages table (Phase 4b backfill must include
   `WHERE active = 1` in the scan query). Rewound / compressed-out messages
   carry `active = 0` and must not produce ToolEvents.
2. **`tool_calls` rows always carry `function.type = "function"`**
   wrapper plus `id`, `call_id`, `response_item_id`. The plan §11.3 missed
   the `call_id` and `response_item_id` extras. Parse tolerantly.
3. **Tool result content is structured JSON.** The plan called it raw text;
   it's a serialised dict like `{"bytes_written": 128, "exit_code": 0,
   "output": "Hello, World!", "error": null}` (terminal) or
   `{"bytes_written":128, "dirs_created":true, "lint":{...},
   "resolved_path":"...", "files_modified":[...]}` (write_file). Parse for
   Success / ErrorMessage / Output extraction.
4. **Provider prefix has a `:suffix` tail.** Models look like
   `nvidia/nemotron-3-ultra:free` — strip the provider segment AND keep the
   `:suffix` (or strip-and-record separately) so pricing lookup works.
5. **`sessions.cwd` exists.** The plan said CWD was missing from the DB —
   wrong on schema v14. The SQLite backfill path can read `cwd` directly,
   so we don't depend on the hook for project root resolution. See the
   reality-check section of `docs/hermes-adapter-plan.md`.
