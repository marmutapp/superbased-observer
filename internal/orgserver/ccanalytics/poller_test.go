package ccanalytics

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
)

func loadFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "sample-response.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

func TestParseAnalyticsResponse_DocumentedSchema(t *testing.T) {
	metrics, hasMore, next, err := parseAnalyticsResponse(loadFixture(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if hasMore || next != "" {
		t.Fatalf("pagination: hasMore=%v next=%q, want false/empty", hasMore, next)
	}

	// Index by (userKey, metric) for assertions.
	type k struct{ user, metric string }
	m := map[k]DailyMetric{}
	for _, dm := range metrics {
		m[k{dm.UserKey, dm.Metric}] = dm
		if dm.Day != "2025-09-01" {
			t.Fatalf("day not derived to UTC YYYY-MM-DD: %q", dm.Day)
		}
	}

	// user_actor row: dev@acme.example. Cost 1025 CENTS -> $10.25 (the 100x trap).
	if got := m[k{"dev@acme.example", MetricCostUSD}]; got.Value != 10.25 {
		t.Fatalf("cost_usd = %v, want 10.25 (cents->dollars)", got.Value)
	}
	if got := m[k{"dev@acme.example", MetricTokensInput}]; got.Value != 100000 {
		t.Fatalf("tokens_input = %v, want 100000", got.Value)
	}
	if got := m[k{"dev@acme.example", MetricLinesAdded}]; got.Value != 1543 {
		t.Fatalf("lines_added = %v, want 1543", got.Value)
	}
	if got := m[k{"dev@acme.example", "tool_edit_tool_accepted"}]; got.Value != 45 {
		t.Fatalf("tool_edit_tool_accepted = %v, want 45", got.Value)
	}
	if got := m[k{"dev@acme.example", MetricCostUSD}]; got.ActorType != ActorUser {
		t.Fatalf("dev actor_type = %q, want user_actor", got.ActorType)
	}

	// api_actor row: keyed by api_key_name, no email, actor_type api_actor.
	ci := m[k{"ci-runner-prod", MetricCostUSD}]
	if ci.Value != 1.40 { // 140 cents
		t.Fatalf("ci cost_usd = %v, want 1.40", ci.Value)
	}
	if ci.ActorType != ActorAPI {
		t.Fatalf("ci actor_type = %q, want api_actor", ci.ActorType)
	}
}

func TestPollDay_PaginationAndUpsert(t *testing.T) {
	ctx := context.Background()
	d, err := orgdb.Open(ctx, orgdb.Options{Path: filepath.Join(t.TempDir(), "s.db")})
	if err != nil {
		t.Fatalf("orgdb.Open: %v", err)
	}
	defer func() { _ = d.Close() }()

	fixture := loadFixture(t)
	// Serve the fixture as page 1 with has_more=true → page 2 with the same
	// body but has_more=false, to exercise the cursor loop.
	page2 := `{"data":[],"has_more":false,"next_page":null}`
	var gotKey, gotStartingAt string
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotKey = r.Header.Get("x-api-key")
		gotStartingAt = r.URL.Query().Get("starting_at")
		if r.URL.Query().Get("page") == "" {
			// First page: rewrite has_more=true, next_page="p2".
			body := string(fixture)
			body = replaceLast(body, `"has_more": false`, `"has_more": true`)
			body = replaceLast(body, `"next_page": null`, `"next_page": "p2"`)
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(page2))
	}))
	defer srv.Close()

	p := &Poller{
		DB: d, HTTPClient: srv.Client(), BaseURL: srv.URL,
		APIKey: "sk-ant-admin-test", OrgID: "org-1",
		Now: func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}
	n, err := p.PollDay(ctx, "2025-09-01")
	if err != nil {
		t.Fatalf("PollDay: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 pages fetched, got %d", calls)
	}
	if gotKey != "sk-ant-admin-test" || gotStartingAt != "2025-09-01" {
		t.Fatalf("request wrong: key=%q starting_at=%q", gotKey, gotStartingAt)
	}
	if n == 0 {
		t.Fatal("no metrics upserted")
	}

	var cost float64
	if err := d.QueryRowContext(ctx,
		`SELECT value FROM cc_analytics_daily WHERE day='2025-09-01' AND user_key='dev@acme.example' AND metric='cost_usd'`).
		Scan(&cost); err != nil {
		t.Fatalf("read: %v", err)
	}
	if cost != 10.25 {
		t.Fatalf("stored cost_usd = %v, want 10.25", cost)
	}

	// Re-poll overwrites (restated daily total), no duplicate rows.
	before := countRows(t, d)
	if _, err := p.PollDay(ctx, "2025-09-01"); err != nil {
		t.Fatalf("re-poll: %v", err)
	}
	if after := countRows(t, d); after != before {
		t.Fatalf("re-poll changed row count %d -> %d", before, after)
	}
}

func TestResolveOrgUserID(t *testing.T) {
	ctx := context.Background()
	d, err := orgdb.Open(ctx, orgdb.Options{Path: filepath.Join(t.TempDir(), "s.db")})
	if err != nil {
		t.Fatalf("orgdb.Open: %v", err)
	}
	defer func() { _ = d.Close() }()
	if _, err := d.ExecContext(ctx,
		`INSERT INTO org_members (user_id, user_name, email, created_at, updated_at)
		 VALUES ('u-1','dev','Dev@Acme.Example','t','t')`); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	// Case-insensitive user_actor match.
	if id, ok := ResolveOrgUserID(ctx, d, ActorUser, "dev@acme.example"); !ok || id != "u-1" {
		t.Fatalf("user match: id=%q ok=%v", id, ok)
	}
	// api_actor never resolves (no email).
	if _, ok := ResolveOrgUserID(ctx, d, ActorAPI, "ci-runner-prod"); ok {
		t.Fatal("api_actor should not resolve")
	}
	// Unknown email → unenrolled (ok=false).
	if _, ok := ResolveOrgUserID(ctx, d, ActorUser, "ghost@nowhere.example"); ok {
		t.Fatal("unknown email should not resolve")
	}
}

func TestValidateKind(t *testing.T) {
	for in, want := range map[string]string{"enterprise": KindEnterprise, "admin": KindAdmin, "": KindAdmin} {
		got, err := ValidateKind(in)
		if err != nil || got != want {
			t.Errorf("ValidateKind(%q) = %q,%v; want %q", in, got, err, want)
		}
	}
	if _, err := ValidateKind("bogus"); err == nil {
		t.Error("expected error for bogus kind")
	}
}

func TestResolveKey_EnvWins(t *testing.T) {
	t.Setenv(apiKeyEnv, "from-env")
	k, err := ResolveKey("")
	if err != nil || k != "from-env" {
		t.Fatalf("ResolveKey env: %q %v", k, err)
	}
}

func countRows(t *testing.T, d *sql.DB) int {
	t.Helper()
	var n int
	if err := d.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM cc_analytics_daily`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// replaceLast replaces the last occurrence of old with repl.
func replaceLast(s, old, repl string) string {
	i := strings.LastIndex(s, old)
	if i < 0 {
		return s
	}
	return s[:i] + repl + s[i+len(old):]
}
