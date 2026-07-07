# testdata/clinecli — Cline CLI fixtures

**Captured**: 2026-06-06 from a live `cline` 3.0.20 install on Windows
(npm-installed, `%APPDATA%\npm\node_modules\cline\`), accessed via WSL2
at `/mnt/c/Users/marmu/.cline/data/`.
**Operator**: Santosh, against `deepseek/deepseek-v4-flash` via the
`cline` cloud provider.
**Anonymisation**: All `D:\programsx\superbased-observer` and
`C:\Users\marmu` paths in committed files were replaced with
`/home/dev/proj` and `/home/dev` respectively. Long `tool_result.result`
file bodies (>500 chars) were replaced with `<redacted: N chars of file
content>` placeholders so the fixture stays small but the structural
shape stays real. Cross-mount Windows backslashes in the embedded
paths (e.g. `/home/dev/proj\PROGRESS.md`) are preserved intentionally —
they exercise the adapter's path normaliser the same way a real
Windows-side capture does.

These fixtures back the cline-cli adapter's table-driven tests
(Commits 5, 6, and 11 of
[`docs/plans/cline-cli-adapter-plan-2026-06-06.md`](../../docs/plans/cline-cli-adapter-plan-2026-06-06.md))
and the §15 reality-check section the implementation session will fill
out after Phase 1 lands.

## File inventory

| File | Purpose | Use in tests |
|------|---------|--------------|
| `sessions.sql` | `.schema` dump of the full `sessions.db` (sessions + subagent_spawn_queue + schedules + schedule_executions + indices) | Build in-memory SQLite for `statedb_test.go` / `parse_test.go` |
| `schema-sessions.sql` | Just the `CREATE TABLE sessions` DDL (28 columns) | Schema-shape pinning test |
| `schema-subagent-spawn-queue.sql` | `CREATE TABLE subagent_spawn_queue` DDL | Subagent-dedup test |
| `schema-schedules.sql` | `CREATE TABLE schedules` DDL | Cron-aware tests (deferred to v2) |
| `schema-schedule-executions.sql` | `CREATE TABLE schedule_executions` DDL | Same |
| `teams.sql` | `.schema` dump of `teams.db` (team_events + team_runs + team_tasks + team_outcomes + team_outcome_fragments + team_runtime_snapshot + team_store_schema_version) | Reference — v1 maps all team_* tools to `mcp_call`, so teams.db is not yet consumed |
| `cron.sql` | `.schema` dump of `cron.db` (cron_specs + cron_runs + cron_event_log + 11 indices) | Reference — cron capture deferred to v2 |
| `sample-session-meta.json` | Redacted copy of a real `<id>.json` — the per-session metadata file paired to messages.json | Parser tests for the `metadata.usage` aggregate and `messages_path` pointer |
| `sample-session-messages.json` | Redacted copy of the paired `<id>.messages.json` | Content-block walker tests (text, thinking, tool_use, tool_result, user_input wrapping) |
| `hooks-jsonl-sample.jsonl` | Synthesized — one line per the 9 hook event types Cline CLI emits to `~/.cline/data/logs/hooks.jsonl` when hook commands are registered OR subagents run | Hook-path parser tests (deferred to Phase 3 commit 9) |
| `reality-check.txt` | PRAGMA outputs, table list, sessions row count + `PRAGMA table_info(sessions)`, subagent_spawn_queue count | Reference for the plan §15 reality-check section; **not loaded by tests** |

## What the captured data covers

The live session is small (6 messages — a "Hi" greeting plus a
follow-up "what kind of model are you" turn). It exercises:

| Cline CLI feature | Where | Test coverage value |
|-------------------|-------|---------------------|
| `tool_use` (`read_files`) | sample-session-messages.json msg 2 | Action normalization → `read_file`; batched `files: [{path: ...}]` input shape |
| `tool_result` paired to `tool_use_id` | msg 3 | tool_result.content is a LIST of `{query, result, success}` dicts |
| `thinking` content block | msgs 2, 4, 6 | Anthropic-style extended thinking trace; parser must skip |
| `text` (user, `<user_input mode="act">…</user_input>` wrapped) | msgs 1, 5 | Strip the XML wrapper when emitting `user_prompt` action target |
| `text` (assistant, free-form) | msgs 4, 6 | Emit as `assistant_text` rows (cline-vscode precedent) |
| Per-message `metrics` | msgs 2, 4, 6 | Per-API-call Tier 2 token capture (richer than session-level aggregates) |
| Per-message `modelInfo` | msgs 2, 4, 6 | Track model + provider per message (provider switching) |
| `cwd` column on sessions table | sample-session-meta.json + sessions.sql | Project-root resolution direct from DB (no env-details scan, unlike cline-vscode V1) |
| `metadata_json.usage` + `aggregateUsage` | sample-session-meta.json | Session-level Tier 2 totals; aggregateUsage rolls subagent costs up |
| `metadata_json.checkpoint.{latest,history}` | sample-session-meta.json | Git-ref history (commit + stash refs) — informational, not consumed |
| `team_name` populated on a non-team session | sample-session-meta.json | `team_name="team-wTRMM"` even though `is_subagent=0` and no parent — the field is opt-in but always-populated when set |
| `<user_input mode="act">` wrapper | msgs 1, 5 | Strip the wrapper; capture the `mode` attribute as metadata (act / plan / chat) |

**Notably absent from this corpus** (deliberate — would need more
elaborate test prompts): every other tool in the 28-tool taxonomy.
The remaining 9 core tools (`apply_patch`, `ask_question`, `editor`,
`fetch_web_content`, `run_commands`, `search_codebase`, `skills`,
`spawn_agent`, `submit_and_exit`) and all 18 `team_*` tools must be
covered synthetically in `normalize_test.go`.

**Also absent**: real subagent rows (corpus has 0 `parent_session_id`
sessions and 0 `subagent_spawn_queue` rows), team mailbox/outcome
content (`teams.db` is shape-intact but empty of run data in this
capture), cron specs (`cron.db` likewise empty).

## Reproducing the dumps

```bash
# In WSL Ubuntu (Windows-side Cline CLI install reached via /mnt/c)
LIVE=/mnt/c/Users/marmu/.cline/data
DEST=/mnt/d/programsx/superbased-observer/testdata/clinecli
SNAP=/tmp/cline-snap

# 1. Snapshot the SQLite DBs so the live cline process can't hold an
#    exclusive lock during the dump. Copy main + WAL + SHM so the
#    snapshot is consistent.
mkdir -p "$SNAP"
cp "$LIVE/db/sessions.db" "$LIVE/db/sessions.db-wal" "$LIVE/db/sessions.db-shm" "$SNAP/"
cp "$LIVE/db/teams.db"    "$LIVE/db/teams.db-wal"    "$LIVE/db/teams.db-shm"    "$SNAP/"
cp "$LIVE/db/cron.db"     "$LIVE/db/cron.db-wal"     "$LIVE/db/cron.db-shm"     "$SNAP/"

# 2. Dump schemas only.
sqlite3 "$SNAP/sessions.db" '.schema' > "$DEST/sessions.sql"
sqlite3 "$SNAP/sessions.db" '.schema sessions' > "$DEST/schema-sessions.sql"
sqlite3 "$SNAP/teams.db"    '.schema' > "$DEST/teams.sql"
sqlite3 "$SNAP/cron.db"     '.schema' > "$DEST/cron.sql"

# 3. Anonymise the per-session JSON via the Python helper.
python3 .tmp-run/clinecli_anonymize.py
```

The hooks JSONL sample is hand-synthesised (the live `hooks.jsonl` is
empty until the operator registers a hook command file or runs a
subagent) — see plan §6 for the documented payload shapes per event.

## Notes for the implementing session

These are the §15 reality-check candidates we'd write back into
`docs/plans/cline-cli-adapter-plan-2026-06-06.md` after Phase 1:

1. **Per-message `metrics` exists** — plan §4 + §7 missed this. Each
   assistant message in `<id>.messages.json` carries its own
   `inputTokens / outputTokens / cacheReadTokens / cacheWriteTokens /
   cost` block. So Tier 2 capture can be **per-message**, not just
   per-session aggregate (the plan said v1 ships session-level only).
   Per-call attribution should track the per-message metrics directly;
   the session-level `metadata_json.usage` is redundant.

2. **Per-message `modelInfo`** — `{id, provider, family?}`. Lets us
   capture per-message model + provider when Cline switches mid-session.
   Plan §3 only mentioned the session-level `sessions.provider` /
   `sessions.model` columns.

3. **`tool_result.content` is a list of structured dicts** — each item
   has `{query, result, success}` keys for `read_files` (plural inputs).
   `success` is per-result-item, not just on the tool_result wrapper.
   The plan §4 said "structured JSON" but didn't enumerate the per-item
   keys. Affects how parse.go extracts errors — needs to read both
   `is_error` on the wrapper AND `success: false` on per-item content
   to flag failures correctly.

4. **`agent: "lead"`** — confirms the plan's parent/subagent model.
   Lead sessions have `agent="lead"`; subagent sessions will have
   `agent="<some-name>"`. The corpus didn't exercise a subagent so the
   non-lead string is unobserved.

5. **Schema fresh-install reality** — `PRAGMA user_version` is 0, no
   migration history applied. Confirms fresh-install side of the
   "v1 vs. legacy-migrated" distinction the plan §3 flagged. We still
   need to test against a migrated install (some unit test fixtures
   that simulate the 12-ALTER-TABLE migration arc) before declaring
   v1 done.

6. **`team_name` populated on non-team sessions** — the live session
   has `team_name="team-wTRMM"` with `is_subagent=0` and no parent
   linkage. The field is set on session start whenever the workspace
   has a team config, regardless of whether team tools fire. Adapter
   should surface `team_name` to `ActionMetadata` but NOT treat it as
   a subagent linkage signal.

7. **`messages.json` write granularity** — plan §10/§14 question 1 is
   "write-amplification — is the whole file rewritten on every event?"
   This corpus is too small (462 KB messages.json from a 6-turn
   session) to answer. To verify, a longer session (50+ turns) needs
   to be captured with `inotifywait -m` on the messages.json file
   running concurrently. Deferred to Phase 2 commit 6 (the
   content-block walker) when we ship the messages.json watcher.

## Off-limits files (NEVER commit, NEVER read)

Per plan Appendix B:

- `~/.cline/data/settings/providers.json` — carries WorkOS OAuth
  tokens + refresh tokens + per-provider API keys (OpenRouter,
  Anthropic, OpenAI, etc.). The adapter never reads this file; the
  provider / model fields on `sessions` provide everything we need.
- `~/.cline/data/cache/user_input_history.jsonl` — cross-session
  prompt history.
- `~/.cline/data/secrets.json` — discovered during fixture capture,
  NOT in the original plan Appendix B. 1409 bytes at the root of
  `data/`; presumed to carry secrets. **Add to plan Appendix B in the
  Phase 1 reality-check update.**
- `~/.cline/data/globalState.json` — 2487 bytes at the root of `data/`;
  may carry workspace-spanning state. Not currently planned for use;
  inventory it if a v2 feature needs it.
