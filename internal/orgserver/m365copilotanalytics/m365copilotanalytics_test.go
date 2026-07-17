package m365copilotanalytics

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

// metricValue reads one stored daily metric.
func metricValue(t *testing.T, db *sql.DB, surface Surface, userKey, appClass, metric string) (float64, string, string) {
	t.Helper()
	var v float64
	var unit, actor sql.NullString
	err := db.QueryRow(
		`SELECT value, unit, actor_type FROM m365_copilot_analytics_daily
		   WHERE surface=? AND user_key=? AND app_class=? AND metric=?`,
		string(surface), userKey, appClass, metric,
	).Scan(&v, &unit, &actor)
	if err != nil {
		t.Fatalf("metricValue(%s,%s,%s,%s): %v", surface, userKey, appClass, metric, err)
	}
	return v, unit.String, actor.String
}

func dailyRowCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM m365_copilot_analytics_daily`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func contentRowCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM m365_copilot_content`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

const may1 = "2026-05-01T00:00:00Z"

// graphPageBody is a getAllEnterpriseInteractions page: two BizChat interactions
// (a prompt + a response) and one Word prompt, all on 2026-05-01.
const graphPageBody = `{
  "value": [
    {"id":"i1","sessionId":"s1","requestId":"r1","appClass":"IPM.SkypeTeams.Message.Copilot.BizChat",
     "interactionType":"userPrompt","createdDateTime":"2026-05-01T09:00:00Z",
     "from":{"user":{"id":"u1","displayName":"Dev","userIdentityType":"aadUser"}},
     "body":{"contentType":"text","content":"summarize the doc"},
     "attachments":[{"a":1}]},
    {"id":"i2","sessionId":"s1","requestId":"r1","appClass":"IPM.SkypeTeams.Message.Copilot.BizChat",
     "interactionType":"aiResponse","createdDateTime":"2026-05-01T09:00:02Z",
     "from":{"user":{"id":"copilot","userIdentityType":"aadUser"}},
     "body":{"contentType":"text","content":"here is the summary"}},
    {"id":"i3","sessionId":"s2","appClass":"Microsoft.Office.Word",
     "interactionType":"userPrompt","createdDateTime":"2026-05-01T10:00:00Z",
     "from":{"user":{"id":"u1","userIdentityType":"aadUser"}},
     "body":{"contentType":"text","content":"rewrite this paragraph"}}
  ]
}`

func newGraphServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok123" {
			t.Errorf("auth header = %q, want Bearer tok123", got)
		}
		_, _ = w.Write([]byte(body))
	}))
}

func TestPollGraph_MetricsAndContent(t *testing.T) {
	srv := newGraphServer(t, graphPageBody)
	defer srv.Close()

	db := testDB(t)
	p, err := NewPoller(db, string(SurfaceGraph), srv.URL, StaticToken("tok123"), "tenant-1", "org_1")
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	p.UserIDs = []string{"dev@acme.com"}

	start, _ := time.Parse(time.RFC3339, may1)
	n, err := p.PollWindow(context.Background(), start, start.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("PollWindow: %v", err)
	}
	// 2 appClass buckets (bizchat, word) × 4 metrics = 8 daily rows.
	if n != 8 {
		t.Fatalf("daily rows = %d, want 8", n)
	}

	// BizChat: 2 interactions, 1 prompt, 1 response, 1 attachment.
	if v, u, a := metricValue(t, db, SurfaceGraph, "dev@acme.com", AppBizChat, MetricInteractions); v != 2 || u != string(UnitInteractions) || a != ActorUser {
		t.Errorf("bizchat interactions = %v/%s/%s, want 2/interactions/user", v, u, a)
	}
	if v, _, _ := metricValue(t, db, SurfaceGraph, "dev@acme.com", AppBizChat, MetricPrompts); v != 1 {
		t.Errorf("bizchat prompts = %v, want 1", v)
	}
	if v, _, _ := metricValue(t, db, SurfaceGraph, "dev@acme.com", AppBizChat, MetricResponses); v != 1 {
		t.Errorf("bizchat responses = %v, want 1", v)
	}
	if v, _, _ := metricValue(t, db, SurfaceGraph, "dev@acme.com", AppBizChat, MetricAttachments); v != 1 {
		t.Errorf("bizchat attachments = %v, want 1", v)
	}
	// Word: 1 interaction, 1 prompt.
	if v, _, _ := metricValue(t, db, SurfaceGraph, "dev@acme.com", AppWord, MetricPrompts); v != 1 {
		t.Errorf("word prompts = %v, want 1", v)
	}

	// Content table: 3 interaction rows, bodies stored (StoreContent default on).
	if c := contentRowCount(t, db); c != 3 {
		t.Fatalf("content rows = %d, want 3", c)
	}
	var content, hash string
	if err := db.QueryRow(`SELECT content, content_hash FROM m365_copilot_content WHERE interaction_id=?`, "i1").
		Scan(&content, &hash); err != nil {
		t.Fatalf("content scan: %v", err)
	}
	if content != "summarize the doc" {
		t.Errorf("content = %q, want the prompt body", content)
	}
	if hash != hashBody("summarize the doc") {
		t.Errorf("content_hash mismatch")
	}
}

func TestPollGraph_MetadataOnly_NullsContent(t *testing.T) {
	srv := newGraphServer(t, graphPageBody)
	defer srv.Close()

	db := testDB(t)
	p, _ := NewPoller(db, string(SurfaceGraph), srv.URL, StaticToken("tok123"), "tenant-1", "org_1")
	p.UserIDs = []string{"dev@acme.com"}
	p.StoreContent = false // metadata-only

	start, _ := time.Parse(time.RFC3339, may1)
	if _, err := p.PollWindow(context.Background(), start, start.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("PollWindow: %v", err)
	}

	// Content column NULL, hash still present.
	var content sql.NullString
	var hash string
	if err := db.QueryRow(`SELECT content, content_hash FROM m365_copilot_content WHERE interaction_id=?`, "i1").
		Scan(&content, &hash); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if content.Valid {
		t.Errorf("content = %q, want NULL in metadata-only mode", content.String)
	}
	if hash == "" {
		t.Errorf("content_hash empty; must be present even in metadata-only mode")
	}
}

func TestPollGraph_ScrubApplied(t *testing.T) {
	srv := newGraphServer(t, graphPageBody)
	defer srv.Close()

	db := testDB(t)
	p, _ := NewPoller(db, string(SurfaceGraph), srv.URL, StaticToken("tok123"), "tenant-1", "org_1")
	p.UserIDs = []string{"dev@acme.com"}
	p.Scrub = func(s string) string { return "[scrubbed]" }

	start, _ := time.Parse(time.RFC3339, may1)
	if _, err := p.PollWindow(context.Background(), start, start.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("PollWindow: %v", err)
	}
	var content, hash string
	if err := db.QueryRow(`SELECT content, content_hash FROM m365_copilot_content WHERE interaction_id=?`, "i1").
		Scan(&content, &hash); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if content != "[scrubbed]" || hash != hashBody("[scrubbed]") {
		t.Errorf("scrub not applied: content=%q hash=%q", content, hash)
	}
}

func TestPollGraph_FollowsNextLink(t *testing.T) {
	var hits int
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if strings.Contains(r.URL.RawQuery, "page2") {
			_, _ = w.Write([]byte(`{"value":[{"id":"i9","appClass":"bizchat","interactionType":"userPrompt","createdDateTime":"2026-05-01T11:00:00Z","from":{"user":{"userIdentityType":"aadUser"}},"body":{"content":"p2"}}]}`))
			return
		}
		// First page: one interaction + a nextLink pointing back to this server.
		_, _ = w.Write([]byte(`{"@odata.nextLink":"` + srv.URL + `/x?page2=1","value":[{"id":"i8","appClass":"bizchat","interactionType":"userPrompt","createdDateTime":"2026-05-01T10:00:00Z","from":{"user":{"userIdentityType":"aadUser"}},"body":{"content":"p1"}}]}`))
	}))
	defer srv.Close()

	db := testDB(t)
	p, _ := NewPoller(db, string(SurfaceGraph), srv.URL, StaticToken("tok123"), "t", "org")
	p.UserIDs = []string{"dev@acme.com"}
	start, _ := time.Parse(time.RFC3339, may1)
	if _, err := p.PollWindow(context.Background(), start, start.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("PollWindow: %v", err)
	}
	if hits != 2 {
		t.Errorf("server hits = %d, want 2 (nextLink not followed)", hits)
	}
	if c := contentRowCount(t, db); c != 2 {
		t.Errorf("content rows = %d, want 2 (both pages)", c)
	}
}

func TestPollGraph_ThrottleRetry(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(graphPageBody))
	}))
	defer srv.Close()

	db := testDB(t)
	p, _ := NewPoller(db, string(SurfaceGraph), srv.URL, StaticToken("tok123"), "t", "org")
	p.UserIDs = []string{"dev@acme.com"}
	start, _ := time.Parse(time.RFC3339, may1)
	if _, err := p.PollWindow(context.Background(), start, start.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("PollWindow after retry: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (retry once after 429)", calls)
	}
}

func TestPollGraph_Idempotent(t *testing.T) {
	srv := newGraphServer(t, graphPageBody)
	defer srv.Close()
	db := testDB(t)
	p, _ := NewPoller(db, string(SurfaceGraph), srv.URL, StaticToken("tok123"), "t", "org")
	p.UserIDs = []string{"dev@acme.com"}
	start, _ := time.Parse(time.RFC3339, may1)
	for i := 0; i < 2; i++ {
		if _, err := p.PollWindow(context.Background(), start, start.AddDate(0, 0, 1)); err != nil {
			t.Fatalf("PollWindow %d: %v", i, err)
		}
	}
	if r := dailyRowCount(t, db); r != 8 {
		t.Errorf("daily rows after 2 polls = %d, want 8 (upsert, not duplicate)", r)
	}
	if c := contentRowCount(t, db); c != 3 {
		t.Errorf("content rows after 2 polls = %d, want 3 (upsert)", c)
	}
}

func TestProbeGraph(t *testing.T) {
	// Success: server returns a page; probe counts the first-page interactions
	// without walking pagination or writing to a DB (nil DB is fine).
	srv := newGraphServer(t, graphPageBody)
	defer srv.Close()
	p, err := NewPoller(nil, string(SurfaceGraph), srv.URL, StaticToken("tok123"), "t", "")
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	start, _ := time.Parse(time.RFC3339, may1)
	n, err := p.ProbeGraph(context.Background(), "dev@acme.com", start, start.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("ProbeGraph: %v", err)
	}
	if n != 3 {
		t.Errorf("probe count = %d, want 3 (interactions on the first page)", n)
	}

	// Empty user is rejected before any request.
	if _, err := p.ProbeGraph(context.Background(), "  ", start, start.AddDate(0, 0, 1)); err == nil {
		t.Error("expected error for empty probe user")
	}

	// Graph 403 surfaces the status in the error (doctor maps it to a consent hint).
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer deny.Close()
	pd, _ := NewPoller(nil, string(SurfaceGraph), deny.URL, StaticToken("tok123"), "t", "")
	if _, err := pd.ProbeGraph(context.Background(), "dev@acme.com", start, start.AddDate(0, 0, 1)); err == nil ||
		!strings.Contains(err.Error(), "403") {
		t.Errorf("probe against 403 = %v, want an error mentioning 403", err)
	}
}

func TestTokenSource_ClientCredentials(t *testing.T) {
	var gotGrant, gotScope string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotGrant = r.PostFormValue("grant_type")
		gotScope = r.PostFormValue("scope")
		if !strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token") {
			t.Errorf("token path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"token_type":"Bearer","expires_in":3600,"access_token":"abc.def"}`))
	}))
	defer srv.Close()

	ts := NewTokenSource("tenant-x", "client-y", "secret-z", srv.URL, srv.Client())
	tok, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "abc.def" {
		t.Errorf("token = %q, want abc.def", tok)
	}
	if gotGrant != "client_credentials" {
		t.Errorf("grant_type = %q, want client_credentials", gotGrant)
	}
	if gotScope != graphScope {
		t.Errorf("scope = %q, want %q", gotScope, graphScope)
	}

	// Second call is served from cache (no new HTTP call needed for correctness;
	// just assert it still returns the token).
	if tok2, err := ts.Token(context.Background()); err != nil || tok2 != "abc.def" {
		t.Errorf("cached Token = %q/%v", tok2, err)
	}
}

func TestTokenSource_ErrorHidesSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"bad secret"}`))
	}))
	defer srv.Close()
	ts := NewTokenSource("t", "c", "super-secret-value", srv.URL, srv.Client())
	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if strings.Contains(err.Error(), "super-secret-value") {
		t.Errorf("error leaked the secret: %v", err)
	}
}

func TestParsePurviewRecord(t *testing.T) {
	rec := `{"Id":"a1","RecordType":261,"Workload":"Copilot","CreationTime":"2026-05-01T12:00:00Z",
	  "UserId":"dev@acme.com","CopilotEventData":{"AppHost":"Word","ThreadId":"th1",
	  "AccessedResources":[{"Id":"d1","Name":"plan.docx","SensitivityLabelId":"conf"},{"Id":"d2"}],
	  "AISystemPlugin":[{"Id":"BingWebSearch"}]}}`
	metrics, err := parsePurviewRecord([]byte(rec))
	if err != nil {
		t.Fatalf("parsePurviewRecord: %v", err)
	}
	var gov, accessed, grounded float64
	for _, m := range metrics {
		if m.Surface != SurfacePurview || m.AppClass != AppWord {
			t.Errorf("unexpected surface/appclass %s/%s", m.Surface, m.AppClass)
		}
		switch m.Metric {
		case MetricGovInteractions:
			gov = m.Value
		case MetricAccessedResources:
			accessed = m.Value
		case MetricGroundedInteractions:
			grounded = m.Value
		}
	}
	if gov != 1 || accessed != 2 || grounded != 1 {
		t.Errorf("gov=%v accessed=%v grounded=%v, want 1/2/1", gov, accessed, grounded)
	}

	// Non-Copilot record is skipped.
	if m, _ := parsePurviewRecord([]byte(`{"RecordType":1,"Workload":"Exchange"}`)); m != nil {
		t.Errorf("non-copilot record should yield no metrics, got %v", m)
	}
}

func TestPollPurview_ScaffoldNoop(t *testing.T) {
	db := testDB(t)
	p, err := NewPoller(db, string(SurfacePurview), "", StaticToken("tok"), "t", "org")
	if err != nil {
		t.Fatalf("NewPoller purview: %v", err)
	}
	start, _ := time.Parse(time.RFC3339, may1)
	n, err := p.PollWindow(context.Background(), start, start.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("purview poll: %v", err)
	}
	if n != 0 {
		t.Errorf("scaffolded purview poll wrote %d rows, want 0", n)
	}
}

func TestLoadUsageSummary(t *testing.T) {
	srv := newGraphServer(t, graphPageBody)
	defer srv.Close()
	db := testDB(t)
	p, _ := NewPoller(db, string(SurfaceGraph), srv.URL, StaticToken("tok123"), "t", "org_1")
	p.UserIDs = []string{"dev@acme.com"}
	start, _ := time.Parse(time.RFC3339, may1)
	if _, err := p.PollWindow(context.Background(), start, start.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("PollWindow: %v", err)
	}

	sum, err := LoadUsageSummary(context.Background(), db, "org_1")
	if err != nil {
		t.Fatalf("LoadUsageSummary: %v", err)
	}
	if sum.TotalPrompts != 2 || sum.TotalResp != 1 {
		t.Errorf("totals prompts=%v resp=%v, want 2/1", sum.TotalPrompts, sum.TotalResp)
	}
	if len(sum.ByAppClass) != 2 {
		t.Errorf("appclass buckets = %d, want 2", len(sum.ByAppClass))
	}
}

func TestLoadInteractionContent_AdminGate(t *testing.T) {
	srv := newGraphServer(t, graphPageBody)
	defer srv.Close()
	db := testDB(t)
	p, _ := NewPoller(db, string(SurfaceGraph), srv.URL, StaticToken("tok123"), "t", "org")
	p.UserIDs = []string{"dev@acme.com"}
	start, _ := time.Parse(time.RFC3339, may1)
	if _, err := p.PollWindow(context.Background(), start, start.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("PollWindow: %v", err)
	}

	// Non-admin scope: bodies withheld, hashes present.
	res, err := LoadInteractionContent(context.Background(), db, "s1", false)
	if err != nil {
		t.Fatalf("LoadInteractionContent(non-admin): %v", err)
	}
	if len(res.Entries) != 2 {
		t.Fatalf("session s1 entries = %d, want 2", len(res.Entries))
	}
	if res.ContentAvailable {
		t.Error("non-admin scope must NOT disclose content")
	}
	for _, e := range res.Entries {
		if e.Content != "" {
			t.Errorf("non-admin content leaked: %q", e.Content)
		}
		if e.ContentHash == "" {
			t.Error("content_hash should always be present")
		}
	}

	// Admin scope: bodies disclosed.
	res2, err := LoadInteractionContent(context.Background(), db, "s1", true)
	if err != nil {
		t.Fatalf("LoadInteractionContent(admin): %v", err)
	}
	if !res2.ContentAvailable {
		t.Error("admin scope should disclose content")
	}
	var gotPrompt bool
	for _, e := range res2.Entries {
		if e.Content == "summarize the doc" {
			gotPrompt = true
		}
	}
	if !gotPrompt {
		t.Error("admin scope did not return the prompt body")
	}
}

func TestResolveUserIDs(t *testing.T) {
	db := testDB(t)
	seedMember(t, db, "u1", "a@acme.com")
	seedMember(t, db, "u2", "b@acme.com")
	ids, err := ResolveUserIDs(context.Background(), db, "")
	if err != nil {
		t.Fatalf("ResolveUserIDs: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("resolved user ids = %v, want 2", ids)
	}
}

func TestResolveOrgUserID(t *testing.T) {
	db := testDB(t)
	seedMember(t, db, "u1", "dev@acme.com")
	ctx := context.Background()
	if id, ok := ResolveOrgUserID(ctx, db, ActorUser, "DEV@acme.com"); !ok || id != "u1" {
		t.Errorf("email join = %q/%v, want u1/true", id, ok)
	}
	if _, ok := ResolveOrgUserID(ctx, db, ActorUser, "bare-object-id"); ok {
		t.Error("bare object id (no @) should not resolve")
	}
	if _, ok := ResolveOrgUserID(ctx, db, ActorAutomation, "svc@acme.com"); ok {
		t.Error("non-user actor should not resolve")
	}
}

func seedMember(t *testing.T, db *sql.DB, userID, email string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO org_members (user_id, user_name, email, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		userID, userID, email, may1, may1,
	); err != nil {
		t.Fatalf("seed member: %v", err)
	}
}

func TestResolveClientSecret(t *testing.T) {
	t.Setenv(clientSecretEnv, "envsecret")
	if k, err := ResolveClientSecret("", nil); err != nil || k != "envsecret" {
		t.Errorf("env secret = %q/%v", k, err)
	}
	t.Setenv(clientSecretEnv, "")
	f := t.TempDir() + "/s"
	if err := os.WriteFile(f, []byte("  filesecret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if k, err := ResolveClientSecret(f, nil); err != nil || k != "filesecret" {
		t.Errorf("file secret = %q/%v", k, err)
	}
	if _, err := ResolveClientSecret("", nil); err == nil {
		t.Error("expected error with no env and no file")
	}
}

// bufLogger returns a slog logger writing to buf so tests can assert on WARN
// output.
func bufLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestResolveClientSecret_PermWarn(t *testing.T) {
	const secretVal = "top-secret-value"

	// (a) 0644 file → WARN fires, names the file, recommends chmod 600, and
	// never logs the secret contents.
	t.Setenv(clientSecretEnv, "")
	loose := t.TempDir() + "/loose"
	if err := os.WriteFile(loose, []byte(secretVal), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if k, err := ResolveClientSecret(loose, bufLogger(&buf)); err != nil || k != secretVal {
		t.Fatalf("resolve(0644) = %q/%v", k, err)
	}
	logged := buf.String()
	if !strings.Contains(logged, "group/world-readable") {
		t.Errorf("expected perm WARN for 0644, got: %q", logged)
	}
	if !strings.Contains(logged, loose) {
		t.Errorf("WARN should name the file %q, got: %q", loose, logged)
	}
	if !strings.Contains(logged, "chmod 600") {
		t.Errorf("WARN should recommend chmod 600, got: %q", logged)
	}
	if strings.Contains(logged, secretVal) {
		t.Errorf("WARN leaked the secret contents: %q", logged)
	}

	// (b) 0600 file → no WARN.
	tight := t.TempDir() + "/tight"
	if err := os.WriteFile(tight, []byte(secretVal), 0o600); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if _, err := ResolveClientSecret(tight, bufLogger(&buf)); err != nil {
		t.Fatalf("resolve(0600): %v", err)
	}
	if s := buf.String(); s != "" {
		t.Errorf("no WARN expected for 0600, got: %q", s)
	}

	// (c) env-sourced secret → env takes precedence, the file stat is never
	// reached, so no WARN even with a loose file argument present.
	t.Setenv(clientSecretEnv, secretVal)
	buf.Reset()
	if k, err := ResolveClientSecret(loose, bufLogger(&buf)); err != nil || k != secretVal {
		t.Fatalf("resolve(env) = %q/%v", k, err)
	}
	if s := buf.String(); s != "" {
		t.Errorf("no WARN expected for env-sourced secret, got: %q", s)
	}
}

func TestClassifyAppClass(t *testing.T) {
	cases := map[string]string{
		"IPM.SkypeTeams.Message.Copilot.BizChat": AppBizChat,
		"Microsoft.Office.Word":                  AppWord,
		"Microsoft.Office.Outlook":               AppOutlook,
		"Microsoft.Office.Excel":                 AppExcel,
		"something.Teams.here":                   AppTeams,
		"WebChat":                                AppWebChat,
		"":                                       AppOther,
		"totally-unknown":                        AppOther,
	}
	for raw, want := range cases {
		if got := classifyAppClass(raw); got != want {
			t.Errorf("classifyAppClass(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestNewPollerErrors(t *testing.T) {
	db := testDB(t)
	if _, err := NewPoller(db, "nope", "", StaticToken("k"), "t", "org"); err == nil {
		t.Error("expected error for unknown surface")
	}
	if _, err := NewPoller(db, string(SurfaceGraph), "", nil, "t", "org"); err == nil {
		t.Error("expected error for nil token provider")
	}
}

func TestSchedulerPollRecentAndRun(t *testing.T) {
	srv := newGraphServer(t, graphPageBody)
	defer srv.Close()
	db := testDB(t)
	seedMember(t, db, "u1", "dev@acme.com") // resolved as the poll user set

	p, _ := NewPoller(db, string(SurfaceGraph), srv.URL, StaticToken("tok123"), "t", "")
	s := NewScheduler(p, 1, 0, nil)
	// Freeze the clock so the trailing window covers 2026-05-01.
	fixed := time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fixed }

	s.pollRecent(context.Background())
	if r := dailyRowCount(t, db); r == 0 {
		t.Fatal("pollRecent wrote no rows; user-resolve → poll path not exercised")
	}

	// Run polls once immediately, then returns when ctx is already cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Run(ctx) // returns promptly (immediate poll + cancelled select)
}

func TestActorForIdentityAndDayFallback(t *testing.T) {
	if actorForIdentity("application") != ActorAutomation {
		t.Error("application identity should be automation")
	}
	if actorForIdentity("weird") != ActorUser {
		t.Error("unknown identity defaults to user")
	}
	// utcDayFromTimestamp date-only fallback branch.
	if d := utcDayFromTimestamp("2026-05-01"); d != "2026-05-01" {
		t.Errorf("date-only fallback = %q", d)
	}
	if d := utcDayFromTimestamp("bad"); d != "" {
		t.Errorf("unparseable ts = %q, want empty", d)
	}
}

func TestLoadUsageSummaryAllOrgs(t *testing.T) {
	db := testDB(t)
	// Empty store → orgPredicate("") branch, no rows.
	sum, err := LoadUsageSummary(context.Background(), db, "")
	if err != nil {
		t.Fatalf("LoadUsageSummary(all orgs): %v", err)
	}
	if len(sum.ByAppClass) != 0 {
		t.Errorf("empty store buckets = %d, want 0", len(sum.ByAppClass))
	}
}

func TestSchedulerDefaults(t *testing.T) {
	s := NewScheduler(&Poller{spec: surfaceRegistry[SurfaceGraph]}, 0, 0, nil)
	if s.interval != 24*time.Hour {
		t.Errorf("interval = %v, want 24h", s.interval)
	}
	if s.lag != 24*time.Hour {
		t.Errorf("lag = %v, want 24h", s.lag)
	}
	if s.Surface() != SurfaceGraph {
		t.Errorf("surface = %v, want graph", s.Surface())
	}
}
