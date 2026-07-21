package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/orgserver/auth"
	gen "github.com/marmutapp/superbased-observer/internal/orgserver/dashboard/gen"
	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
	"github.com/marmutapp/superbased-observer/internal/orgserver/rollup"
)

func newAPIWithData(t *testing.T) *API {
	t.Helper()
	d, err := orgdb.Open(context.Background(), orgdb.Options{Path: filepath.Join(t.TempDir(), "server.db")})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// admin (boss), lead (alice→team-a), member (bob→team-b lead too for cross test).
	for _, u := range [][2]string{{"u-boss", "boss@acme.example"}, {"u-alice", "alice@acme.example"}, {"u-bob", "bob@acme.example"}} {
		exec(`INSERT INTO org_members (user_id, user_name, email, active, created_at, updated_at) VALUES (?, ?, ?, 1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, u[0], u[1], u[1])
	}
	exec(`INSERT INTO org_teams (team_id, display_name, created_at, updated_at) VALUES ('team-a','Team A','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO org_teams (team_id, display_name, created_at, updated_at) VALUES ('team-b','Team B','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO org_team_members (team_id, user_id, role) VALUES ('team-a','u-alice','lead')`)
	exec(`INSERT INTO org_team_members (team_id, user_id, role) VALUES ('team-b','u-bob','lead')`)
	exec(`INSERT INTO org_team_members (team_id, user_id, role) VALUES ('team-b','u-alice','member')`)

	return NewAPI(d, rollup.NewCache(0), Options{AdminEmails: []string{"boss@acme.example"}}, nil)
}

// do builds a request authenticated as userID and runs fn (a handler method).
func do(userID, method, target string, body any, fn func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, target, bytes.NewReader(b))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	r = r.WithContext(auth.ContextWithUserID(r.Context(), userID))
	w := httptest.NewRecorder()
	fn(w, r)
	return w
}

func TestAPI_Unauthenticated401(t *testing.T) {
	a := newAPIWithData(t)
	r := httptest.NewRequest("GET", "/api/org/overview", nil) // no user id in context
	w := httptest.NewRecorder()
	a.OrgOverview(w, r, gen.OrgOverviewParams{})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no-session overview = %d, want 401", w.Code)
	}
}

func TestAPI_AdminVsLeadScoping(t *testing.T) {
	a := newAPIWithData(t)

	// Admin can read any team's detail.
	if w := do("u-boss", "GET", "/api/org/teams/team-a", nil, func(w http.ResponseWriter, r *http.Request) {
		a.OrgTeamDetail(w, r, "team-a", gen.OrgTeamDetailParams{})
	}); w.Code != http.StatusOK {
		t.Errorf("admin team-a detail = %d, want 200", w.Code)
	}

	// Alice leads team-a → 200 on team-a, 403 on team-b.
	if w := do("u-alice", "GET", "/api/org/teams/team-a", nil, func(w http.ResponseWriter, r *http.Request) {
		a.OrgTeamDetail(w, r, "team-a", gen.OrgTeamDetailParams{})
	}); w.Code != http.StatusOK {
		t.Errorf("alice team-a detail = %d, want 200", w.Code)
	}
	if w := do("u-alice", "GET", "/api/org/teams/team-b", nil, func(w http.ResponseWriter, r *http.Request) {
		a.OrgTeamDetail(w, r, "team-b", gen.OrgTeamDetailParams{})
	}); w.Code != http.StatusForbidden {
		t.Errorf("alice team-b detail = %d, want 403 (leads team-a only)", w.Code)
	}
}

func TestAPI_DevelopersWritesAuditBeforeData(t *testing.T) {
	a := newAPIWithData(t)
	w := do("u-alice", "GET", "/api/org/teams/team-a/developers", nil, func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Forwarded-For", "9.9.9.9, 10.0.0.1")
		a.OrgTeamDevelopers(w, r, "team-a", gen.OrgTeamDevelopersParams{})
	})
	if w.Code != http.StatusOK {
		t.Fatalf("developers = %d, want 200", w.Code)
	}
	var n int
	var action, ip string
	if err := a.db.QueryRow(`SELECT COUNT(*), MAX(action), MAX(source_ip) FROM audit_log WHERE target_team_id='team-a'`).Scan(&n, &action, &ip); err != nil {
		t.Fatal(err)
	}
	if n != 1 || action != rollup.ActionViewDevelopers || ip != "9.9.9.9" {
		t.Errorf("audit row = n%d action%q ip%q, want 1/view_team_developers/9.9.9.9", n, action, ip)
	}
}

func TestAPI_PeopleWritesAuditBeforeData(t *testing.T) {
	a := newAPIWithData(t)
	w := do("u-boss", "GET", "/api/org/people", nil, func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Forwarded-For", "7.7.7.7")
		a.OrgPeople(w, r, gen.OrgPeopleParams{})
	})
	if w.Code != http.StatusOK {
		t.Fatalf("people = %d, want 200", w.Code)
	}
	var n int
	var action, ip string
	if err := a.db.QueryRow(`SELECT COUNT(*), MAX(action), MAX(source_ip) FROM audit_log WHERE action=?`,
		rollup.ActionViewPeople).Scan(&n, &action, &ip); err != nil {
		t.Fatal(err)
	}
	if n != 1 || action != rollup.ActionViewPeople || ip != "7.7.7.7" {
		t.Errorf("audit row = n%d action%q ip%q, want 1/view_org_developers/7.7.7.7", n, action, ip)
	}
}

func TestAPI_PeopleLeadScoped(t *testing.T) {
	a := newAPIWithData(t)
	// Alice leads team-a; the People leaderboard must be scoped to her teams.
	w := do("u-alice", "GET", "/api/org/people", nil, func(w http.ResponseWriter, r *http.Request) {
		a.OrgPeople(w, r, gen.OrgPeopleParams{})
	})
	if w.Code != http.StatusOK {
		t.Fatalf("alice people = %d, want 200", w.Code)
	}
	var res rollup.PeopleResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// No activity seeded in this fixture → empty leaderboard, but the call
	// must succeed + be scoped (never 500 / never admin-wide).
	for _, p := range res.People {
		if p.UserID == "u-bob" {
			t.Errorf("alice (team-a lead) leaderboard leaked bob (team-b only)")
		}
	}
}

func TestAPI_LogDrillDownScoped(t *testing.T) {
	a := newAPIWithData(t)
	// Alice leads team-a: allowed.
	if w := do("u-alice", "POST", "/api/org/audit/log-drill-down", map[string]string{"team_id": "team-a"}, a.OrgLogDrillDown); w.Code != http.StatusNoContent {
		t.Errorf("alice drill-down team-a = %d, want 204", w.Code)
	}
	// Alice is only a member of team-b: forbidden to drill down.
	if w := do("u-alice", "POST", "/api/org/audit/log-drill-down", map[string]string{"team_id": "team-b"}, a.OrgLogDrillDown); w.Code != http.StatusForbidden {
		t.Errorf("alice drill-down team-b = %d, want 403", w.Code)
	}
}

func TestAPI_BudgetCRUDAdminOnly(t *testing.T) {
	a := newAPIWithData(t)
	create := map[string]any{"scope": "team", "scope_id": "team-a", "monthly_usd_cap": 100.0}

	// Non-admin (alice) is forbidden.
	if w := do("u-alice", "POST", "/api/org/budgets", create, a.OrgBudgetCreate); w.Code != http.StatusForbidden {
		t.Errorf("alice budget create = %d, want 403", w.Code)
	}

	// Admin create → 201 with a BudgetStatus body.
	w := do("u-boss", "POST", "/api/org/budgets", create, a.OrgBudgetCreate)
	if w.Code != http.StatusCreated {
		t.Fatalf("admin budget create = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var b rollup.BudgetStatus
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil || b.ID == "" || b.ScopeLabel != "Team A" {
		t.Fatalf("create body = %+v err=%v", b, err)
	}

	// Duplicate scope → 409.
	if w := do("u-boss", "POST", "/api/org/budgets", create, a.OrgBudgetCreate); w.Code != http.StatusConflict {
		t.Errorf("duplicate budget = %d, want 409", w.Code)
	}
	// Invalid cap → 400.
	if w := do("u-boss", "POST", "/api/org/budgets", map[string]any{"scope": "team", "scope_id": "x", "monthly_usd_cap": 0}, a.OrgBudgetCreate); w.Code != http.StatusBadRequest {
		t.Errorf("zero cap = %d, want 400", w.Code)
	}

	// Delete it → 204; deleting again → 404.
	if w := do("u-boss", "DELETE", "/api/org/budgets/"+b.ID, nil, func(w http.ResponseWriter, r *http.Request) { a.OrgBudgetDelete(w, r, b.ID) }); w.Code != http.StatusNoContent {
		t.Errorf("delete budget = %d, want 204", w.Code)
	}
	if w := do("u-boss", "DELETE", "/api/org/budgets/"+b.ID, nil, func(w http.ResponseWriter, r *http.Request) { a.OrgBudgetDelete(w, r, b.ID) }); w.Code != http.StatusNotFound {
		t.Errorf("delete missing budget = %d, want 404", w.Code)
	}
}

func TestAPI_RevokeAndTeamRole(t *testing.T) {
	a := newAPIWithData(t)
	ctx := context.Background()
	// Seed an issued bearer for bob.
	if _, err := a.db.ExecContext(ctx,
		`INSERT INTO issued_bearers (jti, user_id, issued_at, expires_at) VALUES ('jti-1','u-bob','2026-05-01T00:00:00Z','2026-08-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}

	// List bob's bearers (admin).
	w := do("u-boss", "GET", "/api/org/admin/bearers?user_id=u-bob", nil, func(w http.ResponseWriter, r *http.Request) {
		a.OrgListBearers(w, r, gen.OrgListBearersParams{UserId: "u-bob"})
	})
	var br rollup.BearersResult
	if err := json.Unmarshal(w.Body.Bytes(), &br); err != nil || len(br.Bearers) != 1 || br.Bearers[0].Revoked {
		t.Fatalf("bearers = %+v err=%v", br, err)
	}

	// Revoke jti-1.
	if w := do("u-boss", "POST", "/api/org/admin/revoke", map[string]string{"jti": "jti-1"}, a.OrgRevokeBearer); w.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d, want 204", w.Code)
	}
	var revoked int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM revoked_bearers WHERE jti='jti-1'`).Scan(&revoked)
	if revoked != 1 {
		t.Errorf("revoked_bearers has %d rows, want 1", revoked)
	}

	// Promote bob to lead of team-b (he's already lead; promote member alice in team-b).
	if w := do("u-boss", "POST", "/api/org/admin/team-role", map[string]string{"team_id": "team-b", "user_id": "u-alice", "role": "lead"}, a.OrgSetTeamRole); w.Code != http.StatusNoContent {
		t.Fatalf("set role = %d, want 204", w.Code)
	}
	var role string
	_ = a.db.QueryRow(`SELECT role FROM org_team_members WHERE team_id='team-b' AND user_id='u-alice'`).Scan(&role)
	if role != "lead" {
		t.Errorf("alice team-b role = %q, want lead", role)
	}
	// Promote a non-member → 404.
	if w := do("u-boss", "POST", "/api/org/admin/team-role", map[string]string{"team_id": "team-a", "user_id": "u-bob", "role": "lead"}, a.OrgSetTeamRole); w.Code != http.StatusNotFound {
		t.Errorf("promote non-member = %d, want 404", w.Code)
	}
}

func TestAPI_OverviewScopeIsolation(t *testing.T) {
	a := newAPIWithData(t)
	// bob (lead of team-b only) sees team_count 1, never team-a.
	w := do("u-bob", "GET", "/api/org/teams", nil, func(w http.ResponseWriter, r *http.Request) { a.OrgTeams(w, r, gen.OrgTeamsParams{}) })
	var res rollup.TeamsResult
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if len(res.Teams) != 1 || res.Teams[0].TeamID != "team-b" {
		t.Errorf("bob teams = %+v, want only team-b", res.Teams)
	}
}

func TestAPI_MembersAdminOnly(t *testing.T) {
	a := newAPIWithData(t)
	ctx := context.Background()

	// Seed an inactive user that must not appear in the list.
	if _, err := a.db.ExecContext(ctx,
		`INSERT INTO org_members (user_id, user_name, email, active, created_at, updated_at)
		 VALUES ('u-ghost', 'ghost', 'ghost@acme.example', 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}

	// No session → 401.
	r := httptest.NewRequest("GET", "/api/org/members", nil)
	w := httptest.NewRecorder()
	a.OrgMembers(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no-session members = %d, want 401", w.Code)
	}

	// Non-admin (alice leads team-a) → 403.
	if w := do("u-alice", "GET", "/api/org/members", nil, a.OrgMembers); w.Code != http.StatusForbidden {
		t.Errorf("alice members = %d, want 403", w.Code)
	}

	// Admin (boss) → 200 with the three active members, sorted by email,
	// and the inactive ghost user filtered out.
	w2 := do("u-boss", "GET", "/api/org/members", nil, a.OrgMembers)
	if w2.Code != http.StatusOK {
		t.Fatalf("boss members = %d, want 200; body=%s", w2.Code, w2.Body.String())
	}
	var res rollup.MembersResult
	if err := json.Unmarshal(w2.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode members: %v; body=%s", err, w2.Body.String())
	}
	gotEmails := make([]string, len(res.Members))
	for i, m := range res.Members {
		gotEmails[i] = m.Email
	}
	wantEmails := []string{"alice@acme.example", "bob@acme.example", "boss@acme.example"}
	if !slicesEqual(gotEmails, wantEmails) {
		t.Errorf("members emails = %v, want %v (sorted, no ghost)", gotEmails, wantEmails)
	}
	// Spot-check fields: alice's user_id + user_name + display_name omitted.
	for _, m := range res.Members {
		if m.Email == "alice@acme.example" && (m.UserID != "u-alice" || m.UserName != "alice@acme.example") {
			t.Errorf("alice projected = %+v, want user_id=u-alice user_name=alice@acme.example", m)
		}
	}
}

func TestAPI_TelemetryAdminOnly(t *testing.T) {
	a := newAPIWithData(t)

	// No session → 401.
	r := httptest.NewRequest("GET", "/api/org/telemetry", nil)
	w := httptest.NewRecorder()
	a.OrgTelemetry(w, r, gen.OrgTelemetryParams{})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no-session telemetry = %d, want 401", w.Code)
	}

	// Non-admin (alice leads team-a) → 403: native-console tables are
	// org-aggregate admin data with no per-developer scope to apply.
	if w := do("u-alice", "GET", "/api/org/telemetry", nil, func(w http.ResponseWriter, r *http.Request) {
		a.OrgTelemetry(w, r, gen.OrgTelemetryParams{})
	}); w.Code != http.StatusForbidden {
		t.Errorf("alice telemetry = %d, want 403", w.Code)
	}

	// Admin (boss) → 200 with the honest empty state (no poller wired).
	w2 := do("u-boss", "GET", "/api/org/telemetry", nil, func(w http.ResponseWriter, r *http.Request) {
		a.OrgTelemetry(w, r, gen.OrgTelemetryParams{})
	})
	if w2.Code != http.StatusOK {
		t.Fatalf("boss telemetry = %d, want 200; body=%s", w2.Code, w2.Body.String())
	}
	var res rollup.TelemetryResult
	if err := json.Unmarshal(w2.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode telemetry: %v; body=%s", err, w2.Body.String())
	}
	if res.Configured || len(res.Vendors) != 0 {
		t.Errorf("telemetry = configured %v vendors %d, want false/0 (no poller wired)", res.Configured, len(res.Vendors))
	}
}

func TestAPI_RoutingAdminOnly(t *testing.T) {
	a := newAPIWithData(t)

	// No session → 401.
	r := httptest.NewRequest("GET", "/api/org/routing", nil)
	w := httptest.NewRecorder()
	a.OrgRouting(w, r, gen.OrgRoutingParams{})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no-session routing = %d, want 401", w.Code)
	}

	// Non-admin (alice leads team-a) → 403: routing_summaries is an org-aggregate
	// surface with no per-developer scope to apply (mirrors telemetry).
	if w := do("u-alice", "GET", "/api/org/routing", nil, func(w http.ResponseWriter, r *http.Request) {
		a.OrgRouting(w, r, gen.OrgRoutingParams{})
	}); w.Code != http.StatusForbidden {
		t.Errorf("alice routing = %d, want 403", w.Code)
	}

	// Admin (boss) → 200 with the honest empty state (no node shared a summary).
	w2 := do("u-boss", "GET", "/api/org/routing", nil, func(w http.ResponseWriter, r *http.Request) {
		a.OrgRouting(w, r, gen.OrgRoutingParams{})
	})
	if w2.Code != http.StatusOK {
		t.Fatalf("boss routing = %d, want 200; body=%s", w2.Code, w2.Body.String())
	}
	var res rollup.RoutingResult
	if err := json.Unmarshal(w2.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode routing: %v; body=%s", err, w2.Body.String())
	}
	if res.Configured || res.TotalDecisions != 0 {
		t.Errorf("routing = configured %v decisions %d, want false/0 (no summary shared)", res.Configured, res.TotalDecisions)
	}
}

func TestAPI_SessionsAudited(t *testing.T) {
	a := newAPIWithData(t)

	// No session → 401.
	r := httptest.NewRequest("GET", "/api/org/sessions", nil)
	w := httptest.NewRecorder()
	a.OrgSessions(w, r, gen.OrgSessionsParams{})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no-session sessions = %d, want 401", w.Code)
	}

	// Alice (lead team-a, not admin) lists → 200 and a view_org_sessions audit
	// row is written (no sessions are seeded, so the page is empty — but the
	// disclosure is still recorded).
	if w := do("u-alice", "GET", "/api/org/sessions", nil, func(w http.ResponseWriter, r *http.Request) {
		a.OrgSessions(w, r, gen.OrgSessionsParams{})
	}); w.Code != http.StatusOK {
		t.Fatalf("alice sessions = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// Detail for an unknown id → 404, but the audit row is written FIRST (the
	// audit-before-data invariant), carrying the id in target_detail.
	if w := do("u-alice", "GET", "/api/org/sessions/s-x", nil, func(w http.ResponseWriter, r *http.Request) {
		a.OrgSessionDetail(w, r, "s-x")
	}); w.Code != http.StatusNotFound {
		t.Errorf("alice unknown session = %d, want 404", w.Code)
	}

	var listN, detailN int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='view_org_sessions' AND COALESCE(target_detail,'')=''`).Scan(&listN); err != nil {
		t.Fatalf("audit list count: %v", err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='view_org_sessions' AND target_detail='s-x'`).Scan(&detailN); err != nil {
		t.Fatalf("audit detail count: %v", err)
	}
	if listN < 1 {
		t.Errorf("no view_org_sessions list audit row written")
	}
	if detailN < 1 {
		t.Errorf("no view_org_sessions detail audit row (audit must precede the 404)")
	}
}

func TestAPI_SessionMessagesAudited(t *testing.T) {
	a := newAPIWithData(t)

	// No session → 401.
	r := httptest.NewRequest("GET", "/api/org/sessions/s-x/messages", nil)
	w := httptest.NewRecorder()
	a.OrgSessionMessages(w, r, "s-x")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no-session messages = %d, want 401", w.Code)
	}

	// Alice (lead, not admin) requests an unknown id → 404, but the DEEPER
	// view_session_messages audit row is written FIRST (audit-before-data),
	// carrying the id — and it is a DISTINCT action from view_org_sessions.
	if w := do("u-alice", "GET", "/api/org/sessions/s-x/messages", nil, func(w http.ResponseWriter, r *http.Request) {
		a.OrgSessionMessages(w, r, "s-x")
	}); w.Code != http.StatusNotFound {
		t.Errorf("alice unknown messages = %d, want 404", w.Code)
	}

	var msgN, crossover int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='view_session_messages' AND target_detail='s-x'`).Scan(&msgN); err != nil {
		t.Fatalf("audit messages count: %v", err)
	}
	if msgN < 1 {
		t.Errorf("no view_session_messages audit row (must precede the 404)")
	}
	// The deeper disclosure must NOT masquerade as the metadata view.
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='view_org_sessions' AND target_detail='s-x'`).Scan(&crossover); err != nil {
		t.Fatalf("audit crossover count: %v", err)
	}
	if crossover != 0 {
		t.Errorf("messages disclosure wrote a view_org_sessions row (must be the distinct view_session_messages action)")
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
