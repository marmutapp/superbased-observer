# Open Interpreter fixtures

Captured 2026-07-29 from a live WSL install (`interpreter 0.0.28`,
`~/.openinterpreter/`). The only *fresh* data on the box; the Windows
mirror (`/mnt/c/Users/auzy_/.openinterpreter/`) has never recorded an
interactive session (its `state_5.sqlite::threads` is empty — see the
plan doc §5) so no Windows session fixture exists.

**Headline finding**: Open Interpreter (`interpreter` binary,
`docs.openinterpreter.com`) is a rebrand of OpenAI's own Codex CLI —
same Rust crates (`codex_core`, `codex_tui`, `codex_app_server`,
`codex_api`, `codex_mcp`, `codex_http_client`, …), same on-disk layout
(`sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl`, `state_N.sqlite`
threads index), same event schema (`session_meta` / `event_msg` /
`response_item` / `turn_context` / `world_state`), same `token_count`
field names (`input_tokens` / `cached_input_tokens` /
`reasoning_output_tokens` / `total_token_usage`), same OAuth
`auth.json` shape (`auth_mode`/`tokens.{id_token,access_token,
refresh_token,account_id}`), even the same internal rate-limit bucket
name (`rate_limits.limit_id: "codex"`, un-rebranded leftover) and the
same `$CODEX_HOME`-style env-var pattern (renamed `INTERPRETER_HOME`,
defaulting to `~/.openinterpreter`). System/base instructions literally
say "You are Codex, an agent based on GPT-5." `interpreter --help`
itself titles every subcommand "Codex" (`exec — Run Codex
non-interactively`, `doctor — Diagnose local Codex installation, …`).
See `docs/plans/openinterpreter-adapter-plan-2026-07-29.md` for full
evidence and the adapter design.

## Contents

| File | Source | Notes |
|---|---|---|
| `sessions/2026/07/17/rollout-2026-07-17T14-59-49-019f6f69-23a0-7e32-bdba-9a9fabc946be.jsonl` | `~/.openinterpreter/sessions/2026/07/17/` | Full session, copied verbatim (25 lines, 58KB). Scanned clean for `sk-`/`Bearer`/`api_key`/emails before copying — none found. This session's `cwd` happens to be this repo (`/home/marmutapp/superbased-observer`) — real content, not fabricated, but nothing sensitive. |
| `config-wsl.toml` | `~/.openinterpreter/config.toml` | Per-project trust-level table only. No secrets. |
| `config-windows.toml` | `/mnt/c/Users/auzy_/.openinterpreter/config.toml` | Different project (`c:\programsx\regulation`) + `[windows] sandbox = "unelevated"`. No secrets. |
| `version.json` | `~/.openinterpreter/version.json` | Update-check cache. |
| `goals_1.schema.sql` | `.schema` dump of `~/.openinterpreter/goals_1.sqlite` | Empty (0 rows) — token-budget/goal tracking feature, unused in this install. |
| `logs_2.schema.sql` | `.schema` dump of `~/.openinterpreter/logs_2.sqlite` | Schema only. 2047 rows of Rust `tracing` debug/trace logs (hyper connection pool, `codex_*` module spans, MCP plumbing). Row **content** deliberately NOT copied — several rows carry `auth_header_attached=true auth_header_name="authorization" auth_mode="Chatgpt"` metadata (header *names*, not values, but excluded out of caution per the task's credential-material rule). Not a conversation/token store; see plan doc §5 for why it's out of scope for the adapter. |
| `memories_1.schema.sql` | `.schema` dump of `~/.openinterpreter/memories_1.sqlite` | Empty (0 rows) — memory-distillation feature, unused. |
| `state_5.schema.sql` | `.schema` dump of `~/.openinterpreter/state_5.sqlite` | Session **index** (not content) — one row per thread pointing at its `rollout_path`. |
| `state_5.sample_row.sql` | Query output | The one live row, all columns except the giant `sandbox_policy` JSON blob (replaced with its `.type` discriminator, `"managed"`) — that blob is a filesystem-permission policy tree, not secret, but is large/noisy and adds nothing the JSONL doesn't already carry. |

## NOT copied (deliberately)

- `auth.json` — OAuth token material (`id_token`/`access_token`/
  `refresh_token`/`account_id`). Never touched beyond `python3 -c
  json.load(...).keys()` to confirm field NAMES only.
- `logs_2.sqlite` row bodies (see above).
- `models_cache.json`, `plugins/`, `skills/`, `shell_snapshots/`,
  `packages/` — vendored/cache data, irrelevant to the adapter.
- Windows `cap_sid` — Windows ACL SIDs (not credentials, but
  irrelevant and mildly identifying; skipped).
- The `.tmp/plugins/` git checkout (a `pypi`-scale plugin marketplace
  mirror) — irrelevant.

## Regenerating

```bash
sqlite3 'file:~/.openinterpreter/<db>.sqlite?mode=ro&immutable=1' '.schema'
```

Never open these DBs for write; the live install may hold a WAL lock.
