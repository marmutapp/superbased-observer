# Dashboard Walkthrough

A guided tour of the SuperBased Observer dashboard. The dashboard is a
read-only window into the SQLite database that the observer writes to.
It runs locally on `127.0.0.1:8081` and never makes network calls.

> **Prefer in-platform help?** Press `?` anywhere in the dashboard to
> open the searchable Help drawer. Every column header, KPI tile, and
> chart label is hover-explainable. Click for details.

---

## What the dashboard is for

You're looking at recordings of your AI-coding tools (Claude Code,
Cursor, Codex, Cline, Copilot). Every tool call becomes an **action**.
Every conversation is a **session**. Every API call (when you point
the tool at the local proxy) becomes an **api turn**, with the upstream
usage envelope captured verbatim.

The dashboard surfaces:

1. **What it cost you** — tokens and dollars, per model, per day.
2. **What got done** — sessions, actions, tool mix.
3. **What was wasted** — stale re-reads, repeated commands, cache misses.
4. **What was compressed** — bytes saved by the conversation pipeline.

The numbers are most accurate when the proxy is engaged
(`ANTHROPIC_BASE_URL=http://127.0.0.1:8820`). Without the proxy, the
observer falls back to parsing the AI client's JSONL session log —
which works, but is noisier.

---

## First five minutes

1. **Open `http://127.0.0.1:8081/`.** You land on the **Overview** tab.
2. **Scan the four KPI tiles** at the top. Each is hover-explainable.
   - **Sessions** — total recorded sessions across all tools.
   - **API turns (proxy)** — how many requests the proxy captured.
     If this number is low or zero, the proxy isn't engaged.
   - **Token rows (jsonl)** — token-usage events recovered from the AI
     client's session log. Plentiful but unreliable.
   - **Failures (24h)** — recent action failures. If non-zero, jump to
     **Discovery** for stale-reread / repeated-command signals.
3. **Look at the two time-series charts** below the tiles. Cost per
   day broken into the four billing buckets (net input, cache read,
   cache write, output) and total actions per day with the failure
   subset overlaid.
4. **Top models / Top tools** — at-a-glance ranking of what you spent
   tokens on and which AI client did the most work in the window.
5. **Use the toolbar** to scope. The window selector (7d / 30d / 90d /
   1y), the tool filter (claude-code / cursor / …), and the project
   filter all re-fire the active tab.

If anything looks off — say cache write totals are zero — it's almost
always because the proxy isn't engaged. Pick the env var that matches
your AI client and set it in the shell that launches the client:

- **Claude Code** (or Cursor in Anthropic mode):
  `ANTHROPIC_BASE_URL=http://127.0.0.1:8820`
- **Codex** (or Cursor in OpenAI mode):
  `OPENAI_BASE_URL=http://127.0.0.1:8820/v1` (the `/v1` matters)

Watch **API turns (proxy)** climb. Both variables can coexist on the
same machine — one proxy port handles both providers, routing by URL
path. See `docs/proxy-routing.md` for per-client recipes including
persistent configs.

---

## Per-tab tour

### Overview
KPI tiles + four charts (cost over time, actions over time, top models,
top tools) + tools-over-time stacked area. Use this as your homepage.

### Cost
**Per-model** breakdown of the four billable token buckets and the
computed dollar cost. The summary line at the top of the panel sums
the window. Hover any column header for definitions and formulas.

The four buckets exist because Anthropic's prompt caching adds two
extra columns beyond plain input/output:
- **Net Input** — fresh prompt tokens at the standard input rate.
- **Cache Read** — prompt tokens served from cache, billed at ~10% of
  input rate.
- **Cache Write** — tokens newly written to cache, billed at 1.25× (5m
  tier) or 2× (1h tier) of input rate.
- **Output** — what the model generated, billed at the model's output
  rate (~5× input).

The token volume per day chart at the bottom is the same data as the
Overview cost chart, rendered as stacked bars.

### Sessions
One row per AI-coding session. Click a row to open the session detail
modal — action breakdown chart, token buckets, cost, and recent
actions in that session. Quality / Errors / Redundancy columns appear
only when `observer score` has been run.

### Actions
The flat firehose. Each row is one normalised tool call (action types
are uniform across all adapters). Filter by type with the dropdown.

### Tools
Per AI-client aggregates — actions, failures, success rate, sessions
reached, first/last seen. "Tool" here means the *client* (claude-code,
cursor, codex, …), not the per-message tool name.

### Compression

Three sections, ordered most-to-least important:

1. **KPI tiles** — Tokens saved (est.), Dollars saved (est.), Bytes
   saved, Turns compressed. Tokens and $ are the headline metrics
   because they're what bill you. Bytes are the source of truth (the
   proxy can measure them directly), but token counts depend on
   content type so bytes alone mislead when comparing across models.

2. **Savings per day chart** — daily tokens-saved (left axis) and
   bytes-saved (right axis). Days with no compression activity are
   filtered out so the chart isn't full of zeros. Two axes because
   tokens ≈ bytes ÷ 4; one axis would crush whichever line is smaller.

3. **Savings by mechanism chart** — per-day stacked bar showing which
   compression mechanism (json / code / logs / text / diff / html /
   drop) actually saved the bytes. Tells you whether the savings come
   from cheap content-type rewrites or expensive low-importance
   drops.
4. **Per-model breakdown table** — same data sliced by model. Negative
   savings (compression added overhead beyond what was dropped) are
   flagged in red. Hover the bytes-saved column for the original →
   compressed values.
5. **Recent compression events table** — the most granular view: one
   row per individual compression decision with mechanism, original /
   compressed / saved bytes, and the message slot in the conversation
   it operated on. For drops, the `Importance` column shows the score
   the budget enforcer used to pick the message for eviction.

The events table data lives in `compression_events` (migration 009).
Pre-009 turns don't populate; only new proxy traffic does.

**How it works.** The proxy intercepts every upstream API request,
parses the messages array, scores each message for importance, and
either rewrites it (truncates large `tool_result` blocks) or drops it
entirely (replacing with a marker message so the model still sees a
placeholder). `cache_control` breakpoints get re-aligned to the new
prefix boundary so prompt caching still works on the trimmed body.

**Why bytes-then-tokens.** The proxy reads HTTP bodies; bytes are
trivially measurable. Tokens require running a tokenizer or trusting
upstream's count, neither of which is feasible at proxy time. So the
proxy records bytes, and the dashboard derives tokens as `bytes ÷ 4`
(Anthropic's tokenizer averages ~4 chars/token on English/code).
Multiply by the model's input rate to get dollars.

**Why negative savings can happen.** When the original payload is
small, the marker messages can take up more bytes than the dropped
content saved. This is normal on tiny requests and shouldn't trigger
action. Persistent negatives across high-volume models suggest the
pipeline's `min_bytes_to_compress` threshold needs raising.

**Empty tab?** You're not using the proxy. Set the env var matching
your AI client and watch this tab populate:
- Claude Code: `ANTHROPIC_BASE_URL=http://127.0.0.1:8820`
- Codex / OpenAI-compatible: `OPENAI_BASE_URL=http://127.0.0.1:8820/v1`

Both work simultaneously. See `docs/proxy-routing.md` for the full
per-client setup.

### Discovery
Wasted-effort signals plus the cross-tool overlap surface:
- **Stale re-reads** — files re-read after they changed *within the
  same session*. Cross-session re-reads are excluded (a fresh session
  has no memory of a prior session's read, so flagging it as "stale"
  would be misleading).
- **Repeated commands** — commands run multiple times with no relevant
  inputs changed in between.
- **Cross-tool overlap** — files touched by ≥2 AI clients in the
  window (e.g. claude-code AND cursor both edited `auth.ts`). This is
  the visible side of *cross-platform tool calling* — see below.

#### Cross-tool sharing via MCP

The observer ships a 12-tool MCP server that gets registered with
every AI client during `observer init` (Claude Code, Cursor, Codex).
Every one of these tools queries the **unified database** — not the
client's own scoped slice. The result: when one AI agent records an
action, every other connected agent's MCP query can see it.

Concrete examples:
- Claude Code runs `go test` → Cursor's `get_last_test_result` MCP
  call returns Claude Code's output without re-running.
- Cursor edits `auth.ts` → Codex's `check_file_freshness("auth.ts")`
  reflects the edit immediately.
- Any agent's `get_failure_context` returns the most recent failures
  recorded by *any* AI client.

The 12 cross-querying tools:
`check_file_freshness`, `get_file_history`, `get_session_summary`,
`search_past_outputs`, `get_last_test_result`, `get_failure_context`,
`get_action_details`, `check_command_freshness`,
`get_session_recovery_context`, `get_project_patterns`,
`get_cost_summary`, `get_redundancy_report`.

Two layers ship today:
1. **Read-side via MCP queries** — any agent reads any other agent's
   recorded data on demand.
2. **Write-side via patterns** — `observer patterns` derives
   `cross_tool_file` patterns and `observer suggest` writes them into
   `CLAUDE.md` / `AGENTS.md` / `.cursorrules` so future sessions in
   any client inherit them.

Not yet shipped: synthetic tool-result injection (replaying agent A's
exact tool execution into agent B's conversation). The MCP query
model gives you the *result* on demand instead, which is the same
practical outcome with less coupling.

### Patterns
Decay-weighted patterns derived from session activity (knowledge
snippets, command pairs, cross-tool files, …). Output of `observer
patterns`. Each pattern carries a confidence score (0–1) and an
observation count.

---

## Calculation methodology

### Token math
Anthropic's usage envelope reports four numbers per turn:
`input_tokens` (already net of cache hits), `cache_read_input_tokens`,
`cache_creation_input_tokens`, `output_tokens`.

The dashboard treats them as four separate buckets because they bill
at different rates. Total prompt context for a turn is
`net_input + cache_read + cache_creation`.

### Cost math
When the upstream envelope reports `cost_usd`, that wins (this is
reliability=high). Otherwise the cost engine multiplies tokens by the
model's pricing entry:

```
cost_usd =
    net_input          × p.input
  + cache_read         × p.cache_read
  + cache_creation_5m  × p.cache_creation
  + cache_creation_1h  × p.cache_creation_1h
  + output             × p.output
```

The 1h cache tier is split out because Anthropic prices it at 2× the
5m tier. The 5m portion is derived as
`cache_creation_tokens − cache_creation_1h_tokens`.

If the model has no pricing entry, reliability=unreliable and the
cost column shows `$0.0000`.

### Dedup (proxy vs JSONL)
Proxy-captured `api_turns` are 1:1 with HTTP requests — no dedup
needed. JSONL token rows are noisier: AI clients echo cumulative
usage on every content block of a multi-block response, so one API
call writes N rows. The Claude Code adapter dedupes on Anthropic
`message.id` at write time. The cost engine adds a defence-in-depth
dedup on `(model, ts-bucketed-to-minute, input, output, cache_*)` at
read time.

If you see "47% of token rows collapsed", that's migration 007 —
historical residue from before the dedup was added.

### Freshness classification
Per-read tag set at write time by the freshness engine. Comparison is
*per session*:
- **fresh** — first read in this session, OR re-read where the content
  hash matches the prior read.
- **stale** — re-read where the content hash differs from the prior
  read in the same session (the file changed between reads).
- **missing** — file no longer exists.
- **modified-elsewhere** — file changed between reads, but not by an
  observable AI action.
- **unknown** — couldn't classify.

The "stale rereads" panel on the Discovery tab counts only the **stale**
tag. Cross-session reads do NOT count — a brand-new session has no
memory of what a previous session read.

### Compression savings

Three derived metrics, all signed (can go negative):

```
saved_bytes      = original_bytes − compressed_bytes
tokens_saved_est = saved_bytes ÷ 4
                   (~Claude tokenizer ratio for English/code)
cost_saved_usd_est = tokens_saved_est × pricing.input ÷ 1,000,000
                     (uses the row's model input rate; compressed
                      content is prompt context = input tokens)
```

`saved_pct = saved_bytes / original_bytes`. Negative means compression
added overhead (marker bytes exceeded what was dropped). Common on
small payloads (the marker placeholder costs a fixed minimum); rare on
long conversations where the dropped tool_result content is large.

Aggregates are simple sums across rows. Negative individual rows can
still net positive at the total — and usually do.

Treat `tokens_saved_est` as ±20% accurate. The bytes-to-tokens ratio
isn't constant — it drifts to ~3 chars/token on prose-heavy content and
~5 on highly-repetitive code. Treat `cost_saved_usd_est` as ±25%
accurate (token approximation × per-model rates that change).

---

## Glossary

| Term | Quick definition |
|---|---|
| **Session** | One continuous AI-coding conversation in a single tool, in a single project. |
| **Action** | One normalised tool call recorded by an adapter (read_file, run_command, …). |
| **API turn** | One HTTP request captured by the local proxy, with the upstream usage envelope intact. |
| **Token row** | One token-usage event recovered by parsing the AI client's JSONL session log. |
| **Tool** | The AI client that produced an action (claude-code, cursor, codex, cline, copilot). |
| **Project** | A working-directory root that owns sessions and actions. |
| **Net Input** | Fresh prompt tokens (cache hits subtracted). |
| **Cache Read** | Prompt tokens served from Anthropic's ephemeral cache. |
| **Cache Write** | Tokens newly written to the ephemeral cache. |
| **Output** | Model-generated tokens. |
| **5m / 1h tier** | Anthropic cache TTL tiers. 1h is priced at 2× the 5m tier. |
| **Reliability** | Cost-engine confidence: high (recorded) > medium (computed) > low (estimated) > unreliable (no pricing). |
| **Conversation compression** | Pre-forward compression of conversation payloads in the proxy. |
| **Shell compression** | Pre-recording filtering of shell command output. |
| **Marker** | Placeholder inserted in place of dropped content. |
| **Stale (freshness)** | Re-read after the file changed *within the same session*. |
| **No-change rerun** | Same command re-run with no relevant inputs changed in between. |

---

## See also

- `docs/analytics-audit.md` — the audit that drove the recent
  correctness fixes. Explains the A1–A4, B1–B2, C1–C5 codes.
- `docs/proxy-routing.md` — how to point each AI client at the
  observer proxy.
- `docs/intelligence.md` — `observer discover` / `score` / `patterns`
  / `learn` / `suggest` reference.
- `docs/compression.md` — the shell-output and conversation-compression
  pipelines.
- `superbased-final-spec-v2.md` — the full spec.
