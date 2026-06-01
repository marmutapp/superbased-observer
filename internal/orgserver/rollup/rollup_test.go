package rollup

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"testing"
	"time"

	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
)

// fixedNow anchors the window so seeded timestamps stay inside it.
var fixedNow = time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := orgdb.Open(context.Background(), orgdb.Options{Path: filepath.Join(t.TempDir(), "server.db")})
	if err != nil {
		t.Fatalf("orgdb.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// seed builds a small org: teams A (lead alice, member carol) and B (lead bob);
// project /repo/x (team A only) and /repo/y (cross-team: carol + bob → overlap).
// One token_usage row duplicates a proxy api_turns request_id and must be
// dropped from spend.
func seed(t *testing.T, d *sql.DB) {
	t.Helper()
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed exec: %v\n%s", err, q)
		}
	}

	// Identity.
	for _, u := range []struct{ id, name, email, disp string }{
		{"u-alice", "alice", "alice@acme.example", "Alice"},
		{"u-bob", "bob", "bob@acme.example", "Bob"},
		{"u-carol", "carol", "carol@acme.example", "Carol"},
	} {
		exec(`INSERT INTO org_members (user_id, user_name, email, display_name, active, created_at, updated_at)
		      VALUES (?, ?, ?, ?, 1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, u.id, u.name, u.email, u.disp)
	}
	exec(`INSERT INTO org_teams (team_id, display_name, created_at, updated_at) VALUES ('team-a','Team A','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO org_teams (team_id, display_name, created_at, updated_at) VALUES ('team-b','Team B','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO org_team_members (team_id, user_id, role) VALUES ('team-a','u-alice','lead')`)
	exec(`INSERT INTO org_team_members (team_id, user_id, role) VALUES ('team-a','u-carol','member')`)
	exec(`INSERT INTO org_team_members (team_id, user_id, role) VALUES ('team-b','u-bob','lead')`)

	// Sessions.
	sess := func(id, user, tool, proj, started string) {
		exec(`INSERT INTO sessions (id, user_id, project_root, tool, started_at, total_actions, pushed_at, pushed_by_user_id)
		      VALUES (?, ?, ?, ?, ?, 1, '2026-05-26T11:00:00Z', ?)`, id, user, proj, tool, started, user)
	}
	sess("s-a1", "u-alice", "claude-code", "/repo/x", "2026-05-20T09:00:00Z")
	sess("s-b1", "u-bob", "codex", "/repo/y", "2026-05-21T09:00:00Z")
	sess("s-c1", "u-carol", "claude-code", "/repo/y", "2026-05-22T09:00:00Z")

	// Actions.
	act := func(sf, ev, user, sessID, ts string) {
		exec(`INSERT INTO actions (source_file, source_event_id, user_id, session_id, timestamp, tool, action_type, success, is_sidechain, pushed_at, pushed_by_user_id)
		      VALUES (?, ?, ?, ?, ?, 'claude-code', 'read_file', 1, 0, '2026-05-26T11:00:00Z', ?)`, sf, ev, user, sessID, ts, user)
	}
	act("f-a", "a-a1", "u-alice", "s-a1", "2026-05-20T09:05:00Z")
	act("f-b", "a-b1", "u-bob", "s-b1", "2026-05-21T09:05:00Z")
	act("f-c", "a-c1", "u-carol", "s-c1", "2026-05-22T09:05:00Z")
	act("f-c", "a-c2", "u-carol", "s-c1", "2026-05-22T09:06:00Z")

	// API turns (proxy-observed, authoritative cost).
	apiTurn := func(user, sess, proj, model, reqID string, cost float64, ts string) {
		exec(`INSERT INTO api_turns (user_id, session_id, project_root, timestamp, provider, model, request_id, input_tokens, output_tokens, cost_usd, http_status, pushed_at, pushed_by_user_id)
		      VALUES (?, ?, ?, ?, 'anthropic', ?, ?, 100, 50, ?, 200, '2026-05-26T11:00:00Z', ?)`, user, sess, proj, ts, model, reqID, cost, user)
	}
	apiTurn("u-alice", "s-a1", "/repo/x", "claude", "req-a1", 0.10, "2026-05-20T10:00:00Z")
	apiTurn("u-bob", "s-b1", "/repo/y", "gpt", "req-b1", 0.20, "2026-05-21T10:00:00Z")

	// Token usage. The first row duplicates req-a1 (a proxy turn) → must be
	// excluded from spend. The rest are unique → included.
	tu := func(sf, ev, user, sess, proj, model string, cost float64, ts string) {
		exec(`INSERT INTO token_usage (source_file, source_event_id, user_id, session_id, project_root, timestamp, tool, model, input_tokens, output_tokens, estimated_cost_usd, source, reliability, pushed_at, pushed_by_user_id)
		      VALUES (?, ?, ?, ?, ?, ?, 'claude-code', ?, 100, 50, ?, 'jsonl', 'reliable', '2026-05-26T11:00:00Z', ?)`, sf, ev, user, sess, proj, ts, model, cost, user)
	}
	tu("f-a", "req-a1", "u-alice", "s-a1", "/repo/x", "claude", 0.99, "2026-05-20T10:00:01Z") // DUP of proxy → dropped
	tu("f-a", "t-a2", "u-alice", "s-a1", "/repo/x", "claude", 0.05, "2026-05-20T11:00:00Z")
	tu("f-c", "t-c1", "u-carol", "s-c1", "/repo/y", "claude", 0.07, "2026-05-22T11:00:00Z")
	tu("f-b", "t-b2", "u-bob", "s-b1", "/repo/y", "gpt", 0.03, "2026-05-21T11:00:00Z")
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

var (
	adminScope = Scope{Admin: true}
	aliceScope = Scope{TeamIDs: []string{"team-a"}} // alice leads team A
	bobScope   = Scope{TeamIDs: []string{"team-b"}}
	w30        = Window{Days: 30}
)

func TestOverview_AdminDedupsProxyDoubleCount(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	got, err := Overview(context.Background(), d, w30, adminScope, fixedNow)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	// 0.10 + 0.20 + 0.05 + 0.07 + 0.03; the 0.99 proxy-dup is excluded.
	if !near(got.TotalCostUSD, 0.45) {
		t.Errorf("TotalCostUSD = %v, want 0.45 (proxy dup must be dropped)", got.TotalCostUSD)
	}
	if got.ActiveDevelopers != 3 {
		t.Errorf("ActiveDevelopers = %d, want 3", got.ActiveDevelopers)
	}
	if got.TotalSessions != 3 || got.TotalActions != 4 || got.TotalAPITurns != 2 {
		t.Errorf("counts = sessions %d actions %d turns %d, want 3/4/2", got.TotalSessions, got.TotalActions, got.TotalAPITurns)
	}
	if got.TeamCount != 2 || got.ProjectCount != 2 {
		t.Errorf("TeamCount=%d ProjectCount=%d, want 2/2", got.TeamCount, got.ProjectCount)
	}
}

func TestOverview_LeadScopedToOwnTeam(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	got, err := Overview(context.Background(), d, w30, aliceScope, fixedNow)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	// Team A = alice (0.10+0.05) + carol (0.07) = 0.22.
	if !near(got.TotalCostUSD, 0.22) {
		t.Errorf("alice-scoped TotalCostUSD = %v, want 0.22", got.TotalCostUSD)
	}
	if got.ActiveDevelopers != 2 {
		t.Errorf("alice-scoped ActiveDevelopers = %d, want 2 (alice+carol)", got.ActiveDevelopers)
	}
	if got.TeamCount != 1 {
		t.Errorf("alice-scoped TeamCount = %d, want 1", got.TeamCount)
	}
}

func TestTeams_DeveloperAttributedSpend(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	got, err := Teams(context.Background(), d, w30, adminScope, fixedNow)
	if err != nil {
		t.Fatalf("Teams: %v", err)
	}
	byID := map[string]TeamRollup{}
	for _, tr := range got.Teams {
		byID[tr.TeamID] = tr
	}
	if a := byID["team-a"]; !near(a.CostUSD, 0.22) || a.MemberCount != 2 || a.ActiveDevelopers != 2 {
		t.Errorf("team-a = %+v, want cost 0.22 members 2 devs 2", a)
	}
	if b := byID["team-b"]; !near(b.CostUSD, 0.23) || b.MemberCount != 1 {
		t.Errorf("team-b = %+v, want cost 0.23 members 1", b)
	}
}

func TestTeamDetail_AndNotFound(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	got, found, err := TeamDetail(context.Background(), d, w30, "team-a", fixedNow)
	if err != nil || !found {
		t.Fatalf("TeamDetail: err=%v found=%v", err, found)
	}
	if !near(got.CostUSD, 0.22) || got.APITurnCount != 1 || got.ActionCount != 3 {
		t.Errorf("team-a detail = cost %v turns %d actions %d, want 0.22/1/3", got.CostUSD, got.APITurnCount, got.ActionCount)
	}
	if len(got.TopModels) == 0 || got.TopModels[0].Model != "claude" {
		t.Errorf("top models = %+v, want claude first", got.TopModels)
	}
	if _, found, _ := TeamDetail(context.Background(), d, w30, "nope", fixedNow); found {
		t.Error("unknown team should be found=false")
	}
}

func TestDevelopers_FullRoster(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	got, found, err := Developers(context.Background(), d, w30, "team-a", fixedNow)
	if err != nil || !found {
		t.Fatalf("Developers: err=%v found=%v", err, found)
	}
	if len(got.Developers) != 2 {
		t.Fatalf("developers = %d, want 2 (alice, carol)", len(got.Developers))
	}
	byID := map[string]DeveloperRollup{}
	for _, dv := range got.Developers {
		byID[dv.UserID] = dv
	}
	if a := byID["u-alice"]; !near(a.CostUSD, 0.15) || a.Role != "lead" {
		t.Errorf("alice = %+v, want cost 0.15 role lead", a)
	}
	if c := byID["u-carol"]; !near(c.CostUSD, 0.07) || c.Role != "member" {
		t.Errorf("carol = %+v, want cost 0.07 role member", c)
	}
}

func TestProjects_OverlapIndicator(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	got, err := Projects(context.Background(), d, w30, adminScope, fixedNow)
	if err != nil {
		t.Fatalf("Projects: %v", err)
	}
	byRoot := map[string]ProjectRollup{}
	for _, p := range got.Projects {
		byRoot[p.ProjectRoot] = p
	}
	y := byRoot["/repo/y"]
	if len(y.Teams) != 2 {
		t.Errorf("/repo/y teams = %d, want 2 (cross-team overlap)", len(y.Teams))
	}
	x := byRoot["/repo/x"]
	if len(x.Teams) != 1 || x.Teams[0].TeamID != "team-a" {
		t.Errorf("/repo/x teams = %+v, want [team-a]", x.Teams)
	}
}

func TestProjectDetail_ByHashAndScope(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	id := ProjectID("/repo/x")
	got, found, err := ProjectDetail(context.Background(), d, w30, id, adminScope, fixedNow)
	if err != nil || !found {
		t.Fatalf("ProjectDetail: err=%v found=%v", err, found)
	}
	if got.ProjectRoot != "/repo/x" || !near(got.CostUSD, 0.15) {
		t.Errorf("detail = root %q cost %v, want /repo/x 0.15", got.ProjectRoot, got.CostUSD)
	}
	// bob (team-b) cannot resolve /repo/x — it is not in his scope.
	if _, found, _ := ProjectDetail(context.Background(), d, w30, id, bobScope, fixedNow); found {
		t.Error("bob should not resolve /repo/x (out of scope)")
	}
}

func TestScopeSpend_TeamAndProject(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	ctx := context.Background()
	if v, _ := ScopeSpend(ctx, d, "team", "team-a", 30, fixedNow); !near(v, 0.22) {
		t.Errorf("team-a spend = %v, want 0.22", v)
	}
	if v, _ := ScopeSpend(ctx, d, "project", "/repo/x", 30, fixedNow); !near(v, 0.15) {
		t.Errorf("/repo/x spend = %v, want 0.15", v)
	}
}

func TestAudit_ScopeAndPagination(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := WriteAudit(ctx, d, "u-alice", ActionDrillDown, "team-a", "u-carol", "1.2.3.4", fixedNow); err != nil {
			t.Fatalf("WriteAudit: %v", err)
		}
	}
	if err := WriteAudit(ctx, d, "u-bob", ActionDrillDown, "team-b", "", "5.6.7.8", fixedNow); err != nil {
		t.Fatalf("WriteAudit: %v", err)
	}

	// Admin sees all 4.
	all, err := Audit(ctx, d, adminScope, 100, 0)
	if err != nil {
		t.Fatalf("Audit admin: %v", err)
	}
	if len(all.Entries) != 4 {
		t.Errorf("admin audit = %d rows, want 4", len(all.Entries))
	}
	if all.Entries[0].ActorEmail == "" {
		t.Error("actor email should be resolved")
	}

	// Lead alice sees only team-a's 3.
	lead, _ := Audit(ctx, d, aliceScope, 100, 0)
	if len(lead.Entries) != 3 {
		t.Errorf("alice audit = %d rows, want 3 (team-a only)", len(lead.Entries))
	}

	// Pagination: limit 2 → has_more.
	page, _ := Audit(ctx, d, adminScope, 2, 0)
	if len(page.Entries) != 2 || !page.HasMore || page.NextOffset != 2 {
		t.Errorf("page = %d rows has_more=%v next=%d, want 2/true/2", len(page.Entries), page.HasMore, page.NextOffset)
	}
}

func TestBudgets_CurrentSpendAndRatio(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	ctx := context.Background()
	if _, err := d.ExecContext(ctx,
		`INSERT INTO budgets (id, scope, scope_id, monthly_usd_cap, alert_thresholds, created_at, updated_at)
		 VALUES ('b1','team','team-a',1.0,'[0.75,0.9,1.0]','2026-05-01T00:00:00Z','2026-05-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert budget: %v", err)
	}
	got, err := Budgets(ctx, d, fixedNow)
	if err != nil {
		t.Fatalf("Budgets: %v", err)
	}
	if len(got.Budgets) != 1 {
		t.Fatalf("budgets = %d, want 1", len(got.Budgets))
	}
	b := got.Budgets[0]
	if !near(b.CurrentSpendUSD, 0.22) || !near(b.CurrentRatio, 0.22) || b.ScopeLabel != "Team A" {
		t.Errorf("budget = spend %v ratio %v label %q, want 0.22/0.22/Team A", b.CurrentSpendUSD, b.CurrentRatio, b.ScopeLabel)
	}
}

func TestCache_ReadThroughAndInvalidate(t *testing.T) {
	c := NewCache(8)
	calls := 0
	load := func() (int, error) { calls++; return 42, nil }
	key := CacheKey("overview", adminScope, w30)
	for i := 0; i < 3; i++ {
		v, err := Cached(c, key, time.Minute, load)
		if err != nil || v != 42 {
			t.Fatalf("Cached = %v, %v", v, err)
		}
	}
	if calls != 1 {
		t.Errorf("loader called %d times, want 1 (cached)", calls)
	}
	c.Invalidate()
	if _, _ = Cached(c, key, time.Minute, load); calls != 2 {
		t.Errorf("after invalidate loader called %d times, want 2", calls)
	}
	// Nil cache degrades to direct load.
	if v, _ := Cached[int](nil, key, time.Minute, load); v != 42 || calls != 3 {
		t.Errorf("nil-cache path: v=%d calls=%d", v, calls)
	}
}
