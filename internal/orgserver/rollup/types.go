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

	// --- Phase 1 enrichment (additive; every field is omitempty so the v1
	// wire shape is byte-identical when these are unset). Each is a
	// content-free aggregate over the SAME non-content columns the v1 fields
	// use — token counts, costs, http_status, error_class, durations,
	// success flags, tool/model labels. No sentinel/content column is read.
	// Proxy-only metrics degrade to an honest empty: Latency is nil when no
	// api_turns carry timing; Errors.HTTPErrors / ByErrorClass are 0/empty
	// without proxy capture but the action-level rate still computes from
	// the watcher-fed actions table. ---
	Tokens       *TokenBuckets     `json:"tokens,omitempty"`
	Cache        *CacheStats       `json:"cache,omitempty"`
	Reliability  *ReliabilitySplit `json:"reliability,omitempty"`
	Errors       *ErrorStats       `json:"errors,omitempty"`
	Latency      *LatencyStats     `json:"latency,omitempty"`
	ToolMix      []ToolSlice       `json:"tool_mix,omitempty"`
	ModelMix     []ModelSlice      `json:"model_mix,omitempty"`
	ActionsByDay []DayCount        `json:"actions_by_day,omitempty"`
	HourOfDay    []HourCount       `json:"hour_of_day,omitempty"`
	Deltas       *PeriodDeltas     `json:"deltas,omitempty"`
}

// TokenBuckets is the org-wide split of token volume into the Anthropic
// billing buckets (+ reasoning), deduped across the proxy and JSONL tiers.
// Cache buckets are zero when no tier captured cache activity (e.g. an org
// whose tools never routed through the proxy); reasoning is zero for
// providers that fold it into output_tokens.
type TokenBuckets struct {
	NetInput   int64 `json:"net_input"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Output     int64 `json:"output"`
	Reasoning  int64 `json:"reasoning"`
}

// CacheStats summarizes prompt-cache efficiency. HitRatio = read / (input +
// read); ReadWriteRatio is the "R/W ×" the primary Cache page shows
// (read ÷ write). Both are 0 when the denominators are 0 (no cache activity).
type CacheStats struct {
	ReadTokens     int64   `json:"read_tokens"`
	WriteTokens    int64   `json:"write_tokens"`
	InputTokens    int64   `json:"input_tokens"`
	HitRatio       float64 `json:"hit_ratio"`
	ReadWriteRatio float64 `json:"read_write_ratio"`
}

// ReliabilitySplit reports how much of the window's spend came from the
// authoritative proxy (api_turns.cost_usd) vs estimated JSONL capture
// (token_usage.estimated_cost_usd). ProxyShare = proxy / (proxy + estimated).
type ReliabilitySplit struct {
	ProxyCostUSD     float64 `json:"proxy_cost_usd"`
	EstimatedCostUSD float64 `json:"estimated_cost_usd"`
	ProxyShare       float64 `json:"proxy_share"`
}

// ErrorStats is the reliability split: the watcher-fed action error rate
// (always available) plus the proxy-only HTTP error view (zero/empty without
// proxy capture). ByErrorClass is the api_turns.error_class leaderboard.
type ErrorStats struct {
	TotalActions    int64      `json:"total_actions"`
	FailedActions   int64      `json:"failed_actions"`
	ActionErrorRate float64    `json:"action_error_rate"`
	APITurns        int64      `json:"api_turns"`
	HTTPErrors      int64      `json:"http_errors"`
	ByErrorClass    []KeyCount `json:"by_error_class,omitempty"`
}

// LatencyStats is the proxy-only latency view (median TTFT + median/p95 total
// response). The whole struct is nil when no api_turns carry timing — an
// honest "needs proxy capture" empty state, never a fabricated zero.
type LatencyStats struct {
	SampleSize    int64 `json:"sample_size"`
	MedianTTFTMs  int64 `json:"median_ttft_ms"`
	MedianTotalMs int64 `json:"median_total_ms"`
	P95TotalMs    int64 `json:"p95_total_ms"`
}

// ToolSlice is one tool's share of cost + token volume (top-N, with the
// remainder folded into a synthetic "other" row).
type ToolSlice struct {
	Tool    string  `json:"tool"`
	CostUSD float64 `json:"cost_usd"`
	Tokens  int64   `json:"tokens"`
}

// ModelSlice is one model's share of cost + token volume (top-N + "other").
type ModelSlice struct {
	Model   string  `json:"model"`
	CostUSD float64 `json:"cost_usd"`
	Tokens  int64   `json:"tokens"`
}

// DayCount is one calendar day's count (UTC, YYYY-MM-DD) for a time series.
type DayCount struct {
	Date  string `json:"date"`
	Count int64  `json:"count"`
}

// HourCount is one hour-of-day bucket (0–23, UTC) for the activity histogram.
type HourCount struct {
	Hour  int   `json:"hour"`
	Count int64 `json:"count"`
}

// KeyCount is a generic label→count pair (e.g. error_class distribution).
type KeyCount struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

// PeriodDeltas compares the current window to the immediately preceding window
// of the same length. The fractional fields are signed ((cur-prior)/prior) and
// are 0 when there is no prior activity; HasPrior distinguishes "flat" from
// "no comparison baseline" so the dashboard can suppress a misleading 0%.
type PeriodDeltas struct {
	CostUSD       float64 `json:"cost_usd"`
	Sessions      float64 `json:"sessions"`
	Actions       float64 `json:"actions"`
	PriorCostUSD  float64 `json:"prior_cost_usd"`
	PriorSessions int64   `json:"prior_sessions"`
	PriorActions  int64   `json:"prior_actions"`
	HasPrior      bool    `json:"has_prior"`
}

// PersonRollup is one developer's org-wide activity row for the People
// leaderboard (GET /api/org/people, AUDITED). Content-free: identity is the
// SCIM email/display name that already ships on the wire; the metrics are
// counts/cost/labels. Spark is the trailing 7-day daily spend series.
type PersonRollup struct {
	UserID       string    `json:"user_id"`
	Email        string    `json:"email"`
	DisplayName  string    `json:"display_name,omitempty"`
	CostUSD      float64   `json:"cost_usd"`
	SessionCount int64     `json:"session_count"`
	ActionCount  int64     `json:"action_count"`
	Tokens       int64     `json:"tokens"`
	TopTool      string    `json:"top_tool,omitempty"`
	TopModel     string    `json:"top_model,omitempty"`
	LastActive   string    `json:"last_active,omitempty"`
	Spark        []float64 `json:"spark,omitempty"`
}

// PeopleResult powers GET /api/org/people — the org-wide per-developer
// leaderboard. This is the SAME privacy-sensitive disclosure class as the
// per-team Developers() drill-down, so the handler writes an audit_log row
// BEFORE returning. It does NOT require SCIM team groups (it keys on the
// developer identity that ships on every row), so it lights up for a
// no-SCIM-groups org where the per-team Teams view is empty.
type PeopleResult struct {
	WindowDays int            `json:"window_days"`
	People     []PersonRollup `json:"people"`
}

// --- Phase 3: Tools / Models / Activity (all content-free aggregates) -------

// ToolRow is one AI tool's org-wide usage row for GET /api/org/tools. AvgTTFTMs
// is proxy-only (0 when no api_turns carry timing for the tool).
type ToolRow struct {
	Tool        string       `json:"tool"`
	CostUSD     float64      `json:"cost_usd"`
	Tokens      int64        `json:"tokens"`
	Buckets     TokenBuckets `json:"buckets"`
	Sessions    int64        `json:"sessions"`
	ActiveDevs  int64        `json:"active_devs"`
	ActionCount int64        `json:"action_count"`
	SuccessRate float64      `json:"success_rate"`
	AvgTTFTMs   int64        `json:"avg_ttft_ms"`
}

// ToolsResult powers GET /api/org/tools.
type ToolsResult struct {
	WindowDays int       `json:"window_days"`
	Tools      []ToolRow `json:"tools"`
}

// ModelRow is one model's org-wide usage row for GET /api/org/models.
type ModelRow struct {
	Model      string       `json:"model"`
	CostUSD    float64      `json:"cost_usd"`
	Tokens     int64        `json:"tokens"`
	Buckets    TokenBuckets `json:"buckets"`
	Sessions   int64        `json:"sessions"`
	ActiveDevs int64        `json:"active_devs"`
	AvgTTFTMs  int64        `json:"avg_ttft_ms"`
}

// ModelsResult powers GET /api/org/models.
type ModelsResult struct {
	WindowDays int        `json:"window_days"`
	Models     []ModelRow `json:"models"`
}

// ToolDayCount is one (day, tool) action count for the stacked activity chart.
type ToolDayCount struct {
	Date  string `json:"date"`
	Tool  string `json:"tool"`
	Count int64  `json:"count"`
}

// DayBuckets is one day's token volume split into the billing buckets.
type DayBuckets struct {
	Date       string `json:"date"`
	NetInput   int64  `json:"net_input"`
	CacheRead  int64  `json:"cache_read"`
	CacheWrite int64  `json:"cache_write"`
	Output     int64  `json:"output"`
	Reasoning  int64  `json:"reasoning"`
}

// DowHourCount is one day-of-week × hour-of-day action-intensity cell (UTC).
// Dow is 0=Sunday … 6=Saturday.
type DowHourCount struct {
	Dow   int   `json:"dow"`
	Hour  int   `json:"hour"`
	Count int64 `json:"count"`
}

// ActivityResult powers GET /api/org/activity — the time-grid surface.
type ActivityResult struct {
	WindowDays   int            `json:"window_days"`
	CostByDay    []CostPoint    `json:"cost_by_day"`
	ActionsByDay []DayCount     `json:"actions_by_day"`
	ToolByDay    []ToolDayCount `json:"tool_by_day"`
	TokensByDay  []DayBuckets   `json:"tokens_by_day"`
	HourOfDay    []HourCount    `json:"hour_of_day"`
	DowHour      []DowHourCount `json:"dow_hour"`
}

// --- Phase 4: native-console vendor telemetry (content-free aggregates) -----

// TelemetryResult powers GET /api/org/telemetry — the native-console vendor
// analytics surface (Claude Code / Codex / Copilot org-analytics tables, all
// server-side only, never on the agent push wire). Org-aggregate and
// admin-scoped: there is no per-developer attribution here, only per-vendor
// totals. Configured is false when no poller has populated any of the three
// tables in the window — the honest "native telemetry not configured" empty
// state for the common case (most orgs wire no native-console poller).
type TelemetryResult struct {
	WindowDays int               `json:"window_days"`
	Configured bool              `json:"configured"`
	Vendors    []VendorTelemetry `json:"vendors"`
}

// VendorTelemetry is one native-console vendor's org-aggregate metrics for the
// window. Field presence is vendor-shaped (omitempty): only Claude Code carries
// Acceptance; only Copilot carries Seats; Tokens is absent for Copilot. The
// cross-vendor UNIT TRAP is honored — CostUSD sums only USD-unit cost, and
// Codex ChatGPT-Enterprise credits live in CreditsCost (never added to
// dollars); CostUnit ("usd" | "credits" | "mixed" | "") names what was present.
type VendorTelemetry struct {
	Vendor       string           `json:"vendor"`       // claude_code | codex | copilot
	DisplayName  string           `json:"display_name"` // "Claude Code" | "Codex" | "GitHub Copilot"
	Days         int64            `json:"days"`         // distinct days with data in window
	LastPulledAt string           `json:"last_pulled_at,omitempty"`
	CostUSD      float64          `json:"cost_usd"`
	CostUnit     string           `json:"cost_unit,omitempty"`
	CreditsCost  float64          `json:"credits_cost,omitempty"` // Codex ChatGPT-Enterprise credits (NOT USD)
	Tokens       *TokenBuckets    `json:"tokens,omitempty"`
	Acceptance   *AcceptanceStats `json:"acceptance,omitempty"` // Claude Code only
	Seats        *SeatStats       `json:"seats,omitempty"`      // Copilot only (latest snapshot)
	Engagement   []KeyCount       `json:"engagement,omitempty"` // additive count metrics
	Surfaces     []string         `json:"surfaces,omitempty"`   // multi-surface vendors (codex/copilot)
}

// AcceptanceStats is the Claude Code edit-acceptance view: lines/edits accepted
// vs rejected (summed across tools), with the derived AcceptRate = accepted /
// (accepted + rejected). A headline metric no local-capture path can produce.
type AcceptanceStats struct {
	Accepted   int64   `json:"accepted"`
	Rejected   int64   `json:"rejected"`
	AcceptRate float64 `json:"accept_rate"`
}

// SeatStats is Copilot's seat utilization from the LATEST seat-breakdown
// snapshot in the window (point-in-time — seat counts are NOT additive across
// days). Utilization = Active / Total (0 when Total is 0).
type SeatStats struct {
	Total       float64 `json:"total"`
	Active      float64 `json:"active"`
	Inactive    float64 `json:"inactive"`
	Utilization float64 `json:"utilization"`
}

// TeamRollup is a team list item for GET /api/org/teams. Spark is the trailing
// 7-day daily team spend (sum of the team members' spend); TopTools is the
// team's most-used tools by cost (top-N). Both omitempty — content-free.
type TeamRollup struct {
	TeamID           string    `json:"team_id"`
	DisplayName      string    `json:"display_name"`
	MemberCount      int64     `json:"member_count"`
	ActiveDevelopers int64     `json:"active_developers"`
	CostUSD          float64   `json:"cost_usd"`
	SessionCount     int64     `json:"session_count"`
	ActionCount      int64     `json:"action_count"`
	Spark            []float64 `json:"spark,omitempty"`
	TopTools         []string  `json:"top_tools,omitempty"`
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
// Spark is the trailing 7-day daily project spend; Buckets is the net-input /
// output token split for a per-row mini-bar (pointer so the all-zero case omits
// honestly). Both content-free.
type ProjectRollup struct {
	ProjectID        string        `json:"project_id"`
	ProjectRoot      string        `json:"project_root"`
	Teams            []TeamRef     `json:"teams"`
	CostUSD          float64       `json:"cost_usd"`
	SessionCount     int64         `json:"session_count"`
	ActiveDevelopers int64         `json:"active_developers"`
	Tools            []string      `json:"tools"`
	Spark            []float64     `json:"spark,omitempty"`
	Buckets          *TokenBuckets `json:"buckets,omitempty"`
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

// RoutingResult powers GET /api/org/routing (admin-only) — the model-routing
// org surface (§R19). It aggregates the routing_summaries table, which receives
// the node-side §R19.4 AGGREGATE push (counts + decision-time dollar estimates
// by day × tier × reason × mode) ONLY when the node operator opts in with
// [org_client.share].routing_summary. Every field is content-free: tier/reason
// are closed enums (never a model id), and identity columns (user_email,
// pushed_by_user_id) are never read.
//
// Configured is false when no node has shared a routing summary in the window
// — the honest "no routing summaries shared" empty state (the common case,
// since the share is off by default).
type RoutingResult struct {
	WindowDays int  `json:"window_days"`
	Configured bool `json:"configured"`
	// TotalDecisions is every routing decision the engine made; TotalApplied is
	// the subset it actually rewrote (advise-mode decisions are counted but not
	// applied).
	TotalDecisions int64 `json:"total_decisions"`
	TotalApplied   int64 `json:"total_applied"`
	// EstSavingsUSD / CacheForfeitUSD are decision-time estimate sums;
	// NetSavingsUSD = EstSavingsUSD − CacheForfeitUSD (a switch can forfeit a
	// warm prompt cache, so net is the honest figure).
	EstSavingsUSD   float64 `json:"est_savings_usd"`
	CacheForfeitUSD float64 `json:"cache_forfeit_usd"`
	NetSavingsUSD   float64 `json:"net_savings_usd"`
	// AdviseDecisions / EnforceDecisions split TotalDecisions by mode.
	AdviseDecisions  int64 `json:"advise_decisions"`
	EnforceDecisions int64 `json:"enforce_decisions"`
	// ByDay is the per-day trend (ascending); ByTier / ByReason are the
	// distribution leaderboards (descending by decisions).
	ByDay    []RoutingDayPoint `json:"by_day"`
	ByTier   []RoutingDimCount `json:"by_tier"`
	ByReason []RoutingDimCount `json:"by_reason"`
}

// RoutingDayPoint is one day of the routing trend.
type RoutingDayPoint struct {
	Date          string  `json:"date"`
	Decisions     int64   `json:"decisions"`
	Applied       int64   `json:"applied"`
	EstSavingsUSD float64 `json:"est_savings_usd"`
}

// RoutingDimCount is one tier/reason bucket's rollup.
type RoutingDimCount struct {
	Key           string  `json:"key"`
	Decisions     int64   `json:"decisions"`
	Applied       int64   `json:"applied"`
	EstSavingsUSD float64 `json:"est_savings_usd"`
}

// SessionRow is one session in the org-wide session list (GET /api/org/sessions,
// AUDITED). It names a developer, so listing it is the same privacy-sensitive
// disclosure class as People — the HANDLER writes a view_org_sessions audit row
// before returning. Project identity is the content-free hash-derived ProjectID
// (the raw project_root path is NEVER selected); no message content, no command
// targets, no git remote.
type SessionRow struct {
	SessionID    string  `json:"session_id"`
	UserID       string  `json:"user_id"`
	Email        string  `json:"email,omitempty"`
	DisplayName  string  `json:"display_name,omitempty"`
	Tool         string  `json:"tool,omitempty"`
	Model        string  `json:"model,omitempty"`
	ProjectID    string  `json:"project_id,omitempty"` // hash-derived; never the raw path
	StartedAt    string  `json:"started_at,omitempty"`
	EndedAt      string  `json:"ended_at,omitempty"`
	CostUSD      float64 `json:"cost_usd"`
	Tokens       int64   `json:"tokens"`
	ActionCount  int64   `json:"action_count"`
	APITurnCount int64   `json:"api_turn_count"`
}

// SessionsResult powers GET /api/org/sessions — a paginated, scoped, AUDITED
// session list. Total is the full scoped+windowed+filtered count (for the
// pager); Sessions holds the requested page.
type SessionsResult struct {
	WindowDays int          `json:"window_days"`
	Total      int64        `json:"total"`
	Limit      int          `json:"limit"`
	Offset     int          `json:"offset"`
	Sessions   []SessionRow `json:"sessions"`
}

// ActionTypeCount is one action-type bucket in a session detail (content-free:
// action_type is a normalized enum, never the command/target).
type ActionTypeCount struct {
	ActionType   string `json:"action_type"`
	Count        int64  `json:"count"`
	SuccessCount int64  `json:"success_count"`
}

// SessionDetailResult powers GET /api/org/sessions/{id} (AUDITED) — a single
// session's rollup: token buckets, action-type breakdown, cost, counts. There
// is explicitly NO message content (it is not on the org wire) and no raw path.
type SessionDetailResult struct {
	SessionID    string            `json:"session_id"`
	UserID       string            `json:"user_id"`
	Email        string            `json:"email,omitempty"`
	DisplayName  string            `json:"display_name,omitempty"`
	Tool         string            `json:"tool,omitempty"`
	Model        string            `json:"model,omitempty"`
	ProjectID    string            `json:"project_id,omitempty"`
	StartedAt    string            `json:"started_at,omitempty"`
	EndedAt      string            `json:"ended_at,omitempty"`
	CostUSD      float64           `json:"cost_usd"`
	Tokens       int64             `json:"tokens"`
	ActionCount  int64             `json:"action_count"`
	APITurnCount int64             `json:"api_turn_count"`
	Buckets      TokenBuckets      `json:"buckets"`
	ActionTypes  []ActionTypeCount `json:"action_types"`
}

// MessageEntry is one captured native-OTel content body for a session — a
// prompt, a tool input, or a tool output. Content is the scrubbed body (already
// scrubbed on the agent at capture); it is omitted when the node shipped only
// the hash (metadata-only / no full-content opt-in). ContentHash is always
// present. Project/command/path identifiers are never carried here.
type MessageEntry struct {
	Kind        string `json:"kind"` // prompt | tool_input | tool_output | raw_body
	RequestID   string `json:"request_id,omitempty"`
	ToolUseID   string `json:"tool_use_id,omitempty"`
	Timestamp   string `json:"timestamp,omitempty"`
	Content     string `json:"content,omitempty"` // omitted when the body was not shared (hash-only)
	ContentHash string `json:"content_hash"`      // always present (content-free)
}

// MessagesResult powers GET /api/org/sessions/{id}/messages (AUDITED, deeper
// than the metadata detail) — the captured OTel content bodies for one session.
// ContentAvailable is true when at least one entry carries a body; it is false
// both when the node never captured content (e.g. a tool without native-OTel)
// and when it shipped hashes only — the UI distinguishes those by whether
// Messages is empty. Identity is resolved from the authoritative SCIM roster.
type MessagesResult struct {
	SessionID        string         `json:"session_id"`
	UserID           string         `json:"user_id"`
	Email            string         `json:"email,omitempty"`
	DisplayName      string         `json:"display_name,omitempty"`
	ProjectID        string         `json:"project_id,omitempty"` // hash-derived; never the raw path
	ContentAvailable bool           `json:"content_available"`
	Messages         []MessageEntry `json:"messages"`
}
