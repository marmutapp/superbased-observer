// Org-dashboard E2E (M3): brings up the full M1+M2+M3 stack through the
// assembled Server.Handler() and exercises the /api/org/* surface with real
// SAML sessions.
//
//   - Conformance: every documented /api/org/* route is mounted and
//     SAML-gated (unauthenticated → 401, never 404).
//   - Admin (email in [dashboard] admin_emails) sees org-wide aggregates that
//     reflect the seeded data, and any team.
//   - A team lead sees their team but is FORBIDDEN (403) on another team's
//     detail/developers and on the admin-only budgets surface — no team-lead
//     can reach another team's data, even by URL.
//   - The developer drill-down writes an audit_log row.
package e2e

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/crewjam/saml"
	"github.com/crewjam/saml/logger"

	"github.com/marmutapp/superbased-observer/internal/orgserver"
	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
)

const (
	adminEmail = "admin@acme.example"
	leadEmail  = "lead@acme.example"
)

func TestOrgDashboardE2E(t *testing.T) {
	ctx := context.Background()

	// In-process IdP whose session we mutate between logins to authenticate
	// different users (admin, then lead) through the same SP.
	idpKey, idpCert := genRSACert(t, "e2e-idp-dash")
	sess := &saml.Session{ID: "s", CreateTime: time.Now(), ExpireTime: time.Now().Add(time.Hour)}
	idp := &saml.IdentityProvider{
		Key: idpKey, Certificate: idpCert, Logger: logger.DefaultLogger,
		SessionProvider: fixedSessionProvider{session: sess},
	}
	idpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		out, err := xml.MarshalIndent(idp.Metadata(), "", "  ")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/samlmetadata+xml")
		_, _ = w.Write(out)
	}))
	defer idpServer.Close()
	idpURL, _ := url.Parse(idpServer.URL)
	idp.MetadataURL = *idpURL
	idp.SSOURL = *mustURL(idpServer.URL + "/sso")

	cfg := writeServerConfig(t, idpServer.URL)
	cfg.Dashboard.AdminEmails = []string{adminEmail} // admin@ is the org admin

	srv, err := orgserver.New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("orgserver.New: %v", err)
	}
	// Close the server's DB handle before the test returns, so t.TempDir's
	// RemoveAll can delete server.db on Windows (this test drives Handler()
	// directly and never calls Run, which is where the DB is otherwise closed).
	defer func() { _ = srv.Close() }()
	spServer := httptest.NewServer(srv.Handler())
	defer spServer.Close()
	client := spServer.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	spMD := fetchSPMetadata(t, spServer.URL)
	idp.ServiceProviderProvider = spProvider{md: spMD}

	// --- 0. Conformance: every /api/org/* route mounted + SAML-gated --------
	for _, rt := range []struct{ method, path string }{
		{"GET", "/api/org/overview"},
		{"GET", "/api/org/teams"},
		{"GET", "/api/org/teams/t1"},
		{"GET", "/api/org/teams/t1/developers"},
		{"GET", "/api/org/projects"},
		{"GET", "/api/org/projects/p1"},
		{"GET", "/api/org/budgets"},
		{"POST", "/api/org/budgets"},
		{"PUT", "/api/org/budgets/b1"},
		{"DELETE", "/api/org/budgets/b1"},
		{"GET", "/api/org/audit"},
		{"POST", "/api/org/audit/log-drill-down"},
		{"GET", "/api/org/admin/bearers?user_id=u1"},
		{"POST", "/api/org/admin/revoke"},
		{"POST", "/api/org/admin/team-role"},
	} {
		req, _ := http.NewRequest(rt.method, spServer.URL+rt.path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", rt.method, rt.path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("conformance %s %s: status=%d, want 401 (mounted + SAML-gated)", rt.method, rt.path, resp.StatusCode)
		}
	}

	// --- 1. Provision admin + lead via SCIM, then seed teams + data ---------
	adminID := scimProvisionEmail(t, spServer.URL, client, adminEmail)
	leadID := scimProvisionEmail(t, spServer.URL, client, leadEmail)
	seedDashboardData(t, cfg.Server.DBPath, adminID, leadID)

	// --- 2. Admin sees org-wide aggregates reflecting the seed --------------
	adminCookie := loginAs(t, idp, sess, spMD, spServer.URL, client, adminEmail)
	var ov struct {
		TotalCostUSD  float64 `json:"total_cost_usd"`
		TotalSessions int64   `json:"total_sessions"`
		TeamCount     int64   `json:"team_count"`
	}
	getJSON(t, client, spServer.URL+"/api/org/overview", adminCookie, http.StatusOK, &ov)
	if ov.TotalSessions != 1 || ov.TeamCount != 2 || ov.TotalCostUSD < 0.09 {
		t.Errorf("admin overview = %+v, want sessions=1 teams=2 cost≈0.10", ov)
	}
	// Admin can read any team.
	if code := getStatus(t, client, spServer.URL+"/api/org/teams/team-y", adminCookie); code != http.StatusOK {
		t.Errorf("admin team-y = %d, want 200", code)
	}

	// --- 3. Lead is scoped to their team; 403 elsewhere ---------------------
	leadCookie := loginAs(t, idp, sess, spMD, spServer.URL, client, leadEmail)
	if code := getStatus(t, client, spServer.URL+"/api/org/teams/team-x", leadCookie); code != http.StatusOK {
		t.Errorf("lead team-x = %d, want 200", code)
	}
	if code := getStatus(t, client, spServer.URL+"/api/org/teams/team-y", leadCookie); code != http.StatusForbidden {
		t.Errorf("lead team-y = %d, want 403 (off-team)", code)
	}
	if code := getStatus(t, client, spServer.URL+"/api/org/teams/team-y/developers", leadCookie); code != http.StatusForbidden {
		t.Errorf("lead team-y developers = %d, want 403", code)
	}
	if code := getStatus(t, client, spServer.URL+"/api/org/budgets", leadCookie); code != http.StatusForbidden {
		t.Errorf("lead budgets = %d, want 403 (admin only)", code)
	}

	// --- 4. The developer drill-down writes an audit row --------------------
	if code := getStatus(t, client, spServer.URL+"/api/org/teams/team-x/developers", leadCookie); code != http.StatusOK {
		t.Errorf("lead team-x developers = %d, want 200", code)
	}
	assertAuditRow(t, cfg.Server.DBPath, "team-x")
}

// ---- M3 helpers ------------------------------------------------------------

func scimProvisionEmail(t *testing.T, base string, client *http.Client, email string) string {
	t.Helper()
	body := `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"` + email +
		`","emails":[{"value":"` + email + `","primary":true}],"active":true}`
	req, _ := http.NewRequest(http.MethodPost, base+"/scim/v2/Users", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+scimToken)
	req.Header.Set("Content-Type", "application/scim+json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("scim create %s: status=%d body=%s", email, resp.StatusCode, raw)
	}
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatalf("scim create %s returned no id", email)
	}
	return id
}

// seedDashboardData seeds teams + memberships + a little pushed data directly
// in the server DB (the SCIM/push paths are E2E-tested separately; this keeps
// the M3 authorization test focused). lead leads team-x; team-y exists with no
// lead. One session + one token_usage row attributes spend to the lead on
// team-x within the default window.
func seedDashboardData(t *testing.T, dbPath, adminID, leadID string) {
	t.Helper()
	d, err := orgdb.Open(context.Background(), orgdb.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("open server db: %v", err)
	}
	defer func() { _ = d.Close() }()
	ts := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	push := time.Now().UTC().Format(time.RFC3339)
	stmts := []struct {
		q    string
		args []any
	}{
		{`INSERT INTO org_teams (team_id, display_name, created_at, updated_at) VALUES ('team-x','Team X',?,?)`, []any{ts, ts}},
		{`INSERT INTO org_teams (team_id, display_name, created_at, updated_at) VALUES ('team-y','Team Y',?,?)`, []any{ts, ts}},
		{`INSERT INTO org_team_members (team_id, user_id, role) VALUES ('team-x', ?, 'lead')`, []any{leadID}},
		{`INSERT INTO org_team_members (team_id, user_id, role) VALUES ('team-y', ?, 'member')`, []any{adminID}},
		{`INSERT INTO sessions (id, user_id, project_root, tool, started_at, total_actions, pushed_at, pushed_by_user_id)
		  VALUES ('s-x', ?, '/repo/x', 'claude-code', ?, 1, ?, ?)`, []any{leadID, ts, push, leadID}},
		{`INSERT INTO token_usage (source_file, source_event_id, user_id, session_id, project_root, timestamp, tool, model, input_tokens, output_tokens, estimated_cost_usd, source, reliability, pushed_at, pushed_by_user_id)
		  VALUES ('f','t-x', ?, 's-x', '/repo/x', ?, 'claude-code', 'claude', 100, 50, 0.10, 'jsonl', 'reliable', ?, ?)`, []any{leadID, ts, push, leadID}},
	}
	for _, s := range stmts {
		if _, err := d.ExecContext(context.Background(), s.q, s.args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, s.q)
		}
	}
}

func assertAuditRow(t *testing.T, dbPath, teamID string) {
	t.Helper()
	d, err := orgdb.Open(context.Background(), orgdb.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("open server db: %v", err)
	}
	defer func() { _ = d.Close() }()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE target_team_id = ? AND action = 'view_team_developers'`, teamID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Errorf("audit_log rows for %s = %d, want >= 1 (drill-down audited)", teamID, n)
	}
}

// loginAs mutates the IdP session to email, performs a real SAML login, and
// returns the resulting dashboard session cookie.
func loginAs(t *testing.T, idp *saml.IdentityProvider, sess *saml.Session, spMD *saml.EntityDescriptor, base string, client *http.Client, email string) *http.Cookie {
	t.Helper()
	sess.NameID, sess.UserName, sess.UserEmail = email, email, email
	c := samlLogin(t, idp, spMD, base, client)
	if c == nil {
		t.Fatalf("SAML login as %s produced no cookie", email)
	}
	return c
}

func getStatus(t *testing.T, client *http.Client, url string, cookie *http.Cookie) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.AddCookie(cookie)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func getJSON(t *testing.T, client *http.Client, url string, cookie *http.Cookie, wantStatus int, out any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.AddCookie(cookie)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: status=%d, want %d; body=%s", url, resp.StatusCode, wantStatus, raw)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("GET %s: decode: %v", url, err)
	}
}
