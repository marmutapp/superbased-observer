// Typed client for the org dashboard API (/api/org/*). Shapes mirror the Go
// rollup result types (internal/orgserver/rollup), which the OpenAPI spec
// references via x-go-type — one source of truth.

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    public readonly path: string,
    body: string,
  ) {
    super(`api ${status} ${path}: ${body.slice(0, 200)}`);
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: {
      Accept: "application/json",
      ...(init?.body ? { "Content-Type": "application/json" } : {}),
      ...(init?.headers ?? {}),
    },
  });
  if (!res.ok) {
    const body = await res.text().catch(() => "");
    throw new ApiError(res.status, path, body);
  }
  if (res.status === 204) return undefined as T;
  return res.json() as Promise<T>;
}

function withDays(path: string, days?: number): string {
  return days ? `${path}?days=${days}` : path;
}

// withDaysTool builds the "?days=&tool=" query suffix for the §6c-3 tool-
// filterable endpoints (Activity / People). Returns "" when neither is set.
function withDaysTool(days?: number, tool?: string): string {
  const p = new URLSearchParams();
  if (days) p.set("days", String(days));
  if (tool) p.set("tool", tool);
  const qs = p.toString();
  return qs ? `?${qs}` : "";
}

// --- wire types ------------------------------------------------------------

export interface CostPoint {
  date: string;
  cost_usd: number;
}
export interface TeamRef {
  team_id: string;
  display_name: string;
}
export interface TeamSpend {
  team_id: string;
  display_name: string;
  cost_usd: number;
}
export interface ProjectSpend {
  project_id: string;
  project_root: string;
  cost_usd: number;
}
export interface ModelSpend {
  model: string;
  cost_usd: number;
  tokens: number;
}
// --- Phase 1 Overview enrichment (additive; every block is optional so an
// older server that hasn't shipped them degrades to the v1 surface). ---
export interface TokenBuckets {
  net_input: number;
  cache_read: number;
  cache_write: number;
  output: number;
  reasoning: number;
}
export interface CacheStats {
  read_tokens: number;
  write_tokens: number;
  input_tokens: number;
  hit_ratio: number;
  read_write_ratio: number;
}
export interface ReliabilitySplit {
  proxy_cost_usd: number;
  estimated_cost_usd: number;
  proxy_share: number;
}
export interface KeyCount {
  key: string;
  count: number;
}
export interface ErrorStats {
  total_actions: number;
  failed_actions: number;
  action_error_rate: number;
  api_turns: number;
  http_errors: number;
  by_error_class?: KeyCount[];
}
export interface LatencyStats {
  sample_size: number;
  median_ttft_ms: number;
  median_total_ms: number;
  p95_total_ms: number;
}
export interface ToolSlice {
  tool: string;
  cost_usd: number;
  tokens: number;
}
export interface ModelSlice {
  model: string;
  cost_usd: number;
  tokens: number;
}
export interface DayCount {
  date: string;
  count: number;
}
export interface HourCount {
  hour: number;
  count: number;
}
export interface PeriodDeltas {
  cost_usd: number;
  sessions: number;
  actions: number;
  prior_cost_usd: number;
  prior_sessions: number;
  prior_actions: number;
  has_prior: boolean;
}
export interface Overview {
  window_days: number;
  total_cost_usd: number;
  total_sessions: number;
  total_actions: number;
  total_api_turns: number;
  active_developers: number;
  team_count: number;
  project_count: number;
  cost_by_day: CostPoint[];
  top_teams: TeamSpend[];
  top_projects: ProjectSpend[];
  // Enriched (optional).
  tokens?: TokenBuckets;
  cache?: CacheStats;
  reliability?: ReliabilitySplit;
  errors?: ErrorStats;
  latency?: LatencyStats;
  tool_mix?: ToolSlice[];
  model_mix?: ModelSlice[];
  actions_by_day?: DayCount[];
  hour_of_day?: HourCount[];
  deltas?: PeriodDeltas;
}
export interface TeamRollup {
  team_id: string;
  display_name: string;
  member_count: number;
  active_developers: number;
  cost_usd: number;
  session_count: number;
  action_count: number;
  spark?: number[];
  top_tools?: string[];
}
export interface TeamsResult {
  window_days: number;
  teams: TeamRollup[];
}
export interface TeamDetail {
  team_id: string;
  display_name: string;
  window_days: number;
  cost_usd: number;
  session_count: number;
  action_count: number;
  api_turn_count: number;
  member_count: number;
  active_developers: number;
  cost_by_day: CostPoint[];
  top_projects: ProjectSpend[];
  top_models: ModelSpend[];
}
export interface DeveloperRollup {
  user_id: string;
  email: string;
  display_name: string;
  role: string;
  cost_usd: number;
  session_count: number;
  action_count: number;
  last_active?: string;
}
export interface DevelopersResult {
  team_id: string;
  display_name: string;
  window_days: number;
  developers: DeveloperRollup[];
}
export interface PersonRollup {
  user_id: string;
  email: string;
  display_name?: string;
  cost_usd: number;
  session_count: number;
  action_count: number;
  tokens: number;
  top_tool?: string;
  top_model?: string;
  last_active?: string;
  spark?: number[];
}
export interface PeopleResult {
  window_days: number;
  people: PersonRollup[];
}
// --- Phase 3: Tools / Models / Activity ---
export interface ToolRow {
  tool: string;
  cost_usd: number;
  tokens: number;
  buckets: TokenBuckets;
  sessions: number;
  active_devs: number;
  action_count: number;
  success_rate: number;
  avg_ttft_ms: number;
}
export interface ToolsResult {
  window_days: number;
  tools: ToolRow[];
}
export interface ModelRow {
  model: string;
  cost_usd: number;
  tokens: number;
  buckets: TokenBuckets;
  sessions: number;
  active_devs: number;
  avg_ttft_ms: number;
}
export interface ModelsResult {
  window_days: number;
  models: ModelRow[];
}
export interface ToolDayCount {
  date: string;
  tool: string;
  count: number;
}
export interface DayBuckets {
  date: string;
  net_input: number;
  cache_read: number;
  cache_write: number;
  output: number;
  reasoning: number;
}
export interface DowHourCount {
  dow: number;
  hour: number;
  count: number;
}
export interface ActivityResult {
  window_days: number;
  cost_by_day: CostPoint[];
  actions_by_day: DayCount[];
  tool_by_day: ToolDayCount[];
  tokens_by_day: DayBuckets[];
  hour_of_day: HourCount[];
  dow_hour: DowHourCount[];
}
// --- Phase 4: native-console vendor telemetry ---
export interface AcceptanceStats {
  accepted: number;
  rejected: number;
  accept_rate: number;
}
export interface SeatStats {
  total: number;
  active: number;
  inactive: number;
  utilization: number;
}
export interface VendorTelemetry {
  vendor: string;
  display_name: string;
  days: number;
  last_pulled_at?: string;
  cost_usd: number;
  cost_unit?: string;
  credits_cost?: number;
  tokens?: TokenBuckets;
  acceptance?: AcceptanceStats;
  seats?: SeatStats;
  engagement?: KeyCount[];
  surfaces?: string[];
}
export interface TelemetryResult {
  window_days: number;
  configured: boolean;
  vendors: VendorTelemetry[];
}
// Model-routing org rollup (§R19) — admin-only. Mirrors rollup.RoutingResult.
export interface RoutingDayPoint {
  date: string;
  decisions: number;
  applied: number;
  est_savings_usd: number;
}
export interface RoutingDimCount {
  key: string;
  decisions: number;
  applied: number;
  est_savings_usd: number;
}
export interface RoutingResult {
  window_days: number;
  configured: boolean;
  total_decisions: number;
  total_applied: number;
  est_savings_usd: number;
  cache_forfeit_usd: number;
  net_savings_usd: number;
  advise_decisions: number;
  enforce_decisions: number;
  by_day: RoutingDayPoint[];
  by_tier: RoutingDimCount[];
  by_reason: RoutingDimCount[];
}
// Org-tier observability analytics (obs-org-tier T1). Mirrors
// rollup.ObsAnalyticsResult — content-free cost/token/latency/error rollups.
export interface ObsDayPoint {
  date: string;
  traces: number;
  tokens: number;
  cost_usd: number;
  error_traces: number;
}
export interface ObsDimCount {
  key: string;
  traces: number;
  tokens: number;
  cost_usd: number;
}
export interface ObsAnalyticsResult {
  window_days: number;
  configured: boolean;
  total_traces: number;
  total_spans: number;
  total_tokens: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  reasoning_tokens: number;
  total_cost_usd: number;
  error_traces: number;
  error_rate: number;
  avg_duration_ms: number;
  by_day: ObsDayPoint[];
  by_model: ObsDimCount[];
  by_project: ObsDimCount[];
  by_source: ObsDimCount[];
  // Latency depth (OP5) — present when a node shared T2 span structure.
  latency_configured: boolean;
  latency_p50_ms: number;
  latency_p95_ms: number;
  latency_p99_ms: number;
  by_kind: ObsKindLatency[];
  error_causes: ObsDimCount[];
}
export interface ObsKindLatency {
  kind: string;
  spans: number;
  p50_ms: number;
  p95_ms: number;
  avg_ms: number;
}
// Org trajectory explorer (obs-org-tier T2). Mirrors rollup.ObsTrajectories*.
export interface ObsTraceListRow {
  trace_id: string;
  root_name: string;
  source: string;
  session_id: string;
  status: string;
  email: string;
  started_at: string;
  duration_ms: number;
  span_count: number;
  total_tokens: number;
  cost_usd: number;
}
export interface ObsTrajectoriesResult {
  window_days: number;
  configured: boolean;
  traces: ObsTraceListRow[];
}
// The proxy-exact wedge (joined from api_turns by request_id).
export interface ObsProxyEnrichment {
  found: boolean;
  provider: string;
  model: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_creation_tokens: number;
  cost_usd: number;
}
export interface ObsSpanDetail {
  span_id: string;
  parent_span_id: string;
  kind: string;
  name: string;
  started_at: string;
  ended_at: string;
  duration_ms: number;
  status: string;
  model: string;
  provider: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  reasoning_tokens: number;
  total_tokens: number;
  cost_usd: number;
  cost_source: string;
  request_id: string;
  enrichment?: ObsProxyEnrichment;
}
export interface ObsTraceDetailResult {
  trace: ObsTraceListRow;
  spans: ObsSpanDetail[];
}
// Audited span-content viewer (obs-org-tier T3).
export interface ObsContentEntry {
  span_id: string;
  kind: string;
  content_hash: string;
  content: string;
  has_raw: boolean;
  timestamp: string;
}
export interface ObsContentResult {
  trace_id: string;
  entries: ObsContentEntry[];
  any_raw: boolean;
}
// Org eval-health (obs-org-tier T4). Mirrors rollup.ObsEvals*.
export interface ObsEvalScorer {
  scorer_name: string;
  total: number;
  passed: number;
  pass_rate: number;
  mean_score: number;
  min_score: number;
  pass_rate_delta: number;
}
export interface ObsEvalRunGroup {
  day: string;
  dataset_name: string;
  run_name: string;
  source: string;
  total: number;
  passed: number;
  pass_rate: number;
  mean_score: number;
  regressed: boolean;
  scorers: ObsEvalScorer[];
}
export interface ObsEvalsResult {
  window_days: number;
  configured: boolean;
  runs: ObsEvalRunGroup[];
}
// Org observability cost attribution (obs-org-tier OP6). Mirrors rollup.ObsCost*.
export interface ObsCostBucket {
  key: string;
  label: string;
  cost_usd: number;
  tokens: number;
  traces: number;
  cost_share: number;
}
export interface ObsCostResult {
  window_days: number;
  configured: boolean;
  total_cost_usd: number;
  by_developer: ObsCostBucket[];
  by_project: ObsCostBucket[];
  by_model: ObsCostBucket[];
}
// Org observability alerting (obs-org-tier OP6b). Mirrors obsalert.*.
export interface ObsAlertRule {
  id: string;
  name: string;
  metric: string;
  comparator: string;
  threshold: number;
  window_days: number;
  webhook_url: string;
  cooldown_minutes: number;
  enabled: boolean;
  last_fired_at: string;
  last_value: number;
  breaching: boolean;
}
export interface ObsAlertEvent {
  rule_id: string;
  metric: string;
  threshold: number;
  value: number;
  delivered: boolean;
  fired_at: string;
}
export interface ObsAlertsResult {
  rules: ObsAlertRule[];
  events: ObsAlertEvent[];
}
export interface NewObsAlert {
  name?: string;
  metric: string;
  comparator?: string;
  threshold: number;
  window_days?: number;
  webhook_url?: string;
  cooldown_minutes?: number;
}
// Org session list/detail (§6c-2) — AUDITED. Mirrors rollup.SessionRow etc.
export interface SessionRow {
  session_id: string;
  user_id: string;
  email?: string;
  display_name?: string;
  tool?: string;
  model?: string;
  project_id?: string;
  started_at?: string;
  ended_at?: string;
  cost_usd: number;
  tokens: number;
  action_count: number;
  api_turn_count: number;
}
export interface SessionsResult {
  window_days: number;
  total: number;
  limit: number;
  offset: number;
  sessions: SessionRow[];
}
export interface ActionTypeCount {
  action_type: string;
  count: number;
  success_count: number;
}
export interface SessionDetailResult {
  session_id: string;
  user_id: string;
  email?: string;
  display_name?: string;
  tool?: string;
  model?: string;
  project_id?: string;
  started_at?: string;
  ended_at?: string;
  cost_usd: number;
  tokens: number;
  action_count: number;
  api_turn_count: number;
  buckets: TokenBuckets;
  action_types: ActionTypeCount[];
}
// Captured OTel message bodies for one session (§7) — a DEEPER, separately
// audited disclosure (view_session_messages). content is present only where the
// node shared it (hash-only otherwise); captured today only on Claude Code
// nodes with native-OTel logging on. Mirrors rollup.MessagesResult.
export interface MessageEntry {
  kind: string; // prompt | tool_input | tool_output | raw_body
  request_id?: string;
  tool_use_id?: string;
  timestamp?: string;
  content?: string; // omitted when the body was not shared (hash-only)
  content_hash: string;
}
export interface MessagesResult {
  session_id: string;
  user_id: string;
  email?: string;
  display_name?: string;
  project_id?: string;
  content_available: boolean;
  messages: MessageEntry[];
}
export interface SessionsQuery {
  days?: number;
  limit?: number;
  offset?: number;
  tool?: string;
  model?: string;
}
// Live presence (§6d). Mirrors rollup.LiveResult.
export interface LiveSession {
  session_id: string;
  user_id: string;
  email?: string;
  display_name?: string;
  tool?: string;
  model?: string;
  last_active: string;
  cost_usd: number;
  action_count: number;
}
export interface LiveResult {
  window_minutes: number;
  active_devs: number;
  sessions: LiveSession[];
}
// Movers (§6d). Mirrors rollup.MoversResult.
export interface MoverRow {
  key: string;
  current_usd: number;
  prior_usd: number;
  delta_usd: number;
}
export interface MoversResult {
  window_days: number;
  dimension: string;
  increases: MoverRow[];
  decreases: MoverRow[];
  new_entrants: MoverRow[];
}
// Monthly report (§6d). Mirrors rollup.ReportResult.
export interface KeyCost {
  key: string;
  cost_usd: number;
  tokens: number;
}
export interface ReportSession {
  session_id: string;
  email?: string;
  tool?: string;
  cost_usd: number;
}
export interface ReportResult {
  month: string;
  total_usd: number;
  by_model: KeyCost[];
  by_tool: KeyCost[];
  by_project: KeyCost[];
  top_sessions: ReportSession[];
}
// Org advisor (§6d). Mirrors rollup.SuggestionsResult.
export interface Suggestion {
  id: string;
  severity: string;
  title: string;
  detail: string;
  metric?: string;
}
export interface SuggestionsResult {
  window_days: number;
  suggestions: Suggestion[];
}
export interface ProjectRollup {
  project_id: string;
  project_root: string;
  teams: TeamRef[];
  cost_usd: number;
  session_count: number;
  active_developers: number;
  tools: string[];
  spark?: number[];
  buckets?: TokenBuckets;
}
export interface ProjectsResult {
  window_days: number;
  projects: ProjectRollup[];
}
export interface ProjectDetail {
  project_id: string;
  project_root: string;
  window_days: number;
  teams: TeamRef[];
  cost_usd: number;
  session_count: number;
  active_developers: number;
  tools: string[];
  cost_by_day: CostPoint[];
  top_models: ModelSpend[];
}
export interface BudgetStatus {
  id: string;
  scope: "team" | "project";
  scope_id: string;
  scope_label: string;
  monthly_usd_cap: number;
  alert_webhook_url?: string;
  alert_thresholds: number[];
  current_spend_usd: number;
  current_ratio: number;
  last_fired_threshold: number;
  created_at: string;
  updated_at: string;
}
export interface BudgetsResult {
  budgets: BudgetStatus[];
}
export interface BudgetInput {
  scope: "team" | "project";
  scope_id: string;
  monthly_usd_cap: number;
  alert_webhook_url?: string;
  alert_thresholds?: number[];
}
export interface AuditEntry {
  id: number;
  actor_user_id: string;
  actor_email?: string;
  action: string;
  target_team_id?: string;
  target_detail?: string;
  source_ip?: string;
  timestamp: string;
}
export interface AuditResult {
  entries: AuditEntry[];
  next_offset: number;
  has_more: boolean;
}
export interface BearerInfo {
  jti: string;
  issued_at: string;
  expires_at: string;
  revoked: boolean;
}
export interface BearersResult {
  user_id: string;
  bearers: BearerInfo[];
}
export interface Member {
  user_id: string;
  user_name: string;
  email: string;
  display_name?: string;
}
export interface MembersResult {
  members: Member[];
}

// --- guard wire types (mirror rollup.Guard* — guard spec §14.3 / §14.5) -----

export interface GuardTrendPoint {
  date: string;
  deny: number;
  ask: number;
  flag: number;
  mask: number;
  other: number;
  total: number;
}
export interface GuardRuleHit {
  rule_id: string;
  category: string;
  severity: string;
  hits: number;
  agents: number;
  deny_count: number;
  last_seen: string;
}
export interface GuardOverview {
  window_days: number;
  total_events: number;
  deny_count: number;
  ask_count: number;
  flag_count: number;
  mask_count: number;
  enforced_count: number;
  active_agents: number;
  rule_count: number;
  broken_chain_agents: number;
  trend_by_day: GuardTrendPoint[];
  top_rules: GuardRuleHit[];
}
export interface GuardRulesResult {
  window_days: number;
  rules: GuardRuleHit[];
}
export interface GuardTeamPosture {
  team_id: string;
  display_name: string;
  member_count: number;
  active_agents: number;
  events: number;
  deny_count: number;
  ask_count: number;
  flag_count: number;
  mask_count: number;
  enforced_share: number;
  broken_chain_agents: number;
}
export interface GuardTeamsResult {
  window_days: number;
  teams: GuardTeamPosture[];
}
export interface GuardAgentChain {
  user_id: string;
  email: string;
  display_name?: string;
  events: number;
  heads: number;
  unlinked: number;
  segments: number;
  broken: boolean;
  first_seen?: string;
  last_seen?: string;
}
export interface GuardAgentsResult {
  agents: GuardAgentChain[];
}
export interface GuardPolicyBundleInfo {
  version: number;
  signed_at: string;
  created_by: string;
  description?: string;
  toml_bytes: number;
}
export interface GuardPolicyBundlesResult {
  active_version: number;
  signing_configured: boolean;
  bundles: GuardPolicyBundleInfo[];
}
export interface GuardPolicyBundleDetail {
  version: number;
  bundle_toml: string;
  signed_at: string;
  description?: string;
}
export interface GuardRuleDryRun {
  rule_id: string;
  hits: number;
  agents: number;
  computable: boolean;
}
export interface GuardPolicyLintResult {
  ok: boolean;
  problems: string[];
  window_days: number;
  dry_run: GuardRuleDryRun[];
}
export interface GuardPolicyPublishResult {
  version: number;
}

// --- endpoints -------------------------------------------------------------

export const api = {
  overview: (days?: number) => request<Overview>(withDays("/api/org/overview", days)),
  teams: (days?: number) => request<TeamsResult>(withDays("/api/org/teams", days)),
  teamDetail: (id: string, days?: number) =>
    request<TeamDetail>(withDays(`/api/org/teams/${encodeURIComponent(id)}`, days)),
  teamDevelopers: (id: string, days?: number) =>
    request<DevelopersResult>(withDays(`/api/org/teams/${encodeURIComponent(id)}/developers`, days)),
  // Org-wide per-developer leaderboard — AUDITED disclosure (the server writes
  // a view_org_developers audit row). Call ONLY on an explicit user action.
  people: (days?: number) => request<PeopleResult>(withDays("/api/org/people", days)),
  tools: (days?: number) => request<ToolsResult>(withDays("/api/org/tools", days)),
  models: (days?: number) => request<ModelsResult>(withDays("/api/org/models", days)),
  activity: (days?: number, tool?: string) =>
    request<ActivityResult>(`/api/org/activity${withDaysTool(days, tool)}`),
  // Native-console vendor analytics (Claude Code / Codex / Copilot) — admin-only.
  telemetry: (days?: number) => request<TelemetryResult>(withDays("/api/org/telemetry", days)),
  // Model-routing org rollup (§R19) — admin-only.
  routing: (days?: number) => request<RoutingResult>(withDays("/api/org/routing", days)),
  // Org-tier observability analytics (obs-org-tier T1) — admin-only.
  obsAnalytics: (days?: number) => request<ObsAnalyticsResult>(withDays("/api/org/obs/analytics", days)),
  // Org trajectory explorer (obs-org-tier T2) — RBAC-scoped.
  obsTrajectories: (days?: number) => request<ObsTrajectoriesResult>(withDays("/api/org/obs/trajectories", days)),
  obsTrace: (id: string) => request<ObsTraceDetailResult>(`/api/org/obs/trace/${encodeURIComponent(id)}`),
  // Audited span-content viewer (obs-org-tier T3) — writes a view_span_content audit row.
  obsTraceContent: (id: string) => request<ObsContentResult>(`/api/org/obs/trace/${encodeURIComponent(id)}/content`),
  // Org eval-health (obs-org-tier T4) — admin-only.
  obsEvals: (days?: number) => request<ObsEvalsResult>(withDays("/api/org/obs/evals", days)),
  // Org observability cost attribution (obs-org-tier OP6) — admin-only.
  obsCost: (days?: number) => request<ObsCostResult>(withDays("/api/org/obs/cost", days)),
  // Org observability alerting (obs-org-tier OP6b) — admin-only.
  obsAlerts: () => request<ObsAlertsResult>("/api/org/obs/alerts"),
  createObsAlert: (rule: NewObsAlert) =>
    request<{ id: string }>("/api/org/obs/alerts", { method: "POST", body: JSON.stringify(rule) }),
  deleteObsAlert: (id: string) =>
    request<void>(`/api/org/obs/alert/${encodeURIComponent(id)}`, { method: "DELETE" }),
  // Org session list — AUDITED (the server writes a view_org_sessions row).
  sessions: (q: SessionsQuery = {}) => {
    const p = new URLSearchParams();
    if (q.days) p.set("days", String(q.days));
    if (q.limit) p.set("limit", String(q.limit));
    if (q.offset) p.set("offset", String(q.offset));
    if (q.tool) p.set("tool", q.tool);
    if (q.model) p.set("model", q.model);
    const qs = p.toString();
    return request<SessionsResult>(`/api/org/sessions${qs ? `?${qs}` : ""}`);
  },
  // Org session detail — AUDITED (session id in target_detail).
  sessionDetail: (id: string) =>
    request<SessionDetailResult>(`/api/org/sessions/${encodeURIComponent(id)}`),

  // Captured OTel message bodies for one session (§7). DEEPER disclosure than
  // the detail: calling this writes a distinct view_session_messages audit row.
  sessionMessages: (id: string) =>
    request<MessagesResult>(`/api/org/sessions/${encodeURIComponent(id)}/messages`),
  // Live presence — AUDITED (the server writes a view_org_sessions row).
  live: () => request<LiveResult>("/api/org/live"),
  // Period-over-period movers by dimension (model | project | tool).
  movers: (days?: number, dim?: string) => {
    const p = new URLSearchParams();
    if (days) p.set("days", String(days));
    if (dim) p.set("dim", dim);
    const qs = p.toString();
    return request<MoversResult>(`/api/org/movers${qs ? `?${qs}` : ""}`);
  },
  // Monthly cost statement (calendar month YYYY-MM; default current).
  report: (month?: string) =>
    request<ReportResult>(`/api/org/report${month ? `?month=${encodeURIComponent(month)}` : ""}`),
  // Org-wide cost/hygiene advisories.
  suggestions: (days?: number) => request<SuggestionsResult>(withDays("/api/org/suggestions", days)),
  projects: (days?: number) => request<ProjectsResult>(withDays("/api/org/projects", days)),
  projectDetail: (id: string, days?: number) =>
    request<ProjectDetail>(withDays(`/api/org/projects/${encodeURIComponent(id)}`, days)),
  budgets: () => request<BudgetsResult>("/api/org/budgets"),
  createBudget: (b: BudgetInput) =>
    request<BudgetStatus>("/api/org/budgets", { method: "POST", body: JSON.stringify(b) }),
  updateBudget: (id: string, b: BudgetInput) =>
    request<BudgetStatus>(`/api/org/budgets/${encodeURIComponent(id)}`, {
      method: "PUT",
      body: JSON.stringify(b),
    }),
  deleteBudget: (id: string) =>
    request<void>(`/api/org/budgets/${encodeURIComponent(id)}`, { method: "DELETE" }),
  audit: (limit: number, offset: number) =>
    request<AuditResult>(`/api/org/audit?limit=${limit}&offset=${offset}`),
  logDrillDown: (teamId: string) =>
    request<void>("/api/org/audit/log-drill-down", {
      method: "POST",
      body: JSON.stringify({ team_id: teamId }),
    }),
  listBearers: (userId: string) =>
    request<BearersResult>(`/api/org/admin/bearers?user_id=${encodeURIComponent(userId)}`),
  revokeBearer: (jti: string) =>
    request<void>("/api/org/admin/revoke", { method: "POST", body: JSON.stringify({ jti }) }),
  setTeamRole: (teamId: string, userId: string, role: "member" | "lead") =>
    request<void>("/api/org/admin/team-role", {
      method: "POST",
      body: JSON.stringify({ team_id: teamId, user_id: userId, role }),
    }),
  mintEnrolmentToken: (userId: string, ttlDays?: number) =>
    request<{ token: string; token_id: string; user_id: string; expires_at: string }>(
      "/api/org/enrolment-tokens",
      { method: "POST", body: JSON.stringify({ user_id: userId, ttl_days: ttlDays }) },
    ),
  listOrgMembers: () => request<MembersResult>("/api/org/members"),

  // Guard rollups + policy authoring (guard spec §14.3 / §14.5).
  guardOverview: (days?: number) =>
    request<GuardOverview>(withDays("/api/org/guard/overview", days)),
  guardRules: (days?: number) =>
    request<GuardRulesResult>(withDays("/api/org/guard/rules", days)),
  guardTeams: (days?: number) =>
    request<GuardTeamsResult>(withDays("/api/org/guard/teams", days)),
  // Server-side AUDITED disclosure — call only on an explicit user action.
  guardAgents: () => request<GuardAgentsResult>("/api/org/guard/agents"),
  guardPolicyBundles: () =>
    request<GuardPolicyBundlesResult>("/api/org/guard/policy/bundles"),
  guardPolicyBundleDetail: (version: number) =>
    request<GuardPolicyBundleDetail>(`/api/org/guard/policy/bundles/${version}`),
  guardPolicyLint: (bundleToml: string) =>
    request<GuardPolicyLintResult>("/api/org/guard/policy/lint", {
      method: "POST",
      body: JSON.stringify({ bundle_toml: bundleToml }),
    }),
  guardPolicyPublish: (bundleToml: string, description?: string) =>
    request<GuardPolicyPublishResult>("/api/org/guard/policy/publish", {
      method: "POST",
      body: JSON.stringify({ bundle_toml: bundleToml, description }),
    }),
};
