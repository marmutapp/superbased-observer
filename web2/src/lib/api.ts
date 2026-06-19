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
}
export interface TeamRollup {
  team_id: string;
  display_name: string;
  member_count: number;
  active_developers: number;
  cost_usd: number;
  session_count: number;
  action_count: number;
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
export interface ProjectRollup {
  project_id: string;
  project_root: string;
  teams: TeamRef[];
  cost_usd: number;
  session_count: number;
  active_developers: number;
  tools: string[];
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
