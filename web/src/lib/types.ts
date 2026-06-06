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
};

export type StatusSnapshot = {
  db_path: string;
  db_size_bytes: number;
  schema_version: number;
  counts: SnapshotCounts;
  last_action_at?: string;
  last_action_tool?: string;
  per_tool_last_seen: ToolActivity[];
  recent_failures_24h: number;
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
  tool_duration_ms?: number;
  tool_call_count: number;
  // Per-turn reasoning effort — codex collaboration_mode.settings
  // .reasoning_effort, antigravity SKU-encoded (low/medium/high), or
  // any other adapter that captures a reasoning-effort knob. Empty
  // when the adapter doesn't expose one (Anthropic models, etc.).
  effort_level?: string;
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

export type CompressionRetrieval = {
  days: number;
  stash_retrievals: number;
  search_hits: number;
  total_stashes: number;
  retrieve_rate: number;
  top_retrieved_shas: ShaCount[];
  top_searched_actions: ActionIdCount[];
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
