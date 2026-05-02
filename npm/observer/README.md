# @superbased/observer

[![npm](https://img.shields.io/npm/v/@superbased/observer.svg)](https://www.npmjs.com/package/@superbased/observer)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://www.apache.org/licenses/LICENSE-2.0)
[![Platforms: Linux • macOS • Windows](https://img.shields.io/badge/platforms-linux%20%7C%20macos%20%7C%20windows-blue.svg)](https://github.com/marmutapp/superbased-observer)

**Capture, normalize, compress, and analyze every AI coding tool call you
run** — across Claude Code, Codex, Cursor, Cline / Roo Code, GitHub
Copilot, OpenCode, OpenClaw, and Pi — in one local single-binary
tool. No telemetry, no cloud, no data leaves your machine.

```
  ┌──────────────┐    ┌──────────────────────────────────┐
  │  AI clients  │ ─▶ │ observer proxy   :8820 (local)   │ ─▶ Anthropic / OpenAI
  │  (Claude     │    │   • intercepts every API call    │
  │   Code,      │    │   • compresses conversations     │
  │   Cursor,    │    │   • records api_turns + tokens   │
  │   Codex …)   │    └──────────────┬───────────────────┘
  │              │                   ▼
  │              │ ◀── observer mcp (stdio JSON-RPC) ◀── 12 cross-client tools
  └──────┬───────┘
         │ JSONL session logs
         ▼
  ┌──────────────────────────────────────────────────────────────┐
  │  observer watch    SQLite store    :8081 dashboard           │
  │   • normalizes tool calls            • cost over time        │
  │   • freshness, redundancy detection  • compression savings   │
  │   • patterns, suggestions            • discovery / waste     │
  └──────────────────────────────────────────────────────────────┘
```

# Table of contents

- [Install](#install)
- [Five-minute quickstart](#five-minute-quickstart)
- [Per-AI-client setup](#per-ai-client-setup)
- [Architecture in detail](#architecture-in-detail)
- [Dashboard tour](#dashboard-tour)
- [MCP tools reference](#mcp-tools-reference)
- [Compression mechanisms](#compression-mechanisms)
- [Cost and token math](#cost-and-token-math)
- [Terminology and glossary](#terminology-and-glossary)
- [CLI reference](#cli-reference)
- [Configuration](#configuration)
- [Troubleshooting](#troubleshooting)
- [Security and privacy](#security-and-privacy)
- [Source, contributing, license](#source-contributing-license)


## Install

**Use a global install** (`-g`) so the `observer` command is available
on your `$PATH` from any directory:

```bash
npm install -g @superbased/observer
observer --version
```

If you install locally (without `-g`) the binary lives at
`./node_modules/.bin/observer` and isn't on your `$PATH`. Run it
with `npx`:

```bash
npm install @superbased/observer    # local install
npx observer --version              # ↑ what to use everywhere `observer` is shown below
```

A note for shared / CI machines where `npm install -g` may need
`sudo`: see [Troubleshooting → EACCES](#npm-install--g-fails-with-eacces-permission-denied)
for user-writable-prefix and version-manager fixes.

Pre-built binaries ship for:

| Platform              | Architecture |
|-----------------------|--------------|
| Linux                 | x64, arm64   |
| macOS (Intel)         | x64          |
| macOS (Apple Silicon) | arm64        |
| Windows               | x64          |

The package uses the `optionalDependencies`-per-platform pattern (same
shape as `esbuild` / `swc` / `@biomejs/biome`) — only the binary
matching your machine downloads. No postinstall network calls, no
compile step.

If your platform isn't listed, build from source — instructions in
the [main repo](https://github.com/marmutapp/superbased-observer).


## Five-minute quickstart

```bash
# 1) Install + register hooks with whichever AI clients you use.
npm install -g @superbased/observer
observer init                 # interactive: pick clients

# 2) Start the long-running services (watcher + dashboard + proxy).
observer start &              # all three in one process
                              # (or run them separately, see CLI ref)

# 3) Engage the proxy by pointing your AI client at the local URL.
#    See "Per-AI-client setup" for the matching env var.
export ANTHROPIC_BASE_URL=http://127.0.0.1:8820     # Claude Code
export OPENAI_BASE_URL=http://127.0.0.1:8820/v1     # Codex / OpenAI

# 4) Open the dashboard.
open http://127.0.0.1:8081/   # macOS
xdg-open http://127.0.0.1:8081/   # Linux
start http://127.0.0.1:8081/   # Windows
```

After ten minutes of normal AI-coding usage, the dashboard will be
populated with cost over time, per-tool activity, compression
savings, and stale-reread waste signals.


## Per-AI-client setup

Different clients send to different upstreams. The local proxy on
`127.0.0.1:8820` handles all of them — routes by URL path. Set the
env var that matches your client(s); both can coexist on one machine.

| AI client                       | Env var                                               | Capture mode |
|---------------------------------|-------------------------------------------------------|--------------|
| **Claude Code**                 | `ANTHROPIC_BASE_URL=http://127.0.0.1:8820`            | proxy + JSONL |
| **Cursor** (Anthropic mode)     | `ANTHROPIC_BASE_URL=http://127.0.0.1:8820`            | proxy + JSONL |
| **Codex**                       | `OPENAI_BASE_URL=http://127.0.0.1:8820/v1` (note `/v1`) | proxy + JSONL |
| **Cursor** (OpenAI mode)        | `OPENAI_BASE_URL=http://127.0.0.1:8820/v1`            | proxy + JSONL |
| **Cline / Roo Code**            | `ANTHROPIC_BASE_URL=...` or `OPENAI_BASE_URL=...` per provider | proxy + JSONL |
| **GitHub Copilot**              | (no proxy yet)                                        | JSONL only   |
| **OpenCode** ([opencode.ai](https://opencode.ai/)) | (no proxy yet)                     | SQLite — actual install path is `~/.local/share/opencode/opencode.db` (XDG). Captures **token counts + model + cost** per assistant message from OpenCode's InfoData (`tokens.input/output/reasoning/cache.{read,write}` + `cost`); **subtask** parts → `spawn_subagent` actions; **todo** table → `todo_update` actions; tool-name coverage extended to webfetch/websearch/task/todowrite/todoread/multiedit. Tagged `Source=jsonl, Reliability=approximate`. |
| **OpenClaw** ([openclaw.ai](https://openclaw.ai/)) | (no proxy yet)                     | JSONL + sqlite — `~/.openclaw/tasks/runs.sqlite` + `~/.openclaw/agents/<agent>/sessions/sessions.json` |
| **Pi** ([pi.dev](https://pi.dev/)) | (no proxy yet)                                       | JSONL — `~/.pi/agent/sessions/--<path>--/*.jsonl` (per upstream `docs/session-format.md` v3). Captures user / assistant / toolResult / `bashExecution` message roles; `usage.cost.total` → per-message USD; terminal `stopReason` (stop/length/error/aborted) → `task_complete` with `success=false` for failures (mid-turn `toolUse` is correctly skipped); `thinking` blocks surface as preceding reasoning. Tagged `Source=jsonl, Reliability=approximate`. |

**JSONL-only** clients are captured passively by the watcher (always-on,
no setup beyond `observer init`). You won't see real-time cost numbers
for them on the Compression tab (those need the proxy), but every tool
call shows up on Sessions / Actions / Discovery / Tools / Patterns and
the JSONL-derived token counts feed the Cost tab. Reliability tagging
is per-adapter: Claude Code emits `unreliable` (the JSONL stream uses
streaming-time placeholder counts per spec §24); Codex / Cline / Pi /
OpenCode / OpenClaw emit `approximate` (provider-reported usage that
hasn't been reconciled against an upstream invoice).

### Persistent setups

**Claude Code** (`~/.claude/settings.json`):

```json
{
  "env": { "ANTHROPIC_BASE_URL": "http://127.0.0.1:8820" },
  "hooks": { /* `observer init` writes these */ }
}
```

**Codex** (`~/.codex/config.toml`):

```toml
[env]
OPENAI_BASE_URL = "http://127.0.0.1:8820/v1"
```

**Shell rc** (`~/.bashrc` / `~/.zshrc`) — affects every program:

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8820
export OPENAI_BASE_URL=http://127.0.0.1:8820/v1
```


## Architecture in detail

Five components running side by side:

### 1. JSONL adapters (passive ingest)

Watch `~/.claude/projects/`, `~/.codex/sessions/`, etc. for new
session log lines. Normalize per-client tool names to a shared
taxonomy (`read_file`, `run_command`, `spawn_subagent`, …) and write
them into the actions table. **Always-on**, no setup required —
`observer init` registers the hooks once.

What this gets you: every tool call you've ever run, queryable.

### 2. API reverse proxy (active capture)

A localhost HTTP server (`127.0.0.1:8820`) you point your AI client
at via `ANTHROPIC_BASE_URL` / `OPENAI_BASE_URL`. Intercepts every
request before it hits Anthropic / OpenAI and:

- Records exact token usage from the upstream `usage` envelope (the
  most accurate cost source — proxy beats JSONL parsing here).
- Runs the **conversation compression pipeline** to trim large
  `tool_result` blocks and drop low-importance messages before
  forwarding upstream.
- Captures the `cost_usd` the upstream reports (when present).

What this gets you: ground-truth cost numbers and conversation
compression savings you can measure.

### 3. SQLite store

A single file at `~/.observer/observer.db`. Tables include:

- `projects`, `sessions`, `actions` — the taxonomy
- `api_turns` — one row per proxy-intercepted upstream request
- `token_usage` — JSONL-derived token-row events (deduped via spec §A1)
- `file_state` — content hashes for freshness classification
- `compression_events` — per-event compression detail (post-migration 010)
- `project_patterns` — derived patterns from `observer patterns`
- `failure_context`, `action_excerpts` — diagnostic data

Pure-Go via `modernc.org/sqlite`, no CGO. WAL mode by default.

### 4. Local dashboard (`:8081`)

Eight tabs covering: Overview, Cost, Sessions, Actions, Tools,
Compression, Discovery, Patterns. See [Dashboard tour](#dashboard-tour).

Static HTML + Chart.js. No analytics, no external requests.

### 5. MCP server (stdio)

12 read-only tools the AI client itself can call mid-conversation —
`check_file_freshness`, `get_last_test_result`, `search_past_outputs`,
etc. Powers cross-client tool sharing: if Claude Code ran `go test`,
Cursor's MCP query for the latest test result will return Claude
Code's run. See [MCP tools reference](#mcp-tools-reference).


## Dashboard tour

Open `http://127.0.0.1:8081/` after `observer start`. Eight tabs.

### Overview tab

High-level snapshot of the selected window:

- **KPI tiles**: Sessions count, API turns (proxy-captured), Token
  rows (JSONL-recovered), Failures (24h)
- **Cost over time** chart — daily token volume, split into the four
  billable buckets (net input / cache read / cache write / output)
- **Actions over time** chart — total actions vs failures
- **Top models (by tokens)** chart — top-8 models stacked by net
  input / cache read / output
- **Top tools (actions over time)** — per-AI-client stacked-area
  showing when each client is active

### Cost tab

Per-model breakdown over the selected window. Tokens split into the
four billing buckets, with computed dollar cost and a reliability
flag. Cost is always computed locally as `tokens × pricing_table[model]`
— neither Anthropic nor OpenAI returns cost in their API responses, so
the proxy can't capture upstream-billed cost. Reliability values:
`accurate` (proxy-captured tokens, exact rate), `approximate` (JSONL-
sourced tokens, rate may be a family-prefix fallback), `unreliable`
(Claude Code JSONL streaming placeholders, ~10% off output), `unknown`
(no pricing entry for the model). Two adapters — OpenCode and Pi —
write their own per-turn cost into `estimated_cost_usd`; the engine
uses those as-is when present.

Hover any column header for tooltip; click for the full definition
in the help drawer.

### Sessions tab

One row per AI-coding session. Each session has a stable ID, a tool
(claude-code / cursor / codex / cline / copilot), a working-directory
project, action count, sub-agent action count (when the session
spawned sub-agents via the `Agent` tool), per-session **Tokens** and
**Cost** columns, and — if `observer score` has run — quality / errors
/ redundancy ratios. The `~` suffix on Cost flags rows whose pricing
was tier-fallback rather than billing-grade ("accurate" reliability).

Click a row to open the session-detail modal:

- **Top tiles** — Tool, Started, Actions count (ok/fail), Cost.
- **Action breakdown** — chart of action_type counts.
- **Tokens** — the four billing buckets (Net Input / Cache Read /
  Cache Write / Output) for the whole session, per-turn-deduped
  (proxy preferred, JSONL fills gaps).
- **Per-model breakdown** — when a session uses multiple models
  (Claude Code's main + sub-agent dispatches always do), a row per
  model with its tokens and cost.
- **Messages** — per-message timeline keyed on the upstream
  Anthropic `msg_xxx`. Each row shows the message id, role, model,
  the message's own token bucket, cost, and a `N ▾` pill that
  expands inline to show the contained tool calls. Toggle radio at
  the top: **Tool messages only** (default — assistant turns with
  ≥1 tool call + user prompts) vs **All messages** (also pure-text
  assistant replies). Truncated IDs (session_id, message_id)
  show a dotted underline on hover and copy the full value to
  clipboard on click; truncated text fields (target, error message)
  click to expand in-place. Server-side paginated at 50/100/200
  messages per page (selectable in the panel footer) — keeps the
  browser responsive on multi-thousand-message sessions. Requires
  `observer backfill --message-id` on first upgrade for historical
  sessions to surface their parent message ids.

### Actions tab

The flat firehose: every recorded tool call, normalized across
adapters. Filter by action type (`read_file`, `write_file`,
`run_command`, `spawn_subagent`, `todo_update`, `mcp_call`, …).
Pagination caps at 50 rows per page; total count is shown next to
the heading.

### Tools tab

Per-AI-client (the *client* — claude-code / cursor / codex / etc., not
the per-tool name) aggregates plus three views:

- **KPI tiles**: Total actions, Distinct tools, Overall success rate,
  Busiest tool
- **Activity over time** stacked-area showing per-tool action volume
  per day
- **Action-type mix per tool** horizontal stacked bar — what each
  tool actually does (read_file vs edit_file vs run_command vs
  search_text vs spawn_subagent)
- The full per-tool aggregate table with first/last seen

### Compression tab

How many tokens and dollars the conversation-compression pipeline
saved by trimming requests before forwarding upstream:

- **KPI tiles**: Tokens saved (est.), Dollars saved (est.), Bytes
  saved, Turns compressed
- **Savings per day** chart — daily tokens-saved (left axis) and
  bytes-saved (right axis)
- **Savings by mechanism** stacked bar — segments per mechanism
  (json / code / logs / text / diff / html / drop). **Toggle the
  y-axis between tokens and bytes** with the chart-header switch.
- **Per-model breakdown table** — tokens saved ~, $ saved ~, bytes
  saved, saved %, turns, tool-results compressed, dropped, markers
- **Recent compression events** — paginated per-event detail with
  mechanism, original / compressed / saved bytes, message slot, importance
  score (for drops), and a **Source** column showing whether the
  event came from a main-thread or sub-agent runtime call

### Discovery tab

Wasted-effort signals:

- **Stale rereads** — files re-read after they changed inside the
  same session. KPI tiles show count, ~Tokens wasted, ~$ wasted (at
  your blended input rate), affected files. The **CROSS-THREAD**
  column flags re-reads that crossed the parent ↔ sub-agent
  boundary — these are the "pass content via Agent's prompt
  parameter" candidates.
- **Repeated commands** — commands run multiple times with no
  relevant inputs changed in between (e.g. you ran `go test` three
  times without editing anything between runs).
- **Cross-tool overlap** — files touched by ≥2 AI clients in the
  window (e.g. claude-code AND cursor both edited `auth.ts`). This
  is the visible side of cross-platform tool-call sharing via the
  MCP server.

### Patterns tab

Repeatable behaviours the observer noticed across your sessions —
"after running `go test`, you almost always run `go vet`", "when
working on `auth.ts`, you also touch `login.tsx`", etc. Each pattern
has a confidence score (decay-weighted: more observations + recent
observations push it higher) and an observation count.

`observer patterns` derives them; `observer suggest` writes the
high-confidence ones into `CLAUDE.md` / `AGENTS.md` / `.cursorrules`
so new sessions inherit your habits.

### Help drawer

Press `?` anywhere on the dashboard or click the **? Help** button in
the topbar. Every column header, KPI tile, chart label, and filter
control on every tab is annotated — hover any element to see a
one-liner tooltip; click to open the drawer at the matching glossary
entry.

The drawer has full descriptions, formulas, data sources, examples,
"why it matters", "what to do", and cross-links. Search at the top.
Deep-linkable via URL fragment — `#help/metric.stale_count` opens
the drawer at that entry.

Each compression mechanism (json / code / logs / text / diff / html
/ drop) has a "Full methodology · see more" expandable section
explaining the actual algorithm.


## MCP tools reference

Every AI client running through `observer init` gets these 12 tools
registered as an MCP server. They're **read-only** queries against
the unified database, so any agent can read any other agent's
recorded work — true cross-platform tool-call sharing.

| Tool                              | Purpose |
|-----------------------------------|---------|
| `check_file_freshness`            | Has this file been read in the current session? Has it changed since? |
| `get_file_history`                | Full read/edit history of a file across all sessions and clients |
| `get_session_summary`             | Roll-up stats for a session: action count, success rate, cost, token buckets |
| `search_past_outputs`             | Full-text search across recorded tool outputs (FTS5-indexed) |
| `get_last_test_result`            | Most recent `go test` / `npm test` / `pytest` etc. output |
| `get_failure_context`             | Recent failures: which command, which file, which session |
| `get_action_details`              | One specific action's full record (target, args, output excerpt) |
| `check_command_freshness`         | Has this command been run in the current session? With what result? |
| `get_session_recovery_context`    | Recent activity for resuming a paused session |
| `get_project_patterns`            | High-confidence patterns derived from this project's history |
| `get_cost_summary`                | Daily / per-model / per-session cost rollups |
| `get_redundancy_report`           | Stale rereads, repeated commands, cross-tool overlap for the project |

**Cross-tool sharing**: when `observer init` registers the MCP server
with Claude Code AND Cursor (and Codex…), all of them call the same
12 tools against the same database. Cursor's `get_last_test_result`
returns Claude Code's last test run; Codex's `check_file_freshness`
reflects edits made by Cursor.


## Compression mechanisms

The conversation-compression pipeline runs inside the proxy on every
upstream request. It tries to fit the request body within
`target_ratio × original_bytes` (default 0.85) without breaking the
conversation's referential integrity.

Two passes, in order:

### Pass 1 — per-content-type compression

Each `tool_result` block is sniffed for content type, then routed to
a content-aware compressor. Six compressors:

| Mechanism | What it does | When it fires |
|-----------|--------------|---------------|
| **json**  | Replaces every scalar value with a type sentinel (`"<string>"`, `"<number>"`, …) preserving structure (keys, arrays, nesting). Arrays of length > 1 collapse to one element with `_len: N`. | API responses, structured logs, telemetry exports. JSON tool_results are usually the biggest savings target. |
| **code**  | Heuristic skeleton: keeps top-of-file imports + signature lines (function / method / class / struct / interface / type), drops bodies. | Source code files. **Opt-in only** (excluded from default `compress_types`) because the model often needs to read full code. |
| **logs**  | Two-pass: collapses adjacent identical lines to `<line> [×N]`, then head+tail-truncates to 200 lines if still long. | Log-shaped output — `go test ./...`, `npm run build`, polling/retry loops. Lossless on distinct lines; only the truncate pass is lossy. |
| **text**  | Catch-all for content not classified as code/json/logs/diff/html. Head+tail truncation: keeps 40 + 40 lines on inputs over 80 lines. | Markdown bodies, README excerpts, narrative descriptions. |
| **diff**  | Strips unified-diff context beyond ±1 line of each change. Keeps every header, every `+`/`-` line, drops the rest with elision markers. | `git diff`, patch tool outputs. Lossless on changes; lossy only on the cheap-to-rebuild context. |
| **html**  | Three regex passes: strips `<script>`, `<style>`, and HTML comments. Tag attributes + visible text + structural elements survive. | `web_fetch` results pulling whole HTML pages — usually 80%+ scripts/styles. |

### Pass 2 — drop with marker

If Pass 1's compressed body is still over budget, the budget enforcer
ranks remaining messages by **importance score** (a deterministic
weighted sum) and drops the lowest-scored non-preserved ones until
the budget is met. Each dropped message is replaced by a single
marker block (a placeholder text) so the conversation flow stays
intact for the model.

**Importance score** = `0.4 × recency + 0.3 × reference + 0.15 × density + 0.15 × role`

- **Recency** = `(i+1) / n` — newest message scores 1.0, oldest scores 1/n
- **Reference** = `1.0` if any of the message's `tool_use_ids` is
  cited by a later `tool_result`, OR any of its `referenced_ids` points
  to a live tool_use; `0.0` otherwise. Tool-pair-live messages always
  get full weight regardless of position.
- **Density** = fraction of non-whitespace runes (whitespace-padded
  outputs get dropped first)
- **Role** = `system 1.0`, `user 0.9`, `assistant 0.7`, `tool 0.5`
  (tool outputs are the most-compressible by policy)

**Preserved messages** (never droppable):
- The last `PreserveLastN` messages (default 4)
- Any `system` role message
- **Tool-pair-live messages**: any message whose `tool_use_id` is
  referenced by a later tool_result (parent side), AND any
  `tool_result` message whose `referenced_id` points to a live
  tool_use (consumer side)

Tool-pair preservation is symmetric — dropping either side leaves an
orphan that Anthropic rejects with 400.

### Per-event detail

Every drop and every per-type compression is recorded as a row in
the `compression_events` table (post migration 010). The Compression
tab's "Recent compression events" view surfaces these with
mechanism, original / compressed / saved bytes, message slot, and
importance score (for drops).

### Tuning

`observer config` settings (in `~/.observer/config.toml`):

```toml
[compression.conversation]
enabled = true
target_ratio = 0.85
mode = "token"             # or "cache" — restricts drops to tail half
preserve_last_n = 4
prefix_bytes = 8192
compress_types = ["json", "logs", "text"]   # add "code", "diff", "html" to opt in
min_bytes_to_compress = 1024

[compression.conversation.weights]
recency = 0.4
reference = 0.3
density = 0.15
role = 0.15
```

High importance scores on dropped events (≥0.5) suggest the threshold
is too aggressive — raise `target_ratio` (e.g. 0.9 or 0.95).


## Cost and token math

Anthropic's usage envelope reports four token buckets per request,
each at a different rate:

| Bucket             | What it is                                                                  | Bills at                    |
|--------------------|------------------------------------------------------------------------------|-----------------------------|
| `net_input`        | Fresh prompt tokens not served from cache                                   | model's standard input rate |
| `cache_read`       | Prompt tokens served from Anthropic's ephemeral cache                       | ~10% of input rate          |
| `cache_creation`   | Tokens written to ephemeral cache                                            | 1.25× input (5m tier) or 2× input (1h tier) |
| `output`           | What the model generated                                                     | typically 5× input rate     |

`prompt_context = net_input + cache_read + cache_creation`
`total_tokens  = prompt_context + output`

The cost engine (`internal/intelligence/cost`) computes USD via:

```
cost_usd = (net_input × p.input + cache_read × p.cache_read +
            cache_creation_5m × p.cache_creation +
            cache_creation_1h × p.cache_creation_1h +
            output × p.output) ÷ 1,000,000
```

When the upstream API returns `cost_usd` in the response envelope
(proxy-sourced rows), that value is preferred over the computed one —
ground truth, reliability=high.

### Blended input rate

The Discovery tab's "~$ wasted" tile uses a **blended input rate**
computed from your last-30d api_turns mix: each model's input rate
weighted by the prompt-token volume it consumed. Example: if you
spent 70% of prompt tokens on opus-4-7 ($15/1M) and 30% on
haiku-4-5 ($1/1M), the blended rate is `0.7 × 15 + 0.3 × 1 = $10.80/1M`.

Falls back to $3/1M (claude-sonnet-4 input) on fresh installs with
no proxy data.

### JSONL dedup

When the proxy isn't engaged, observer falls back to parsing the AI
client's on-disk session log. Clients echo the same cumulative usage
on every content block of a multi-block response, so naive parsing
counts one API call 2-4×. Two layers of dedup catch this:

1. **Adapter-level**: dedupes on Anthropic `message.id` at write time
2. **Cost-engine-level**: dedupes on `(source_file, model, timestamp-bucketed-to-minute, tokens)` at read time

Migration 007 ran a one-time pass collapsing pre-fix duplicates.


## Terminology and glossary

Quick reference; the in-platform help drawer (press `?` on the
dashboard) has the full versions with cross-links.

- **Action** — one normalized tool call recorded by an adapter. Action
  types are taxonomic and cross-client: `read_file`, `write_file`,
  `edit_file`, `run_command`, `search_text`, `search_files`,
  `web_search`, `web_fetch`, `mcp_call`, `spawn_subagent`,
  `todo_update`, `ask_user`, `task_complete`, `user_prompt`,
  `api_error`, `turn_aborted` (interrupted before completion —
  distinct from task_complete/success=false; v1.4.22+),
  `context_compacted` (upstream-emitted compaction marker, not
  searchable like file edits; v1.4.22+), `system_prompt`
  (system/developer/user-envelope content; v1.4.23+), `unknown`.
- **API turn** — one HTTP request captured by the local proxy. Records
  one row in `api_turns` per request, with the upstream usage envelope
  intact.
- **Cache 5m vs 1h tier** — Anthropic's prompt cache has two TTLs.
  Default is 5 minutes; `cache_control: {type: ephemeral, ttl: 3600}`
  extends to 1 hour at 2× the write cost. Reads bill the same rate
  regardless of tier.
- **Compression event** — one individual compression decision (one
  per-type compress, or one drop) recorded post migration 010.
- **Conversation compression** — pre-forward trimming of API request
  bodies. Pass 1 = per-content-type compression, Pass 2 = drop with
  marker. See [Compression mechanisms](#compression-mechanisms).
- **Cross-platform tool calling** — every AI client connected via
  `observer init` can call the 12 MCP tools against the unified
  database. So Cursor's `get_last_test_result` can return a `go test`
  Claude Code ran an hour earlier.
- **Cross-thread reread** — the parent thread re-reads a file the
  sub-agent already saw (or vice versa) within the same session. Fix:
  pass content via the Agent tool's `prompt` parameter rather than
  letting the child re-read.
- **Freshness state** — per-read tag from the freshness engine: `fresh`
  (first read in this session, OR re-read with same content),
  `stale` (re-read after change in same session), `missing` (file no
  longer exists), `modified-elsewhere` (file changed by something
  other than an observable AI action).
- **Mechanism** — one of `json`, `code`, `logs`, `text`, `diff`,
  `html` (per-content-type compressor) or `drop` (low-importance
  message replaced by a marker).
- **Pattern** — a derived behaviour: `command_pair` (X often
  followed by Y), `cross_tool_file` (file touched by multiple
  clients), `knowledge_snippet` (consistent topic-specific habit),
  `failure_correlation` (X often precedes a failure of Y),
  `session_summary`. Each has a decay-weighted confidence score 0-1.
- **Project** — working-directory root that owns sessions and
  actions. Derived from cwd at session start; `/.git/worktrees/...`
  paths fold back to the working-tree root.
- **Proxy vs JSONL** — proxy intercepts upstream HTTP calls (ground
  truth, reliability=high). JSONL parses the AI client's on-disk
  session log (works without configuring a base URL, but client
  echoes cumulative usage on every block, requiring dedup —
  reliability=unreliable for token counts on Claude Code).
- **Reliability** — cost-engine confidence: `high` (upstream-
  reported), `medium` (computed from known pricing), `low` (some
  buckets estimated), `unreliable` (no pricing entry).
- **Session** — one continuous AI-coding conversation in a single
  tool, scoped to one working directory. Has a stable ID (Claude
  Code's UUID, Codex's rollout ID, …).
- **Sidechain** — actions emitted inside a sub-agent runtime spawned
  via the parent's `Agent` tool. Sub-agents share the parent's
  session_id; the `is_sidechain` column distinguishes them. The
  Discovery tab's CROSS-THREAD column counts stale rereads that
  crossed this boundary.
- **Stale reread** — same-session re-read of a file whose content
  changed between reads. Cross-session reads are excluded (a fresh
  session has no memory of a prior session's read).
- **Tool** — in this dashboard, "tool" means the *AI client*
  (claude-code, cursor, codex, cline, copilot), not the per-tool
  name (`read_file`, `run_command`). The latter is "Tool name" on
  the Actions tab.
- **Tool-pair integrity** — Anthropic requires every `tool_result`
  block to have a corresponding `tool_use` block in a preceding
  message. The compression pipeline preserves both sides of every
  live pair to satisfy this constraint.


## CLI reference

Every command supports `--help` for the full surface.

| Subcommand                         | Purpose |
|------------------------------------|---------|
| `observer init`                    | Register hooks + MCP server with installed AI clients |
| `observer uninstall`               | Reverse `observer init` |
| `observer start`                   | Run watcher + dashboard + proxy in one process (recommended). Flags: `--dashboard-addr ADDR` (default `127.0.0.1:8081`), `--no-dashboard` to skip the HTTP UI. |
| `observer watch`                   | Long-running JSONL watcher only |
| `observer dashboard --addr ADDR`   | HTTP dashboard only |
| `observer proxy start`             | Reverse proxy only |
| `observer scan`                    | One-shot ingest of existing JSONL files (catch-up after install) |
| `observer status`                  | DB stats + recent activity |
| `observer doctor`                  | Diagnostic — checks paths, schemas, hook registration |
| `observer tail`                    | Live tail of incoming events |
| `observer cost`                    | Per-model cost summary CLI |
| `observer score`                   | Compute quality_score / error_rate / redundancy_ratio for sessions |
| `observer discover`                | Stale rereads + repeated commands report (CLI version of the Discovery tab) |
| `observer patterns`                | Derive patterns from session history |
| `observer learn`                   | Adapter for ingesting external JSONL exports |
| `observer suggest`                 | Write high-confidence patterns into CLAUDE.md / AGENTS.md / .cursorrules |
| `observer summarize`               | Roll-up summary across sessions |
| `observer export`                  | Export DB to xlsx / json |
| `observer prune`                   | Manual retention pass (delete old data) |
| `observer backfill --is-sidechain` | Re-walk JSONL to populate `actions.is_sidechain` (added by migration 010) on pre-migration rows. |
| `observer backfill --cache-tier`   | Re-walk JSONL to populate `cache_creation_1h_tokens` (added by migration 008) on pre-migration rows. Run once after upgrading to v1.4.16+ to correct historical 1h-tier cache writes that were silently billed at the cheaper 5m rate. |
| `observer backfill --message-id`   | Re-walk JSONL to populate `message_id` on `actions` and `token_usage` (added by migration 012). Required by the per-message timeline view in the Sessions modal. |
| `observer backfill --all`          | Run every supported backfill in one invocation. Idempotent — safe to re-run. |
| `observer metrics`                 | Prometheus-format metrics endpoint |
| `observer serve`                   | MCP server (stdio JSON-RPC) — usually invoked by `observer init` registration |
| `observer tail`                    | Live event stream |


## Configuration

`~/.observer/config.toml` — created with defaults on first run.

```toml
[paths]
db_path = "~/.observer/observer.db"
log_dir = "~/.observer/logs"

[proxy]
listen_addr = "127.0.0.1"
port = 8820
anthropic_upstream = "https://api.anthropic.com"
openai_upstream = "https://api.openai.com"

[dashboard]
listen_addr = "127.0.0.1"
port = 8081

[compression.conversation]
enabled = true
target_ratio = 0.85
mode = "token"
preserve_last_n = 4
prefix_bytes = 8192
compress_types = ["json", "logs", "text"]
min_bytes_to_compress = 1024

[compression.conversation.weights]
recency = 0.4
reference = 0.3
density = 0.15
role = 0.15

[compression.shell]
enabled = true
# per-command filters configured under [compression.shell.filters]

[retention]
prune_on_startup = true
max_actions = 5_000_000
max_age_days = 365

[pricing]
# Per-model overrides if the baked-in pricing is wrong for you.
# [pricing.models."claude-opus-4-7"]
# input = 15
# output = 75
# cache_read = 1.5
# cache_creation = 18.75
```


## Troubleshooting

### `npm install -g` fails with `EACCES: permission denied`

Default npm puts globals under `/usr/local/lib/node_modules` which
Homebrew-managed Node owns as root on macOS. Three fixes; pick one:

```bash
# 1) RECOMMENDED — point npm at a user-writable prefix.
mkdir -p ~/.npm-global
npm config set prefix '~/.npm-global'
echo 'export PATH=~/.npm-global/bin:$PATH' >> ~/.zshrc
source ~/.zshrc
npm install -g @superbased/observer

# 2) Use a Node version manager — fnm / nvm install Node into your
#    home directory and dodge the permission issue entirely.
brew install fnm
fnm install --lts
npm install -g @superbased/observer

# 3) sudo (works but you'll fight permissions on every update).
sudo npm install -g @superbased/observer
```

### `observer: command not found` after install

The shim binary is at `~/.npm-global/bin/observer` (or wherever your
npm prefix points). Make sure that directory is on `$PATH`:

```bash
echo $PATH | tr ':' '\n' | grep -E 'npm|node'
# add the prefix's bin/ to PATH if missing — see fix above
```

If you installed only a platform package (e.g. `@superbased/observer-darwin-x64`)
without the main `@superbased/observer`, the shim doesn't get created
— there's no `bin` field. Install the main package; npm picks up the
right platform binary automatically via `optionalDependencies`.

### `observer init` says "no tools selected and none auto-detected"

Auto-detection looks for the AI clients' default session-log dirs
(`~/.claude/projects/`, `~/.codex/sessions/`, `~/.cursor/`, etc.).
On a fresh machine where no client has run yet, those dirs don't
exist. Pass the flag explicitly:

```bash
observer init --claude-code     # or --codex / --cursor / --cline / --all
```

This registers hooks regardless — the next time the client runs,
its dirs get created and the watcher picks them up.

### Empty dashboard / "No proxy traffic"

The JSONL adapter populates passively after `observer init`, but
ground-truth cost / compression numbers require the proxy. Set
`ANTHROPIC_BASE_URL=http://127.0.0.1:8820` (Claude Code) or
`OPENAI_BASE_URL=http://127.0.0.1:8820/v1` (Codex) in the shell
that launches your AI client.

Verify with `observer status | grep api_turns` — count should
climb during AI-client activity.

### `observer --version` says `dev`

You're on a non-released build. Reinstall with `npm install -g @superbased/observer` or rebuild with the workflow's `-X main.version=$VERSION` ldflag.

### `tool_result block must have a corresponding tool_use block`

Anthropic 400. Means the conversation-compression pipeline dropped
a `tool_use` while keeping its matching `tool_result`. Versions
prior to 1.3.2 had this bug; upgrade. If you're on 1.3.2+ and still
see it, file an issue with the conversation prefix.

### `tool use concurrency issues`

Anthropic 400 surfaced in Claude Code as this message. Means the
parallel-tool-use case (multiple `tool_use` blocks in one assistant
message) isn't paired correctly with the multi-block tool_result
that follows. Versions prior to 1.3.2 had this bug; upgrade.

### Cross-thread numbers are 0

Pre-migration data was ingested without the `is_sidechain` flag.
Run `observer backfill --is-sidechain` once to re-walk JSONL and
populate the flag on existing rows.

### Migration error: `duplicate column name`

Race condition between concurrent daemon startups, fixed in 1.4.1.
Upgrade. If you still see it, run daemons serially: `observer
watch`, wait, then `observer dashboard`, then `observer proxy
start` (or just use `observer start` which runs all three in one
process — proxy + watcher + dashboard).

### `observer start` log says only `proxy + observer` — no `:8081`

You're on a pre-1.4.7 build. Earlier versions ran only proxy +
watcher under `observer start`; the dashboard had to be started
separately via `observer dashboard --addr 127.0.0.1:8081`. Upgrade
to 1.4.7+ — the dashboard goroutine is now part of `observer start`
and the log line confirms all three: `proxy <addr> + watcher +
dashboard http://127.0.0.1:8081`. Pass `--no-dashboard` to opt out.

### "address already in use" on port 8820

Another `observer proxy start` or `observer start` is still running.
Find it with `pgrep -af 'observer (proxy|start)'` and `kill <pid>`.
On macOS:

```bash
lsof -nP -iTCP:8820 -sTCP:LISTEN
kill <pid>
```

### Dashboard port already in use

```bash
observer dashboard --addr 127.0.0.1:8082    # pick a different port
# or
[dashboard]
port = 8082                                  # in config.toml
```


## Security and privacy

- **No telemetry**. The observer never makes outbound network calls
  except: (a) the proxy forwarding to the upstream API on your
  behalf, (b) optional codegraph-MCP integration if you've configured
  it. Both are explicit.
- **Local-only HTTP**. The proxy and dashboard bind to
  `127.0.0.1` by default. Don't bind to `0.0.0.0` unless you've
  thought about it — there's no auth.
- **Secrets scrubbing**. Tool inputs and outputs are passed through
  a configurable scrubber (`internal/scrub`) that redacts API keys,
  bearer tokens, and other obvious secrets before persistence. Not a
  guarantee — review the rules in `~/.observer/config.toml` for your
  own secret formats.
- **Database**. `~/.observer/observer.db` is a SQLite file. Owns
  the same security as your `~/.claude/` and `~/.codex/` session
  logs (which already contain the same content). Encrypt the disk
  if your threat model needs that.
- **Logs**. `~/.observer/logs/` carries operational logs (start /
  stop, errors, retention). Not request bodies — those are in the
  DB only.


## Source, contributing, license

- **Source**: https://github.com/marmutapp/superbased-observer
- **Issues**: https://github.com/marmutapp/superbased-observer/issues
- **License**: [Apache 2.0](https://github.com/marmutapp/superbased-observer/blob/main/LICENSE)
- **Author**: Santosh Kathira <contact@marmut.app>

This npm package is a thin Node.js shim that resolves the right
pre-built binary at runtime and spawns it. Same shape as `esbuild` /
`swc` / `@biomejs/biome`. The Go source lives in the main repo;
binaries are cross-compiled per release tag via GitHub Actions and
published as `@superbased/observer-<platform>-<arch>` per-platform
packages.
