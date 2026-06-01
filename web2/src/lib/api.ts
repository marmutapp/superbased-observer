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
};
