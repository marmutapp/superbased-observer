// TypeScript shapes for the Go dashboard's /api/* responses.
// Field names mirror the Go JSON tags exactly so renaming on the
// backend doesn't silently break the frontend.

// ---------- /api/status ----------

export type ToolActivity = {
  tool: string;
  last_seen_at: string;
  action_count: number;
};

export type SnapshotCounts = {
  projects: number;
  sessions: number;
  actions: number;
  api_turns: number;
  file_state: number;
  failure_context: number;
  action_excerpts: number;
  token_usage: number;
  // Migration-036 cachetrack surface — drives the sidebar Cache badge.
  // Zero on installs that pre-date 036 or have [cachetrack].enabled
  // = false; the backend reads the count tolerantly.
  cache_events: number;
  suggestions: number;
  // Migration-040/041 surfaces — drive the sidebar Security and
  // Routing badges. Optional: absent entirely when the web build is
  // newer than the daemon serving /api/status.
  guard_events?: number;
  router_decisions?: number;
  // Sessions with activity in the last 15 minutes (the /api/live
  // definition, uncapped) — drives the sidebar Live badge.
  live_sessions?: number;
};

export type StatusSnapshot = {
  // version is the build-stamped binary version (e.g. "1.8.2"). Empty
  // string in dev builds — never compare for update available when
  // version is missing or === "dev".
  version?: string;
  db_path: string;
  db_size_bytes: number;
  schema_version: number;
  counts: SnapshotCounts;
  last_action_at?: string;
  last_action_tool?: string;
  per_tool_last_seen: ToolActivity[];
  recent_failures_24h: number;
  // Serving-process identity (stamped by the dashboard handler). The
  // restart-pending banner compares started_at against config-save
  // timestamps to auto-clear after a real restart.
  started_at?: string;
  uptime_seconds?: number;
};

// ---------- /api/health/watcher ----------

// WatcherHealth mirrors internal/intelligence/dashboard/health.go.
// The sidebar consumes only the rollup counts; the per-file rows are
// typed for the future health panel.
export type WatcherHealthFile = {
  path: string;
  byte_offset: number;
  file_size: number;
  behind_bytes: number;
  last_parsed?: string;
  behind_seconds?: number;
  missing?: boolean;
  orphan_unmatched?: boolean;
  suspected_misrouted?: boolean;
  misroute_reason?: string;
  action_count?: number;
};

export type WatcherHealth = {
  files: WatcherHealthFile[];
  behind_count: number;
  behind_total_bytes: number;
  orphan_count: number;
  suspected_misrouted_count: number;
  checked_at: string;
};

// ---------- /api/status/scoped ----------

// StatusScoped is the window/tool/project-scoped equivalent of
// SnapshotCounts. Used by the Overview + Analysis headline tiles
// (Sessions / API turns / Token rows / Total sessions) so they honor
// the global filters instead of showing all-time numbers under a
// "window 30d" chip.
export type StatusScoped = {
  days: number;
  sessions: number;
  api_turns: number;
  token_usage: number;
  actions: number;
};

// ---------- /api/timeseries/cost ----------

export type CostPoint = {
  bucket: string;
  input: number;
  output: number;
  cache_read: number;
  cache_creation: number;
  cost_usd: number;
  turn_count: number;
  compression_bytes_saved: number;
  compression_tokens_saved_est: number;
  compression_cost_saved_usd_est: number;
  compression_turns: number;
};

export type CostTimeseries = {
  metric: "cost";
  bucket: "day" | "hour";
  days: number;
  series: CostPoint[];
};

// ---------- /api/timeseries/actions ----------

export type ActionsPoint = {
  bucket: string;
  total: number;
  failures: number;
  by_tool: Record<string, number>;
};

export type ActionsTimeseries = {
  metric: "actions";
  bucket: "day" | "hour";
  days: number;
  series: ActionsPoint[];
};

// ---------- /api/tools ----------

export type ToolRow = {
  tool: string;
  action_count: number;
  failure_count: number;
  success_rate: number;
  session_count: number;
  first_seen: string;
  last_seen: string;
};

export type ToolsResponse = {
  days: number;
  since: string;
  tools: ToolRow[];
};

// ---------- /api/projects ----------

export type ProjectRow = {
  root_path: string;
  session_count: number;
  action_count: number;
  last_seen?: string;
};

export type ProjectsResponse = { rows: ProjectRow[] };

// ---------- /api/models (cost.Summary) ----------

export type TokenBundle = {
  input: number;
  output: number;
  cache_read: number;
  cache_creation: number;
  cache_creation_1h: number;
  reasoning: number;
  web_search_requests: number;
};

export type CompressionStats = {
  original_bytes: number;
  compressed_bytes: number;
  compressed_count: number;
  dropped_count: number;
  marker_count: number;
  turns: number;
  tokens_saved_est: number;
  cost_saved_usd_est: number;
};

export type Reliability =
  | "accurate"
  | "approximate"
  | "unreliable"
  | "unknown"
  | "";

export type CostRow = {
  key: string;
  tokens: TokenBundle;
  cost_usd: number;
  ai_cost_usd: number;
  tool_cost_usd: number;
  turn_count: number;
  source: "proxy" | "jsonl" | "mixed" | "";
  reliability: Reliability;
  unknown_models?: string[];
  pricing_source?: string;
  compression: CompressionStats;
  // Fast-tier (Opus 4.8 speed:"fast") subset of this group's turns.
  // fast_cost_usd already carries the 2x FastMultiplier premium.
  fast_turn_count?: number;
  fast_cost_usd?: number;
};

export type CostSummary = {
  group_by: string;
  source: string;
  days?: number;
  since?: string;
  rows: CostRow[];
  total_tokens: TokenBundle;
  total_cost_usd: number;
  turn_count: number;
  reliability: Reliability;
  unknown_model_count?: number;
  total_compression: CompressionStats;
  // Window-wide fast-tier totals (Opus 4.8 speed:"fast"). Rendered as a
  // small stat line when fast_turn_count > 0.
  fast_turn_count?: number;
  total_fast_cost_usd?: number;
  // Per-row cache annotations (spec §13 cost-view-annotation). Keyed by
  // row.key (model name or session_id depending on group_by). Absent when
  // the grouping isn't indexable by cache_events (project / tool) or when
  // no rows have cache events. The frontend looks up cache_by_key[row.key]
  // and renders a small cache pill when the entry exists.
  cache_by_key?: Record<string, SessionCacheAnnotation>;
};

// ---------- /api/sessions ----------

export type SessionRow = {
  id: string;
  tool: string;
  project: string;
  started_at: string;
  last_seen_at?: string;
  duration_seconds: number;
  total_actions: number;
  sidechain_action_count: number;
  quality_score?: number;
  error_rate?: number;
  redundancy_ratio?: number;
  // Spec §14.1 wasteful-subset of redundancy_ratio. Populated only
  // when the session has cache_events (Tier 3 / pre-backfill stay
  // null). Frontend renders as "0.30 (0.20 wasteful)" on the
  // Sessions table when present.
  redundancy_ratio_wasteful?: number;
  stale_reads_wasteful?: number;
  stale_reads_necessary?: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_creation_tokens: number;
  // 1h-ephemeral-tier subset of cache_creation_tokens; the rest is 5m
  // tier. Anthropic-only (non-Anthropic providers always 0).
  cache_creation_1h_tokens: number;
  reasoning_tokens?: number;
  web_search_requests?: number;
  total_tokens: number;
  cost_usd: number;
  ai_cost_usd: number;
  tool_cost_usd: number;
  cost_reliability?: Reliability;
  // Distinct model identifiers seen in this session's api_turns +
  // token_usage rows, ordered by turn count desc (heaviest first).
  // Empty when no proxy/JSONL capture preserved model info.
  models?: string[];
};

export type SessionsResponse = {
  rows: SessionRow[];
  page: number;
  limit: number;
  total: number;
  scored_count: number;
  days: number;
  // Server-side sort echo + page footer totals (server sorts the full
  // filtered set, so the page footer reconciles with the visible rows even
  // when a global cost/token sort surfaced a different slice).
  sort_by?: string;
  sort_dir?: string;
  page_cost_usd?: number;
  page_ai_cost_usd?: number;
  page_tool_cost_usd?: number;
};

// ---------- /api/live ----------

export type LiveAction = {
  id: number;
  timestamp: string;
  action_type: string;
  target?: string;
  success: boolean;
};

export type LiveSession = {
  session_id: string;
  tool: string;
  project_root?: string;
  started_at: string;
  last_activity: string;
  actions_total: number;
  turns: number;
  models?: string[];
  tokens: {
    input: number;
    output: number;
    cache_read: number;
    cache_write: number;
  };
  cost_usd: number;
  recent_actions: LiveAction[];
};

export type LiveResponse = {
  generated_at: string;
  window_minutes: number;
  active: LiveSession[];
};

// ---------- /api/report/monthly ----------

export type MonthlyReportRow = {
  key: string;
  cost_usd: number;
  turns: number;
  sessions?: number;
};

export type MonthlyReport = {
  month: string;
  project?: string;
  generated_at: string;
  totals?: {
    cost_usd: number;
    sessions: number;
    turns: number;
    tokens?: { input: number; output: number; cache_read: number; cache_write: number };
  };
  by_model?: MonthlyReportRow[];
  by_tool?: MonthlyReportRow[];
  by_project?: MonthlyReportRow[];
  savings?: {
    compression_bytes: number;
    compression_tokens: number;
    cache_read_tokens: number;
  };
  top_sessions?: {
    id: string;
    tool: string;
    project?: string;
    started_at: string;
    cost_usd: number;
    turns: number;
  }[];
};

// ---------- /api/experiments ----------

export type ExperimentDef = {
  name: string;
  class: string;
  control: string;
  candidate: string;
  started_at: string;
  stopped_at?: string;
  note?: string;
};

export type ExperimentArmReport = {
  arm: string;
  profile: string;
  sessions: number;
  total_cost_usd: number;
  mean_cost_usd: number;
  sd_cost_usd: number;
  cv_pct: number;
  turns: number;
  mean_turns: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  compression_saved_bytes: number;
  cache_events_by_cause: Record<string, number>;
};

export type ExperimentReportResponse = {
  experiment: ExperimentDef;
  running: boolean;
  window_from: string;
  window_to: string;
  control: ExperimentArmReport;
  candidate: ExperimentArmReport;
  delta_cost_pct: number;
  delta_turns_pct: number;
};

// ---------- /api/budget ----------

export type BudgetScope = {
  root?: string;
  budget_usd: number;
  mtd_usd: number;
  pct: number;
  forecast_usd: number;
  threshold?: "warn80" | "over100" | "";
};

export type BudgetResponse = {
  configured: boolean;
  month: string;
  days_elapsed: number;
  days_in_month: number;
  global?: BudgetScope;
  projects?: BudgetScope[];
};

// ---------- /api/search ----------

export type SearchHit = {
  action_id: number;
  session_id?: string;
  timestamp?: string;
  tool?: string;
  action_type?: string;
  tool_name?: string;
  target?: string;
  snippet?: string;
  error_message?: string;
  rank: number;
};

export type SearchResponse = {
  query: string;
  count: number;
  hits: SearchHit[];
};

// ---------- /api/sessions/calendar ----------

export type SessionsCalendarCell = {
  day: string; // YYYY-MM-DD
  session_count: number;
  cost_usd: number;
};

export type SessionsCalendarResponse = {
  days: number;
  cells: SessionsCalendarCell[];
};

// ---------- /api/actions/day-counts ----------

export type ActionsDayCell = {
  day: string; // YYYY-MM-DD
  count: number;
};

export type ActionsDayCountsResponse = {
  days: number;
  cells: ActionsDayCell[];
};

// ---------- /api/timeseries/tokens-by-model ----------

export type TokensByModelPoint = {
  bucket: string;
  model: string;
  input: number;
  output: number;
  cache_read: number;
  cache_creation: number;
  total_tokens: number;
  cost_usd: number;
  turn_count: number;
};

export type TokensByModelTimeseries = {
  metric: "tokens_by_model";
  bucket: "day";
  days: number;
  series: TokensByModelPoint[];
};

// ---------- /api/cowork/reconcile ----------

export type CoworkReconcileRow = {
  session_id: string;
  title?: string;
  process_name?: string;
  cowork_cost_usd: number;
  derived_cost_usd: number;
  drift_usd: number;
  drift_percent: number;
  over_threshold: boolean;
  unknown_model?: boolean;
  unknown_model_key?: string;
};

export type CoworkReconcileResult = {
  rows: CoworkReconcileRow[];
  sessions_total: number;
  sessions_over_threshold: number;
  cowork_total_usd: number;
  derived_total_usd: number;
  overall_drift_usd: number;
  overall_drift_percent: number;
  drift_threshold_percent: number;
};

// ---------- /api/analysis/headline ----------

export type AnalysisHeadline = {
  days: number;
  period: {
    cost_usd: number;
    prior_cost_usd: number;
    prior_is_zero: boolean;
    delta_pct: number;
    recorded_cost_share_pct: number;
    period_start: string;
    prior_start: string;
  };
  month: {
    to_date_usd: number;
    projection_usd: number;
    prior_month_same_day_usd: number;
    prior_month_is_zero: boolean;
    vs_prior_month_pct: number;
    budget_usd: number;
    budget_pct: number;
    days_elapsed: number;
    days_in_month: number;
  };
  output_rate: {
    output_tokens: number;
    rate_per_million: number;
  };
  cache_savings: {
    usd: number;
    cache_read_tokens: number;
    source_note: string;
  };
  cache: {
    read_tokens: number;
    write_tokens: number;
    efficacy: number;
  };
  high_context: {
    turns_over_100k: number;
    turns_over_200k: number;
    cost_over_100k_usd: number;
    cost_over_200k_usd: number;
    lc_eligible_turns: number;
    lc_surcharge_usd: number;
  };
  per_turn: {
    count: number;
    mean_usd: number;
    p95_usd: number;
  };
  burn_rate: {
    active_hours: number;
    cost_per_hour_usd: number;
    method_note: string;
  };
  top_model: {
    key: string;
    cost_usd: number;
    concentration_pct: number;
  };
  waste: {
    usd: number;
    tokens: number;
    blended_rate_per_million: number;
    source_note: string;
  };
};

// ---------- /api/analysis/trend ----------

export type AnalysisDim = "model" | "project" | "tool";

export type AnalysisTrendPoint = {
  bucket: string;
  key: string;
  total_tokens: number;
  cost_usd: number;
  turn_count: number;
};

export type AnalysisTrend = {
  metric: "trend";
  dim: AnalysisDim;
  bucket: "day";
  days: number;
  series: AnalysisTrendPoint[];
};

// ---------- /api/analysis/movers ----------

export type Mover = {
  key: string;
  prior_usd: number;
  current_usd: number;
  delta_usd: number;
  delta_pct: number;
};

export type Entrant = {
  key: string;
  current_usd: number;
};

// NOTE: increases / decreases / new_entrants come back as JSON `null`
// (not `[]`) when empty, because the Go handler builds them from nil
// slices. Treat as arrays at the read site (default to []).
export type AnalysisMovers = {
  dim: AnalysisDim;
  days: number;
  period_start: string;
  prior_start: string;
  increases: Mover[] | null;
  decreases: Mover[] | null;
  new_entrants: Entrant[] | null;
};

// ---------- /api/analysis/top-sessions ----------

export type TopSession = {
  id: string;
  tool: string;
  started_at: string;
  ended_at?: string;
  models: string[];
  turns: number;
  max_prompt_tokens: number;
  lc_turn_count: number;
  cost_usd: number;
  badges: string[];
};

export type AnalysisTopSessions = {
  days: number;
  limit: number;
  sessions: TopSession[];
};

// ---------- /api/analysis/routing-suggestions ----------

export type RoutingSuggestion = {
  session_id: string;
  current_model: string;
  suggested_model: string;
  current_cost_usd: number;
  suggested_cost_usd: number;
  savings_usd: number;
  reasons: string[];
};

export type AnalysisRoutingSuggestions = {
  days: number;
  suggestions: RoutingSuggestion[];
  total_savings_usd: number;
  sibling_map: Record<string, string>;
  thresholds: {
    max_trivial_prompt_tokens: number;
    max_trivial_output_tokens: number;
    min_savings_usd: number;
  };
  framing_note: string;
};

// ---------- /api/analysis/cost-by-hour ----------

export type HourBucket = {
  hour: number;
  cost_usd: number;
  turn_count: number;
};

export type AnalysisCostByHour = {
  days: number;
  timezone: string;
  buckets: HourBucket[];
};

// ---------- /api/analysis/cost-by-dow-hour ----------

// 2D bucket: day-of-week (0=Sun..6=Sat per Go's time.Weekday()) ×
// hour-of-day (0..23). Cost summed over the window. Frontend renders
// this as a 7×24 heatmap on the Analysis page's "When you spend".
export type DowHourCell = {
  dow: number;
  hour: number;
  cost_usd: number;
  turn_count: number;
};

export type AnalysisCostByDowHour = {
  days: number;
  timezone: string;
  cells: DowHourCell[];
};

// ---------- /api/analysis/cache-savings-trend ----------

export type CacheSavingsPoint = {
  day: string;
  savings_usd: number;
  cache_read_tokens: number;
};

export type AnalysisCacheSavingsTrend = {
  days: number;
  points: CacheSavingsPoint[];
};

// ---------- /api/session/<id> ----------

export type SessionModelBucket = {
  model: string;
  input: number;
  output: number;
  cache_read: number;
  cache_creation: number;
  reasoning?: number;
  web_search_requests?: number;
  turn_count: number;
  cost_usd: number;
  ai_cost_usd: number;
  tool_cost_usd: number;
  // Per-bucket AICost components (v1.6.13). Sum equals ai_cost_usd.
  // Feeds the session-detail Models Used panel's $-mode stacked bar.
  // Zero when the row's cost came from a recorded estimated_cost_usd
  // (OpenCode/Pi) instead of cost-engine pricing — the bar still
  // renders but as a single undifferentiated AI block.
  input_cost_usd: number;
  output_cost_usd: number;
  cache_read_cost_usd: number;
  cache_creation_cost_usd: number;
};

export type ActionBucket = {
  action_type: string;
  count: number;
  failures: number;
};

export type SessionDetail = {
  id: string;
  tool: string;
  project: string;
  model?: string;
  started_at: string;
  ended_at?: string;
  total_actions: number;
  success_actions: number;
  failure_actions: number;
  quality_score?: number;
  error_rate?: number;
  redundancy_ratio?: number;
  tokens: {
    input: number;
    output: number;
    cache_read: number;
    cache_creation: number;
    // 1h-ephemeral-tier subset of cache_creation; the rest is 5m
    // tier. Anthropic-only (0 elsewhere).
    cache_creation_1h: number;
    reasoning: number;
  };
  per_model: SessionModelBucket[];
  cost_usd: number;
  ai_cost_usd: number;
  tool_cost_usd: number;
  tool_breakdown: ActionBucket[];
  // C15 cache annotation — compact glance-view next to the per-model
  // cost breakdown. Nil when the session has no cache_events
  // (pre-cachetrack history / non-Anthropic provider). The full
  // Cache tab loads /api/session/<id>/cache.
  cache_summary?: SessionCacheAnnotation;
};

// ---------- /api/session/<id>/cache (C13) ----------

export type SessionCacheAnnotation = {
  tier: "proxy" | "transcript" | "mixed" | "none";
  event_count: number;
  hit_count: number;
  write_count: number;
  rewrite_count: number;
  reanchor_count: number;
  mispredict_count: number;
  // zero_usage_count are turns whose tokens_read=0 AND
  // tokens_written=0 — observationally vacant; excluded from the
  // §10 mispredict rate per docs/cache-tracking.md.
  zero_usage_count: number;
  tokens_read: number;
  tokens_written: number;
  ratio: number;
  // has_flagged_rewrites means a tools_changed (or other neutral-
  // pill cause) was among the rewrites. UI renders neutrally, not
  // alarm-red.
  has_flagged_rewrites: boolean;
};

export type SessionCacheEntry = {
  prefix_hash: string;
  model: string;
  token_count: number;
  ttl_tier: string;
  tier: string;
  state: string;
  created_at: string;
  last_refresh_at: string;
  expires_at: string;
};

export type SessionCacheEvent = {
  timestamp: string;
  tier: string;
  model: string;
  kind: string;
  cause: string;
  predicted_kind?: string;
  tokens_read: number;
  tokens_written: number;
  message_id?: string;
  zero_usage?: boolean;
};

export type SessionCacheTimelineItem = {
  kind: "baseline" | "anomaly";
  // baseline-only
  count?: number;
  baseline_read_sum?: number;
  baseline_write_sum?: number;
  first_at?: string;
  last_at?: string;
  // anomaly-only
  event?: SessionCacheEvent;
  flagged?: boolean;
};

export type SessionCacheResponse = {
  tier: "proxy" | "transcript" | "mixed" | "none";
  entries: SessionCacheEntry[];
  events: SessionCacheEvent[];
  efficiency: {
    read_tokens: number;
    written_tokens: number;
    ratio: number;
    avoidable_usd: number;
  };
  timeline: SessionCacheTimelineItem[];
};

// ---------- /api/session/<id>/cache/forecast (P2 / §14.2) ----------

export type CacheForecastWarning =
  | "cache_wont_engage"
  | "fast_mode_active"
  | "try_1h_tier"
  | "switch_never_pays_off"
  | "empty_prefix";

export type CacheForecastResponse = {
  session_id: string;
  current_model: string;
  candidate_model: string;

  current_prefix_tokens: number;
  avg_suffix_tokens: number;
  avg_output_tokens: number;
  estimated_remaining_turns: number;
  observed_turns: number;
  current_fast?: boolean;
  has_gaps_over_5_min?: boolean;
  candidate_min_cacheable?: number;

  switch_cost_usd: number;
  per_turn_before_usd: number;
  per_turn_after_usd: number;
  savings_per_turn_usd: number;
  break_even_turns: number;
  estimated_net_savings_usd: number;

  warnings?: CacheForecastWarning[];
};

// ---------- /api/session/<id>/predict (Next-Message Cost & Limit Predictor) ----------

export type PredictWarning =
  | "no_session_history"
  | "turns_inferred_prior"
  | "turns_inferred_default"
  | "empty_prefix"
  | "fast_mode_active";

export type PredictTurnsTier = "observed" | "prior" | "default";

export type PredictBand = {
  turns: number;
  fresh_input: number;
  output: number;
  per_turn_usd: number;
  message_usd: number;
};

export type PredictEstimate = {
  model: string;
  prefix_tokens: number;
  has_estimate: boolean;
  turns_tier: PredictTurnsTier | "";
  low: PredictBand;
  mid: PredictBand;
  high: PredictBand;
  sample_turns: number;
  sample_messages: number;
  warnings?: PredictWarning[];
};

export type PredictLimitGauge = {
  available: boolean;
  needs_proxy: boolean;
  no_window?: boolean;
  // "proxy" (Anthropic response headers) or "transcript" (the tool's own
  // session log, e.g. codex token_count rate_limits). Empty when unavailable.
  source?: string;
  observed_age?: string;
  window_5h_util?: number;
  window_5h_reset?: number;
  window_7d_util?: number;
  window_7d_reset?: number;
  observed_at_unix?: number;
};

export type PredictResponse = {
  session_id: string;
  model: string;
  tool: string;
  estimate: PredictEstimate;
  reason?: string;
  limit: PredictLimitGauge;
};

// Output Composition (Verbosity) — GET /api/session/<id>/verbosity.
export type VerbosityLangBytes = {
  language: string;
  bytes: number;
  category: string;
};

export type VerbosityResponse = {
  session_id: string;
  total_bytes: number;
  code_bytes: number;
  explain_bytes: number;
  code_pct: number;
  explain_pct: number;
  code_explain_ratio?: number;
  by_category: Record<string, number>;
  channels: {
    narrative_bytes: number;
    artifact_bytes: number;
    artifact_untagged_bytes: number;
    written_bytes: number;
    command_bytes: number;
  };
  code_by_language: VerbosityLangBytes[];
  unknown_ext?: VerbosityLangBytes[];
  authored_captured: boolean;
  // Estimated token/$ attribution (plan §7) — labelled "est." on the card.
  // cost_estimated is false (and the $ figures absent) when there is no
  // priced model or no token rows for the session.
  cost_estimated: boolean;
  model?: string;
  est_output_tokens?: number;
  est_reasoning_tokens?: number;
  est_code_tokens?: number;
  est_explain_tokens?: number;
  est_code_usd?: number;
  est_explain_usd?: number;
  est_total_usd?: number;
};

// Output Composition cross-session rollup — GET /api/verbosity/aggregate.
export type VerbosityAggregateGroup = {
  key: string;
  code_bytes: number;
  explain_bytes: number;
  total_bytes: number;
  code_pct: number;
  explain_pct: number;
  code_explain_ratio?: number;
  top_languages: VerbosityLangBytes[];
  est_total_usd?: number;
  cost_estimated: boolean;
};

export type VerbosityAggregateResponse = {
  by: string;
  since_days: number;
  groups: VerbosityAggregateGroup[];
};

// ---------- /api/cache/overview (C14) ----------

export type CacheOverviewGlobal = {
  efficiency: {
    read_tokens: number;
    written_tokens: number;
    ratio: number;
    avoidable_usd: number;
  };
  event_count: number;
  session_count: number;
};

export type CacheOverviewModelRollup = {
  model: string;
  efficiency: {
    read_tokens: number;
    written_tokens: number;
    ratio: number;
    avoidable_usd: number;
  };
  event_count: number;
};

export type CacheOverviewProjectRollup = {
  project_id: number;
  project_root: string;
  efficiency: {
    read_tokens: number;
    written_tokens: number;
    ratio: number;
    avoidable_usd: number;
  };
  event_count: number;
};

export type CacheOverviewCauseRow = {
  cause: string;
  count: number;
  flagged?: boolean;
};

export type CacheOverviewSessionRow = {
  session_id: string;
  model: string;
  // Capture-path label rolled across this session's rewrite events:
  // "proxy" (Tier-1 live), "transcript" (Tier-2 backfill), or
  // "mixed" when both contributed. Omitted on rows with no tier
  // attribution (legacy events on pre-cachetrack corpora).
  tier?: string;
  rewrite_count: number;
  tokens_read: number;
  tokens_written: number;
  top_cause?: string;
};

export type CacheOverviewResponse = {
  global: CacheOverviewGlobal;
  per_model: CacheOverviewModelRollup[];
  per_project: CacheOverviewProjectRollup[];
  top_causes: CacheOverviewCauseRow[];
  worst_sessions: CacheOverviewSessionRow[];
};

// ---------- /api/cache/timeseries ----------

export type CacheTimeseriesPoint = {
  bucket: string;
  read_tokens: number;
  written_tokens: number;
  event_count: number;
  rewrite_count: number;
};

export type CacheTimeseriesResponse = {
  metric: "cache";
  bucket: "day";
  days: number;
  series: CacheTimeseriesPoint[];
};

// ---------- /api/cache/health ----------

export type CacheHealthDominantCause = {
  cause: string;
  count: number;
  share: number;
};

export type CacheHealthSummary = {
  graded_events: number;
  mispredicts: number;
  mispredict_rate: number;
  min_events_threshold: number;
  max_rate_threshold: number;
  gate_passed: boolean;
  bucket_mispredicts: number;
  inconsistent_rewrite_count: number;
  dominant_cause?: CacheHealthDominantCause;
  // Count of non-Anthropic api_turns rows over the last 7 days —
  // the proxy captured them (cost / tokens / model attribution all
  // work) but cachetrack didn't observe them because the engine's
  // attribution rules are Anthropic-shaped. Drives the info banner
  // on the Cache page that explicitly acknowledges "we saw N
  // codex sessions, just couldn't grade them" so the operator
  // doesn't read the empty Cache page as a bug.
  untracked_provider_turns?: number;
  untracked_provider_sessions?: number;
  untracked_provider_top_tool?: string;
  // §15.3 implicit-cache surfaces. Counts events whose kind is in
  // the implicit-cache closed set (implicit_hit / implicit_miss /
  // implicit_write). These events are EXCLUDED from
  // mispredict_rate / graded_events / mispredicts above (the
  // Anthropic gate) by design — the dashboard surfaces them
  // separately so operators can grade implicit-cache quality
  // without contaminating the soak-validated Anthropic signal.
  implicit_cache_events?: number;
  implicit_cache_hits?: number;
  implicit_cache_misses?: number;
  implicit_cache_writes?: number;
  // Consistency = (predicted == observed) / graded; the implicit-
  // cache analog of mispredict_rate. Bootstrap implicit_write
  // turns are excluded from the denominator.
  implicit_cache_consistency_rate?: number;
  implicit_cache_consistency_denom?: number;
  // hits / (hits + misses) — the prefix-survival rate.
  implicit_cache_prefix_churn_rate?: number;
};

// ---------- /api/cache/status ----------

export type CacheWindow = {
  model: string;
  scope: string;
  session_id: string;
  prefix_tokens: number;
  ttl_tier: string;
  expires_at: string;
  last_refresh: string;
  expiry_authoritative: boolean;
  refreshable_by_patch: boolean;
  supports_1h_tier: boolean;
  value_at_risk_usd: number;
};

export type CacheKeepWarmRecommendation = {
  action: "none" | "use_1h_tier" | "patch_ttl" | "replay_ping";
  pays_off: boolean;
  projected_savings_usd: number;
  resume_confidence: number;
  rationale: string;
};

export type CacheWindowStatus = {
  window: CacheWindow;
  severity: "ok" | "soon" | "critical" | "cold";
  seconds_to_expiry: number;
  value_at_risk_usd: number;
  estimated: boolean;
  recommendation: CacheKeepWarmRecommendation;
};

export type CacheStatusResponse = {
  enabled: boolean;
  keepwarm_mode: "off" | "advise" | "enforce";
  windows: CacheWindowStatus[] | null;
};

// ---------- /api/cache/events ----------

export type CacheEventRow = {
  id: number;
  timestamp: string;
  session_id: string;
  model: string;
  tier: string;
  kind: string;
  cause?: string;
  predicted_kind?: string;
  tokens_read: number;
  tokens_written: number;
};

export type CacheEventsResponse = {
  rows: CacheEventRow[];
  total: number;
  limit: number;
  offset: number;
};

// ---------- /api/cache/entry-states ----------

export type CacheEntryStateRow = {
  state: string;
  count: number;
};

export type CacheEntryStatesResponse = {
  rows: CacheEntryStateRow[];
  total: number;
};

// ---------- /api/session/<id>/messages ----------

export type ToolCallRow = {
  // action_id is the actions.id primary key, used by the on-demand
  // /api/action/<id>/full_text endpoint when the copy / view-full-text
  // buttons need the untruncated body. Zero only on synthesized rows
  // (orphan-token stubs); skip the fetch for those.
  action_id: number;
  action_type: string;
  raw_tool_name: string;
  target: string;
  full_text?: string;
  // full_text_elided is true when raw_tool_input exceeded the per-row
  // inline cap (4 KiB) and was truncated for the /messages payload.
  // The UI must fetch /api/action/<id>/full_text for the untruncated
  // body before showing or copying.
  full_text_elided?: boolean;
  // has_full_output is true when actions.raw_tool_output is non-empty
  // for this row — i.e. the adapter captured a tool_result body that's
  // available via /api/action/<id>/full_text. The inline `excerpt`
  // stays 2 KiB (FTS5 cap); this flag tells the UI there's a fuller
  // version to offer behind the view-full-text affordance.
  has_full_output?: boolean;
  excerpt?: string;
  success: boolean;
  error_message?: string;
  timestamp: string;
  duration_ms?: number;
  permission_mode?: string;
  effort_level?: string;
  is_interrupt?: boolean;
  stop_reason?: string;
  service_tier?: string;
};

// ---------- /api/action/<id>/full_text ----------
// Returned by the on-demand fetch when the operator clicks a copy /
// view-full-text button. Both raw_tool_input + raw_tool_output are
// adapter-capped at 1 MiB; rows that overflowed adapter capture carry
// the trailing "…(content truncated at N bytes)…" marker so the UI
// can surface the truncation honestly.
export type ActionFullText = {
  action_id: number;
  action_type: string;
  target?: string;
  raw_tool_input?: string;
  raw_tool_output?: string;
};

export type MessageRow = {
  message_id: string;
  timestamp: string;
  role: string;
  model?: string;
  input: number;
  output: number;
  cache_read: number;
  cache_creation: number;
  cache_creation_1h: number;
  reasoning?: number;
  web_search_requests?: number;
  cost_usd: number;
  ai_cost_usd: number;
  tool_cost_usd: number;
  elapsed_ms?: number;
  // tps_ms is the denominator the Tok/s column divides output by — the
  // best available timing source the backend picked (see tps_basis).
  // Absent when no source applies (e.g. a single-inference non-proxied
  // codex turn) → Tok/s shows "—".
  tps_ms?: number;
  // tps_basis names which timing source tps_ms came from, for the Tok/s
  // tooltip: "measured" (proxy total_response_ms — the real per-call
  // wall-clock), "intra-turn" (MAX−MIN of a codex user-turn's
  // per-inference timestamps), or "elapsed" (gap-to-next-message, the
  // claude-code fallback).
  tps_basis?: "measured" | "intra-turn" | "elapsed";
  tool_duration_ms?: number;
  tool_call_count: number;
  // Per-turn reasoning effort — codex collaboration_mode.settings
  // .reasoning_effort, antigravity SKU-encoded (low/medium/high), or
  // any other adapter that captures a reasoning-effort knob. Empty
  // when the adapter doesn't expose one (Anthropic models, etc.).
  effort_level?: string;
  // Per-turn terminal reason (end_turn / max_tokens / tool_use /
  // stop_sequence / refusal) + served capacity tier (standard /
  // priority / batch), from the transcript. Empty when not captured.
  stop_reason?: string;
  service_tier?: string;
  // Fast indicates this turn was served in the provider's low-latency
  // "fast" tier (Anthropic Opus 4.8 speed:"fast"). cost_usd already
  // carries the 2x FastMultiplier premium. Renders a FAST badge.
  fast?: boolean;
  tool_calls: ToolCallRow[];
};

// NOTE: envelope key is `messages` (not `rows`) — different from
// /api/sessions and /api/actions. Confirmed in dashboard.go:1357.
export type SessionMessages = {
  session_id: string;
  messages: MessageRow[];
  total: number;
  limit: number;
  offset?: number;
};

// ---------- /api/actions (Phase 4 extended) ----------

export type ActionListRow = {
  id: number;
  timestamp: string;
  tool: string;
  session_id: string;
  project: string;
  action_type: string;
  raw_tool_name: string;
  target: string;
  success: boolean;
  error_message?: string;
  message_id: string;
  permission_mode?: string;
  effort_level?: string;
  is_interrupt?: boolean;
  stop_reason?: string;
  service_tier?: string;
  // Source provenance — which JSONL / proxy capture produced this
  // row. May be empty for synthesized rows where the adapter doesn't
  // track an origin file.
  source_file?: string;
  source_event_id?: string;
  // First 280 chars of the action's indexed body (FTS5 excerpt).
  // Surfaces "what did the tool actually do" inline.
  excerpt?: string;
};

export type ActionsResponse = {
  rows: ActionListRow[];
  page: number;
  limit: number;
  total: number;
};

// ---------- /api/discover ----------

export type StaleReadFile = {
  file_path: string;
  project: string;
  stale_count: number;
  cross_thread_stale_count: number;
  total_reads: number;
  est_wasted_tokens: number;
  file_size_bytes: number;
};

export type RepeatedCommand = {
  command: string;
  command_hash: string;
  project: string;
  total_runs: number;
  no_change_reruns: number;
  successful_runs: number;
  failed_runs: number;
};

export type CrossToolFile = {
  file_path: string;
  project: string;
  tools: string[];
  accesses: number;
};

export type DiscoverSummary = {
  total_actions: number;
  stale_read_count: number;
  cross_thread_stale_count: number;
  est_wasted_tokens: number;
  repeated_command_groups: number;
  cross_tool_file_count: number;
  native_action_count: number;
  bash_action_count: number;
};

export type DiscoverResponse = {
  stale_reads: StaleReadFile[] | null;
  stale_total: number;
  stale_page: number;
  stale_limit: number;
  repeated_commands: RepeatedCommand[] | null;
  repeated_total: number;
  repeated_page: number;
  repeated_limit: number;
  cross_tool_files: CrossToolFile[] | null;
  native_vs_bash: unknown;
  summary: DiscoverSummary;
  blended_input_rate_per_million: number;
};

// ---------- /api/patterns ----------

export type PatternType =
  | "hot_file"
  | "cs_change"
  | "edit_test_pair"
  | "knowledge_snippet"
  | "command";

export type PatternRow = {
  project: string;
  pattern_type: string;
  data: string;
  confidence: number;
  observation_count: number;
};

export type PatternsResponse = {
  rows: PatternRow[];
  page: number;
  limit: number;
  total: number;
};

// ---------- /api/patterns/timeseries ----------

export type PatternsTimeseriesPoint = {
  day: string;
  total: number;
  by_type: Record<string, number>;
};

export type PatternsTimeseries = {
  days: number;
  points: PatternsTimeseriesPoint[];
};

// ---------- /api/compression/by-model ----------

export type CompressionByModelRow = {
  model: string;
  mechanism: string;
  events: number;
  original_bytes: number;
  compressed_bytes: number;
  saved_bytes: number;
  saved_tokens_est: number;
  saved_usd_est: number;
};

export type CompressionByModelResponse = {
  days: number;
  rows: CompressionByModelRow[];
};

// ---------- /api/compression/events ----------

export type CompressionEvent = {
  id: number;
  api_turn_id: number;
  timestamp: string;
  mechanism: string;
  original_bytes: number;
  compressed_bytes: number;
  saved_bytes: number;
  original_tokens_est: number;
  compressed_tokens_est: number;
  saved_tokens_est: number;
  saved_usd_est: number;
  msg_index: number;
  importance_score: number;
  model: string;
  session_id: string;
  message_id: string;
  is_subagent_runtime: boolean;
};

export type CompressionEventsResponse = {
  rows: CompressionEvent[];
  page: number;
  limit: number;
  total: number;
};

// ---------- /api/compression/timeseries ----------

export type CompressionMechStats = {
  count: number;
  original_bytes: number;
  compressed_bytes: number;
  saved_bytes: number;
  saved_usd_est: number;
};

export type CompressionTimeseriesPoint = {
  bucket: string;
  by_mechanism: Record<string, CompressionMechStats>;
  total_saved_bytes: number;
  total_saved_usd_est: number;
  total_count: number;
};

export type CompressionTimeseries = {
  metric: "compression_events";
  days: number;
  series: CompressionTimeseriesPoint[];
};

// ---------- /api/compression/retrieval ----------

export type ShaCount = { sha: string; count: number };
export type ActionIdCount = { action_id: number; count: number };

export type StashedSample = {
  sha: string;
  snippet: string;
  bytes: number;
  count: number;
  retrieved_count: number;
};

export type CompressionRetrieval = {
  days: number;
  stash_retrievals: number;
  search_hits: number;
  total_stashes: number;
  retrieve_rate: number;
  top_retrieved_shas: ShaCount[];
  top_searched_actions: ActionIdCount[];
  stashed_samples: StashedSample[];
  hints: unknown[];
};

// ---------- /api/compaction/events ----------

export type CompactionEvent = {
  id: number;
  session_id: string;
  timestamp: string;
  tool: string;
  pre_action_count: number;
  injected_at?: string;
  ghost_files_after_count: number;
  file_snapshot_count: number;
};

export type CompactionEventsResponse = {
  days: number;
  count: number;
  sessions_affected: number;
  injections_fired: number;
  events: CompactionEvent[];
};

// ---------- /api/compression/rolling-cost ----------

export type CompressionRollingCost = {
  days: number;
  summary_calls: number;
  summary_input_tokens: number;
  summary_output_tokens: number;
  summary_cost_usd: number;
  rolling_savings_bytes: number;
  rolling_savings_tokens_est: number;
  rolling_savings_cost_usd_est: number;
  net_delta_usd: number;
};

// ---------- /api/setup/claude + /api/setup/codex ----------

export type SetupClaude = {
  tool: "claude";
  proxy_port: number;
  proxy_url: string;
  credentials_path: string;
  has_oauth_credentials: boolean;
  claude_binary_found: boolean;
  claude_binary_path?: string;
  launcher_command: string;
  status: "oauth_ready" | "api_key_ready" | "claude_not_installed" | string;
  // Durable routing state — the env.ANTHROPIC_BASE_URL block in
  // ~/.claude/settings.json (managed by POST /api/setup/claude).
  settings_path?: string;
  routed_base_url?: string;
  routed_to_observer: boolean;
  would_register: boolean;
  would_register_error?: string;
};

// SetupRoutePostResponse is the shared POST /api/setup/{claude,codex}
// response shape (claudeSetupPostResponse / codexSetupPostResponse
// server-side). Snapshot is the tool's fresh GET snapshot.
export type SetupRoutePostResponse = {
  tool: string;
  config_path: string;
  base_url: string;
  added: boolean;
  already_set: boolean;
  dry_run: boolean;
  error?: string;
};

export type SetupCodex = {
  tool: "codex";
  config_path: string;
  config_exists: boolean;
  proxy_port: number;
  desired_base_url: string;
  desired_model_provider: string;
  current_base_url?: string;
  current_model_provider?: string;
  has_reserved_openai_block: boolean;
  auth_mode?: string;
  status: string;
  would_register: boolean;
  would_register_error?: string;
};

// ---------- /api/tools/breakdown ----------

export type ToolBreakdown = {
  tool: string;
  total: number;
  by_type: Record<string, number>;
};

export type ToolsBreakdownResponse = {
  days: number;
  tools: ToolBreakdown[];
};

// ---------- /api/config ----------

// ModelPricing matches config.ModelPricing 1:1 — the keys come from
// the Go field names (no JSON tag → PascalCase in JSON), so the TS
// shape mirrors that.
export type ModelPricing = {
  Input: number;
  Output: number;
  CacheRead: number;
  CacheCreation: number;
  CacheCreation1h: number;
  LongContextThreshold: number;
  LongContextInput: number;
  LongContextOutput: number;
  LongContextCacheRead: number;
  LongContextCacheCreation: number;
  LongContextCacheCreation1h: number;
};

export type IntelligenceConfig = {
  CodeGraph: { Enabled: boolean };
  Pricing: { Models: Record<string, ModelPricing> };
  APIKeyEnv: string;
  SummaryModel: string;
  MonthlyBudgetUSD: number;
};

// The full Config object — most sections are opaque to the dashboard
// (we render them as read-only JSON). Intelligence + a few others
// are typed enough to drive forms.
export type ConfigShape = {
  Observer: Record<string, unknown>;
  Intelligence: IntelligenceConfig;
  Proxy: Record<string, unknown>;
  Compression: Record<string, unknown>;
  [key: string]: unknown;
};

export type ConfigResponse = {
  config_path: string;
  config: ConfigShape;
  editable_sections: string[];
  // Every resolvable compression profile — built-ins + user profiles
  // (P3.4). Dynamic option source for the Settings Profiles selects.
  profile_names?: string[];
};

// GET /api/tools/status — the Connected-tools matrix (P4.1). One row
// per supported tool; nil probes mean the integration doesn't exist
// for that tool (n/a, not false).
export type ToolProbe = {
  registered: boolean;
  partial?: boolean;
  detail?: string;
};

export type ToolStatusRow = {
  tool: string;
  detected: boolean;
  detected_path?: string;
  enabled: boolean;
  action_count: number;
  last_seen_at?: string;
  hooks?: ToolProbe;
  mcp?: ToolProbe;
  proxy?: ToolProbe;
};

export type ToolsStatusResponse = {
  tools: ToolStatusRow[];
  generated_at: string;
};

// GET /api/mcp/value — the MCP value meter (P4.10). Mirrors the
// advisor's mcp_overhead math (~1,900 schema tokens/turn; 3 calls per
// 100 turns = worth-it line). verdict no_data covers BOTH "no turns"
// and "audit disabled" — silence with audit off means invisibility,
// not absence.
export type MCPValueResponse = {
  window_days: number;
  audit_enabled: boolean;
  calls: number;
  ok_calls: number;
  denied_calls: number;
  bytes_returned: number;
  by_tool: { tool: string; calls: number; bytes: number }[];
  turns_estimate: number;
  calls_per_100_turns: number;
  overhead_tokens_estimate: number;
  schema_tokens_per_turn: number;
  threshold_calls_per_100: number;
  verdict: "active" | "low_use" | "unused" | "no_data";
};

// POST /api/tools/launch — best-effort terminal launch (P4.6).
// command is ALWAYS set (the copy-paste fallback); spawned=false with
// detail is the honest no-spawn outcome, never an error state.
export type ToolLaunchResponse = {
  tool: string;
  routed: boolean;
  command: string;
  method: string;
  spawned: boolean;
  detail?: string;
};

// GET /api/health/doctor — the `observer doctor` checks (P4.8).
export type DoctorReport = {
  checks: {
    name: string;
    status: "ok" | "warn" | "fail";
    message: string;
    details?: string[];
  }[];
  ok: number;
  warn: number;
  fail: number;
  all_ok: boolean;
  generated_at: string;
};

// GET /api/health/failures — recent failures grouped by command
// (P4.11). recovered = at least one attempt in the group eventually
// succeeded; the error/session/project fields come from the group's
// most recent failure.
export type HealthFailuresResponse = {
  window_days: number;
  total: number;
  failures: {
    command: string;
    fails: number;
    retries: number;
    recovered: boolean;
    last_at: string;
    error_category?: string;
    error_message?: string;
    session_id: string;
    project?: string;
  }[];
};

// GET /api/setup/codex-hooks — codex per-hook trust state (P4.3).
// Read-only: trust is approved inside codex itself; the instruction
// string tells the user exactly what to run there.
export type CodexHookTrust = {
  status:
    | "no_codex"
    | "no_hooks"
    | "config_missing"
    | "needs_trust"
    | "all_trusted"
    | "feature_disabled";
  hooks_file?: string;
  config_file?: string;
  registered_events?: string[];
  trusted_events?: string[];
  untrusted_events?: string[];
  feature_flag_enabled: boolean;
  instruction?: string;
};

// GET /api/config/profiles/<name> — one profile resolved against the
// master config, plus its raw TOML body for display.
export type ProfileShowResponse = {
  name: string;
  builtin: boolean;
  editable: boolean;
  raw: string;
  resolved: {
    Conversation?: {
      Mode?: string;
      TargetRatio?: number;
      PreserveLastN?: number;
      CompressTypes?: string[];
      Logs?: { MaxLines?: number; Head?: number; Tail?: number };
    };
    [key: string]: unknown;
  };
};

// The baked-in defaults come from `cost.Pricing` (snake_case JSON
// tags), NOT `config.ModelPricing` (PascalCase). Two distinct Go
// structs, two distinct wire shapes. Convert between them at the
// override-add boundary.
export type CostPricing = {
  input: number;
  output: number;
  cache_read: number;
  cache_creation: number;
  cache_creation_1h: number;
  long_context_threshold?: number;
  long_context_input?: number;
  long_context_output?: number;
  long_context_cache_read?: number;
  long_context_cache_creation?: number;
  long_context_cache_creation_1h?: number;
  web_search_per_request?: number;
};

export type PricingDefaultsResponse = {
  defaults: Record<string, CostPricing>;
};

export function costPricingToConfig(p: CostPricing): ModelPricing {
  return {
    Input: p.input ?? 0,
    Output: p.output ?? 0,
    CacheRead: p.cache_read ?? 0,
    CacheCreation: p.cache_creation ?? 0,
    CacheCreation1h: p.cache_creation_1h ?? 0,
    LongContextThreshold: p.long_context_threshold ?? 0,
    LongContextInput: p.long_context_input ?? 0,
    LongContextOutput: p.long_context_output ?? 0,
    LongContextCacheRead: p.long_context_cache_read ?? 0,
    LongContextCacheCreation: p.long_context_cache_creation ?? 0,
    LongContextCacheCreation1h: p.long_context_cache_creation_1h ?? 0,
  };
}

// ---------- /api/backfill/status + /api/backfill/run + jobs ----------

export type BackfillMode = {
  mode: string;
  flag: string;
  description: string;
  candidates: number; // -1 = file scan needed
  candidates_note?: string;
};

export type BackfillStatusResponse = {
  modes: BackfillMode[];
};

export type BackfillJobStatus = "running" | "done" | "failed" | string;

export type BackfillJob = {
  id: string;
  mode: string;
  status: BackfillJobStatus;
  started_at: string;
  ended_at?: string;
  exit_code?: number;
  output: string;
  error?: string;
};

export type BackfillRunResponse = {
  job_id: string;
  mode: string;
  status: BackfillJobStatus;
  started_at: string;
};

export type BackfillJobsListResponse = {
  jobs: BackfillJob[];
};

// ---- Teams / Org enrolment -------------------------------------------------

export type EnrolmentLastPush = {
  pushed_at: string;
  status: string;
  row_count: number;
  bytes: number;
  error?: string;
};

// EnrolmentStatus is GET /api/enrolment/status. `enrolled` is always present;
// the rest are populated only when enrolled. On a solo-local install (org mode
// off) the server returns { enrolled: false }.
export type EnrolmentStatus = {
  enrolled: boolean;
  org_id?: string;
  org_name?: string;
  org_server_url?: string;
  user_email?: string;
  enrolled_at?: string;
  credential_store?: string;
  last_push?: EnrolmentLastPush | null;
};

// --- Advisor (suggestions engine, spec §15.7) ---

// AdvisorSuggestion is one row of GET /api/suggestions.
export type AdvisorSuggestion = {
  dedup_key: string;
  detector: string;
  category: "cost" | "latency" | "quality" | "hygiene";
  scope: "session" | "project" | "global";
  scope_id?: string;
  severity: "info" | "advice" | "warning";
  title: string;
  nudge: string;
  savings_usd?: number;
  savings_min?: number;
  confidence: number;
  evidence: {
    numbers?: Record<string, number>;
    items?: { label: string; value: number; unit?: string }[];
    math?: string;
  };
  computed_at: string;
  window_days: number;
  // C3 (P4.4): machine-actionable remediation. kind drives the one
  // renderer in Suggestions.tsx; unknown kinds render nothing.
  action?: { kind: string; target: string; label: string };
};

// AdvisorReport is GET /api/suggestions.
export type AdvisorReport = {
  suggestions: AdvisorSuggestion[] | null;
  total_savings_usd: number;
  total_savings_min: number;
  window_days: number;
  generated_at: string;
  sessions_scanned: number;
};

// AdvisorListResponse is the paginated/filtered GET /api/suggestions shape
// (filters + pagination are handler-side; totals reflect the filtered set
// pre-pagination).
export type AdvisorListResponse = {
  suggestions: AdvisorSuggestion[] | null;
  total_count: number;
  page: number;
  limit: number;
  total_savings_usd: number;
  total_savings_min: number;
  by_category: Record<string, number>;
  by_detector: Record<string, number>;
  window_days: number;
  generated_at: string;
  sessions_scanned: number;
};

// ---------- Process Observability (docs/process-observability.md §13) ----------
// /api/session/<id>/processes (SessionProcessResponse) +
// /api/process/findings (ProcessFindingsResponse). Node-local OS-level
// process capture: the per-session tree + observe-only findings.

// ProcessFinding mirrors store.ProcessFindingRow — an observe-only §14 signal
// derived from the captured envelope (never blocks).
export type ProcessFinding = {
  rule_id: string;
  severity: string; // "info" | "warn" | "high"
  process_key: string;
  session_id: string;
  tool?: string;
  exe_basename?: string;
  target_kind?: string;
  target?: string;
  detail?: string;
  timestamp: string;
  action_id?: number;
  turn_index?: number;
};

// MetricSample is one sparkline point (mirrors processobs.MetricSample): a
// timestamped resource reading. cpu_ms cumulative; ws = working set bytes;
// rb/wb = cumulative disk read/write bytes.
export type MetricSample = {
  t: string;
  cpu_ms: number;
  ws: number;
  rb: number;
  wb: number;
};

// ProcessNode mirrors dashboard.ProcessNode — one process in the tree, with
// children nested. action_id/command carry the §9.2.4 message/action link;
// the metric fields + metric_samples carry the §dashboard-enhancements
// resource snapshot (Windows poll capturer; network absent — needs ETW).
export type ProcessNode = {
  process_key: string;
  pid: number;
  ppid: number;
  exe: string;
  argv_preview?: string;
  cwd?: string;
  attribution_source: string;
  attribution_confidence: string;
  exited: boolean;
  exit_code: number;
  exit_signal?: number;
  duration_ms: number;
  // started_at is the wall-clock instant the process was first observed
  // (RFC3339 UTC); the drawer shows the actual capture time. Empty when
  // unknown.
  started_at?: string;
  is_boundary?: boolean;
  action_id?: number;
  turn_index?: number;
  command?: string;
  // message_id is the assistant message that issued the spawning command
  // (§9.2.4) — present only on action-correlated processes.
  message_id?: string;
  seccomp_mode?: string;
  capabilities_eff?: string;
  apparmor_label?: string;
  selinux_label?: string;
  container_id?: string;
  // Resource metrics (present only when captured; network absent — needs ETW).
  cpu_ms?: number;
  working_set_bytes?: number;
  peak_rss_bytes?: number;
  read_bytes?: number;
  write_bytes?: number;
  thread_count?: number;
  handle_count?: number;
  metric_samples?: MetricSample[];
  children: ProcessNode[];
};

export type SessionProcessResponse = {
  session_id: string;
  total: number;
  roots: ProcessNode[];
  findings: ProcessFinding[];
};

// /api/process/findings — recent cross-session rollup.
export type ProcessFindingsResponse = {
  window_hours: number;
  total: number;
  by_rule: Record<string, number>;
  by_severity: Record<string, number>;
  findings: ProcessFinding[];
};
