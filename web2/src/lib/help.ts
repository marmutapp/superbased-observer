// Org dashboard help registry. Schema mirrors web/src/lib/help.ts, but the
// entries are ORG-specific — seeded with the metrics the Teams admin dashboard
// surfaces (cache R/W, the proxy-vs-JSONL reliability split, vendor acceptance
// rate, the cross-vendor unit trap, seat utilization, audited disclosure,
// proxy-only degradation, prior-period deltas). It is intentionally NOT a
// wholesale copy of the 164-entry primary-dashboard registry — those describe
// pages the org dashboard does not have. Grows as later phases add surfaces.

export type HelpCategory =
  | "tab"
  | "tile"
  | "chart"
  | "column"
  | "filter"
  | "metric"
  | "calc"
  | "glossary";

export type HelpEntry = {
  id: string;
  category: HelpCategory;
  title: string;
  // One-liner shown in the hover tooltip.
  oneLiner: string;
  // Longer detail rendered in the drawer.
  detail?: string;
  // Optional formula / source / example fields, shown when present.
  formula?: string;
  source?: string;
  example?: string;
  // Cross-links by id — drawer renders these as jump-to chips.
  related?: string[];
};

export const HELP_REGISTRY: HelpEntry[] = [
  // ----- tab -----
  {
    id: "tab.overview",
    category: "tab",
    title: "Overview",
    oneLiner:
      "Org-wide snapshot — spend, token buckets, cache efficiency, reliability, activity rhythm, and tool/model mix for the selected window.",
    detail:
      "Aggregates every enrolled developer's activity, scoped to your role (admins see the whole org; leads see their teams). KPI tiles compare this window to the prior window of equal length (the delta sub-lines). Latency and cache cards depend on proxy capture and show an honest empty state when no node routed through the proxy in the window.",
    related: [
      "metric.capture_tier",
      "metric.reliability_split",
      "metric.cache_ratio",
      "metric.deltas",
      "glossary.scope",
    ],
  },
  {
    id: "tab.teams",
    category: "tab",
    title: "Teams",
    oneLiner:
      "Per-team rollup — spend, active developers, sessions, a 7-day spend sparkline, and each team's top tools.",
    detail:
      "Teams come from your SCIM group provisioning. A no-groups org will see no teams here — use the People page for a per-developer view that works without groups. Click a team to open its audited developer drill-down.",
    related: ["tab.people", "column.spark", "column.top_tool", "glossary.scope"],
  },
  {
    id: "tab.people",
    category: "tab",
    title: "People",
    oneLiner:
      "Org-wide per-developer leaderboard. Works without SCIM groups. The named leaderboard is an audited disclosure.",
    detail:
      "The aggregate header (active developers, total spend, avg/developer, sessions) is content-free and loads by default. The named per-developer leaderboard is the same privacy-sensitive disclosure class as the per-team developer drill-down: loading it writes a view_org_developers row to the audit log, so it appears only on an explicit click — never as a page-load side effect.",
    related: ["glossary.audited_disclosure", "glossary.scope", "tab.audit"],
  },
  {
    id: "tab.projects",
    category: "tab",
    title: "Projects",
    oneLiner:
      "Per-project rollup — spend, tokens, sessions, a 7-day sparkline, a net-input/output token mini-bar, and the tools each project used.",
    detail:
      "Projects are keyed by a content-free project hash — the raw repository path never leaves a node unless its operator opts into full-content sharing. Click a project for its detail rollup.",
    related: ["column.spark", "column.token_bar", "glossary.content_free"],
  },
  {
    id: "tab.tools",
    category: "tab",
    title: "Tools",
    oneLiner:
      "Per-AI-tool breakdown — cost, tokens, sessions, active developers, and (when proxy-captured) success rate and latency.",
    detail:
      "One row per AI client (claude-code, codex, cursor, copilot, …) aggregated across the org. Success rate and latency are proxy-only; they degrade to an honest empty when a tool's traffic bypassed the proxy.",
    related: ["metric.reliability_split", "glossary.proxy_vs_jsonl"],
  },
  {
    id: "tab.models",
    category: "tab",
    title: "Models",
    oneLiner:
      "Per-model breakdown — cost and the four token buckets (net input / cache read / cache write / output) plus reasoning, by model.",
    detail:
      "Model strings are normalized at capture. Cost uses the same deduplicated accounting as the rest of the dashboard: proxy-reported per-turn cost wins; estimated cost from token rows fills the rest.",
    related: ["metric.token_buckets", "metric.reliability_split"],
  },
  {
    id: "tab.activity",
    category: "tab",
    title: "Activity",
    oneLiner:
      "When the org works — actions and tokens by day, plus an hour-of-day intensity grid.",
    detail:
      "Time-grid view of the same content-free activity columns the other pages roll up (action_type, tool, token buckets), bucketed by day and hour for a rhythm read.",
    related: ["tab.overview"],
  },
  {
    id: "tab.telemetry",
    category: "tab",
    title: "Native telemetry",
    oneLiner:
      "Vendor admin-console analytics — acceptance rate, seat utilization, and vendor cost mix from Claude Code / Codex / Copilot org pollers.",
    detail:
      "Surfaces the cc/codex/copilot analytics tables when an org admin has wired the native-console poller. These are metrics no local-capture path produces (notably suggestion acceptance rate and seat utilization). Shows an honest 'not configured' state when no poller is wired — the common case.",
    related: ["metric.accept_rate", "metric.seat_utilization", "glossary.unit_trap"],
  },
  {
    id: "tab.routing",
    category: "tab",
    title: "Routing",
    oneLiner:
      "Model-routing decisions enrolled nodes shared — counts, decision-time savings, advise/enforce split, and per-tier/reason distributions.",
    detail:
      "Org-aggregate routing summary (§R19). A node shares this only when its operator opts in with [org_client.share].routing_summary; the share carries closed-enum dimensions (tier/reason/mode) + counts + dollar estimates — never a model id or content. Admin-only. Empty when no node has shared.",
    related: ["metric.routing_savings", "metric.routing_mode", "glossary.content_free"],
  },
  {
    id: "tab.sessions",
    category: "tab",
    title: "Sessions",
    oneLiner:
      "Scoped session list with a per-session drill-down (token buckets, action-type mix) — an audited disclosure.",
    detail:
      "Each row names a developer, so loading the list (or opening a detail) writes a view_org_sessions audit_log row before returning — the same disclosure class as People. Scoped to your role. Project identity is a hash, never the raw path. The detail can also surface captured message content where a node opted to share it — a separate, deeper disclosure (see Message content).",
    related: ["glossary.audited_disclosure", "glossary.message_content", "glossary.content_free", "tab.audit"],
  },
  {
    id: "glossary.message_content",
    category: "glossary",
    title: "Message content",
    oneLiner:
      "Captured prompts / tool input/output for a session — shown only where the node opted to share it, and viewed under a separate, deeper audit.",
    detail:
      "Message bodies are never default-shared. They reach the org server only from nodes that enabled native-OTel capture AND content sharing (full_content / admin_managed) — there is no remote toggle the admin can force. Today only Claude Code populates them. Expanding the Messages section on a session detail writes a distinct view_session_messages audit_log row (deeper than the metadata view). Where a node shipped only hashes, the body shows as 'not shared'. Admins can set a default-off retention horizon (content_retention.otel_content_days) that NULLs old bodies while keeping the hash.",
    related: ["tab.sessions", "glossary.audited_disclosure"],
  },
  {
    id: "tab.live",
    category: "tab",
    title: "Live",
    oneLiner: "Sessions active in the last 15 minutes — who's working now. Audited, auto-refreshing.",
    detail:
      "Any session with an action, API turn, or token row in the last 15 minutes, scoped to your role. Names a developer, so it is an audited disclosure (a view_org_sessions row is written). Auto-refreshes every 10s while the tab is visible. Content-free.",
    related: ["tab.sessions", "glossary.audited_disclosure"],
  },
  {
    id: "tab.movers",
    category: "tab",
    title: "Movers",
    oneLiner: "Period-over-period spend movement by model, project, or tool — top increases, decreases, and new entrants.",
    detail:
      "Compares this window's spend against the prior window of equal length, grouped by the chosen dimension. New entrants are keys present this period but absent last period. Project keys are hashes, never raw paths.",
    related: ["metric.deltas", "glossary.content_free"],
  },
  {
    id: "tab.report",
    category: "tab",
    title: "Cost statement",
    oneLiner: "A print-friendly monthly statement — spend by model, tool, and project plus the top sessions. File → Print for a PDF.",
    detail:
      "Pick a calendar month; the statement shows total spend, the model/tool/project breakdowns, and the 25 most expensive sessions, scoped to your role. Costs use the same proxy-deduplicated accounting as the rest of the dashboard. Content-free (project = hash).",
    related: ["tile.total_spend", "glossary.content_free"],
  },
  {
    id: "tab.suggestions",
    category: "tab",
    title: "Suggestions",
    oneLiner: "Org-wide cost/hygiene advisories computed read-side from the dashboard's content-free metrics.",
    detail:
      "Threshold-based advisories over the enriched Overview signals: model concentration, proxy-capture share, and cache reuse. Nothing is stored or sent — it is pure read-side math. An empty list means nothing crossed a threshold.",
    related: ["metric.reliability_split", "metric.cache_ratio", "tab.routing"],
  },
  {
    id: "tab.security",
    category: "tab",
    title: "Security",
    oneLiner:
      "Guard activity across the org — decision mix, enforcement share, and the rule/agent leaderboards.",
    detail:
      "Aggregates content-free guard_events: how often guards fired, how often they enforced vs advised, and which rules and agents accounted for the most decisions. No prompt or output content is involved.",
    related: ["glossary.content_free"],
  },
  {
    id: "tab.audit",
    category: "tab",
    title: "Audit log",
    oneLiner:
      "The org's audit trail — every privacy-sensitive disclosure (who viewed which developers' data, when).",
    detail:
      "Each audited disclosure (loading the People leaderboard, opening a team's developer drill-down) writes a row here before the data is returned, so per-developer visibility is itself accountable.",
    related: ["glossary.audited_disclosure", "tab.people"],
  },

  // ----- tile / metric -----
  {
    id: "tile.total_spend",
    category: "tile",
    title: "Total spend",
    oneLiner: "Deduplicated dollar spend across the org for the window.",
    detail:
      "Per-turn-deduped scan of api_turns (proxy) ∪ token_usage (JSONL): proxy-reported cost_usd wins when present, otherwise the cost engine prices the token row. The delta sub-line compares to the prior window of equal length.",
    formula:
      "Σ rowCost over [now - days, now], rowCost = api_turns.cost_usd if present else CostEngine(model, bundle)",
    related: ["metric.reliability_split", "metric.deltas"],
  },
  {
    id: "tile.active_developers",
    category: "tile",
    title: "Active developers",
    oneLiner: "Distinct enrolled developers with any activity in the window.",
    detail:
      "Counts distinct user_id with at least one session, action, or token row in the window, scoped to your role. The companion 'avg / developer' tile divides total spend by this count.",
    related: ["tab.people", "glossary.scope"],
  },
  {
    id: "tile.sessions",
    category: "tile",
    title: "Sessions",
    oneLiner: "Total AI-coding sessions across the org in the window.",
    detail:
      "A session is one continuous AI-coding conversation in a single tool. Counted from the sessions the enrolled nodes pushed, scoped to your role.",
    related: ["tab.overview"],
  },
  {
    id: "metric.capture_tier",
    category: "metric",
    title: "Capture tier",
    oneLiner:
      "Whether the org's data came through the proxy (richest) or JSONL-only (latency/cache unavailable).",
    detail:
      "The Overview pill reads green 'Proxy + JSONL' when any api_turns rows exist in the window — meaning at least one node routed through the local proxy, so latency, cache, and per-turn HTTP status are available. It reads warn 'JSONL only' when no proxy turns landed (the watcher still recovered tokens/cost from session logs, but latency and cache cannot be reconstructed). Never a fake zero — proxy-only cards show a labeled empty instead.",
    related: ["metric.reliability_split", "glossary.proxy_vs_jsonl"],
  },
  {
    id: "metric.reliability_split",
    category: "metric",
    title: "Reliability / source split",
    oneLiner:
      "Share of cost measured by the proxy (exact) vs estimated from JSONL token rows.",
    detail:
      "Proxy-captured turns carry the upstream usage envelope, so their cost is exact. JSONL-recovered token rows are deduplicated and priced by the cost engine — accurate but estimated. A high proxy share means the dashboard's dollars are mostly measured rather than inferred.",
    formula: "proxy_share = proxy_cost / (proxy_cost + estimated_cost)",
    related: ["metric.capture_tier", "glossary.proxy_vs_jsonl"],
  },
  {
    id: "metric.token_buckets",
    category: "metric",
    title: "Token buckets",
    oneLiner:
      "The four billable token buckets — net input, cache read, cache write, output — plus reasoning.",
    detail:
      "Net input excludes cached tokens (it is the genuinely new prompt content). Cache read is served from the provider prefix cache (~0.1× input rate on Anthropic). Cache write is the setup cost of stashing context for reuse. Output is generated tokens; reasoning tokens, where a provider exposes them, are billed at the output rate.",
    related: ["metric.cache_ratio", "tab.models"],
  },
  {
    id: "metric.cache_ratio",
    category: "metric",
    title: "Cache R/W ratio",
    oneLiner:
      "Cache-read tokens ÷ cache-write tokens — how much the org reuses provider-cached prefixes.",
    detail:
      "Higher is better: each token written to the cache is paying off across more reads. Rendered as '—' (not '0.0×') when the corpus has no cache writes yet, to avoid reading as 'no cache benefit'. This is the org roll-up of the same signal the primary dashboard's Cache page tracks per session.",
    formula: "Σ cache_read_tokens / Σ cache_creation_tokens",
    related: ["metric.token_buckets", "metric.cache_efficiency"],
  },
  {
    id: "metric.cache_efficiency",
    category: "metric",
    title: "Cache efficacy",
    oneLiner:
      "cache_read share of (cache_read + cache_write) — how much cache traffic is reuse vs setup.",
    detail:
      "High = you are reusing cached prompts (good). Low = you are paying to cache and not getting reuse back. Distinct from the R/W ratio: efficacy is bounded 0–100%, the ratio is unbounded.",
    formula: "cache_read / (cache_read + cache_creation)",
    related: ["metric.cache_ratio", "metric.token_buckets"],
  },
  {
    id: "metric.error_rate",
    category: "metric",
    title: "Error rate",
    oneLiner:
      "Share of failed actions, plus the per-turn error-class mix (proxy-only).",
    detail:
      "The action failure share (actions.success = 0) is available from JSONL capture. The richer error-class distribution (HTTP status / error_class per turn) is proxy-only and degrades to the action share when no proxy turns landed.",
    related: ["metric.capture_tier", "glossary.proxy_vs_jsonl"],
  },
  {
    id: "metric.latency",
    category: "metric",
    title: "Latency",
    oneLiner:
      "Median time-to-first-token and total response time — proxy-only.",
    detail:
      "Latency is reconstructed from the proxy's own timing of the upstream request, so it exists ONLY when a node routed through the proxy. When the window is JSONL-only, the latency cards show a labeled empty state rather than a fabricated number.",
    related: ["metric.capture_tier", "glossary.proxy_vs_jsonl"],
  },
  {
    id: "metric.accept_rate",
    category: "metric",
    title: "Acceptance rate",
    oneLiner:
      "Suggestion lines accepted vs rejected — a vendor-analytics metric no local-capture path produces.",
    detail:
      "Sourced from the native-console analytics tables (Claude Code / Copilot). Only available when an org admin has wired the vendor's org-analytics poller; otherwise the Native telemetry page shows 'not configured'.",
    related: ["tab.telemetry", "metric.unit_trap"],
  },
  {
    id: "metric.seat_utilization",
    category: "metric",
    title: "Seat utilization",
    oneLiner:
      "Active seats vs provisioned seats — how much of the paid Copilot seat allocation is actually used.",
    detail:
      "Sourced from the Copilot org-analytics poller. Copilot cost is seat/account-level (not per-turn), so it is never folded into the per-turn spend accounting — it lives only on the Native telemetry surface.",
    related: ["tab.telemetry", "metric.unit_trap"],
  },
  {
    id: "metric.deltas",
    category: "metric",
    title: "Prior-period delta",
    oneLiner:
      "Comparison to the prior window of equal length — up renders red, down renders green for spend.",
    detail:
      "Every windowed KPI compares this window (now − days … now) to the immediately preceding window of the same length. For cost-shaped metrics an increase is colored as a caution (red) and a decrease as a saving (green).",
    related: ["tile.total_spend"],
  },

  {
    id: "metric.routing_savings",
    category: "metric",
    title: "Routing net savings",
    oneLiner:
      "Decision-time estimated savings minus cache forfeit — the honest net of routing to a cheaper model.",
    detail:
      "Switching a turn to a cheaper model saves on the per-token rate but can forfeit a warm prompt cache (the next turn pays to re-cache). Gross savings is the rate delta the engine estimated at decision time; net subtracts the estimated cache forfeit. Both are decision-time estimates the node computed, not realized billing.",
    formula: "net_savings_usd = est_savings_usd − cache_forfeit_usd",
    related: ["tab.routing", "metric.routing_mode"],
  },
  {
    id: "metric.routing_mode",
    category: "metric",
    title: "Advise vs enforce",
    oneLiner:
      "Whether a routing decision only logged a recommendation (advise) or actually rewrote the model (enforce).",
    detail:
      "Routing mode is a node-side choice. Advise decisions are counted but not applied (the operator sees the recommendation); enforce decisions rewrite the request. There is NO remote enforce toggle — the org cannot flip a node into enforce (§R23). Enforce share = enforce ÷ total decisions.",
    related: ["tab.routing", "metric.routing_savings", "glossary.content_free"],
  },

  // ----- column -----
  {
    id: "column.spark",
    category: "column",
    title: "7-day sparkline",
    oneLiner: "Trailing daily spend for the row, as a tiny inline chart.",
    detail:
      "Seven points, one per day, of the row's (team's / project's / developer's) cost. Content-free — it is daily dollars, nothing more. Renders '—' when there are fewer than two days of data.",
    related: ["metric.deltas"],
  },
  {
    id: "column.top_tool",
    category: "column",
    title: "Top tool",
    oneLiner: "The AI client the row used most in the window.",
    detail:
      "The most-used tool by action count (Teams shows up to a few as dots). Content-free — only the tool name, never any content it touched.",
    related: ["tab.tools"],
  },
  {
    id: "column.token_bar",
    category: "column",
    title: "Token mini-bar",
    oneLiner: "Per-row split of net-input vs output tokens.",
    detail:
      "A compact two-segment bar showing the balance between net input (prompt) and output (generation) tokens for the project, so you can spot read-heavy vs generation-heavy repos at a glance.",
    related: ["metric.token_buckets"],
  },

  // ----- glossary -----
  {
    id: "glossary.audited_disclosure",
    category: "glossary",
    title: "Audited disclosure",
    oneLiner:
      "Surfaces that name a developer or list their sessions write an audit row BEFORE returning the data.",
    detail:
      "Per-developer visibility is itself accountable: loading the People leaderboard or a team's developer drill-down records who viewed it, when, and at what scope in the audit log. Aggregate, non-identifying rollups are not audited.",
    related: ["tab.audit", "tab.people"],
  },
  {
    id: "glossary.proxy_vs_jsonl",
    category: "glossary",
    title: "Proxy vs JSONL (degradation)",
    oneLiner:
      "Token/cost/action metrics survive on JSONL alone; latency, cache, and per-turn HTTP status need the proxy.",
    detail:
      "Nodes that route through the local proxy push exact per-turn usage, latency, and cache data. Nodes that call the provider directly still have their session logs parsed (JSONL), recovering tokens/cost/actions but NOT latency/cache/HTTP-status. Every proxy-only metric on this dashboard either falls back to a JSONL equivalent or shows an honest labeled empty — never a fabricated zero.",
    related: ["metric.capture_tier", "metric.reliability_split"],
  },
  {
    id: "glossary.unit_trap",
    category: "glossary",
    title: "Cross-vendor unit trap",
    oneLiner:
      "Vendor analytics report different units — don't sum them as if they were the same.",
    detail:
      "Claude Code, Codex, and Copilot native-console analytics each report in their own unit and surface (per-actor token rollups, ChatGPT-Enterprise buckets, seat-level engagement). The Native telemetry surface keeps a unit discriminator so these are never naively added together; Copilot's seat/account-level cost in particular is never merged into per-turn spend.",
    related: ["tab.telemetry", "metric.seat_utilization", "metric.accept_rate"],
  },
  {
    id: "glossary.scope",
    category: "glossary",
    title: "Role scope",
    oneLiner:
      "Admins see the whole org; team leads see their teams' union; plain members see themselves.",
    detail:
      "Every rollup honors the caller's role. The same endpoint returns different breadth depending on who asks — there is no client-side trust; scoping is enforced server-side in each rollup query.",
    related: ["tab.people", "glossary.audited_disclosure"],
  },
  {
    id: "glossary.content_free",
    category: "glossary",
    title: "Content-free aggregation",
    oneLiner:
      "Every metric here aggregates only non-content columns and hashed dimensions.",
    detail:
      "The dashboard never reads prompt/output content, raw file paths, commands, or git remotes. Projects are keyed by hash; raw paths ship only when a node operator opts into full-content sharing locally — an org admin cannot flip that remotely. New rollups carry a privacy test asserting their SQL touches no content column.",
    related: ["glossary.proxy_vs_jsonl", "tab.projects"],
  },

  // ----- observability (trajectories nav group) -----
  {
    id: "tab.obs_analytics",
    category: "tab",
    title: "Trajectory analytics",
    oneLiner:
      "Org-aggregate trajectory trends — cost, volume, latency, errors — across enrolled nodes. Content-free.",
    detail:
      "The T1 obs_summary surface: per-day × model × project-hash totals member nodes push under [org_client.share].obs_summary (default off). Trace/span/token/cost/latency/error sums only — no names, no bodies. Latency percentiles and by-kind/error-cause depth appear when nodes also opt into obs_traces.",
    related: ["tab.obs_explorer", "tile.obs.traces", "tile.obs.cost", "glossary.trajectory", "glossary.content_free"],
  },
  {
    id: "tab.obs_explorer",
    category: "tab",
    title: "Trajectory explorer",
    oneLiner:
      "Trace + span structure shared by enrolled nodes — click a trace for the span tree and proxy-verified cost.",
    detail:
      "The T2 obs_traces surface: span topology (kinds, names, durations, tokens, cost, request_id) nodes push under [org_client.share].obs_traces (default off) — never a prompt or response. When a span's request_id matches a proxied api_turn on this server, the trace detail shows the proxy's exact cost/cache split (the 'proxy-verified' wedge).",
    related: ["tab.obs_analytics", "tile.obs.spans", "glossary.span", "glossary.trajectory"],
  },
  {
    id: "tab.obs_cost",
    category: "tab",
    title: "Trajectory cost",
    oneLiner:
      "Cost of shared trajectories attributed by developer, project, and model.",
    detail:
      "Aggregates trajectory cost over the obs_summary / obs_traces a node shares, split by developer / project-hash / model. Proxy-exact where a span matched a proxied turn; otherwise the exporter-supplied cost.",
    related: ["tab.obs_analytics", "tile.obs.cost", "tile.obs.developers", "glossary.content_free"],
  },
  {
    id: "tab.obs_evals",
    category: "tab",
    title: "Eval health",
    oneLiner:
      "Eval-run summaries shared by enrolled nodes — pass rates, mean scores, per-scorer regression. Admin-only, content-free.",
    detail:
      "The T4 obs_eval_summary surface: per-run pass counts + mean/min score per scorer nodes push under [org_client.share].obs_eval_summary (default off) — never the reference or output text. Each run's per-scorer delta vs the prior run flags regressions.",
    related: ["tab.obs_analytics", "tile.obs.pass_rate", "tile.obs.regressed", "glossary.eval_run"],
  },
  {
    id: "tab.obs_alerts",
    category: "tab",
    title: "Trajectory alerts",
    oneLiner:
      "Rules over the trajectory-analytics metrics — threshold crossing + cooldown fires a webhook.",
    detail:
      "Admin-defined rules evaluate a metric from Trajectory analytics on a schedule; a sustained crossing (with cooldown) records an event and posts to the configured webhook. Mirrors the budget-alert loop.",
    related: ["tab.obs_analytics", "tile.obs.error_rate"],
  },
  {
    id: "tile.obs.traces",
    category: "tile",
    title: "Traces tile",
    oneLiner: "Count of shared trajectories in the window.",
    detail:
      "Distinct traces enrolled nodes shared for this window. Each trace is one top-level run of an instrumented app or agent.",
    related: ["tab.obs_explorer", "glossary.trajectory"],
  },
  {
    id: "tile.obs.spans",
    category: "tile",
    title: "Spans tile",
    oneLiner: "Total spans across the shared trajectories.",
    detail:
      "Sum of span counts over the shared traces. A span is one timed unit of work — agent step, LLM call, tool call, retrieval — nested into the trace tree.",
    related: ["tab.obs_explorer", "glossary.span"],
  },
  {
    id: "tile.obs.tokens",
    category: "tile",
    title: "Tokens tile",
    oneLiner: "LLM tokens recorded across the shared trajectories.",
    detail:
      "Input + output tokens reported on LLM spans. Spans without usage contribute nothing.",
    related: ["tab.obs_analytics", "tile.obs.cost"],
  },
  {
    id: "tile.obs.cost",
    category: "tile",
    title: "Trajectory cost tile",
    oneLiner: "Cost of shared trajectories — proxy-exact where a span matched a proxied turn.",
    detail:
      "Where an LLM span's request_id matched a proxied api_turn on this server, the figure is the proxy's exact billed cost (the wedge); otherwise it's the exporter-supplied cost.",
    related: ["tab.obs_cost", "glossary.proxy_vs_jsonl"],
  },
  {
    id: "tile.obs.error_rate",
    category: "tile",
    title: "Error rate tile",
    oneLiner: "Fraction of shared trajectories whose root status is error.",
    detail:
      "errors ÷ traces over the window. A trajectory counts as an error when its root span status is 'error'.",
    related: ["tab.obs_analytics", "tab.obs_alerts"],
  },
  {
    id: "tile.obs.latency",
    category: "tile",
    title: "Latency percentile tiles",
    oneLiner: "P50 / P95 / P99 span latency across shared trajectories.",
    detail:
      "Per-span duration percentiles over the shared spans. Requires nodes to opt into obs_traces (span structure); absent on summary-only sharing.",
    related: ["tab.obs_analytics", "glossary.span"],
  },
  {
    id: "tile.obs.runs",
    category: "tile",
    title: "Runs tile",
    oneLiner: "Count of shared eval runs in the window.",
    detail: "Eval-run summaries enrolled nodes shared. Each run scored one dataset with one scorer configuration.",
    related: ["tab.obs_evals", "glossary.eval_run"],
  },
  {
    id: "tile.obs.pass_rate",
    category: "tile",
    title: "Average pass rate tile",
    oneLiner: "Mean pass rate across the shared eval runs.",
    detail: "Average of each run's pass rate (items meeting the scorer's threshold ÷ total) over the window.",
    related: ["tab.obs_evals", "tile.obs.regressed"],
  },
  {
    id: "tile.obs.regressed",
    category: "tile",
    title: "Regressed runs tile",
    oneLiner: "Shared runs with at least one per-scorer drop vs the prior run.",
    detail: "A run is flagged regressed when any scorer's pass rate fell relative to the previous run of the same dataset.",
    related: ["tab.obs_evals", "glossary.eval_run"],
  },
  {
    id: "tile.obs.developers",
    category: "tile",
    title: "Developers tile",
    oneLiner: "Distinct developers trajectory cost is attributed to.",
    detail: "Count of developer identities the shared trajectory cost splits across in the window.",
    related: ["tab.obs_cost", "glossary.content_free"],
  },
  {
    id: "tile.obs.projects",
    category: "tile",
    title: "Projects tile",
    oneLiner: "Distinct projects trajectory cost is attributed to.",
    detail: "Count of project hashes the shared trajectory cost splits across. Projects are keyed by hash unless a node opts into full-content sharing.",
    related: ["tab.obs_cost", "glossary.content_free"],
  },
  {
    id: "tile.obs.duration",
    category: "tile",
    title: "Duration tile",
    oneLiner: "Wall-clock duration of the trajectory.",
    detail: "End-to-end span of the trace, from the earliest span start to the latest span end.",
    related: ["tab.obs_explorer", "glossary.span"],
  },
  {
    id: "glossary.trajectory",
    category: "glossary",
    title: "Trajectory",
    oneLiner: "One end-to-end run of an instrumented app or agent, captured as an OTLP trace.",
    detail:
      "A trajectory is a single trace: a tree of spans describing one run — the agent loop, the model calls it made, the tools it invoked. Nodes ingest them locally via OpenTelemetry and share only the structure (content-free) when opted in.",
    related: ["glossary.span", "tab.obs_explorer"],
  },
  {
    id: "glossary.span",
    category: "glossary",
    title: "Span",
    oneLiner: "One timed unit of work inside a trajectory — an agent step, LLM call, tool call, or retrieval.",
    detail:
      "Spans nest into the trace tree and carry a kind (agent / llm / tool / retriever / chain / embedding / guardrail / evaluator), timing, status, and kind-specific attributes — LLM spans add model and token usage. The org tier ships span structure only, never bodies.",
    related: ["glossary.trajectory", "tab.obs_explorer"],
  },
  {
    id: "glossary.eval_run",
    category: "glossary",
    title: "Eval run",
    oneLiner: "One scoring pass of a scorer over a dataset, producing pass counts and a mean score.",
    detail:
      "Runs make trajectory quality measurable and comparable over time. The org tier ships per-run summaries (pass/total + mean/min per scorer), never per-item bodies; comparing a run to the prior run surfaces regressions.",
    related: ["tile.obs.pass_rate", "tab.obs_evals"],
  },
];

const BY_ID = new Map(HELP_REGISTRY.map((e) => [e.id, e]));

export function getHelp(id: string | null | undefined): HelpEntry | undefined {
  if (!id) return undefined;
  return BY_ID.get(id);
}

export function helpByCategory(): Record<HelpCategory, HelpEntry[]> {
  const out: Record<HelpCategory, HelpEntry[]> = {
    tab: [],
    tile: [],
    chart: [],
    column: [],
    filter: [],
    metric: [],
    calc: [],
    glossary: [],
  };
  for (const e of HELP_REGISTRY) out[e.category].push(e);
  return out;
}
