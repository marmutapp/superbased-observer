package codexanalytics

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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
		`SELECT value, unit, actor_type FROM codex_analytics_daily
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
	if err := db.QueryRow(`SELECT COUNT(*) FROM codex_analytics_daily`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

const may1 = "2026-05-01T00:00:00Z"

func may1Unix() int64 { t, _ := time.Parse(time.RFC3339, may1); return t.Unix() }

func TestPollChatGPTEnterprise(t *testing.T) {
	body := `{
	  "data": [
	    {"start_time":"2026-05-01T00:00:00Z",
	     "user":{"email":"Dev@Acme.com","id":"wsu_1"},
	     "credits":1234, "threads":12, "turns":48,
	     "tokens":{"text_input":100000,"cached_input":20000,"output":5000}}
	  ],
	  "has_more": false, "next_page": null }`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("auth header = %q", got)
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	db := testDB(t)
	p, err := NewPoller(db, string(SurfaceChatGPTEnterprise), srv.URL, "k", "ws_123", "org_1")
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	start, _ := time.Parse(time.RFC3339, may1)
	n, err := p.PollWindow(context.Background(), start, start.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("PollWindow: %v", err)
	}
	if n != 6 {
		t.Fatalf("rows = %d, want 6", n)
	}

	cost, unit, actor := metricValue(t, db, SurfaceChatGPTEnterprise, "Dev@Acme.com", MetricCost)
	if cost != 1234 || unit != string(UnitCredits) || actor != ActorUser {
		t.Errorf("cost = %v/%s/%s, want 1234/credits/user", cost, unit, actor)
	}
	if v, u, _ := metricValue(t, db, SurfaceChatGPTEnterprise, "Dev@Acme.com", MetricTokensCached); v != 20000 || u != string(UnitTokens) {
		t.Errorf("cached tokens = %v/%s, want 20000/tokens", v, u)
	}
	if v, u, _ := metricValue(t, db, SurfaceChatGPTEnterprise, "Dev@Acme.com", MetricThreads); v != 12 || u != string(UnitCount) {
		t.Errorf("threads = %v/%s, want 12/count", v, u)
	}
}

// TestPollChatGPTEnterprise_SampleShape validates the parser against the richer
// reconstructed live-shape sample (docs/plans/codex-analytics-sample-response.json):
// bucket_start + actor.{user_email,user_id} + per-model tokens under by_model[].
func TestPollChatGPTEnterprise_SampleShape(t *testing.T) {
	body := `{
	  "data": [
	    {"bucket_start":"2026-06-13T00:00:00Z","bucket_end":"2026-06-14T00:00:00Z","granularity":"day",
	     "actor":{"user_email":"dev0@example.com","user_id":"user_R0"},
	     "credits":1840,"threads":14,"turns":96,
	     "by_client":[{"client":"cli","credits":980}],
	     "by_model":[
	       {"model":"gpt-5.5-codex","tokens":{"text_input":220000,"cached_input":60000,"output":48000},"credits":1600},
	       {"model":"gpt-5.4-codex","tokens":{"text_input":40000,"cached_input":5000,"output":9000},"credits":240}]},
	    {"bucket_start":"2026-06-13T00:00:00Z","granularity":"day",
	     "actor":{"user_email":null,"user_id":"user_R1"},
	     "credits":120,"threads":2,"turns":7,
	     "by_model":[{"model":"gpt-5.5-codex","tokens":{"text_input":18000,"cached_input":2000,"output":3500},"credits":120}]}
	  ],
	  "has_more": false, "next_page": null }`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	db := testDB(t)
	p, err := NewPoller(db, string(SurfaceChatGPTEnterprise), srv.URL, "k", "ws_123", "org_1")
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	start, _ := time.Parse(time.RFC3339, "2026-06-13T00:00:00Z")
	if _, err := p.PollWindow(context.Background(), start, start.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("PollWindow: %v", err)
	}

	// Row 1: identity = actor.user_email; tokens SUMMED across by_model.
	if v, u, a := metricValue(t, db, SurfaceChatGPTEnterprise, "dev0@example.com", MetricCost); v != 1840 || u != string(UnitCredits) || a != ActorUser {
		t.Errorf("row1 cost = %v/%s/%s, want 1840/credits/user", v, u, a)
	}
	if v, _, _ := metricValue(t, db, SurfaceChatGPTEnterprise, "dev0@example.com", MetricTokensInput); v != 260000 {
		t.Errorf("row1 tokens_input = %v, want 260000 (220000+40000 summed)", v)
	}
	if v, _, _ := metricValue(t, db, SurfaceChatGPTEnterprise, "dev0@example.com", MetricTokensCached); v != 65000 {
		t.Errorf("row1 tokens_cached = %v, want 65000", v)
	}
	if v, _, _ := metricValue(t, db, SurfaceChatGPTEnterprise, "dev0@example.com", MetricTokensOutput); v != 57000 {
		t.Errorf("row1 tokens_output = %v, want 57000", v)
	}
	// Row 2: email withheld → identity falls back to user_id (unenrolled at merge).
	if v, _, _ := metricValue(t, db, SurfaceChatGPTEnterprise, "user_R1", MetricCost); v != 120 {
		t.Errorf("row2 (withheld email) cost = %v under user_id key, want 120", v)
	}
	if v, _, _ := metricValue(t, db, SurfaceChatGPTEnterprise, "user_R1", MetricTokensInput); v != 18000 {
		t.Errorf("row2 tokens_input = %v, want 18000", v)
	}
}

func TestPollOpenAIOrg(t *testing.T) {
	u1 := may1Unix()
	usage := fmt.Sprintf(`{"data":[{"start_time":%d,"results":[
	   {"input_tokens":141201,"output_tokens":9756,"input_cached_tokens":100,"num_model_requests":470,"user_id":"user_a"},
	   {"input_tokens":50,"output_tokens":5,"input_cached_tokens":0,"num_model_requests":2,"user_id":""}
	 ]}],"has_more":false,"next_page":null}`, u1)
	costs := fmt.Sprintf(`{"data":[{"start_time":%d,"results":[
	   {"amount":{"value":0.13080438,"currency":"usd"},"user_id":"user_a"}
	 ]}],"has_more":false,"next_page":null}`, u1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case openAIUsagePath:
			_, _ = w.Write([]byte(usage))
		case openAICostsPath:
			_, _ = w.Write([]byte(costs))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	db := testDB(t)
	p, err := NewPoller(db, string(SurfaceOpenAIOrg), srv.URL, "k", "", "org_1")
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	start, _ := time.Parse(time.RFC3339, may1)
	if _, err := p.PollWindow(context.Background(), start, start.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("PollWindow: %v", err)
	}

	// Cost is DOLLARS, not divided by 100.
	if v, u, a := metricValue(t, db, SurfaceOpenAIOrg, "user_a", MetricCost); v != 0.13080438 || u != string(UnitUSD) || a != ActorUser {
		t.Errorf("cost = %v/%s/%s, want 0.13080438/usd/user", v, u, a)
	}
	if v, _, _ := metricValue(t, db, SurfaceOpenAIOrg, "user_a", MetricTokensInput); v != 141201 {
		t.Errorf("input tokens = %v, want 141201", v)
	}
	// Null user_id row bucketed under the org-aggregate sentinel as workspace.
	if v, _, a := metricValue(t, db, SurfaceOpenAIOrg, orgAggregateKey, MetricModelRequests); v != 2 || a != ActorWorkspace {
		t.Errorf("aggregate model_requests = %v/%s, want 2/workspace", v, a)
	}
}

func TestPaginationFollowsCursor(t *testing.T) {
	page1 := `{"data":[{"start_time":"2026-05-01T00:00:00Z","user":{"email":"a@x.com"},"credits":1,"tokens":{}}],"has_more":true,"next_page":"P2"}`
	page2 := `{"data":[{"start_time":"2026-05-01T00:00:00Z","user":{"email":"b@x.com"},"credits":2,"tokens":{}}],"has_more":false,"next_page":null}`
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Query().Get("page") == "P2" {
			_, _ = w.Write([]byte(page2))
			return
		}
		_, _ = w.Write([]byte(page1))
	}))
	defer srv.Close()

	db := testDB(t)
	p, _ := NewPoller(db, string(SurfaceChatGPTEnterprise), srv.URL, "k", "ws", "org")
	start, _ := time.Parse(time.RFC3339, may1)
	if _, err := p.PollWindow(context.Background(), start, start.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("PollWindow: %v", err)
	}
	if hits != 2 {
		t.Errorf("server hits = %d, want 2 (cursor not followed)", hits)
	}
	if c := metricCount(t, db, "a@x.com") + metricCount(t, db, "b@x.com"); c != 2 {
		t.Errorf("both pages' cost rows = %d, want 2", c)
	}
}

func metricCount(t *testing.T, db *sql.DB, userKey string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM codex_analytics_daily WHERE user_key=? AND metric=?`,
		userKey, MetricCost).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestUpsertIsIdempotent(t *testing.T) {
	body := `{"data":[{"start_time":"2026-05-01T00:00:00Z","user":{"email":"a@x.com"},"credits":7,"tokens":{}}],"has_more":false,"next_page":null}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	db := testDB(t)
	p, _ := NewPoller(db, string(SurfaceChatGPTEnterprise), srv.URL, "k", "ws", "org")
	start, _ := time.Parse(time.RFC3339, may1)
	for i := 0; i < 2; i++ {
		if _, err := p.PollWindow(context.Background(), start, start.AddDate(0, 0, 1)); err != nil {
			t.Fatalf("PollWindow %d: %v", i, err)
		}
	}
	if rc := rowCount(t, db); rc != 6 {
		t.Errorf("row count after 2 polls = %d, want 6 (upsert, not duplicate)", rc)
	}
	if v, _, _ := metricValue(t, db, SurfaceChatGPTEnterprise, "a@x.com", MetricCost); v != 7 {
		t.Errorf("cost = %v, want 7", v)
	}
}

func TestNewPollerErrors(t *testing.T) {
	db := testDB(t)
	if _, err := NewPoller(db, "nope", "", "k", "", "org"); err == nil {
		t.Error("expected error for unknown surface")
	}
	if _, err := NewPoller(db, string(SurfaceChatGPTEnterprise), "", "k", "", "org"); err == nil {
		t.Error("expected error: chatgpt surface needs workspace_id")
	}
	if _, err := NewPoller(db, string(SurfaceOpenAIOrg), "", "k", "", "org"); err != nil {
		t.Errorf("openai_org should not require workspace_id: %v", err)
	}
}

func TestResolveOrgUserID(t *testing.T) {
	db := testDB(t)
	seedMember(t, db, "u1", "dev@acme.com")
	ctx := context.Background()

	// ChatGPT: email → 1-step join (case-insensitive).
	if id, ok := ResolveOrgUserID(ctx, db, SurfaceChatGPTEnterprise, ActorUser, "DEV@acme.com", nil); !ok || id != "u1" {
		t.Errorf("chatgpt email join = %q/%v, want u1/true", id, ok)
	}
	// ChatGPT: workspace user id (no @) → unresolved.
	if _, ok := ResolveOrgUserID(ctx, db, SurfaceChatGPTEnterprise, ActorUser, "wsu_1", nil); ok {
		t.Error("workspace user id should not resolve")
	}
	// OpenAI-org: user_id → 2-step via Users map → email join.
	m := map[string]string{"user_a": "dev@acme.com"}
	if id, ok := ResolveOrgUserID(ctx, db, SurfaceOpenAIOrg, ActorUser, "user_a", m); !ok || id != "u1" {
		t.Errorf("openai 2-step = %q/%v, want u1/true", id, ok)
	}
	// OpenAI-org: user_id absent from the map → unresolved.
	if _, ok := ResolveOrgUserID(ctx, db, SurfaceOpenAIOrg, ActorUser, "user_x", m); ok {
		t.Error("unmapped user_id should not resolve")
	}
	// Non-user actors never resolve.
	if _, ok := ResolveOrgUserID(ctx, db, SurfaceOpenAIOrg, ActorWorkspace, orgAggregateKey, m); ok {
		t.Error("workspace actor should not resolve")
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
	t.Setenv(apiKeyEnv, "envkey")
	if k, err := ResolveKey(""); err != nil || k != "envkey" {
		t.Errorf("env key = %q/%v", k, err)
	}
	t.Setenv(apiKeyEnv, "")
	f := t.TempDir() + "/k"
	if err := os.WriteFile(f, []byte("  filekey\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if k, err := ResolveKey(f); err != nil || k != "filekey" {
		t.Errorf("file key = %q/%v", k, err)
	}
	if _, err := ResolveKey(""); err == nil {
		t.Error("expected error with no env and no file")
	}
}

func TestSchedulerDefaults(t *testing.T) {
	s := NewScheduler(&Poller{spec: surfaceRegistry[SurfaceOpenAIOrg]}, 0, 0, nil)
	if s.interval != 24*time.Hour {
		t.Errorf("interval = %v, want 24h", s.interval)
	}
	if s.lag != 13*time.Hour {
		t.Errorf("lag = %v, want 13h", s.lag)
	}
}
