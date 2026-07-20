package copilotanalytics

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := orgdb.Open(context.Background(), orgdb.Options{Path: t.TempDir() + "/server.db"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// metricValue reads one stored metric (or fails the lookup).
func metricValue(t *testing.T, db *sql.DB, surface Surface, userKey, metric string) (float64, string, string) {
	t.Helper()
	var v float64
	var unit, actor sql.NullString
	err := db.QueryRow(
		`SELECT value, unit, actor_type FROM copilot_analytics_daily
		   WHERE surface=? AND user_key=? AND metric=?`,
		string(surface), userKey, metric,
	).Scan(&v, &unit, &actor)
	if err != nil {
		t.Fatalf("metricValue(%s,%s,%s): %v", surface, userKey, metric, err)
	}
	return v, unit.String, actor.String
}

func rowCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM copilot_analytics_daily`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func dayUTC(s string) time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return t.UTC()
}

// --- Engagement: the two-step report -> NDJSON fetch, and the token MUST NOT
// leak to the signed download URL. ---

func TestEngagementTwoStepAndTokenNotLeaked(t *testing.T) {
	db := testDB(t)
	var mu sync.Mutex
	var downloadAuth string // captured Authorization on the download-link request
	var apiSawGitHubHeaders bool

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/orgs/acme/copilot/metrics/reports/"):
			// The authenticated report endpoint must carry the GitHub headers.
			if r.Header.Get("Authorization") != "Bearer tok" ||
				r.Header.Get("Accept") != "application/vnd.github+json" ||
				r.Header.Get("X-GitHub-Api-Version") == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			mu.Lock()
			apiSawGitHubHeaders = true
			mu.Unlock()
			_, _ = w.Write([]byte(`{"download_links":["` + srv.URL + `/download/users"],"report_day":"2026-06-14"}`))
		case r.URL.Path == "/download/users":
			// The signed-URL fetch must NOT carry our admin token.
			mu.Lock()
			downloadAuth = r.Header.Get("Authorization")
			mu.Unlock()
			_, _ = w.Write([]byte(`{"report_day":"2026-06-14","user_login":"octodev","total_engaged_users":1,` +
				`"copilot_ide_code_completions":{"editors":[{"name":"vscode","models":[{"name":"default","languages":[` +
				`{"name":"python","total_code_suggestions":320,"total_code_acceptances":190,"total_code_lines_suggested":1400,"total_code_lines_accepted":870},` +
				`{"name":"go","total_code_suggestions":110,"total_code_acceptances":64,"total_code_lines_suggested":520,"total_code_lines_accepted":300}]}]}]},` +
				`"copilot_ide_chat":{"total_chats":22},"copilot_dotcom_chat":{"total_chats":3}}` + "\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p, err := NewPoller(db, string(SurfaceEngagement), srv.URL, "tok", "acme", OwnerOrg, "users-1-day", "org1")
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	n, err := p.PollWindow(context.Background(), dayUTC("2026-06-14"), dayUTC("2026-06-15"))
	if err != nil {
		t.Fatalf("PollWindow: %v", err)
	}
	if n == 0 {
		t.Fatal("no rows written")
	}

	mu.Lock()
	defer mu.Unlock()
	if !apiSawGitHubHeaders {
		t.Error("report API request was missing GitHub headers")
	}
	if downloadAuth != "" {
		t.Errorf("SECURITY: admin token leaked to signed download URL: %q", downloadAuth)
	}

	// suggestions summed across languages: 320+110 = 430; acceptances 190+64 = 254.
	if v, unit, actor := metricValue(t, db, SurfaceEngagement, "octodev", MetricCodeSuggestions); v != 430 || unit != "count" || actor != ActorUser {
		t.Errorf("code_suggestions = %v/%s/%s, want 430/count/user", v, unit, actor)
	}
	if v, _, _ := metricValue(t, db, SurfaceEngagement, "octodev", MetricCodeAcceptances); v != 254 {
		t.Errorf("code_acceptances = %v, want 254", v)
	}
	// chats: ide 22 + dotcom 3 = 25.
	if v, _, _ := metricValue(t, db, SurfaceEngagement, "octodev", MetricChats); v != 25 {
		t.Errorf("chats = %v, want 25", v)
	}
}

// --- Seats: summary counts + per-login activity, with the -ci bot bucketed as
// automation. ---

func TestSeatsSurface(t *testing.T) {
	db := testDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/orgs/acme/copilot/billing":
			_, _ = w.Write([]byte(`{"seat_breakdown":{"total":120,"added_this_cycle":6,"active_this_cycle":98,"inactive_this_cycle":22},"plan_type":"business"}`))
		case "/orgs/acme/copilot/billing/seats":
			_, _ = w.Write([]byte(`{"total_seats":2,"seats":[` +
				`{"assignee":{"login":"octodev","id":100001,"type":"User"},"last_activity_at":"2026-06-14T18:22:10Z","last_activity_editor":"vscode/1.119.0","plan_type":"business"},` +
				`{"assignee":{"login":"octobot-ci","id":100002,"type":"User"},"last_activity_at":"2026-06-13T02:00:00Z","last_activity_editor":"copilot-cli/1.4.0","plan_type":"business"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p, err := NewPoller(db, string(SurfaceSeats), srv.URL, "tok", "acme", OwnerOrg, "", "org1")
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	if _, err := p.PollWindow(context.Background(), dayUTC("2026-06-12"), dayUTC("2026-06-15")); err != nil {
		t.Fatalf("PollWindow: %v", err)
	}

	// Snapshot day is the last full day in the window (2026-06-14).
	if v, unit, actor := metricValue(t, db, SurfaceSeats, orgAggregateKey, MetricSeatsTotal); v != 120 || unit != "seats" || actor != ActorOrg {
		t.Errorf("seats_total = %v/%s/%s, want 120/seats/org", v, unit, actor)
	}
	if v, _, actor := metricValue(t, db, SurfaceSeats, "octodev", MetricActiveSeat); v != 1 || actor != ActorUser {
		t.Errorf("octodev active_seat = %v/%s, want 1/user", v, actor)
	}
	// The -ci seat is bucketed as automation.
	if _, _, actor := metricValue(t, db, SurfaceSeats, "octobot-ci", MetricActiveSeat); actor != ActorAutomation {
		t.Errorf("octobot-ci actor = %s, want automation", actor)
	}
}

// --- Enhanced billing: premium-request $ summed per day. ---

func TestBillingSurface(t *testing.T) {
	db := testDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/organizations/acme/settings/billing/premium_request/usage" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"usageItems":[` +
			`{"date":"2026-06-14","product":"copilot","sku":"Copilot Premium Request","model":"gpt-5.5","unitType":"requests","netAmount":74.0},` +
			`{"date":"2026-06-14","product":"actions","sku":"Actions","netAmount":999.0}]}`))
	}))
	defer srv.Close()

	p, err := NewPoller(db, string(SurfaceBilling), srv.URL, "tok", "acme", OwnerOrg, "", "org1")
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	if _, err := p.PollWindow(context.Background(), dayUTC("2026-06-14"), dayUTC("2026-06-15")); err != nil {
		t.Fatalf("PollWindow: %v", err)
	}
	// Only the copilot line item is summed; the actions item is ignored.
	if v, unit, _ := metricValue(t, db, SurfaceBilling, orgAggregateKey, MetricCost); v != 74.0 || unit != "usd" {
		t.Errorf("cost = %v/%s, want 74/usd", v, unit)
	}
}

// --- Upsert idempotency: re-poll overwrites, never duplicates. ---

func TestUpsertIdempotent(t *testing.T) {
	db := testDB(t)
	ms := []DailyMetric{
		emitMetric("2026-06-14", "octodev", ActorUser, SurfaceEngagement, UnitCount, MetricChats, 25),
	}
	p := &Poller{DB: db, spec: surfaceRegistry[SurfaceEngagement]}
	if _, err := p.upsert(context.Background(), ms); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	ms[0].Value = 30
	if _, err := p.upsert(context.Background(), ms); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	if got := rowCount(t, db); got != 1 {
		t.Errorf("row count = %d, want 1 (overwrite, not insert)", got)
	}
	if v, _, _ := metricValue(t, db, SurfaceEngagement, "octodev", MetricChats); v != 30 {
		t.Errorf("value = %v, want 30 (latest)", v)
	}
}

// --- The sibling cost rollup: seat monthly subscription + per-day overage,
// never summing engagement counts. ---

func TestLoadCostSummary(t *testing.T) {
	db := testDB(t)
	p := &Poller{DB: db, OrgID: "org1", spec: surfaceRegistry[SurfaceSeats]}
	_, _ = p.upsert(context.Background(), []DailyMetric{
		emitMetric("2026-06-14", orgAggregateKey, ActorOrg, SurfaceSeats, UnitSeats, MetricSeatsTotal, 120),
		emitMetric("2026-06-13", orgAggregateKey, ActorOrg, SurfaceSeats, UnitSeats, MetricSeatsTotal, 100), // older snapshot
		emitMetric("2026-06-14", orgAggregateKey, ActorOrg, SurfaceBilling, UnitUSD, MetricCost, 74),
		emitMetric("2026-06-13", orgAggregateKey, ActorOrg, SurfaceBilling, UnitUSD, MetricCost, 10),
		// Engagement count — must NOT be read into cost.
		emitMetric("2026-06-14", "octodev", ActorUser, SurfaceEngagement, UnitCount, MetricChats, 999),
	})

	cs, err := LoadCostSummary(context.Background(), db, "org1", 19)
	if err != nil {
		t.Fatalf("LoadCostSummary: %v", err)
	}
	// Latest snapshot wins (2026-06-14, 120 seats) → monthly 120*19 = 2280.
	if cs.SeatSnapshot.Day != "2026-06-14" || cs.SeatSnapshot.Seats != 120 || cs.SeatSnapshot.MonthlyUSD != 2280 {
		t.Errorf("seat snapshot = %+v, want day 2026-06-14 / 120 / 2280", cs.SeatSnapshot)
	}
	// Overage is additive across days: 10 + 74 = 84.
	if cs.TotalOverageUSD != 84 || len(cs.OverageByDay) != 2 {
		t.Errorf("overage = %v over %d days, want 84/2", cs.TotalOverageUSD, len(cs.OverageByDay))
	}
}

// --- NewPoller guards. ---

func TestNewPollerErrors(t *testing.T) {
	db := testDB(t)
	if _, err := NewPoller(db, "nope", "", "k", "acme", OwnerOrg, "", ""); err == nil {
		t.Error("unknown surface should error")
	}
	if _, err := NewPoller(db, string(SurfaceSeats), "", "k", "", OwnerOrg, "", ""); err == nil {
		t.Error("empty owner should error")
	}
	if _, err := NewPoller(db, string(SurfaceBilling), "", "k", "acme", OwnerEnterprise, "", ""); err == nil {
		t.Error("enterprise billing should error (org-scoped only)")
	}
	if _, err := NewPoller(db, string(SurfaceEngagement), "", "k", "acme", "weird", "", ""); err == nil {
		t.Error("bad owner_type should error")
	}
}

// --- Identity: login -> email map -> org_members join. ---

func TestResolveOrgUserID(t *testing.T) {
	db := testDB(t)
	seedMember(t, db, "u1", "dev@acme.com")
	ctx := context.Background()

	m := map[string]string{"octodev": "DEV@acme.com"} // case-insensitive join
	if id, ok := ResolveOrgUserID(ctx, db, ActorUser, "octodev", m); !ok || id != "u1" {
		t.Errorf("login 2-step = %q/%v, want u1/true", id, ok)
	}
	// Login absent from the map → unresolved (non-enrolled coverage).
	if _, ok := ResolveOrgUserID(ctx, db, ActorUser, "ghost", m); ok {
		t.Error("unmapped login should not resolve")
	}
	// Org-aggregate / non-user actors never resolve.
	if _, ok := ResolveOrgUserID(ctx, db, ActorOrg, orgAggregateKey, m); ok {
		t.Error("org actor should not resolve")
	}
}

func seedMember(t *testing.T, db *sql.DB, userID, email string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO org_members (user_id, user_name, email, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		userID, userID, email, "2026-05-01T00:00:00Z", "2026-05-01T00:00:00Z",
	); err != nil {
		t.Fatalf("seed member: %v", err)
	}
}

func TestResolveKey(t *testing.T) {
	t.Setenv(apiKeyEnv, "envtok")
	if k, err := ResolveKey(""); err != nil || k != "envtok" {
		t.Errorf("env key = %q/%v", k, err)
	}
	t.Setenv(apiKeyEnv, "")
	f := t.TempDir() + "/k"
	if err := os.WriteFile(f, []byte("  filetok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if k, err := ResolveKey(f); err != nil || k != "filetok" {
		t.Errorf("file key = %q/%v", k, err)
	}
	if _, err := ResolveKey(""); err == nil {
		t.Error("expected error with no env and no file")
	}
}

func TestSchedulerDefaults(t *testing.T) {
	s := NewScheduler(&Poller{spec: surfaceRegistry[SurfaceEngagement]}, 0, 0, nil)
	if s.interval != 24*time.Hour {
		t.Errorf("interval = %v, want 24h", s.interval)
	}
	if s.lag != 48*time.Hour {
		t.Errorf("lag = %v, want 48h", s.lag)
	}
}
