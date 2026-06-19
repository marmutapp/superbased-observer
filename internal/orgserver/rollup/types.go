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

// --- Guard rollups (guard spec §14.3, G14) ----------------------------------
// Every guard field below is a count, share, label, or timestamp computed from
// the content-free guard_events columns — reason/excerpt/taint_origin (the
// full_content opt-in columns) are never read by a rollup query.

// GuardTrendPoint is one calendar day's guard-event counts by decision (UTC,
// YYYY-MM-DD). Deny/Ask/Flag/Mask are the enforcement-significant decisions;
// Other catches everything else (allow rows recorded with a note, guard_error,
// future decision values), so Total is always the sum of all five buckets.
type GuardTrendPoint struct {
	Date  string `json:"date"`
	Deny  int64  `json:"deny"`
	Ask   int64  `json:"ask"`
	Flag  int64  `json:"flag"`
	Mask  int64  `json:"mask"`
	Other int64  `json:"other"`
	Total int64  `json:"total"`
}

// GuardRuleHit is one rule-hit leaderboard row. Category/Severity are taken
// from the observed events (MAX over the group — constant per rule id in
// practice, the aggregate is just a deterministic picker for the rare mixed
// case after a catalog change).
type GuardRuleHit struct {
	RuleID    string `json:"rule_id"`
	Category  string `json:"category"`
	Severity  string `json:"severity"`
	Hits      int64  `json:"hits"`
	Agents    int64  `json:"agents"`
	DenyCount int64  `json:"deny_count"`
	LastSeen  string `json:"last_seen"`
}

// GuardOverviewResult powers GET /api/org/guard/overview. BrokenChainAgents is
// computed over the user's FULL pushed history, not the window — the §10.4
// chain is cumulative, and windowing it would misread the oldest in-window
// event's out-of-window predecessor as a break.
type GuardOverviewResult struct {
	WindowDays        int               `json:"window_days"`
	TotalEvents       int64             `json:"total_events"`
	DenyCount         int64             `json:"deny_count"`
	AskCount          int64             `json:"ask_count"`
	FlagCount         int64             `json:"flag_count"`
	MaskCount         int64             `json:"mask_count"`
	EnforcedCount     int64             `json:"enforced_count"`
	ActiveAgents      int64             `json:"active_agents"`
	RuleCount         int64             `json:"rule_count"`
	BrokenChainAgents int64             `json:"broken_chain_agents"`
	TrendByDay        []GuardTrendPoint `json:"trend_by_day"`
	TopRules          []GuardRuleHit    `json:"top_rules"`
}

// GuardRulesResult powers GET /api/org/guard/rules (the full leaderboard).
type GuardRulesResult struct {
	WindowDays int            `json:"window_days"`
	Rules      []GuardRuleHit `json:"rules"`
}

// GuardTeamPosture is one team's guard posture row (§14.3 "per-team
// posture"): how much of the team is guard-active, what the verdict mix
// looks like, and how much of it ran enforced (vs observe-mode capture).
type GuardTeamPosture struct {
	TeamID            string  `json:"team_id"`
	DisplayName       string  `json:"display_name"`
	MemberCount       int64   `json:"member_count"`
	ActiveAgents      int64   `json:"active_agents"`
	Events            int64   `json:"events"`
	DenyCount         int64   `json:"deny_count"`
	AskCount          int64   `json:"ask_count"`
	FlagCount         int64   `json:"flag_count"`
	MaskCount         int64   `json:"mask_count"`
	EnforcedShare     float64 `json:"enforced_share"`
	BrokenChainAgents int64   `json:"broken_chain_agents"`
}

// GuardTeamsResult powers GET /api/org/guard/teams.
type GuardTeamsResult struct {
	WindowDays int                `json:"window_days"`
	Teams      []GuardTeamPosture `json:"teams"`
}

// GuardAgentChain is one developer's audit-chain continuity row, computed
// over their full pushed guard-event history via set-membership: Heads are
// events with an empty chain_prev (chain genesis), Unlinked are events whose
// chain_prev is absent from the user's pushed chain_hash set (a missing
// predecessor), and Segments = Heads + Unlinked is the number of distinct
// chain starts observed. One segment = a continuous history. More than one
// means the chain restarted (agent DB recreated) or rows are missing
// (pruned without checkpoint, or tampered) — Broken flags exactly that.
// A node enrolled mid-history shows 0 heads + 1 unlinked = 1 segment:
// continuous, by design. Reordering within a segment is NOT detectable
// from set-membership; this rollup detects missing/restarted links.
type GuardAgentChain struct {
	UserID      string `json:"user_id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
	Events      int64  `json:"events"`
	Heads       int64  `json:"heads"`
	Unlinked    int64  `json:"unlinked"`
	Segments    int64  `json:"segments"`
	Broken      bool   `json:"broken"`
	FirstSeen   string `json:"first_seen,omitempty"`
	LastSeen    string `json:"last_seen,omitempty"`
}

// GuardAgentsResult powers GET /api/org/guard/agents (audited — per-developer
// rows are a privacy-sensitive disclosure, the developers-drill-down rule).
type GuardAgentsResult struct {
	Agents []GuardAgentChain `json:"agents"`
}

// GuardPolicyBundleInfo is one org policy-bundle version-history row
// (metadata only — `bundle_toml` content rides the detail endpoint).
type GuardPolicyBundleInfo struct {
	Version     int64  `json:"version"`
	SignedAt    string `json:"signed_at"`
	CreatedBy   string `json:"created_by"`
	Description string `json:"description,omitempty"`
	TOMLBytes   int    `json:"toml_bytes"`
}

// GuardPolicyBundlesResult powers GET /api/org/guard/policy/bundles.
// SigningConfigured reports whether the server holds a policy signing key
// path ([policy].signing_key_path) — when false the authoring panel is
// read-only and publish returns 409.
type GuardPolicyBundlesResult struct {
	ActiveVersion     int64                   `json:"active_version"`
	SigningConfigured bool                    `json:"signing_configured"`
	Bundles           []GuardPolicyBundleInfo `json:"bundles"`
}

// GuardPolicyBundleDetail powers GET /api/org/guard/policy/bundles/{version}.
type GuardPolicyBundleDetail struct {
	Version     int64  `json:"version"`
	BundleTOML  string `json:"bundle_toml"`
	SignedAt    string `json:"signed_at"`
	Description string `json:"description,omitempty"`
}

// GuardRuleDryRun is the §14.2 dry-run statistic for one rule id referenced
// by a draft bundle: how many pushed guard events carried that rule id in
// the window (= the events an escalating [[override]] would have affected).
// Computable is false for [[rule]] ids the draft newly declares — a brand-new
// matcher cannot be evaluated server-side against content-free hashes, so its
// zero is "unknowable", not "no hits".
type GuardRuleDryRun struct {
	RuleID     string `json:"rule_id"`
	Hits       int64  `json:"hits"`
	Agents     int64  `json:"agents"`
	Computable bool   `json:"computable"`
}

// GuardPolicyLintResult powers POST /api/org/guard/policy/lint: the same
// guard.Lint("org") refusal the publish gate runs, plus dry-run stats.
type GuardPolicyLintResult struct {
	OK         bool              `json:"ok"`
	Problems   []string          `json:"problems"`
	WindowDays int               `json:"window_days"`
	DryRun     []GuardRuleDryRun `json:"dry_run"`
}

// GuardPolicyPublishResult powers POST /api/org/guard/policy/publish.
type GuardPolicyPublishResult struct {
	Version int64 `json:"version"`
}

// Member is one SCIM-provisioned org user, projected for the admin Invite
// dropdown (GET /api/org/members). Only fields useful for picking a user to
// mint an enrolment token for are exposed; content-bearing fields are not.
type Member struct {
	UserID      string `json:"user_id"`
	UserName    string `json:"user_name"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
}

// MembersResult powers GET /api/org/members (admin-only).
type MembersResult struct {
	Members []Member `json:"members"`
}
