package rollup

import (
	"encoding/json"
	"testing"
)

// These golden encodings pin the dashboard wire contract: the OpenAPI spec
// resolves its response schemas to these Go types via x-go-type, so an
// accidental json-tag rename or field change here silently breaks the
// front-end and the conformance test. The golden string is the guard.
func TestGolden_WireShapes(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want string
	}{
		{
			name: "OverviewResult",
			val: OverviewResult{
				WindowDays: 30, TotalCostUSD: 1.5, TotalSessions: 2, TotalActions: 3,
				TotalAPITurns: 4, ActiveDevelopers: 5, TeamCount: 6, ProjectCount: 7,
				CostByDay:   []CostPoint{{Date: "2026-05-26", CostUSD: 1.5}},
				TopTeams:    []TeamSpend{{TeamID: "t1", DisplayName: "T1", CostUSD: 1.5}},
				TopProjects: []ProjectSpend{{ProjectID: "p1", ProjectRoot: "/r", CostUSD: 1.5}},
			},
			want: `{"window_days":30,"total_cost_usd":1.5,"total_sessions":2,"total_actions":3,"total_api_turns":4,"active_developers":5,"team_count":6,"project_count":7,"cost_by_day":[{"date":"2026-05-26","cost_usd":1.5}],"top_teams":[{"team_id":"t1","display_name":"T1","cost_usd":1.5}],"top_projects":[{"project_id":"p1","project_root":"/r","cost_usd":1.5}]}`,
		},
		{
			name: "DeveloperRollup",
			val:  DeveloperRollup{UserID: "u1", Email: "a@b", DisplayName: "A", Role: "lead", CostUSD: 1, SessionCount: 2, ActionCount: 3, LastActive: "2026-05-26T10:00:00Z"},
			want: `{"user_id":"u1","email":"a@b","display_name":"A","role":"lead","cost_usd":1,"session_count":2,"action_count":3,"last_active":"2026-05-26T10:00:00Z"}`,
		},
		{
			name: "ProjectRollup",
			val:  ProjectRollup{ProjectID: "p1", ProjectRoot: "/r", Teams: []TeamRef{{TeamID: "t1", DisplayName: "T1"}}, CostUSD: 1, SessionCount: 2, ActiveDevelopers: 3, Tools: []string{"claude-code"}},
			want: `{"project_id":"p1","project_root":"/r","teams":[{"team_id":"t1","display_name":"T1"}],"cost_usd":1,"session_count":2,"active_developers":3,"tools":["claude-code"]}`,
		},
		{
			// Phase 6a: ProjectRollup with the per-row spark + token mini-bar.
			name: "ProjectRollup_enriched",
			val:  ProjectRollup{ProjectID: "p1", ProjectRoot: "/r", Teams: []TeamRef{}, CostUSD: 1, SessionCount: 2, ActiveDevelopers: 3, Tools: []string{"claude-code"}, Spark: []float64{0, 1}, Buckets: &TokenBuckets{NetInput: 200, Output: 100}},
			want: `{"project_id":"p1","project_root":"/r","teams":[],"cost_usd":1,"session_count":2,"active_developers":3,"tools":["claude-code"],"spark":[0,1],"buckets":{"net_input":200,"cache_read":0,"cache_write":0,"output":100,"reasoning":0}}`,
		},
		{
			name: "TeamRollup",
			val:  TeamRollup{TeamID: "t1", DisplayName: "T1", MemberCount: 3, ActiveDevelopers: 2, CostUSD: 1.5, SessionCount: 4, ActionCount: 9},
			want: `{"team_id":"t1","display_name":"T1","member_count":3,"active_developers":2,"cost_usd":1.5,"session_count":4,"action_count":9}`,
		},
		{
			// Phase 6a: TeamRollup with the per-row spark + top tools.
			name: "TeamRollup_enriched",
			val:  TeamRollup{TeamID: "t1", DisplayName: "T1", MemberCount: 3, ActiveDevelopers: 2, CostUSD: 1.5, SessionCount: 4, ActionCount: 9, Spark: []float64{0, 1.5}, TopTools: []string{"claude-code", "codex"}},
			want: `{"team_id":"t1","display_name":"T1","member_count":3,"active_developers":2,"cost_usd":1.5,"session_count":4,"action_count":9,"spark":[0,1.5],"top_tools":["claude-code","codex"]}`,
		},
		{
			name: "BudgetStatus",
			val:  BudgetStatus{ID: "b1", Scope: "team", ScopeID: "t1", ScopeLabel: "T1", MonthlyUSDCap: 100, AlertWebhookURL: "https://h", AlertThresholds: []float64{0.75, 0.9, 1}, CurrentSpendUSD: 50, CurrentRatio: 0.5, LastFiredThreshold: 0, CreatedAt: "2026-05-01T00:00:00Z", UpdatedAt: "2026-05-02T00:00:00Z"},
			want: `{"id":"b1","scope":"team","scope_id":"t1","scope_label":"T1","monthly_usd_cap":100,"alert_webhook_url":"https://h","alert_thresholds":[0.75,0.9,1],"current_spend_usd":50,"current_ratio":0.5,"last_fired_threshold":0,"created_at":"2026-05-01T00:00:00Z","updated_at":"2026-05-02T00:00:00Z"}`,
		},
		{
			name: "AuditEntry",
			val:  AuditEntry{ID: 1, ActorUserID: "u1", ActorEmail: "a@b", Action: "drill_down_developers", TargetTeamID: "t1", TargetDetail: "u2", SourceIP: "1.2.3.4", Timestamp: "2026-05-26T10:00:00Z"},
			want: `{"id":1,"actor_user_id":"u1","actor_email":"a@b","action":"drill_down_developers","target_team_id":"t1","target_detail":"u2","source_ip":"1.2.3.4","timestamp":"2026-05-26T10:00:00Z"}`,
		},
		{
			name: "BearersResult",
			val:  BearersResult{UserID: "u1", Bearers: []BearerInfo{{Jti: "j1", IssuedAt: "2026-05-01T00:00:00Z", ExpiresAt: "2026-08-01T00:00:00Z", Revoked: false}}},
			want: `{"user_id":"u1","bearers":[{"jti":"j1","issued_at":"2026-05-01T00:00:00Z","expires_at":"2026-08-01T00:00:00Z","revoked":false}]}`,
		},
		{
			name: "MembersResult",
			val:  MembersResult{Members: []Member{{UserID: "u1", UserName: "alice", Email: "alice@acme.example", DisplayName: "Alice"}}},
			want: `{"members":[{"user_id":"u1","user_name":"alice","email":"alice@acme.example","display_name":"Alice"}]}`,
		},
		{
			name: "MembersResult_omitDisplayName",
			val:  MembersResult{Members: []Member{{UserID: "u1", UserName: "alice", Email: "alice@acme.example"}}},
			want: `{"members":[{"user_id":"u1","user_name":"alice","email":"alice@acme.example"}]}`,
		},
		{
			name: "GuardOverviewResult",
			val: GuardOverviewResult{
				WindowDays: 30, TotalEvents: 10, DenyCount: 1, AskCount: 2, FlagCount: 3, MaskCount: 4,
				EnforcedCount: 5, ActiveAgents: 6, RuleCount: 7, BrokenChainAgents: 1,
				TrendByDay: []GuardTrendPoint{{Date: "2026-05-26", Deny: 1, Ask: 0, Flag: 2, Mask: 0, Other: 1, Total: 4}},
				TopRules:   []GuardRuleHit{{RuleID: "R-001", Category: "destructive", Severity: "high", Hits: 3, Agents: 2, DenyCount: 1, LastSeen: "2026-05-26T10:00:00Z"}},
			},
			want: `{"window_days":30,"total_events":10,"deny_count":1,"ask_count":2,"flag_count":3,"mask_count":4,"enforced_count":5,"active_agents":6,"rule_count":7,"broken_chain_agents":1,"trend_by_day":[{"date":"2026-05-26","deny":1,"ask":0,"flag":2,"mask":0,"other":1,"total":4}],"top_rules":[{"rule_id":"R-001","category":"destructive","severity":"high","hits":3,"agents":2,"deny_count":1,"last_seen":"2026-05-26T10:00:00Z"}]}`,
		},
		{
			name: "GuardTeamPosture",
			val:  GuardTeamPosture{TeamID: "t1", DisplayName: "T1", MemberCount: 5, ActiveAgents: 3, Events: 12, DenyCount: 1, AskCount: 0, FlagCount: 4, MaskCount: 0, EnforcedShare: 0.25, BrokenChainAgents: 1},
			want: `{"team_id":"t1","display_name":"T1","member_count":5,"active_agents":3,"events":12,"deny_count":1,"ask_count":0,"flag_count":4,"mask_count":0,"enforced_share":0.25,"broken_chain_agents":1}`,
		},
		{
			name: "GuardAgentChain",
			val:  GuardAgentChain{UserID: "u1", Email: "a@b", DisplayName: "A", Events: 9, Heads: 1, Unlinked: 1, Segments: 2, Broken: true, FirstSeen: "2026-05-01T00:00:00Z", LastSeen: "2026-05-26T10:00:00Z"},
			want: `{"user_id":"u1","email":"a@b","display_name":"A","events":9,"heads":1,"unlinked":1,"segments":2,"broken":true,"first_seen":"2026-05-01T00:00:00Z","last_seen":"2026-05-26T10:00:00Z"}`,
		},
		{
			name: "GuardPolicyBundlesResult",
			val: GuardPolicyBundlesResult{
				ActiveVersion: 3, SigningConfigured: true,
				Bundles: []GuardPolicyBundleInfo{{Version: 3, SignedAt: "2026-05-26T10:00:00Z", CreatedBy: "boss@acme.example", Description: "tighten exfil", TOMLBytes: 120}},
			},
			want: `{"active_version":3,"signing_configured":true,"bundles":[{"version":3,"signed_at":"2026-05-26T10:00:00Z","created_by":"boss@acme.example","description":"tighten exfil","toml_bytes":120}]}`,
		},
		{
			name: "GuardPolicyLintResult",
			val: GuardPolicyLintResult{
				OK: true, Problems: []string{}, WindowDays: 30,
				DryRun: []GuardRuleDryRun{{RuleID: "R-001", Hits: 3, Agents: 2, Computable: true}},
			},
			want: `{"ok":true,"problems":[],"window_days":30,"dry_run":[{"rule_id":"R-001","hits":3,"agents":2,"computable":true}]}`,
		},
		{
			name: "GuardPolicyPublishResult",
			val:  GuardPolicyPublishResult{Version: 4},
			want: `{"version":4}`,
		},
		// --- Phase 3 Tools / Models / Activity ---
		{
			name: "ToolRow",
			val:  ToolRow{Tool: "claude-code", CostUSD: 0.25, Tokens: 600, Buckets: TokenBuckets{NetInput: 400, Output: 200}, Sessions: 2, ActiveDevs: 3, ActionCount: 4, SuccessRate: 1, AvgTTFTMs: 0},
			want: `{"tool":"claude-code","cost_usd":0.25,"tokens":600,"buckets":{"net_input":400,"cache_read":0,"cache_write":0,"output":200,"reasoning":0},"sessions":2,"active_devs":3,"action_count":4,"success_rate":1,"avg_ttft_ms":0}`,
		},
		{
			name: "ToolsResult",
			val:  ToolsResult{WindowDays: 30, Tools: []ToolRow{{Tool: "codex", CostUSD: 0.2, Tokens: 150, Buckets: TokenBuckets{NetInput: 100, Output: 50}, Sessions: 1, ActiveDevs: 1}}},
			want: `{"window_days":30,"tools":[{"tool":"codex","cost_usd":0.2,"tokens":150,"buckets":{"net_input":100,"cache_read":0,"cache_write":0,"output":50,"reasoning":0},"sessions":1,"active_devs":1,"action_count":0,"success_rate":0,"avg_ttft_ms":0}]}`,
		},
		{
			name: "ModelRow",
			val:  ModelRow{Model: "claude", CostUSD: 0.22, Tokens: 450, Buckets: TokenBuckets{NetInput: 300, Output: 150}, Sessions: 0, ActiveDevs: 2, AvgTTFTMs: 120},
			want: `{"model":"claude","cost_usd":0.22,"tokens":450,"buckets":{"net_input":300,"cache_read":0,"cache_write":0,"output":150,"reasoning":0},"sessions":0,"active_devs":2,"avg_ttft_ms":120}`,
		},
		{
			name: "ModelsResult",
			val:  ModelsResult{WindowDays: 30, Models: []ModelRow{{Model: "gpt", CostUSD: 0.23, Tokens: 300, Buckets: TokenBuckets{NetInput: 200, Output: 100}, ActiveDevs: 1}}},
			want: `{"window_days":30,"models":[{"model":"gpt","cost_usd":0.23,"tokens":300,"buckets":{"net_input":200,"cache_read":0,"cache_write":0,"output":100,"reasoning":0},"sessions":0,"active_devs":1,"avg_ttft_ms":0}]}`,
		},
		{
			name: "ToolDayCount",
			val:  ToolDayCount{Date: "2026-05-20", Tool: "claude-code", Count: 1},
			want: `{"date":"2026-05-20","tool":"claude-code","count":1}`,
		},
		{
			name: "DayBuckets",
			val:  DayBuckets{Date: "2026-05-20", NetInput: 200, Output: 100},
			want: `{"date":"2026-05-20","net_input":200,"cache_read":0,"cache_write":0,"output":100,"reasoning":0}`,
		},
		{
			name: "DowHourCount",
			val:  DowHourCount{Dow: 3, Hour: 9, Count: 1},
			want: `{"dow":3,"hour":9,"count":1}`,
		},
		{
			name: "ActivityResult",
			val:  ActivityResult{WindowDays: 30, CostByDay: []CostPoint{}, ActionsByDay: []DayCount{}, ToolByDay: []ToolDayCount{}, TokensByDay: []DayBuckets{}, HourOfDay: []HourCount{}, DowHour: []DowHourCount{}},
			want: `{"window_days":30,"cost_by_day":[],"actions_by_day":[],"tool_by_day":[],"tokens_by_day":[],"hour_of_day":[],"dow_hour":[]}`,
		},
		// --- Phase 4 native-console telemetry ---
		{
			name: "AcceptanceStats",
			val:  AcceptanceStats{Accepted: 40, Rejected: 10, AcceptRate: 0.8},
			want: `{"accepted":40,"rejected":10,"accept_rate":0.8}`,
		},
		{
			name: "SeatStats",
			val:  SeatStats{Total: 50, Active: 40, Inactive: 10, Utilization: 0.8},
			want: `{"total":50,"active":40,"inactive":10,"utilization":0.8}`,
		},
		{
			name: "VendorTelemetry_full",
			val: VendorTelemetry{
				Vendor: "claude_code", DisplayName: "Claude Code", Days: 2, LastPulledAt: "2026-05-26T06:00:00Z",
				CostUSD: 1.2, CostUnit: "usd", Tokens: &TokenBuckets{NetInput: 1000, Output: 400},
				Acceptance: &AcceptanceStats{Accepted: 40, Rejected: 10, AcceptRate: 0.8},
				Engagement: []KeyCount{{Key: "sessions", Count: 5}},
			},
			want: `{"vendor":"claude_code","display_name":"Claude Code","days":2,"last_pulled_at":"2026-05-26T06:00:00Z","cost_usd":1.2,"cost_unit":"usd","tokens":{"net_input":1000,"cache_read":0,"cache_write":0,"output":400,"reasoning":0},"acceptance":{"accepted":40,"rejected":10,"accept_rate":0.8},"engagement":[{"key":"sessions","count":5}]}`,
		},
		{
			name: "VendorTelemetry_codexMixedUnits",
			val: VendorTelemetry{
				Vendor: "codex", DisplayName: "Codex", Days: 1,
				CostUSD: 0.5, CostUnit: "mixed", CreditsCost: 120,
				Surfaces: []string{"chatgpt_enterprise", "openai_org"},
			},
			want: `{"vendor":"codex","display_name":"Codex","days":1,"cost_usd":0.5,"cost_unit":"mixed","credits_cost":120,"surfaces":["chatgpt_enterprise","openai_org"]}`,
		},
		{
			name: "VendorTelemetry_copilotSeats",
			val: VendorTelemetry{
				Vendor: "copilot", DisplayName: "GitHub Copilot", Days: 1, CostUSD: 4,
				Seats: &SeatStats{Total: 50, Active: 40, Inactive: 10, Utilization: 0.8},
			},
			want: `{"vendor":"copilot","display_name":"GitHub Copilot","days":1,"cost_usd":4,"seats":{"total":50,"active":40,"inactive":10,"utilization":0.8}}`,
		},
		{
			name: "TelemetryResult_empty",
			val:  TelemetryResult{WindowDays: 30, Configured: false, Vendors: []VendorTelemetry{}},
			want: `{"window_days":30,"configured":false,"vendors":[]}`,
		},
		// --- Phase 2 People leaderboard ---
		{
			name: "PersonRollup",
			val:  PersonRollup{UserID: "u1", Email: "a@b", DisplayName: "A", CostUSD: 1.5, SessionCount: 2, ActionCount: 3, Tokens: 400, TopTool: "claude-code", TopModel: "claude", LastActive: "2026-05-26T10:00:00Z", Spark: []float64{0, 1.5}},
			want: `{"user_id":"u1","email":"a@b","display_name":"A","cost_usd":1.5,"session_count":2,"action_count":3,"tokens":400,"top_tool":"claude-code","top_model":"claude","last_active":"2026-05-26T10:00:00Z","spark":[0,1.5]}`,
		},
		{
			name: "PersonRollup_minimal",
			val:  PersonRollup{UserID: "u1", Email: "a@b", CostUSD: 0, SessionCount: 0, ActionCount: 0, Tokens: 0},
			want: `{"user_id":"u1","email":"a@b","cost_usd":0,"session_count":0,"action_count":0,"tokens":0}`,
		},
		{
			name: "PeopleResult",
			val:  PeopleResult{WindowDays: 30, People: []PersonRollup{{UserID: "u1", Email: "a@b", CostUSD: 1, SessionCount: 1, ActionCount: 1, Tokens: 10}}},
			want: `{"window_days":30,"people":[{"user_id":"u1","email":"a@b","cost_usd":1,"session_count":1,"action_count":1,"tokens":10}]}`,
		},
		// --- Phase 1 Overview enrichment sub-shapes ---
		{
			name: "TokenBuckets",
			val:  TokenBuckets{NetInput: 1, CacheRead: 2, CacheWrite: 3, Output: 4, Reasoning: 5},
			want: `{"net_input":1,"cache_read":2,"cache_write":3,"output":4,"reasoning":5}`,
		},
		{
			name: "CacheStats",
			val:  CacheStats{ReadTokens: 1, WriteTokens: 2, InputTokens: 3, HitRatio: 0.5, ReadWriteRatio: 1.5},
			want: `{"read_tokens":1,"write_tokens":2,"input_tokens":3,"hit_ratio":0.5,"read_write_ratio":1.5}`,
		},
		{
			name: "ReliabilitySplit",
			val:  ReliabilitySplit{ProxyCostUSD: 0.3, EstimatedCostUSD: 0.1, ProxyShare: 0.75},
			want: `{"proxy_cost_usd":0.3,"estimated_cost_usd":0.1,"proxy_share":0.75}`,
		},
		{
			name: "ErrorStats",
			val:  ErrorStats{TotalActions: 10, FailedActions: 2, ActionErrorRate: 0.2, APITurns: 5, HTTPErrors: 1, ByErrorClass: []KeyCount{{Key: "overloaded", Count: 3}}},
			want: `{"total_actions":10,"failed_actions":2,"action_error_rate":0.2,"api_turns":5,"http_errors":1,"by_error_class":[{"key":"overloaded","count":3}]}`,
		},
		{
			name: "ErrorStats_omitByErrorClass",
			val:  ErrorStats{TotalActions: 10, FailedActions: 0, ActionErrorRate: 0, APITurns: 0, HTTPErrors: 0},
			want: `{"total_actions":10,"failed_actions":0,"action_error_rate":0,"api_turns":0,"http_errors":0}`,
		},
		{
			name: "LatencyStats",
			val:  LatencyStats{SampleSize: 8, MedianTTFTMs: 120, MedianTotalMs: 800, P95TotalMs: 1500},
			want: `{"sample_size":8,"median_ttft_ms":120,"median_total_ms":800,"p95_total_ms":1500}`,
		},
		{
			name: "ToolSlice",
			val:  ToolSlice{Tool: "claude-code", CostUSD: 1.5, Tokens: 100},
			want: `{"tool":"claude-code","cost_usd":1.5,"tokens":100}`,
		},
		{
			name: "ModelSlice",
			val:  ModelSlice{Model: "claude", CostUSD: 1.5, Tokens: 100},
			want: `{"model":"claude","cost_usd":1.5,"tokens":100}`,
		},
		{
			name: "DayCount",
			val:  DayCount{Date: "2026-05-26", Count: 4},
			want: `{"date":"2026-05-26","count":4}`,
		},
		{
			name: "HourCount",
			val:  HourCount{Hour: 9, Count: 4},
			want: `{"hour":9,"count":4}`,
		},
		{
			name: "PeriodDeltas",
			val:  PeriodDeltas{CostUSD: 0.1, Sessions: -0.2, Actions: 0.3, PriorCostUSD: 5, PriorSessions: 10, PriorActions: 20, HasPrior: true},
			want: `{"cost_usd":0.1,"sessions":-0.2,"actions":0.3,"prior_cost_usd":5,"prior_sessions":10,"prior_actions":20,"has_prior":true}`,
		},
		{
			// The enriched OverviewResult: every v1 field PLUS the additive
			// blocks, pinning that the new json tags attach in the right order
			// and the omitempty fields render when set.
			name: "OverviewResult_enriched",
			val: OverviewResult{
				WindowDays: 30, TotalCostUSD: 0.45, TotalSessions: 3, TotalActions: 4,
				TotalAPITurns: 2, ActiveDevelopers: 3, TeamCount: 2, ProjectCount: 2,
				CostByDay:    []CostPoint{},
				TopTeams:     []TeamSpend{},
				TopProjects:  []ProjectSpend{},
				Tokens:       &TokenBuckets{NetInput: 500, Output: 250},
				Cache:        &CacheStats{InputTokens: 500},
				Reliability:  &ReliabilitySplit{ProxyCostUSD: 0.3, EstimatedCostUSD: 0.15, ProxyShare: 0.6},
				Errors:       &ErrorStats{TotalActions: 4, APITurns: 2},
				Latency:      &LatencyStats{SampleSize: 2, MedianTTFTMs: 100, MedianTotalMs: 200, P95TotalMs: 300},
				ToolMix:      []ToolSlice{{Tool: "claude-code", CostUSD: 0.25, Tokens: 600}},
				ModelMix:     []ModelSlice{{Model: "claude", CostUSD: 0.22, Tokens: 450}},
				ActionsByDay: []DayCount{{Date: "2026-05-22", Count: 2}},
				HourOfDay:    []HourCount{{Hour: 9, Count: 4}},
				Deltas:       &PeriodDeltas{},
			},
			want: `{"window_days":30,"total_cost_usd":0.45,"total_sessions":3,"total_actions":4,"total_api_turns":2,"active_developers":3,"team_count":2,"project_count":2,"cost_by_day":[],"top_teams":[],"top_projects":[],"tokens":{"net_input":500,"cache_read":0,"cache_write":0,"output":250,"reasoning":0},"cache":{"read_tokens":0,"write_tokens":0,"input_tokens":500,"hit_ratio":0,"read_write_ratio":0},"reliability":{"proxy_cost_usd":0.3,"estimated_cost_usd":0.15,"proxy_share":0.6},"errors":{"total_actions":4,"failed_actions":0,"action_error_rate":0,"api_turns":2,"http_errors":0},"latency":{"sample_size":2,"median_ttft_ms":100,"median_total_ms":200,"p95_total_ms":300},"tool_mix":[{"tool":"claude-code","cost_usd":0.25,"tokens":600}],"model_mix":[{"model":"claude","cost_usd":0.22,"tokens":450}],"actions_by_day":[{"date":"2026-05-22","count":2}],"hour_of_day":[{"hour":9,"count":4}],"deltas":{"cost_usd":0,"sessions":0,"actions":0,"prior_cost_usd":0,"prior_sessions":0,"prior_actions":0,"has_prior":false}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.val)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(b) != tc.want {
				t.Errorf("wire shape drift:\n got: %s\nwant: %s", b, tc.want)
			}
		})
	}
}
