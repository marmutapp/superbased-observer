# SuperBased Observer

A single Go binary that captures, normalizes, compresses, and provides
intelligence about AI coding tool activity across **Claude Code**, **OpenAI
Codex**, **Cursor**, **Cline / Roo Code**, **GitHub Copilot**, **OpenCode**,
**OpenClaw**, and **Pi**.

Knowledge captured from one tool benefits all the others working on the same
project — data is organized by git root, not by tool. A read by Claude Code
becomes a freshness signal for Codex; a Cursor compaction is visible from
Cline; cost numbers from every adapter roll up to a single dashboard.

---

## Why this exists

**You can't optimize what you can't see.** AI coding assistants are
expensive (often the second-largest line item on a developer's monthly
spend) and the providers' billing dashboards are coarse — they tell you
*how much* you spent, not *what* you spent it on or *why*.

Observer answers questions like:

- Where did this week's $147 Claude bill come from? Which projects, models,
  sessions, tool calls?
- Did I spend more on Opus or Sonnet? Are my Sonnet sessions hitting the
  long-context tier and getting repriced at 2×?
- How much did I waste re-reading files that hadn't changed since the last
  read in the same session?
- Is my cache hit rate (cache_read / cache_creation) trending down? Am I
  paying to cache prompts that never get reused?
- Could that trivial Opus session have been done by Sonnet for 1/5 the
  cost? (Answer: probably yes — see the routing-efficiency band on the
  Analysis tab.)
- Across Claude Code and Cursor and Codex working in the same repo, what
  files are touched by all three? Where are they stepping on each other?

It does this by sitting **passively** alongside your tools: parsing their
session logs, optionally proxying their API calls for accurate token
counts, and exposing the result through a local web dashboard, an MCP
server (so the tools themselves can query it), and a CLI.

Everything is **local**. The observer never makes a network call on its
own; only the optional API proxy forwards your requests to the upstream
providers exactly as they came in.

---

## What you get

### A unified cost dashboard

```
$ observer dashboard
dashboard listening on http://127.0.0.1:8081 — ctrl-c to stop
```

The dashboard has ten tabs. The substantive ones for cost-conscious
users:

- **Analysis** — six headline KPI tiles (period-over-period spend, MTD
  + projection vs your monthly budget, effective $/1M-token rate, cache
  efficacy, long-context surcharge, Discovery waste $); a daily stacked
  trend chart with a Model / Project / Tool dimension toggle; movers
  table (top 5 cost increases, top 5 decreases, new entrants this
  period); top expensive sessions with explanatory badges (`opus`,
  `lc_tier`, `many_turns`, `large_prompt`); routing-efficiency
  suggestions for trivial Opus sessions that could have used Sonnet.

- **Cost** — per-model token consumption split into the four billable
  buckets (net input, cache read, cache write, output) with computed
  dollar cost. Hover any column header for its definition + formula.

- **Sessions** — one row per session, click for action breakdown +
  per-message timeline + token costs, with click-through to drill into
  individual upstream API turns.

- **Discovery** — the waste detector: stale re-reads, repeated commands
  that ran with no relevant inputs changed, files touched by multiple
  AI clients in the same window.

- **Settings** — fully editable configuration (see below).

### Per-message timeline + LC-tier-aware pricing

Every Claude Code session displays as one row per upstream API message,
expandable to show the contained tool calls. Token costs are computed
**per-turn**: a session's cache_creation tokens that landed in the 1h
ephemeral tier are billed at 2× the 5m rate, and Sonnet 4 / 4.5 turns
whose prompt window exceeds 200K tokens are repriced at the long-context
tier ($6 input / $22.50 output instead of $3 / $15). Same modeling for
gpt-5.4 / 5.5 (>272K) and Gemini 2.5 Pro / 3.1 Pro (>200K). See
`observer cost` for a CLI version of the breakdown.

### A freshness engine

Every file the AI reads is hashed and timestamped. When the same session
re-reads a file, the observer knows whether the content changed since
the last read. Stale re-reads are surfaced on the Discovery tab with an
estimated $ value (token count × your blended input rate) so you can see
which habits are costing you.

### An MCP server with 12 tools

Once registered (`observer init`), every connected AI tool can query
the observer over MCP/stdio:

- `check_file_freshness` — has this file changed since I last read it?
- `get_file_history` — every read/edit of this file across every tool +
  session, plus codegraph enrichment (functions defined, callers,
  imports) when a graph DB is available
- `get_session_summary` — what did session X actually do? AI-generated
  2–4 sentence summaries
- `search_past_outputs` — full-text search of past tool-call outputs
- `check_command_freshness` — did this exact command already run? With
  what result?
- `get_session_recovery_context` — for resuming an interrupted session
- `get_project_patterns` — derived behaviours (hot files, co-changes,
  edit→test pairs)
- `get_last_test_result` — without re-running
- `get_failure_context` — error correlation + retry detection
- `get_action_details` — the raw row, scrubbed
- `get_cost_summary` — per-window spend rollup
- `get_redundancy_report` — what would Discovery flag for this project?

### A multi-layer compression pipeline

- **Shell output filters** — RTK-style truncation of large `bash` /
  `git` / `go test` / `docker` / `kubectl` / `cargo` / `pytest` outputs
  inline before they hit the LLM context.
- **Tool output indexing** — every tool call's output gets indexed into
  FTS5; large outputs are trimmed to a 2KB excerpt cap so the index
  stays compact and `search_past_outputs` stays fast.
- **Conversation compression (proxy)** — when the API proxy is engaged,
  the proxy rewrites large `tool_result` blocks before forwarding to
  Anthropic / OpenAI. Importance-scored, prefix-stable for cache
  alignment. Bytes saved + token-equivalent + dollar-equivalent are
  visible on the Compression tab.

Each layer is independently toggleable from the Settings → Compression
tab.

### A backfill system

When the binary upgrades and adds new schema columns or adapter
behaviour, historical rows in your database don't automatically gain
the new fields. `observer backfill --all` re-walks every adapter's
source files and brings old rows up to current shape. The Settings →
Backfill tab surfaces the same modes as a clickable table with
candidate-row counts (where SQL-checkable) and per-mode Run buttons
that spawn the CLI as a child process and stream output back live.

---

## Install

### Via npm (recommended — pinned version, automatic platform binary)

```bash
npm install -g @superbased/observer
observer --version
```

### Via go install (latest main, builds locally)

```bash
go install github.com/marmutapp/superbased-observer/cmd/observer@latest
observer --version
```

The binary is pure Go — no CGO, no external runtime dependencies. SQLite
storage is pure-Go via `modernc.org/sqlite`.

---

## First-run walkthrough

```bash
# 1. Register hooks + MCP entries with every detected AI tool.
#    Patches ~/.claude/settings.json, ~/.cursor/hooks.json,
#    ~/.claude.json, ~/.cursor/mcp.json, ~/.codex/config.toml.
#    Records a SHA256 of each touched file in
#    ~/.observer/hook_checksums.json so uninstall can detect drift.
observer init --all

# 2. Backfill from existing session logs so the dashboard has history
#    immediately rather than starting empty.
observer scan

# 3. Run the live watcher daemon + dashboard + (optionally) the proxy.
#    Foreground; ctrl-c to stop. Dashboard is at http://localhost:8081.
observer start

# In another shell:
observer dashboard --port 8081       # if you didn't run `start`
open http://localhost:8081           # browse the dashboard
```

For accurate token counts (rather than parsing whatever the JSONL
adapter could see), point your AI tool at the proxy:

```bash
export ANTHROPIC_BASE_URL=http://localhost:8820
export OPENAI_BASE_URL=http://localhost:8820/v1
# Restart the AI tool — it'll route through the proxy from now on.
```

The proxy logs every turn with the exact token counts the provider
returned, including cache-tier breakdowns (5m vs 1h ephemeral) and
1h surcharges that the JSONL adapters can't always disambiguate.

### Verifying the install

```bash
observer doctor          # health checks: DB integrity, hook registration,
                         # MCP entries, pid bridge, schema migrations
observer status          # row counts + recent activity
observer tail            # live-stream captured actions to the terminal
```

---

## Dashboard tabs at a glance

| Tab | What it shows |
|---|---|
| **Overview** | KPI tiles (sessions / API turns / token rows / 24h failures) + cost-over-time + actions-over-time + top models + top tools. |
| **Cost** | Per-model token consumption table with USD cost + per-day per-model stacked bars. Hover columns for definitions. |
| **Analysis** | Six KPI tiles + dimensional trend chart + period-over-period movers + top expensive sessions + routing-efficiency suggestions. |
| **Sessions** | One row per session. Click → action breakdown + per-message timeline + token costs. |
| **Actions** | The flat firehose — every recorded tool call, normalized across adapters. |
| **Tools** | Per-AI-client (claude-code / cursor / codex / cline / copilot / opencode / openclaw / pi) breakdown — when each tool was active and what kind of work it did. |
| **Compression** | Bytes saved by each compression layer + per-model breakdown of conversation-compression savings. |
| **Discovery** | Stale re-reads, repeated commands, cross-tool files. The token-waste estimator runs here. |
| **Patterns** | Derived behaviours fed into `observer suggest` for CLAUDE.md / AGENTS.md / .cursorrules. |
| **Settings** | Fully-editable config. Pricing hot-reloads; other sections need a daemon restart (one-click via the banner that appears after save). |

---

## Settings tab

Every section of `config.toml` is editable from the dashboard:

### Pricing (hot-reloads — no restart)

Table-form editor. Rows = your active overrides; cols = Input / Output /
Cache Read / Cache Creation 5m / Cache Creation 1h. A chevron toggles
six long-context fields per row (threshold + LC-tier rates for each
dimension). Add Override prompt auto-fills from the 95 baked-in default
models when the id matches; custom ids work too. Defaults reference
list at the bottom — every baked-in model with an Override shortcut.
Save writes `config.toml` (with a `.bak` of the prior version) and
`cost.Engine.Reload`s the active table via `atomic.Pointer.Store` —
in-flight cost queries see either the old or new table, never a torn
state.

### Backfill (CLI runs spawned from the UI)

Table of all 14 documented `observer backfill` modes with candidate
counts (SQL-checkable: `is-sidechain`, `cache-tier`, `message-id`) or
"file scan needed" (per-adapter source scans). Per-row Run button +
Run All. Output panel below the table renders captured stdout
incrementally as the subprocess runs. Multiple modes can run
concurrently; the daemon's job registry survives until restart.

### Observer / Watcher / Freshness / Retention / Hooks / Proxy / Compression / Intelligence

Schema-driven per-field forms with checkboxes for booleans, number
inputs for ints/floats, selects for enumerable strings (`log_level`,
conversation `mode`), and comma-separated text inputs for string lists
(adapters, ignore patterns, compress types). Each field has inline
help text describing what it does. Save writes the file; a "Restart
daemon" banner appears at the top of the Settings tab with a confirm
dialog because the proxy / watcher / hook registry consumers bind
these values at startup.

`POST /api/admin/restart` schedules `os.Exit(0)` 500 ms after returning
so the response lands. Whether the daemon comes back depends on your
supervisor — the npm wrapper handles this, foreground shells need a
manual relaunch.

---

## Configuration

Default location: `~/.observer/config.toml`. Override with `--config`.

A minimal config:

```toml
[observer]
db_path = "~/.observer/observer.db"
log_level = "info"

[proxy]
enabled = true
port = 8820
anthropic_upstream = "https://api.anthropic.com"
openai_upstream = "https://api.openai.com"

[intelligence]
monthly_budget_usd = 100  # surfaces on the Analysis tab; 0 hides

[compression.shell]
enabled = true
exclude_commands = ["curl", "playwright"]

[compression.indexing]
enabled = true
max_excerpt_bytes = 2048

[compression.conversation]
enabled = false           # opt-in; modifies request bodies in flight
mode = "token"
target_ratio = 0.85
preserve_last_n = 5
compress_types = ["json", "logs", "text"]

# Pricing overrides — only specify what differs from baked-in defaults.
[intelligence.pricing.models."claude-sonnet-4-6"]
input = 3.0
output = 15.0
cache_read = 0.30
cache_creation = 3.75
cache_creation_1h = 6.00

# Long-context tier (Anthropic Sonnet 1M, gpt-5.4/5.5 >272K, Gemini 2.5
# Pro >200K). When the prompt window exceeds the threshold, every rate
# is replaced with its long_context_* counterpart for that turn.
[intelligence.pricing.models."claude-sonnet-4-5"]
input = 3.0
output = 15.0
cache_read = 0.30
cache_creation = 3.75
cache_creation_1h = 6.00
long_context_threshold = 200000
long_context_input = 6.0
long_context_output = 22.50
long_context_cache_read = 0.60
long_context_cache_creation = 7.50
long_context_cache_creation_1h = 12.00
```

Every key has a TOML environment-variable override of the form
`OBSERVER_<SECTION>_<KEY>` (uppercased, underscores). Nested sections
join with extra underscores: `OBSERVER_COMPRESSION_CONVERSATION_ENABLED=true`.

---

## Post-upgrade hygiene + recovery

After upgrading the binary, or any time the dashboard's Actions tab
looks gappy (a fall-behind watcher, a daemon restart with stale state,
fsnotify event drops on a busy session):

```bash
observer backfill --all
```

This first re-walks every JSONL the adapters know about from offset 0
(`observer scan --force`'s recovery path) and ingests everything the
live watcher might have dropped, then runs the surgical column-update
backfills (cache-tier, message-id, etc.) on top. The
`(source_file, source_event_id)` UNIQUE index keeps the pass
idempotent — already-present rows are no-ops; missing rows get
inserted.

Granular flags (`--cache-tier`, `--message-id`, `--codex-reasoning`,
`--opencode-parts`, `--opencode-tokens`, `--openclaw-model`,
`--openclaw-reasoning`, `--openclaw-action-types`, `--cursor-model`,
`--copilot-message-id`, `--pi-message-id`, `--claudecode-user-prompts`,
`--claudecode-api-errors`, `--is-sidechain`) are available for
targeted re-runs; `observer backfill --help` lists every supported
dimension. The same modes are available as click-to-run buttons on
the Settings → Backfill tab — Run All on that tab does the same scan
+ backfill chain as the CLI's `--all`, with a live toast showing
phase-by-phase progress.

The dashboard surfaces a top-of-page banner whenever the watcher's
parse cursor for any session file is more than 10 KB behind the
on-disk size, so the silent-data-loss case has a visible signal
before you start hunting for missing rows. Click the banner to land
on Settings → Backfill with the recovery affordance pre-pointed.

`observer backfill --help` lists every supported flag with the
schema column or row type each one recovers.

---

## CLI reference

| Command | Purpose |
|---|---|
| `observer init [--all]` | Register hooks + MCP server with every detected AI tool. |
| `observer uninstall [--all] [--purge]` | Reverse of `init`. Refuses to touch drifted configs unless `--force`. `--purge` also deletes `~/.observer/`. |
| `observer scan [--force]` | One-time backfill — parse all known session files into the DB. `--force` ignores the saved parse cursors and re-walks every file from offset 0; the recovery path when the live watcher silently fell behind. |
| `observer watch` | Live fsnotify-based watcher daemon. |
| `observer start` | Proxy + watcher + dashboard in one foreground process (`--no-dashboard` to skip). |
| `observer proxy start` | Run only the API reverse proxy. |
| `observer dashboard [--port N]` | Embedded HTML + `/api/*` JSON endpoints on http://localhost:N (default 8081). |
| `observer cost [--days N] [--group-by model\|session\|day\|project\|tool]` | Token + USD rollup from the CLI. |
| `observer discover` | Print stale re-reads + redundant-commands report. |
| `observer patterns` | Derive hot files, co-changes, common commands, edit→test pairs, onboarding sequences. |
| `observer learn` | Derive correction rules from failure→recovery pairs. |
| `observer suggest` | Compose patterns + corrections into CLAUDE.md / AGENTS.md / .cursorrules. |
| `observer summarize` | Generate AI session summaries (uses Anthropic Haiku). |
| `observer score` | Session quality scoring (error rate, redundancy, onboarding cost, retry cost). |
| `observer status` | Row counts + recent activity. |
| `observer tail` | Live-stream captured actions. |
| `observer doctor` | Health checks: DB integrity, hook checksums, MCP drift, pid bridge. |
| `observer prune` | Run retention now (delete old actions, orphaned sessions, stale logs). |
| `observer metrics [--port N]` | Prometheus `/metrics` endpoint (29 gauge families). |
| `observer export {json\|csv\|xlsx} [--out PATH]` | Dump tables for external analysis. |
| `observer backfill --<mode>` | Re-populate columns added by later migrations. `--all` runs every mode. |
| `observer run <command> [args...]` | Run a command with its stdout streamed through the shell-output filter. |
| `observer hook <tool> <event>` | Hook entrypoint — receives the tool's event on stdin, replies on stdout. |
| `observer serve` | MCP stdio server (spawned by AI tools after `init`). |

`observer <command> --help` for full flag listings.

---

## Architecture

```
┌──────────────┐     ┌──────────────┐     ┌──────────────┐
│  Claude Code │     │    Cursor    │     │    Codex     │   ... 9 adapters
└──────┬───────┘     └──────┬───────┘     └──────┬───────┘
       │ JSONL              │ hook events       │ rollout files
       ▼                    ▼                    ▼
┌────────────────────────────────────────────────────────┐
│              fsnotify Watcher + Adapters                │
│       (one parser per platform, normalized output)      │
└────────────────────────┬───────────────────────────────┘
                         │ ToolEvent / Action / Session
                         ▼
┌────────────────────────────────────────────────────────┐
│   SQLite (WAL, pure-Go via modernc.org/sqlite)          │
│   actions · sessions · projects · file_state ·          │
│   api_turns · token_usage · failure_context ·           │
│   compaction_events · action_excerpts (FTS5)            │
└────────────────────────┬───────────────────────────────┘
                         │
        ┌────────────────┼────────────────────────┐
        ▼                ▼                        ▼
┌──────────────┐  ┌──────────────┐       ┌──────────────┐
│   Dashboard   │  │  MCP Server  │       │   API Proxy  │
│  HTTP+/api/*  │  │ stdio · 12   │       │  Anthropic + │
│  10 tabs      │  │  tools       │       │  OpenAI      │
└──────────────┘  └──────────────┘       └──────────────┘
                                                 │
                                                 ▼
                                          upstream API
                                          (your traffic
                                           passes through
                                           verbatim, with
                                           token tracking
                                           + optional
                                           compression)
```

Adapters parse each platform's session format into a unified `Action`
row. The cost engine reads `api_turns` (proxy, accurate) and
`token_usage` (JSONL adapters, approximate) and deduplicates per-turn
via the upstream `request_id` ↔ `source_event_id` match. The MCP
server exposes 12 tools that query the same database. The dashboard
pulls JSON from the same `/api/*` endpoints the MCP tools use.

---

## Build from source

```bash
git clone https://github.com/marmutapp/superbased-observer
cd superbased-observer
make build        # builds bin/observer
make test         # full test suite (race detector enabled)
make all          # fmt + vet + lint + test + build
```

Requirements: Go 1.22+. No CGO, no external services. SQLite via
`modernc.org/sqlite` (pure Go). golangci-lint optional for `make
lint`.

The `bin/observer` binary embeds the dashboard HTML/JS via `//go:embed`,
so a release artifact is a single static file you can `scp` and run.

---

## Contributing

PRs welcome. The codebase has table-driven tests in every package and
holds itself to a >80% coverage bar on core packages (cost, freshness,
adapters, store). Run `make test` before submitting; `make all`
locally to catch fmt/vet/lint issues. Conventional commits
(`feat:` / `fix:` / `chore:` / `docs:` / `test:` / `refactor:`).

For larger changes, open an issue first to align on scope. Adapter
contributions (a new AI coding tool to support) are particularly
welcome — see the existing adapters in `internal/adapter/` for the
pattern.

---

## License

Apache 2.0 — see `LICENSE`.
