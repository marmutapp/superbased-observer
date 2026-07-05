package rollup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
)

// rootHash is the SHA-256 hex of a raw project_root path; the v1.8.0+
// privacy posture ships only this value server-side, and the rollup
// queries aggregate on it. Tests use it as a lookup key into
// ProjectRollup maps.
func rootHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

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

	// Sessions. project_root_hash mirrors what the v1.8.0 ingest layer
	// computes via hashOrComputed — the column is always populated in
	// real data even when the raw root is stripped under metadata-only
	// mode. Seeding both keeps the fixture compatible with both modes.
	sess := func(id, user, tool, proj, started string) {
		exec(`INSERT INTO sessions (id, user_id, project_root, project_root_hash, tool, started_at, total_actions, pushed_at, pushed_by_user_id)
		      VALUES (?, ?, ?, ?, ?, ?, 1, '2026-05-26T11:00:00Z', ?)`, id, user, proj, rootHash(proj), tool, started, user)
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
		exec(`INSERT INTO api_turns (user_id, session_id, project_root, project_root_hash, timestamp, provider, model, request_id, input_tokens, output_tokens, cost_usd, http_status, pushed_at, pushed_by_user_id)
		      VALUES (?, ?, ?, ?, ?, 'anthropic', ?, ?, 100, 50, ?, 200, '2026-05-26T11:00:00Z', ?)`, user, sess, proj, rootHash(proj), ts, model, reqID, cost, user)
	}
	apiTurn("u-alice", "s-a1", "/repo/x", "claude", "req-a1", 0.10, "2026-05-20T10:00:00Z")
	apiTurn("u-bob", "s-b1", "/repo/y", "gpt", "req-b1", 0.20, "2026-05-21T10:00:00Z")

	// Token usage. The first row duplicates req-a1 (a proxy turn) → must be
	// excluded from spend. The rest are unique → included.
	tu := func(sf, ev, user, sess, proj, model string, cost float64, ts string) {
		exec(`INSERT INTO token_usage (source_file, source_event_id, user_id, session_id, project_root, project_root_hash, timestamp, tool, model, input_tokens, output_tokens, estimated_cost_usd, source, reliability, pushed_at, pushed_by_user_id)
		      VALUES (?, ?, ?, ?, ?, ?, ?, 'claude-code', ?, 100, 50, ?, 'jsonl', 'reliable', '2026-05-26T11:00:00Z', ?)`, sf, ev, user, sess, proj, rootHash(proj), ts, model, cost, user)
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
	// Phase 6a: spark (7d daily team spend) + top tools.
	a := byID["team-a"]
	if len(a.Spark) != sparkDays || !near(sumf(a.Spark), 0.22) {
		t.Errorf("team-a spark = %v, want len %d summing to 0.22", a.Spark, sparkDays)
	}
	if len(a.TopTools) == 0 || a.TopTools[0] != "claude-code" {
		t.Errorf("team-a top tools = %v, want claude-code first", a.TopTools)
	}
	if b := byID["team-b"]; len(b.TopTools) == 0 || b.TopTools[0] != "codex" {
		t.Errorf("team-b top tools = %v, want codex first", b.TopTools)
	}
}

func sumf(xs []float64) float64 {
	var t float64
	for _, x := range xs {
		t += x
	}
	return t
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
		// ProjectRoot is now the project_root_hash in metadata-only +
		// full-content modes alike (the wire field carries the hash —
		// see queryProjectSpend / Projects). Tests look up by hash.
		byRoot[p.ProjectRoot] = p
	}
	y := byRoot[rootHash("/repo/y")]
	if len(y.Teams) != 2 {
		t.Errorf("/repo/y teams = %d, want 2 (cross-team overlap)", len(y.Teams))
	}
	x := byRoot[rootHash("/repo/x")]
	if len(x.Teams) != 1 || x.Teams[0].TeamID != "team-a" {
		t.Errorf("/repo/x teams = %+v, want [team-a]", x.Teams)
	}
	// Phase 6a: per-row spark (7d) + token-bucket split. /repo/x = alice's
	// api_turn (100/50) + the unique tu t-a2 (100/50); the dup req-a1 tu drops.
	if x.Buckets == nil || x.Buckets.NetInput != 200 || x.Buckets.Output != 100 {
		t.Errorf("/repo/x buckets = %+v, want net_input 200 output 100", x.Buckets)
	}
	if len(x.Spark) != sparkDays || !near(sumf(x.Spark), 0.15) {
		t.Errorf("/repo/x spark = %v, want len %d summing to 0.15", x.Spark, sparkDays)
	}
}

func TestProjectDetail_ByHashAndScope(t *testing.T) {
	d := newDB(t)
	seed(t, d)
	id := ProjectIDFromHash(rootHash("/repo/x"))
	got, found, err := ProjectDetail(context.Background(), d, w30, id, adminScope, fixedNow)
	if err != nil || !found {
		t.Fatalf("ProjectDetail: err=%v found=%v", err, found)
	}
	if got.ProjectRoot != rootHash("/repo/x") || !near(got.CostUSD, 0.15) {
		t.Errorf("detail = root %q cost %v, want hash(/repo/x) 0.15", got.ProjectRoot, got.CostUSD)
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
	// Project budgets now key on the project_root_hash, not the raw
	// path — see ScopeSpend doc.
	if v, _ := ScopeSpend(ctx, d, "project", rootHash("/repo/x"), 30, fixedNow); !near(v, 0.15) {
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

// TestProjects_MetadataOnlyMode regresses D1
// (docs/teams-test-open-issues-2026-06-04.md): under the v1.8.0+
// default privacy posture the agent strips the raw `project_root`
// column server-side and ships only `project_root_hash`. The rollup
// queries used to filter `WHERE project_root != ”`, so the Projects
// view / project_count / top_projects were permanently empty in
// metadata-only mode despite the hash being present.
//
// This test seeds api_turns / token_usage with `project_root = ”`
// (the production reality) and only `project_root_hash` populated —
// and asserts the project rollup is non-empty. Before D1, this
// fixture would yield an empty Projects list.
//
// Plus it covers the JOIN-fallback for proxy-fed api_turns: it seeds
// one api_turn with BOTH project columns empty but with a session_id
// pointing at a session that DOES have project_root_hash. The
// fallback path (api_turns LEFT JOIN sessions ON session_id) must
// surface that turn under the session's project.
func TestProjects_MetadataOnlyMode(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed exec: %v\n%s", err, q)
		}
	}

	// Minimal identity: one admin user, one team.
	exec(`INSERT INTO org_members (user_id, user_name, email, display_name, active, created_at, updated_at)
	      VALUES ('u-admin','admin','admin@acme.example','Admin',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO org_teams (team_id, display_name, created_at, updated_at)
	      VALUES ('team-a','Team A','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO org_team_members (team_id, user_id, role) VALUES ('team-a','u-admin','lead')`)

	// Session with ONLY project_root_hash populated — mimics the
	// v1.8.0 metadata-only push (raw column is '' on the wire).
	exec(`INSERT INTO sessions (id, user_id, project_root, project_root_hash, tool, started_at, total_actions, pushed_at, pushed_by_user_id)
	      VALUES ('s-prod','u-admin','',?, 'claude-code','2026-05-22T09:00:00Z',1,'2026-05-26T11:00:00Z','u-admin')`,
		rootHash("/home/dev/work/acme"))

	// api_turn carrying its own hash (full metadata-only mode).
	exec(`INSERT INTO api_turns (user_id, session_id, project_root, project_root_hash, timestamp, provider, model, request_id, input_tokens, output_tokens, cost_usd, http_status, pushed_at, pushed_by_user_id)
	      VALUES ('u-admin','s-prod','',?, '2026-05-22T10:00:00Z','anthropic','claude','req-1',100,50,0.10,200,'2026-05-26T11:00:00Z','u-admin')`,
		rootHash("/home/dev/work/acme"))

	// api_turn with BOTH project columns empty (proxy-fed turn, no
	// project_id on the agent → no hash from the LEFT JOIN projects)
	// — the JOIN-fallback in spendCTE must resolve the hash via
	// session_id → sessions.project_root_hash.
	exec(`INSERT INTO api_turns (user_id, session_id, project_root, project_root_hash, timestamp, provider, model, request_id, input_tokens, output_tokens, cost_usd, http_status, pushed_at, pushed_by_user_id)
	      VALUES ('u-admin','s-prod','','', '2026-05-22T10:05:00Z','anthropic','claude','req-2',100,50,0.20,200,'2026-05-26T11:00:00Z','u-admin')`)

	// Overview: project_count should be 1, not 0.
	ov, err := Overview(ctx, d, w30, adminScope, fixedNow)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if ov.ProjectCount != 1 {
		t.Errorf("ProjectCount = %d, want 1 (metadata-only push must surface project_root_hash)", ov.ProjectCount)
	}
	if len(ov.TopProjects) != 1 {
		t.Fatalf("TopProjects len = %d, want 1", len(ov.TopProjects))
	}
	// Cost includes BOTH api_turns — the second one only resolved via
	// the JOIN-fallback to sessions.
	if !near(ov.TopProjects[0].CostUSD, 0.30) {
		t.Errorf("TopProjects[0].CostUSD = %v, want 0.30 (proxy-fed turn must be attributed via session JOIN)", ov.TopProjects[0].CostUSD)
	}
	wantHash := rootHash("/home/dev/work/acme")
	if ov.TopProjects[0].ProjectRoot != wantHash {
		t.Errorf("TopProjects[0].ProjectRoot = %q, want hash %q", ov.TopProjects[0].ProjectRoot, wantHash)
	}

	// Projects list: same shape.
	pl, err := Projects(ctx, d, w30, adminScope, fixedNow)
	if err != nil {
		t.Fatalf("Projects: %v", err)
	}
	if len(pl.Projects) != 1 {
		t.Fatalf("Projects len = %d, want 1", len(pl.Projects))
	}
	if !near(pl.Projects[0].CostUSD, 0.30) {
		t.Errorf("Projects[0].CostUSD = %v, want 0.30", pl.Projects[0].CostUSD)
	}

	// ProjectDetail round-trip via the {id} path.
	id := ProjectIDFromHash(wantHash)
	det, found, err := ProjectDetail(ctx, d, w30, id, adminScope, fixedNow)
	if err != nil || !found {
		t.Fatalf("ProjectDetail: err=%v found=%v", err, found)
	}
	if !near(det.CostUSD, 0.30) {
		t.Errorf("ProjectDetail cost = %v, want 0.30", det.CostUSD)
	}
}

// TestProjects_NoTeamOverlapTeamsNonNil regresses the v1.11.0 org-dashboard
// Projects render crash (c202d099): a project whose only spender belongs to no
// team has no team overlap, so projectTeams returns no row for its hash and
// teamSet[hash] is nil. Before the fix, Projects assigned that nil straight to
// p.Teams, which marshals to JSON `null` and crashes the dashboard's
// `p.teams.length` read. The fix coerces nil → []TeamRef{} (JSON `[]`).
//
// The fixture seeds an org_member with spend but NO org_team_members row, so
// the project is genuinely team-less. We assert both the Go contract
// (Teams != nil, empty) and the wire shape the frontend actually consumes
// (`"teams":[]`, never `"teams":null`).
func TestProjects_NoTeamOverlapTeamsNonNil(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed exec: %v\n%s", err, q)
		}
	}

	// One member, deliberately NOT enrolled in any team.
	exec(`INSERT INTO org_members (user_id, user_name, email, display_name, active, created_at, updated_at)
	      VALUES ('u-loner','loner','loner@acme.example','Loner',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)

	exec(`INSERT INTO sessions (id, user_id, project_root, project_root_hash, tool, started_at, total_actions, pushed_at, pushed_by_user_id)
	      VALUES ('s-lone','u-loner','/repo/solo',?, 'claude-code','2026-05-22T09:00:00Z',1,'2026-05-26T11:00:00Z','u-loner')`,
		rootHash("/repo/solo"))
	exec(`INSERT INTO api_turns (user_id, session_id, project_root, project_root_hash, timestamp, provider, model, request_id, input_tokens, output_tokens, cost_usd, http_status, pushed_at, pushed_by_user_id)
	      VALUES ('u-loner','s-lone','/repo/solo',?, '2026-05-22T10:00:00Z','anthropic','claude','req-lone',100,50,0.10,200,'2026-05-26T11:00:00Z','u-loner')`,
		rootHash("/repo/solo"))

	got, err := Projects(ctx, d, w30, adminScope, fixedNow)
	if err != nil {
		t.Fatalf("Projects: %v", err)
	}
	if len(got.Projects) != 1 {
		t.Fatalf("Projects len = %d, want 1", len(got.Projects))
	}
	p := got.Projects[0]
	if p.Teams == nil {
		t.Fatalf("Teams = nil for a team-less project; want non-nil empty slice (JSON null crashes the dashboard)")
	}
	if len(p.Teams) != 0 {
		t.Errorf("Teams = %+v, want empty (no team overlap)", p.Teams)
	}

	// Wire-shape contract: the frontend reads `p.teams.length`, so the
	// marshalled field must be `[]`, never `null`.
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if !strings.Contains(string(b), `"teams":[]`) {
		t.Errorf(`marshalled project missing "teams":[]; got %s`, b)
	}
	if strings.Contains(string(b), `"teams":null`) {
		t.Errorf(`marshalled project contains "teams":null (the crash shape); got %s`, b)
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
