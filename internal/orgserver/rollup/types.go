package rollup

// The types below are the dashboard wire contract. The OpenAPI spec references
// them via x-go-type (so the generated stubs share one source of truth) and
// golden_test.go pins their JSON encoding. Every field is a count, cost,
// label, or timestamp — never content.

// Window bounds a rollup query to the trailing Days days (UTC). A zero or
// negative Days is normalised to DefaultWindowDays by the query layer.
type Window struct {
	Days int
}

// CostPoint is one calendar day's spend in a time series (UTC, YYYY-MM-DD).
type CostPoint struct {
	Date    string  `json:"date"`
	CostUSD float64 `json:"cost_usd"`
}

// TeamRef is a minimal team reference used in overlap indicators.
type TeamRef struct {
	TeamID      string `json:"team_id"`
	DisplayName string `json:"display_name"`
}

// TeamSpend is a team's spend, used in top-N lists.
type TeamSpend struct {
	TeamID      string  `json:"team_id"`
	DisplayName string  `json:"display_name"`
	CostUSD     float64 `json:"cost_usd"`
}

// ProjectSpend is a project's spend, used in top-N lists.
type ProjectSpend struct {
	ProjectID   string  `json:"project_id"`
	ProjectRoot string  `json:"project_root"`
	CostUSD     float64 `json:"cost_usd"`
}

// ModelSpend is per-model spend + token volume for a detail view.
type ModelSpend struct {
	Model   string  `json:"model"`
	CostUSD float64 `json:"cost_usd"`
	Tokens  int64   `json:"tokens"`
}

// OverviewResult powers GET /api/org/overview (org-wide, admin-scoped, or
// the union of a lead's teams when lead-scoped).
type OverviewResult struct {
	WindowDays       int            `json:"window_days"`
	TotalCostUSD     float64        `json:"total_cost_usd"`
	TotalSessions    int64          `json:"total_sessions"`
	TotalActions     int64          `json:"total_actions"`
	TotalAPITurns    int64          `json:"total_api_turns"`
	ActiveDevelopers int64          `json:"active_developers"`
	TeamCount        int64          `json:"team_count"`
	ProjectCount     int64          `json:"project_count"`
	CostByDay        []CostPoint    `json:"cost_by_day"`
	TopTeams         []TeamSpend    `json:"top_teams"`
	TopProjects      []ProjectSpend `json:"top_projects"`
}

// TeamRollup is a team list item for GET /api/org/teams.
type TeamRollup struct {
	TeamID           string  `json:"team_id"`
	DisplayName      string  `json:"display_name"`
	MemberCount      int64   `json:"member_count"`
	ActiveDevelopers int64   `json:"active_developers"`
	CostUSD          float64 `json:"cost_usd"`
	SessionCount     int64   `json:"session_count"`
	ActionCount      int64   `json:"action_count"`
}

// TeamsResult wraps the team list with the window it was computed for.
type TeamsResult struct {
	WindowDays int          `json:"window_days"`
	Teams      []TeamRollup `json:"teams"`
}

// TeamDetailResult powers GET /api/org/teams/{id}. It is aggregate-only — no
// per-developer split (that is the audited developers endpoint).
type TeamDetailResult struct {
	TeamID           string         `json:"team_id"`
	DisplayName      string         `json:"display_name"`
	WindowDays       int            `json:"window_days"`
	CostUSD          float64        `json:"cost_usd"`
	SessionCount     int64          `json:"session_count"`
	ActionCount      int64          `json:"action_count"`
	APITurnCount     int64          `json:"api_turn_count"`
	MemberCount      int64          `json:"member_count"`
	ActiveDevelopers int64          `json:"active_developers"`
	CostByDay        []CostPoint    `json:"cost_by_day"`
	TopProjects      []ProjectSpend `json:"top_projects"`
	TopModels        []ModelSpend   `json:"top_models"`
}

// DeveloperRollup is one developer's activity within a team, returned only by
// the audited drill-down endpoint.
type DeveloperRollup struct {
	UserID       string  `json:"user_id"`
	Email        string  `json:"email"`
	DisplayName  string  `json:"display_name"`
	Role         string  `json:"role"`
	CostUSD      float64 `json:"cost_usd"`
	SessionCount int64   `json:"session_count"`
	ActionCount  int64   `json:"action_count"`
	LastActive   string  `json:"last_active,omitempty"`
}

// DevelopersResult powers GET /api/org/teams/{id}/developers (audited).
type DevelopersResult struct {
	TeamID      string            `json:"team_id"`
	DisplayName string            `json:"display_name"`
	WindowDays  int               `json:"window_days"`
	Developers  []DeveloperRollup `json:"developers"`
}

// ProjectRollup is a project list item for GET /api/org/projects. Teams is the
// tool-overlap indicator: the set of teams whose members touched the project.
type ProjectRollup struct {
	ProjectID        string    `json:"project_id"`
	ProjectRoot      string    `json:"project_root"`
	Teams            []TeamRef `json:"teams"`
	CostUSD          float64   `json:"cost_usd"`
	SessionCount     int64     `json:"session_count"`
	ActiveDevelopers int64     `json:"active_developers"`
	Tools            []string  `json:"tools"`
}

// ProjectsResult wraps the project list.
type ProjectsResult struct {
	WindowDays int             `json:"window_days"`
	Projects   []ProjectRollup `json:"projects"`
}

// ProjectDetailResult powers GET /api/org/projects/{id}.
type ProjectDetailResult struct {
	ProjectID        string       `json:"project_id"`
	ProjectRoot      string       `json:"project_root"`
	WindowDays       int          `json:"window_days"`
	Teams            []TeamRef    `json:"teams"`
	CostUSD          float64      `json:"cost_usd"`
	SessionCount     int64        `json:"session_count"`
	ActiveDevelopers int64        `json:"active_developers"`
	Tools            []string     `json:"tools"`
	CostByDay        []CostPoint  `json:"cost_by_day"`
	TopModels        []ModelSpend `json:"top_models"`
}

// BudgetStatus is a budget row joined with its current rolling-30-day spend.
type BudgetStatus struct {
	ID                 string    `json:"id"`
	Scope              string    `json:"scope"`
	ScopeID            string    `json:"scope_id"`
	ScopeLabel         string    `json:"scope_label"`
	MonthlyUSDCap      float64   `json:"monthly_usd_cap"`
	AlertWebhookURL    string    `json:"alert_webhook_url,omitempty"`
	AlertThresholds    []float64 `json:"alert_thresholds"`
	CurrentSpendUSD    float64   `json:"current_spend_usd"`
	CurrentRatio       float64   `json:"current_ratio"`
	LastFiredThreshold float64   `json:"last_fired_threshold"`
	CreatedAt          string    `json:"created_at"`
	UpdatedAt          string    `json:"updated_at"`
}

// BudgetsResult wraps the budget list.
type BudgetsResult struct {
	Budgets []BudgetStatus `json:"budgets"`
}

// AuditEntry is one audit-log row, actor email resolved for display.
type AuditEntry struct {
	ID           int64  `json:"id"`
	ActorUserID  string `json:"actor_user_id"`
	ActorEmail   string `json:"actor_email,omitempty"`
	Action       string `json:"action"`
	TargetTeamID string `json:"target_team_id,omitempty"`
	TargetDetail string `json:"target_detail,omitempty"`
	SourceIP     string `json:"source_ip,omitempty"`
	Timestamp    string `json:"timestamp"`
}

// AuditResult powers GET /api/org/audit (offset-paginated).
type AuditResult struct {
	Entries    []AuditEntry `json:"entries"`
	NextOffset int          `json:"next_offset"`
	HasMore    bool         `json:"has_more"`
}

// BearerInfo is one live bearer for a developer, for the admin Revoke list.
type BearerInfo struct {
	Jti       string `json:"jti"`
	IssuedAt  string `json:"issued_at"`
	ExpiresAt string `json:"expires_at"`
	Revoked   bool   `json:"revoked"`
}

// BearersResult powers GET /api/org/admin/bearers (a developer's bearers).
type BearersResult struct {
	UserID  string       `json:"user_id"`
	Bearers []BearerInfo `json:"bearers"`
}
