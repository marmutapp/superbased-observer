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
