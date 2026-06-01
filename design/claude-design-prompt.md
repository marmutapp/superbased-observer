# SuperBased Observer — Frontend Redesign Brief for Claude Design

## What This Product Is

SuperBased Observer is a **universal AI agent observability and monitoring tool**. It's a single Go binary that captures, normalizes, and analyzes tool call activity from **11 AI coding assistants** — Claude Code, OpenAI Codex, Cursor, Cline, GitHub Copilot, Claude Cowork, OpenClaw, OpenCode, Pi, Gemini CLI, and Antigravity. Think of it as **Datadog for AI coding agents**: it watches what your AI tools are doing across all your projects and gives you cost tracking, efficiency insights, compression savings, pattern detection, and full session replay.

The product has five integrated capabilities:
1. **Observer** — Capture and normalize tool call data from all platforms via log parsing, hooks, and an API proxy
2. **Freshness Engine** — Track file content hashes to detect stale re-reads and unnecessary re-operations
3. **MCP Server** — Expose project knowledge to AI tools on demand via 12+ MCP tools (check_file_freshness, get_file_history, get_last_test_result, get_failure_context, etc.) that enable cross-tool sharing (when Claude Code reads a file, Cursor's MCP query can see it)
4. **Intelligence** — Analytics, redundancy detection, pattern learning, failure correlation, and instruction file generation
5. **Compression** — Multi-layer token reduction: 8 compression mechanisms (json value-erasure, code compressor, log pipeline with ANSI strip + dedup + anomaly preservation, diff context stripper, HTML cruft removal, text head/tail truncation, tool schema trimming, CCR stash-and-retrieve), plus a budget enforcer that drops low-importance messages

The current dashboard is functional but looks like a developer's internal debug tool — dense monospaced tables, raw numbers, **everything in monospace font**, minimal visual hierarchy, no personality. **I need you to redesign the entire frontend into a premium, modern 2026 observability product** that a developer would be proud to show their team and that could credibly sit next to tools like Linear, Vercel Dashboard, or Raycast.

The screenshots I've shared show every page of the current dashboard. Use them as the **functional spec** — every piece of data shown needs to exist in the redesign, but the layout, hierarchy, visual treatment, and interaction patterns should be completely reimagined.

---

## Design Direction & Aesthetic

**Vibe**: Dark-mode-first. Think the love child of **Linear's precision**, **Vercel's dashboard elegance**, and **Grafana's data density** — but with a unique identity. Clean, confident, spacious where it matters, dense where density serves the user.

**Key design principles**:
- **Scannable at a glance, deep on demand.** Hero metrics should pop instantly. Details should be one click away, not crammed into the same view.
- **Color with purpose.** Use color to encode meaning (tool identity, cost severity, success/failure), not decoration. Each AI tool (Claude Code, Codex, Cursor, Cline, Copilot, Cowork, etc.) should have a distinct, consistent brand color used everywhere.
- **Typography hierarchy matters.** The current UI uses monospace for everything. Use a modern sans-serif (Inter, Geist, or similar) for UI, with monospace reserved for code paths, commands, and session IDs only.
- **Motion and micro-interactions.** Subtle transitions on tab switches, card hovers, chart tooltips. Nothing flashy — just polished.
- **Responsive layout.** 3-4 column grid on wide screens, gracefully collapsing. Cards, not raw tables, for summary data.
- **Consistent component library.** Every stat card, chart, table, and filter should feel like it belongs to the same system.

**Current palette** (for reference — the existing UI uses GitHub's dark theme tokens):
- `--bg: #0e1117` / `--panel: #161b22` / `--panel2: #1c2128` / `--line: #30363d`
- `--fg: #e6edf3` / `--fg-mute: #8b949e` / `--accent: #58a6ff`
- Chart colors: `#58a6ff` (blue), `#d2a8ff` (lavender), `#ffa657` (orange), `#f778ba` (pink), `#56d364` (green)

**New palette direction** (adapt as you see fit — break away from the GitHub look):
- Background: Deep navy/charcoal (#0a0a0f → #12121a gradient) — deeper and more unique than the current #0e1117
- Cards/surfaces: Slightly elevated (#1a1a2e with subtle border, maybe a faint gradient or glassmorphism)
- Primary accent: Electric blue or teal (not GitHub-blue)
- Success/savings: Emerald green
- Cost/spend: Amber/gold
- Failures/errors: Coral red
- Token bucket colors (used in cost charts): Distinct colors for Net Input, Cache Read, Cache Write, Output — these are the four Anthropic billing buckets and appear in many charts
- **Tool colors** (these appear on EVERY tab — consistency is critical):
  - claude-code: Orange/amber (Anthropic brand)
  - codex: Green (OpenAI brand)
  - cursor: Purple
  - cline: Blue
  - copilot: Grey-blue (GitHub brand)
  - cowork: Pink/rose
  - antigravity: Teal
  - opencode / openclaw / pi / gemini: Additional distinct colors
- **Action type colors** (appear on Tools breakdown chart, Actions tab): read_file, write_file, edit_file, run_command, search_text, search_files, web_search, web_fetch, spawn_subagent, mcp_call, user_prompt, task_complete, tool_failure, permission_request, etc. — each needs a consistent color
- **Reliability badges**: `accurate` (green), `approximate` (yellow), `unreliable` (red) — these appear in the Cost table

---

## Pages to Redesign (10 total)

### 1. OVERVIEW (Home Dashboard)

**Current state**: 4 stat cards at top (Sessions, API Turns (proxy), Token Rows (JSONL), Failures (24h)), two time-series charts (Cost over time with 4 stacked token buckets + model line, Actions over time with total + failures), and two bottom panels (Top models by tokens as horizontal bar, Top tools actions over time as stacked area). The current cards show raw counts with muted subtitles.

**API endpoints that feed this page**:
- `/api/status` → session count, api_turns count, token_usage row count, failure count
- `/api/timeseries/cost?days=N` → daily cost broken into net_input, cache_read, cache_write, output
- `/api/timeseries/actions?days=N` → daily action count by tool, with failure subset
- `/api/models?days=N` → top models by token sum (net_input + cache_read + output)
- `/api/tools?days=N` → per-tool action counts, success rate, sessions

**Redesign direction**:
- Hero section with 4-6 metric cards in a polished grid. Each card should have: the metric value (large), a label, a sparkline or trend indicator (up/down % vs prior period), and subtle iconography. The "API Turns (proxy)" card is important — when it's zero, the proxy isn't engaged and data quality drops. Consider a visual warning state for this.
- **Cost over time** chart: Make this the centerpiece. Area chart with gradient fill, broken down by the four Anthropic billing buckets (Net Input, Cache Read, Cache Write, Output) as stacked layers. Tooltips should show daily cost + breakdown. Currently rendered as a Chart.js line chart with stacked fills.
- **Actions over time**: Stacked area by tool (each tool in its brand color). The current chart shows total + failure overlay. Toggle between "by tool" and "by action type."
- **Top models by tokens**: Horizontal bar chart or treemap. Models include: claude-opus-4-7, claude-sonnet-4-6, gpt-5.5, claude-haiku-4-5, gemini-2.5-pro-high, etc. Model names should be readable. Currently uses a bar chart via Chart.js with a legend showing Net Input / Cache Read / Output.
- **Top tools**: Either a donut/ring chart for proportion, or keep as stacked area. Currently shows tools like claude-code, codex, antigravity, cowork, cursor, copilot, opencode stacked over time.
- Add a "Recent sessions" mini-list at the bottom — 5 most recent sessions with tool icon, project name, duration, cost, and action count. Clicking drills into the session detail.
- The time window selector (currently `<select>` dropdowns for window: 7d/30d/90d/1y/All, tool: all/claude-code/codex/..., project: all/path1/path2/...) should be a polished filter bar: pill-style selectors, a date range picker, multi-select for tools and projects. These filters are GLOBAL — they re-fire the active tab on change.

### 2. COST Tab

**Current state**: A summary line at top (total cost, turns, net input sum, cache_read sum, cache_write sum, output sum, volatility/unreliable model warnings), then a massive model-by-model table with 14+ columns: Model, Net In %, Cache Rd %, Cache Wr %, Output %, Net Input, Cache Read, Cache Write, Output, Reasoning, Cost, Turns, Source (proxy/jsonl), Reliability badge (accurate/approximate/unreliable), Billing. Below: "Token volume per day" stacked bar chart (4 buckets), "Token volume per day split by model" stacked bar chart, and conditionally a "Cowork cost reconciliation" card.

**API endpoints**:
- `/api/cost?days=N` → per-model token breakdown + cost + reliability + turn count
- `/api/timeseries/cost?days=N` → daily token volumes by bucket
- `/api/timeseries/tokens-by-model?days=N` → daily tokens split by model
- `/api/cowork/reconcile?days=N` → Cowork vs Observer cost comparison per session

**Redesign direction**:
- Top: Summary bar with total spend, average daily spend, and period-over-period delta. The current summary line text is dense — break it into scannable cards.
- Model cost table: This is the most data-dense table in the entire dashboard. Modernize into a styled data table with alternating row shading, sortable columns, inline bar visualizations for the token columns (tiny horizontal bars showing relative magnitude), and color-coded reliability badges (`accurate` = green pill, `approximate` = yellow, `unreliable` = red). The percentage columns (Net In %, Cache Rd %, etc.) show each bucket's share of that model's total — consider tiny pie/donut inline or just colored bars.
- Consider collapsible row groups (group all `claude-*` models, all `gpt-*` models, all `gemini-*` models).
- Token volume charts: Side by side. Use consistent colors for the 4 billing buckets (Net Input, Cache Read, Cache Write, Output). The "by model" chart shows top 6 models + "other" — use model-specific colors.
- Cowork reconciliation: Only renders when Cowork data exists. Shows 4 stat cards (Compactions, Sessions Matched, Injection Rate, Reject Rate) and a per-session table with Observer cost vs Cowork cost and drift %. Color-code drift (green = under threshold, red = over).
- Add a "Cost projection" mini-card: "At this rate, your monthly spend will be ~$X." (The Analysis tab already has `projection_usd` in its API — surface it here too).

### 3. ANALYSIS Tab

**Current state**: 10 stat cards in a 2x4 grid + 2 extra:
- **Spend to Date**: total cost with vs-prior-period % delta and recorded_share_pct
- **Month to Date**: MTD cost with vs-prior-month-same-day comparison + projection + budget % (if configured)
- **$/M Output**: output_rate — the clean "what are you paying per million output tokens" metric
- **Cache Savings**: counterfactual $ saved (cache_read priced at input rate vs cache_read rate)
- **Cache Efficacy**: cache_read / (cache_read + cache_write) — how well caching is working
- **High-Context Turns**: turns over 100K/200K prompt tokens with attributed cost (these are expensive!)
- **$ per Turn**: mean and p95 cost per API turn (variance signal)
- **Burn Rate**: cost_per_hour during active hours
- **Top Model**: highest-spend model + concentration % (concentration risk signal)
- **Waste**: estimated $ wasted on stale re-reads + repeated commands

Then: "Daily spend by model/project/tool" bar chart (with a **3-way dimension toggle**: Model | Project | Tool), "When you spend — cost by hour of day (UTC)" bar chart, "Cache savings trend" line chart, "What changed — top movers" table (top movers by absolute $, period vs prior), "New this period" table (models that appeared in current period but not prior), "Top expensive sessions" leaderboard table (top 14 by cost, clickable to session detail), and "Routing efficiency suggestions" table (model downgrade opportunities with estimated savings).

**API endpoints**:
- `/api/analysis/headline?days=N` → all tile values (period, month, output_rate, cache_savings, cache, high_context, per_turn, burn_rate, top_model, waste)
- `/api/analysis/trend?days=N&dim=model|project|tool` → daily spend by dimension
- `/api/analysis/movers?days=N` → top movers + new entrants
- `/api/analysis/top-sessions?days=N` → top 14 sessions by cost
- `/api/analysis/routing-suggestions?days=N` → model downgrade opportunities
- `/api/analysis/cost-by-hour?days=N` → spend by hour-of-day (0-23 UTC)
- `/api/analysis/cache-savings-trend?days=N` → daily cache savings

**Redesign direction**:
- This is the **insights** page — make it feel like an analyst's briefing. Lead with a 2x4+2 grid of stat cards, each with trend sparklines. The current cards already show prior-period deltas (green/red % change) — keep that but make it visually stronger.
- The **dimension toggle** (Model | Project | Tool) on the trend chart is a great UX pattern — keep it but make it more prominent, maybe as a segmented control.
- "When you spend" (hourly cost): Convert to an actual **heatmap grid** (hours on X, days-of-week on Y, color intensity = spend). Much more glanceable than the current 24-bar chart.
- Cache savings: Prominent — this is a key value prop. Show cumulative savings as a big number with a celebratory treatment (e.g., "You've saved $X through caching"). The efficacy % could be a gauge/ring.
- **Top movers / New this period**: Side-by-side cards. Top movers shows model name, prior $, current $, $ change (green/red), and % change. "New this period" shows models that weren't seen in the prior period with their cost — these are new model adoptions.
- **Top expensive sessions**: Style as a leaderboard with rank numbers. Each row: tool icon, session ID (clickable), model, tokens, cost, a "target prompt" tag for sessions with notably large prompts. Currently shows top 14 sessions.
- **Routing efficiency suggestions**: This is uniquely actionable — style it as an "Opportunities" card with a lightbulb icon. Each suggestion shows: current model → suggested model, context window (5K/50K threshold), estimated savings per session, and how many sessions it could apply to. "Informational only — model choice may be deliberate."

### 4. SESSIONS Tab

**Current state**: Dense table with 15+ columns: Session ID (truncated, click-to-copy), Tool (claude-code/codex/cowork/etc.), Project path, Elapsed (duration), Started timestamp, Created timestamp, Status, Actions count, Tokens, Cost/Turn, Model(s), GPT % (multi-model ratio), MCP X (cross-tool events), Total $. Rows are clickable → opens a **session detail modal** with: per-model token breakdown chart, action breakdown chart, cost summary, and a full messages timeline table (each message has: role, model, tokens, elapsed time, tool time, cost, expandable tool calls inline). The modal is ~1400px wide.

Sortable by any column (click headers). Current pagination shows 50 rows per page. Session IDs are UUID-truncated with dotted-underline click-to-copy affordance.

**API endpoints**:
- `/api/sessions?days=N&tool=X&project=Y&offset=O&limit=L&sort=field&dir=asc|desc` → paginated session list
- `/api/session/<id>` → full session detail with messages, token breakdown, action breakdown

**Redesign direction**:
- Card-based list or a polished data table. Each session row should show: tool icon (colored), session title or ID (truncated), project name, model(s) used (as small pills), elapsed time, action count, total cost, and a mini status indicator.
- The **session detail modal** is already very rich — redesign it as a full slide-over panel or dedicated sub-page. The messages timeline is the centerpiece: each message shows role (user/assistant/tool), model, tokens, elapsed/tool time, cost, and expandable inline tool calls. Make the tool calls visually distinct (indented cards, code-style formatting for file paths and commands).
- Filterable/sortable by any column. Add search by session ID or project.
- Group sessions by day with date headers, or offer a calendar-style heatmap view toggle.
- For Cowork sessions: Show the Cowork-specific metadata (CoworkProcessName like "jolly-tender-wozniak", CoworkTitle, HostLoopMode).
- Quality/Errors/Redundancy score columns appear when `observer score` has been run — design for both states.

### 5. ACTIONS Tab

**Current state**: Full firehose log. Table with: Timestamp, Tool, Type (action type), Effort level, Permission mode, Session (truncated ID), Message/content preview. Has 5 filter controls above: action type dropdown (28 action types!), effort level (minimal/low/medium/high/xhigh/max), permission mode (default/plan/acceptEdits/auto/dontAsk/bypassPermissions), "interrupted only" checkbox, "AI messages only" checkbox. Paginated at 50 rows.

The 28 action types: read_file, write_file, edit_file, run_command, search_text, search_files, web_search, web_fetch, spawn_subagent, todo_update, ask_user, mcp_call, user_prompt, user_prompt_expansion, task_complete, tool_failure, permission_request, permission_denied, post_tool_batch, setup, instructions_loaded, config_change, session_start, session_end, subagent_start, subagent_stop, notification, cwd_change, api_error.

**API endpoint**: `/api/actions?days=N&type=X&effort=E&permission=P&interrupt=bool&assistant_text=bool&offset=O&limit=L`

**Redesign direction**:
- This is the **event log / audit trail**. Design it like a structured log viewer (think: Datadog Log Explorer or Sentry's issues list).
- Each action row: Timestamp, tool icon, action type badge (color-coded by category: file ops=blue, commands=green, search=purple, web=teal, meta=grey, failures=red), effort badge, content preview, and session link.
- The 5 filter controls should be redesigned as faceted filtering — either a left sidebar with collapsible filter groups, or a horizontal filter bar with dropdowns. Active filters shown as removable pills. The "interrupted only" and "AI messages only" toggles are good — keep them as toggle switches.
- Expandable rows: clicking a row expands to show full details (file path, command, message content, token usage for that action) without navigating away.
- Consider a toggle between "table view" (dense, current) and "timeline view" (vertical timeline with cards, showing the flow of a session).

### 6. TOOLS Tab

**Current state**: 4 stat cards (Total Actions, Distinct Tools, Overall Success Rate, Busiest Tool), "Activity over time" stacked area chart, "Action-type mix per tool" horizontal stacked bar chart, and "Per-tool aggregates" table.

**Redesign direction**:
- Hero: Keep the 4 stat cards but style them. The "Busiest Tool" card could show the tool icon prominently.
- Activity over time: Clean stacked area with tool brand colors. Add hover crosshair with tooltip.
- Action-type mix: This is one of the most interesting visualizations. Consider making it a full-width **horizontal 100% stacked bar for each tool** with a proper legend, or an interactive treemap. Each action type should have a distinct, consistent color.
- Per-tool table: Modernize. Each row gets the tool's icon/color, action count with a relative bar, success rate as a colored progress bar, and first/last seen dates.
- Add a "Tool comparison" view: Select 2-3 tools and see their metrics side by side.

### 7. COMPRESSION Tab

**Current state**: This is the most complex tab with 7 distinct sections:

1. **Setup banners** (conditional): Claude Code proxy route explanation, Codex config setup card, "Codex is active in JSONL-only mode" warning banner with "Configure now" button. These show/hide based on what's configured.
2. **4 KPI cards**: Actions (est.) with byte→token conversion, $/saved (est.) with input rate, Bytes Saved (actual), Results count with "N dropped / M markers"
3. **Savings per day** dual-axis chart: left axis = tokens saved (bars), right axis = $ saved (line). Days with no compression filtered out.
4. **Savings by mechanism** stacked bar with a **3-way unit toggle** (tokens | $ | bytes): Shows per-day breakdown by mechanism — the 8 mechanisms are json, code, logs, text, diff, html, drop, read_cache, tools, stash. Each has a distinct color.
5. **Per-model breakdown table**: model name, events, tokens saved, bytes saved, save %
6. **Recent compression events** paginated table: individual compression decisions with mechanism, original/compressed/saved bytes, save %, importance score, message slot
7. **Reversibility — CCR retrieve rate** section: 4 stat cards (Stashes written, Retrievals, Retrieve Rate, Search Hits), two side-by-side tables (Top retrieved SHAs, Top searched actions by FTS5 hit count)
8. **Compaction events (D23)** section: 4 stat cards (Compactions, Sessions Affected, Injections Fired, Reject Rate), table of compaction events with pre-compact action count, file snapshot count, ghost files, deleted files
9. **Rolling-summarisation net cost (D20)** section: 4 stat cards (Summary Calls, Cost of summaries, Cache-creation saved, Net delta)

**API endpoints**:
- `/api/compression/events?days=N&offset=O&limit=L` → paginated events
- `/api/compression/timeseries?days=N&bucket=day` → daily savings by mechanism
- `/api/compression/retrieval?days=N` → CCR stash/retrieve metrics
- `/api/compression/rolling-cost?days=N` → rolling-summarisation cost ledger
- `/api/compaction/events?days=N` → compaction event list
- `/api/setup/claude` → proxy config status
- `/api/setup/codex` → codex config status

**Redesign direction**:
- This page tells the story of "how much money and tokens Observer is saving you." Lead with that narrative.
- Big hero number: "Total savings: $X.XX" with a subtitle "across N compression events" and "N bytes saved." Make this feel celebratory.
- **Setup banners**: Move to a dismissible/collapsible "Setup" accordion or banner at the very top. Once the proxy is configured, these should collapse to a single-line status indicator ("Proxy active on :8820 — capturing Claude Code + Codex"). Don't let setup instructions dominate the page for configured users.
- **Savings over time**: Clean area chart with the $ savings line prominent. The dual-axis (tokens + $) is fine but make $ the primary visual.
- **Mechanism breakdown**: The 3-way unit toggle (tokens | $ | bytes) is a great pattern — keep it but style it as a polished segmented control. Consider a donut or ring chart alongside the stacked bar showing overall proportions by mechanism. Each mechanism needs its own consistent color and a one-line description on hover (e.g., "json: value-erasure preserving structure", "drop: low-importance message replacement", "stash: CCR disk offload").
- **CCR retrieve rate section**: This is a strategic metric — frame it as "Is stash-and-retrieve paying off?" The retrieve rate is the headline; top SHAs and searched actions are the drill-down.
- **Compaction events (D23)**: Frame as "Post-compact recovery" — show how the proxy re-injects context after Claude Code's /compact.
- **Rolling-summarisation net cost (D20)**: This is a simple cost-benefit card — net delta positive = paying off, negative = losing money. Traffic-light treatment.
- Recent events table: Dense and technical. Keep it but improve readability — zebra striping, monospace for file paths, color-coded save percentages (green gradient), mechanism badges matching the chart colors.

### 8. DISCOVERY Tab

**Current state**: Three distinct sections:

1. **Stale Re-reads** (same-session only): 4 stat cards (File re-reads count, Unique files, Estimated waste $ using blended input rate, Top N file count), then "Top files re-read" paginated table with columns: File path, Reads, Stale (count), Cross-threaded (stale from subagent re-reads), Est Wasted Tokens, Project. An important badge: "same-session only" — cross-session reads don't count as stale because a new session has no memory.

2. **Repeated commands**: Paginated table with: Command string (monospace), Runs, No-Change Rounds (ran but output unchanged), Failed count, Project. Examples: `git status --short` (24 runs, 1 no-change), `go build ./... 2>&1 | head -30` (20 runs, 0 no-change), `go test -race ./... 2>&1 | grep -ci "FAIL"` (19 runs).

3. **Cross-tool overlap** (multi-client): Table of files touched by 2+ AI clients in the window. Currently shows "no data" in the screenshots — this is normal when only one tool is heavily used.

**API endpoint**: `/api/discover?days=N` → stale reads, repeated commands, cross-tool overlap all in one response

**Redesign direction**:
- This is the **waste detection** page. Frame it as "Here's what your AI tools are doing redundantly."
- Lead with a waste summary hero: Total estimated waste in $, number of stale re-reads, number of no-change reruns. Make the $ number prominent.
- **Stale re-reads**: Visualize as a ranked list of files with a horizontal bar showing re-read count. Color-code by staleness severity. Each file path should be monospaced with the project name as a colored badge. The "same-session only" scoping note is important context — keep it visible but don't let it dominate.
- **Repeated commands**: Ranked list with command in monospace, run count, no-change-round count as a fraction (e.g., "3/24 runs unchanged"), and a "waste score." Group by project. These are usually git, go build, and test commands — the most common AI agent habits.
- **Cross-tool overlap**: This is Observer's unique multi-tool value prop. When there IS data, visualize as a connection graph or matrix showing which tools touched which files. When empty (common with single-tool users), show a friendly empty state: "No cross-tool overlap detected. This surface lights up when 2+ AI clients (e.g., Claude Code and Cursor) work on the same project files." Explain the MCP cross-tool query mechanism briefly.
- Consider a "Recommendations" section: actionable suggestions derived from the waste data.

### 9. PATTERNS Tab

**Current state**: Paginated table with columns: Pattern (type), Project, Frequency (decimal 0-1), Observations (count), Rule (long text string). Pattern types observed in screenshots: hot_file, cs_change, edit_test_pair, knowledge_snippet. Rule text contains file paths, shell commands, and regex-like descriptions (e.g., `File path=PROGRESS.md → matched >5 isolated rereads` or `internal/intelligence/dashboard/static/index.go ... <test-command> go test ...`).

**API endpoint**: `/api/patterns?days=N&offset=O&limit=L`

**Redesign direction**:
- Patterns are Observer's "learned behaviors" — the output of `observer patterns`, which uses decay-weighted analysis of session activity. Present them as **insight cards** rather than a flat table.
- Each pattern gets a card with: pattern type badge (color-coded: hot_file=amber, cs_change=blue, edit_test_pair=green, knowledge_snippet=purple), project name badge, frequency as a bar or gauge (0.0→1.0), the rule text (formatted nicely — file paths highlighted in monospace, commands in code blocks), and observation count.
- Group by pattern type with collapsible sections, or offer tab-style filtering by type.
- These patterns feed into `observer suggest` which writes rules into `CLAUDE.md` / `AGENTS.md` / `.cursorrules` — mention this connection in the UI. A "Generate suggestions" button could be a nice addition.
- For **edit_test_pair** patterns: Visualize the file→test mapping as a mini dependency graph (file node → test file node → test command).
- For **knowledge_snippet** patterns: Show the knowledge content more prominently — these are learned facts about the codebase.
- Add a visual: "Pattern discovery over time" — when were new patterns first detected? A simple sparkline or timeline.

### 10. SETTINGS Tab

**Current state**: Two-column layout — left sidebar (220px) with 11 section buttons, main content area. Sections:

1. **Pricing**: Override editor table (model, input, cache_read, cache_write, output rates per million tokens) with "Put all" save button. Collapsible "Baked-in defaults" showing 100+ models with their default rates. Pricing is hot-reloadable — saves write config.toml and refresh the cost engine live.
2. **Backfill**: Table of available backfill tasks — each row: Mode name, Flag (CLI flag), Candidates (SQL-countable row count), Description, Status, Action ("Run" button). Modes include: cache-tier, cache-field, message-id, opencode-message-id, codex-message-id, openclaw-parts, opensearch-failures, codex-futures, opencode-paths, openai-article-types, cowork-reasoning, cowork-rescan, codex-rescan, cursor-model, capified-message-id. Running a backfill spawns a subprocess and shows a toast with live progress.
3. **Observer**: Read-only config display
4. **Watcher**: Read-only config display
5. **Freshness**: Read-only config display
6. **Retention**: Read-only config display
7. **Hooks**: Read-only config display
8. **Proxy**: Read-only config display
9. **Compression**: Read-only config display
10. **Intelligence**: Editable fields — summary_model (e.g., claude-haiku-4-5), monthly_budget_usd, code_graph.enabled checkbox. Has a "Save Intelligence" button.
11. **Antigravity**: Read-only config display

There's also a **right-side help panel** with contextual help text for the selected section (e.g., the Compression section help text explains what each sub-tab shows and how to interpret the data).

**API endpoints**:
- `GET /api/config` → full config object
- `PUT /api/config/pricing` → save pricing overrides (hot-reload)
- `GET /api/config/pricing/defaults` → baked-in pricing table
- `PUT /api/config/section/<name>` → save a config section
- `GET /api/backfill/status` → candidate counts for each backfill mode
- `POST /api/backfill/run` → start a backfill job (returns job ID)
- `GET /api/backfill/jobs/<id>` → poll job status/progress

**Redesign direction**:
- Clean settings layout. The current left rail + content + right help panel is already a good structure — refine it.
- **Pricing overrides**: Style the table as a proper data grid with inline editing. Add validation (rates must be positive numbers). The "Baked-in defaults" collapsible should expand to a searchable, sortable table of all 100+ models with their default rates. Add a "Reset to default" per-override action.
- **Backfill**: Show each task as a card with description, candidate count badge, last-run status indicator, and a prominent "Run" button. Show progress/status (toast with spinner currently) when a backfill is running — consider an inline progress bar. Group related backfill modes (e.g., all cache-related, all adapter-specific rescans).
- **Read-only sections** (Observer, Watcher, etc.): Currently render as pre-formatted TOML text. Make them readable — syntax-highlighted config display with section headers and inline comments explaining each field.
- **Intelligence section**: The summary_model selector should be a proper dropdown (list common models). The monthly_budget_usd input should show the budget bar from the Analysis tab as a preview. code_graph.enabled should be a styled toggle switch.
- **Help panel**: Keep it but make it dismissible/collapsible. Currently it has rich per-tab documentation explaining what each dashboard section shows, how to interpret data, and what actions to take.

---

## Global UI Elements

### Header
Current: Logo + "SuperBased Observer Dashboard" + db file path + schema version + last activity timestamp + last refresh time + "? Help" button. The header is informational-dense — simplify it. The db path and schema version are dev details that could go into Settings. Keep: logo, product name, last activity indicator, help button, and maybe a status dot (green = watcher active, yellow = proxy not configured).

### Navigation
The current top tab bar has 10 tabs: Overview, Cost, Analysis, Sessions, Actions, Tools, Compression, Discovery, Patterns, Settings. All rendered as `<button>` elements with `.active` class.

Redesign options:
- **Left sidebar** navigation with icons + labels, collapsible to icons-only. This gives more vertical space for the data-dense dashboard content. Group logically:
  - **Monitor**: Overview, Sessions, Actions
  - **Analyze**: Cost, Analysis, Tools
  - **Optimize**: Compression, Discovery, Patterns
  - **Configure**: Settings
- Or keep top tabs but make them more polished: rounded pill style with active indicator animation.

### Filter Bar (Global)
Current implementation: `<div class="toolbar">` with 3 `<select>` dropdowns (window: 7d/30d/90d/1y/All, tool: populated from `/api/tools`, project: populated from `/api/projects`), a Refresh button (accent blue), and an "Export Excel" ghost button. Status message on the right.

Redesign as a **polished, persistent filter bar**:
- Date range: Segmented control for presets (7d, 14d, 30d, 90d, All) + a calendar icon that opens a custom date picker
- Tool filter: Multi-select with tool icons/colors as chips (show the tool's brand color dot next to each)
- Project filter: Searchable dropdown with project paths (these can be long file paths — truncate with tooltip)
- The selected filters should persist across tab switches (they already do in the current implementation).

### Help System
The current dashboard has a comprehensive **in-platform help system** (`help.js`) with:
- A `data-help="<id>"` attribute on every interactive element
- Hover tooltips on column headers, KPI tiles, chart labels, and filters
- A slide-out **Help drawer** (press `?` anywhere) with searchable registry
- 100+ help entries organized by category: Tabs, Tiles, Charts, Columns, Filters, Metrics, Calculations, Glossary
- Each entry has: title, one-liner (hover tooltip), description (drawer detail), optional formula, source, example, and related cross-links

**This is a major asset — preserve it in the redesign.** The help system should be styled to match the new design language, but the registry and hover-explain pattern should carry over. Consider making it even more discoverable — maybe a pulsing `?` icon on first visit, or contextual help cards that appear when a section is empty.

### Data Export
Current: "Export Excel" button downloads a multi-sheet .xlsx workbook honoring current filters (`/api/export.xlsx`). Redesign as a dropdown menu: Export Excel (.xlsx), Export CSV, Copy to Clipboard.

### Session Detail Modal
The current session detail modal is a critical UI component — it opens when clicking any session row. Currently 1400px wide with:
- Session metadata header (ID, tool, project, model, timestamps)
- Two charts side by side (action breakdown donut, token usage bar)
- Cost summary cards
- Full **messages timeline table** with expandable tool calls inline — each row shows: message index, role, model, tokens (in/out), elapsed time, tool execution time, cost, and a "N ▾" pill that expands to show inline tool calls with their targets, errors, and results.

This modal is the deepest drill-down in the entire UI. In the redesign, consider making it a full slide-over panel (like Linear's issue detail) rather than a centered modal, so users can reference the session list behind it.

### Empty States
Design friendly, branded empty states for when there's no data. Different scenarios:
- **New install**: "Welcome to Observer! No sessions recorded yet. Set up your first adapter..." with quick-start links
- **Proxy not configured**: "Compression tab is empty because the proxy isn't engaged. Set ANTHROPIC_BASE_URL=..." (this is already done via setup cards, but redesign them)
- **Filtered to nothing**: "No data matches your filters. Try widening the date range or removing tool/project filters."

### Loading States
Currently uses a top-of-page load bar (`#load-bar`). Add skeleton loaders for charts and tables, subtle shimmer animation on cards.

---

## Technical Context

### Current Architecture
The frontend is a **single-file embedded SPA** — one `index.html` file (~3000 lines of inline HTML + CSS + JS) with Chart.js for all charts. It's embedded in the Go binary via `//go:embed static` and served from memory. There's a separate `help.js` (help registry) and `help.css`. No build step, no bundler, no framework — pure vanilla JS with `fetch()` calls to 40+ `/api/*` JSON endpoints.

The current CSS uses CSS custom properties (`:root { --bg, --panel, --fg, --accent, ... }`) and a hand-rolled component system (`.card`, `.panel`, `.grid.cols-4`, `.toolbar`, `.pill`, `.pager`, `.modal`).

### API Backend (stays as-is)
The Go backend serves 40+ JSON endpoints — all documented per-tab above. The endpoints accept query params for filtering (days, tool, project, offset, limit, sort, dir) and return JSON. The frontend redesign is purely a UI/UX concern — the data model and API stay the same.

**Complete API endpoint list** (registered in `dashboard.Handler()`):
- Status: `/api/status`, `/api/health/watcher`
- Cost: `/api/cost`, `/api/timeseries/cost`, `/api/timeseries/tokens-by-model`
- Sessions: `/api/sessions`, `/api/session/<id>`
- Actions: `/api/actions`
- Analysis: `/api/analysis/headline`, `/api/analysis/trend`, `/api/analysis/movers`, `/api/analysis/top-sessions`, `/api/analysis/routing-suggestions`, `/api/analysis/cost-by-hour`, `/api/analysis/cache-savings-trend`
- Tools: `/api/tools`, `/api/tools/breakdown`, `/api/models`
- Discovery: `/api/discover`, `/api/patterns`
- Compression: `/api/compression/events`, `/api/compression/timeseries`, `/api/compression/retrieval`, `/api/compression/rolling-cost`, `/api/compaction/events`
- Config: `/api/config`, `/api/config/pricing`, `/api/config/pricing/defaults`, `/api/config/section/<name>`
- Setup: `/api/setup/codex`, `/api/setup/codex-hooks`, `/api/setup/claude`
- Backfill: `/api/backfill/status`, `/api/backfill/run`, `/api/backfill/jobs/<id>`
- Admin: `/api/admin/restart`
- Meta: `/api/projects`, `/api/cowork/reconcile`, `/api/codex/support`
- Export: `/api/export.xlsx`

### Target Tech Stack for Redesign
- **React + TypeScript SPA** (Vite build, still embeddable in Go binary via `embed.FS`)
- **Tailwind CSS** for styling
- **Recharts** or **Tremor** for charts (replacing Chart.js)
- **TanStack Table** for data tables (sortable, filterable, paginated)
- **Framer Motion** for transitions and micro-interactions
- **Lucide** for icons
- **React Router** for URL-based tab navigation + filter persistence

---

## Key Domain Concepts (for realistic mockup data)

Use these real-world values from the screenshots when populating mockups:

- **Models**: claude-opus-4-7, claude-sonnet-4-6, gpt-5.5, claude-haiku-4-5-20251001, gemini-2.5-pro-high, claude-sonnet-4-6 (these are the real model strings)
- **Tools**: claude-code (dominant, ~33K actions), codex (~720), antigravity (~160), cowork (~350), cursor (~44), copilot (~5), opencode (~7)
- **Cost range**: ~$6000 total over 30 days, ~$200/day burn rate, ~$0.35/turn average
- **Sessions**: ~500 sessions in 30 days, top sessions costing $100-300 each
- **Cache efficacy**: ~98% (very high — most tokens served from cache)
- **Token volumes**: ~700K-900K tokens/day, with cache_read dominating
- **Compression savings**: ~45K actions compressed, ~$0.22 saved, ~180KB bytes saved (modest — depends on proxy usage)
- **Patterns**: hot_file, cs_change, edit_test_pair, knowledge_snippet types
- **Projects**: paths like `marmutapp/superbased-observer`, `off/npos`

## What I Need From You

1. **Full-page high-fidelity mockups** for all 10 pages listed above.
2. Start with the **Overview (Home)** page and the **Analysis** page — these are the most visually impactful.
3. Then do **Compression** (most complex tab, 7 sections) and **Sessions** (including the session detail slide-over).
4. Design a **consistent component system**: stat cards (with sparklines + trend indicators), chart containers (with dimension toggles and unit switches), data tables (sortable, paginated, with inline visualizations), filter controls (global bar + per-tab facets), navigation (sidebar or top tabs), badges (tool colors, reliability, action types, pattern types), tooltips (from the help system), modals/slide-overs.
5. Show **both states**: populated with data (use realistic numbers above) and empty/zero states (new install, proxy not configured, filtered to nothing).
6. **Preserve the help system affordance**: every column header, KPI tile, and chart label should have a hover-explain indicator (subtle `?` icon or dotted underline) and the Help drawer should be accessible via `?` key.
7. Make it look like a **$50/month SaaS product**, not a free dev tool. Premium, polished, confident. This is Datadog for AI agents — it should feel that way.

The attached screenshots show every current page in detail — use them as the data/content reference, but don't be constrained by the current layout. Reimagine everything.
