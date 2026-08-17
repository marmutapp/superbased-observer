package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// openStatusline is the statusline-endpoint sibling of analysis_test.go's
// openHeadline helper: fires the HTTP request through the real mux and
// decodes the JSON body into the wire struct.
func openStatusline(t *testing.T, server *Server, sessionID string) StatuslineResponse {
	t.Helper()
	url := "/api/statusline"
	if sessionID != "" {
		url += "?session_id=" + sessionID
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	server.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("GET %s: %d body=%s", url, rr.Code, rr.Body.String())
	}
	var got StatuslineResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// statuslineTestAnchor is the fixed instant every test in this file pins
// Server.now to (directly, or as the starting point of a mutable fake
// clock) instead of time.Now(). Midday UTC keeps "anchor minus a few
// hours" landing in the same UTC calendar day and "anchor minus 36h/72h"
// landing clearly outside it, regardless of the wall-clock time the
// suite actually runs at.
//
// This replaces the prior time.Now()-based seeding, which failed
// deterministically in the ~1-2h after UTC midnight: rows seeded at
// time.Now().Add(-time.Hour) fall into YESTERDAY once now() itself is
// past midnight by less than an hour, while the handler still buckets
// "today" from s.now()'s UTC calendar day (dashboard.go's dayStart) —
// so today_usd computed 0 against an assertion expecting a nonzero sum.
// CI run 31228313133 hit exactly this at 00:20 UTC.
var statuslineTestAnchor = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

// newStatuslineFixture opens a fresh scratch DB + store + a seed project
// (via one Ingest call, matching the analysis_test.go convention so the
// project/session FK rows exist before InsertAPITurn/InsertTokenEvents).
// The returned Server has its now() seam pinned to statuslineTestAnchor;
// tests that need the clock to actually advance (the TTL cache tests)
// override server.now with their own mutable closure afterward.
func newStatuslineFixture(t *testing.T) (*store.Store, *Server) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := openTestDB(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)
	root := t.TempDir()
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{{
		SourceFile: "f", SourceEventID: "e1", SessionID: "sSeed",
		ProjectRoot: root, Timestamp: statuslineTestAnchor.Add(-time.Hour),
		Tool: models.ToolClaudeCode, ActionType: models.ActionReadFile,
		Target: "a.go", Success: true,
	}}, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}
	server, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	server.now = func() time.Time { return statuslineTestAnchor }
	return st, server
}

// TestHandleStatuslineTile_EmptyDB pins the fresh-install shape: zero
// today total, both session pointers nil (no session_id was supplied),
// a well-formed generated_at.
func TestHandleStatuslineTile_EmptyDB(t *testing.T) {
	_, server := newStatuslineFixture(t)
	got := openStatusline(t, server, "")
	if got.TodayUSD != 0 {
		t.Errorf("today_usd: got %v want 0", got.TodayUSD)
	}
	if got.SessionUSD != nil || got.SessionCacheReadShare != nil {
		t.Errorf("session fields must be nil with no session_id: usd=%v share=%v",
			got.SessionUSD, got.SessionCacheReadShare)
	}
	if _, err := time.Parse(time.RFC3339, got.GeneratedAt); err != nil {
		t.Errorf("generated_at not RFC3339: %q (%v)", got.GeneratedAt, err)
	}
}

// TestHandleStatuslineTile_TodayTotal_RecordedCost pins the simple case:
// a proxy-recorded api_turns row today sums straight into today_usd.
func TestHandleStatuslineTile_TodayTotal_RecordedCost(t *testing.T) {
	st, server := newStatuslineFixture(t)
	now := statuslineTestAnchor
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sSeed", Provider: models.ProviderAnthropic,
		Model: "claude-sonnet-4-6", InputTokens: 50_000, OutputTokens: 10_000,
		Timestamp: now.Add(-time.Hour), RequestID: "msg_today_1",
		CostUSD: 1.23,
	}); err != nil {
		t.Fatal(err)
	}
	got := openStatusline(t, server, "")
	if !approx(got.TodayUSD, 1.23) {
		t.Errorf("today_usd: got %v want 1.23", got.TodayUSD)
	}
}

// TestHandleStatuslineTile_TodayTotal_UnrecordedPricedByEngine is the
// WP0-correction pin: a token_usage row with EstimatedCostUSD unset
// (0 — the real-world shape for major sources, since claude-code prices
// at query time, never at ingest) must still contribute a nonzero
// today_usd once priced through the cost engine. Before the fix
// (SUM(estimated_cost_usd)) this would measure $0.0 exactly like WP0's
// live-corpus finding over 1,151 real rows.
func TestHandleStatuslineTile_TodayTotal_UnrecordedPricedByEngine(t *testing.T) {
	st, server := newStatuslineFixture(t)
	now := statuslineTestAnchor
	if _, err := st.InsertTokenEvents(context.Background(), []models.TokenEvent{{
		SourceFile: "f.jsonl", SourceEventID: "req_unrecorded_1",
		SessionID: "sSeed", ProjectRoot: "", Timestamp: now.Add(-time.Hour),
		Tool: models.ToolClaudeCode, Model: "claude-sonnet-4-6",
		InputTokens: 50_000, OutputTokens: 10_000,
		Source: models.TokenSourceJSONL, Reliability: models.ReliabilityApproximate,
		EstimatedCostUSD: 0, // the WP0 reality: unpopulated for major sources
	}}); err != nil {
		t.Fatal(err)
	}
	got := openStatusline(t, server, "")
	// Sonnet 4.6 standard: input $3/M, output $15/M.
	// 50K × $3 + 10K × $15 / 1M = 0.15 + 0.15 = 0.30
	const want = 0.30
	if got.TodayUSD <= 0 {
		t.Fatalf("today_usd must be > 0 for an unrecorded-but-priceable row (WP0 correction): got %v", got.TodayUSD)
	}
	if !approx(got.TodayUSD, want) {
		t.Errorf("today_usd: got %v want %v", got.TodayUSD, want)
	}
}

// TestHandleStatuslineTile_ExcludesYesterday confirms the one-day
// window actually excludes a turn from outside it — the whole reason
// this endpoint doesn't just SUM everything.
func TestHandleStatuslineTile_ExcludesYesterday(t *testing.T) {
	st, server := newStatuslineFixture(t)
	now := statuslineTestAnchor
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sSeed", Provider: models.ProviderAnthropic,
		Model: "claude-sonnet-4-6", InputTokens: 50_000, OutputTokens: 10_000,
		Timestamp: now.Add(-36 * time.Hour), RequestID: "msg_yesterday",
		CostUSD: 9.99,
	}); err != nil {
		t.Fatal(err)
	}
	got := openStatusline(t, server, "")
	if got.TodayUSD != 0 {
		t.Errorf("today_usd must exclude a turn from >24h ago: got %v", got.TodayUSD)
	}
}

// TestHandleStatuslineTile_TodayDedupesProxyRecordedTokenUsageRow pins
// the proxy_turn_ids exclusion trick borrowed from
// handleAnalysisHeadline: a token_usage row whose source_event_id
// matches an api_turns.request_id already counted must NOT be summed
// a second time.
func TestHandleStatuslineTile_TodayDedupesProxyRecordedTokenUsageRow(t *testing.T) {
	st, server := newStatuslineFixture(t)
	now := statuslineTestAnchor
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sSeed", Provider: models.ProviderAnthropic,
		Model: "claude-sonnet-4-6", InputTokens: 50_000, OutputTokens: 10_000,
		Timestamp: now.Add(-time.Hour), RequestID: "msg_dup_1",
		CostUSD: 1.23,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertTokenEvents(context.Background(), []models.TokenEvent{{
		SourceFile: "f.jsonl", SourceEventID: "msg_dup_1",
		SessionID: "sSeed", Timestamp: now.Add(-time.Hour),
		Tool: models.ToolClaudeCode, Model: "claude-sonnet-4-6",
		InputTokens: 50_000, OutputTokens: 10_000,
		Source: models.TokenSourceJSONL, Reliability: models.ReliabilityApproximate,
		EstimatedCostUSD: 1.23,
	}}); err != nil {
		t.Fatal(err)
	}
	got := openStatusline(t, server, "")
	if !approx(got.TodayUSD, 1.23) {
		t.Errorf("today_usd must dedup the JSONL row already covered by the proxy turn: got %v want 1.23", got.TodayUSD)
	}
}

// TestHandleStatuslineTile_FastTierUnrecordedPricing mirrors analysis_
// test.go's F6 fast-tier pin (TestAnalysisHeadline_CacheSavingsFastTier
// PricedAtFastRate): an unrecorded row's Fast flag must be threaded
// through to cost.Compute, not silently dropped (which would price at
// standard, non-fast rates).
func TestHandleStatuslineTile_FastTierUnrecordedPricing(t *testing.T) {
	st, server := newStatuslineFixture(t)
	now := statuslineTestAnchor
	if _, err := st.InsertTokenEvents(context.Background(), []models.TokenEvent{{
		SourceFile: "f.jsonl", SourceEventID: "req_fast_1",
		SessionID: "sSeed", Timestamp: now.Add(-time.Hour),
		Tool: models.ToolClaudeCode, Model: "claude-opus-4-8",
		CacheReadTokens: 100_000, Fast: true,
		Source: models.TokenSourceJSONL, Reliability: models.ReliabilityApproximate,
		EstimatedCostUSD: 0,
	}}); err != nil {
		t.Fatal(err)
	}
	got := openStatusline(t, server, "")
	// Opus 4.8 cache_read $0.50/M × fast(2) × 100K = 0.10.
	const want = 0.10
	if !approx(got.TodayUSD, want) {
		t.Errorf("today_usd (fast-tier): got %v want %v (bundle.Fast may not have been threaded through)", got.TodayUSD, want)
	}
}

// TestHandleStatuslineTile_SessionScoped_MatchingSession pins the
// all-time (not day-windowed) session query plus the cache-read-share
// computation. Session's rows are intentionally OUTSIDE today's window
// to prove session_usd doesn't inherit the day filter.
func TestHandleStatuslineTile_SessionScoped_MatchingSession(t *testing.T) {
	st, server := newStatuslineFixture(t)
	now := statuslineTestAnchor
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sScoped", Provider: models.ProviderAnthropic,
		Model:       "claude-sonnet-4-6",
		InputTokens: 25_000, CacheReadTokens: 75_000, OutputTokens: 10_000,
		Timestamp: now.Add(-72 * time.Hour), RequestID: "msg_scoped_1",
		CostUSD: 2.00,
	}); err != nil {
		t.Fatal(err)
	}
	got := openStatusline(t, server, "sScoped")
	if got.SessionUSD == nil {
		t.Fatal("session_usd must be non-nil for a known session_id")
	}
	if !approx(*got.SessionUSD, 2.00) {
		t.Errorf("session_usd: got %v want 2.00", *got.SessionUSD)
	}
	if got.SessionCacheReadShare == nil {
		t.Fatal("session_cache_read_share must be non-nil for a known session_id")
	}
	// prompt window = 25K + 75K = 100K; cache_read share = 75K/100K = 0.75
	const wantShare = 0.75
	if !approx(*got.SessionCacheReadShare, wantShare) {
		t.Errorf("session_cache_read_share: got %v want %v", *got.SessionCacheReadShare, wantShare)
	}
	// today_usd must be unaffected — the session's only turn is 72h old.
	if got.TodayUSD != 0 {
		t.Errorf("today_usd must stay 0 (session turn is outside today): got %v", got.TodayUSD)
	}
}

// TestHandleStatuslineTile_SessionScoped_UnknownSession pins the
// null-not-zero contract: an unrecognized session_id must leave both
// session pointers nil rather than rendering a fabricated 0.
func TestHandleStatuslineTile_SessionScoped_UnknownSession(t *testing.T) {
	_, server := newStatuslineFixture(t)
	got := openStatusline(t, server, "does-not-exist")
	if got.SessionUSD != nil {
		t.Errorf("session_usd must be nil for an unknown session_id: got %v", *got.SessionUSD)
	}
	if got.SessionCacheReadShare != nil {
		t.Errorf("session_cache_read_share must be nil for an unknown session_id: got %v", *got.SessionCacheReadShare)
	}
}

// TestHandleStatuslineTile_ResponseShapeStable pins the exact field
// names + presence the CLI's DaemonTile will mirror (plan §2's wire
// contract). Decodes into a raw map so an accidental rename or a
// dropped/added field is caught even if the typed struct in this file
// were edited to match a bug.
func TestHandleStatuslineTile_ResponseShapeStable(t *testing.T) {
	_, server := newStatuslineFixture(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/statusline", nil)
	server.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("GET /api/statusline: %d body=%s", rr.Code, rr.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantKeys := []string{"today_usd", "session_usd", "session_cache_read_share", "generated_at"}
	if len(raw) != len(wantKeys) {
		t.Errorf("response has %d keys, want exactly %d: %v", len(raw), len(wantKeys), raw)
	}
	for _, k := range wantKeys {
		if _, ok := raw[k]; !ok {
			t.Errorf("response missing key %q: %v", k, raw)
		}
	}
	if v, ok := raw["session_usd"]; !ok || v != nil {
		t.Errorf(`session_usd must serialize as JSON null (not omitted, not 0) when no session_id given: got %v`, v)
	}
	if v, ok := raw["session_cache_read_share"]; !ok || v != nil {
		t.Errorf(`session_cache_read_share must serialize as JSON null when no session_id given: got %v`, v)
	}
}

// withStatuslineTTL overrides the package-level statuslineTTL for the
// duration of one test (or benchmark), restoring the previous value via
// tb.Cleanup. statuslineTTL is package-scoped rather than a Server field
// because this F6 fix's file set is restricted to
// statusline.go/statusline_test.go (see statuslineTTL's doc comment) —
// this helper is the seam tests use in its place.
func withStatuslineTTL(tb testing.TB, ttl time.Duration) {
	tb.Helper()
	old := statuslineTTL
	statuslineTTL = ttl
	tb.Cleanup(func() { statuslineTTL = old })
}

// TestHandleStatuslineTile_CacheHitWithinTTL pins the F6 caching fix: a
// second request for the same (day-window, session_id) key made within
// the TTL window must return the response computed by the FIRST request,
// even though the underlying data changed in between — proving the
// handler served from cache rather than re-running the query.
func TestHandleStatuslineTile_CacheHitWithinTTL(t *testing.T) {
	st, server := newStatuslineFixture(t)
	// The cache check is `now.Before(entry.expiresAt)` — a mutable pinned
	// clock (rather than newStatuslineFixture's frozen default) so this
	// test's intent (two calls land inside the same TTL window) holds
	// regardless of how long the test actually takes to run, not just
	// because the wall clock happened to advance by less than the 2s
	// default TTL.
	current := statuslineTestAnchor
	server.now = func() time.Time { return current }
	now := current
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sSeed", Provider: models.ProviderAnthropic,
		Model: "claude-sonnet-4-6", InputTokens: 50_000, OutputTokens: 10_000,
		Timestamp: now.Add(-time.Hour), RequestID: "msg_cachehit_1",
		CostUSD: 1.00,
	}); err != nil {
		t.Fatal(err)
	}
	first := openStatusline(t, server, "")
	if !approx(first.TodayUSD, 1.00) {
		t.Fatalf("first call today_usd: got %v want 1.00", first.TodayUSD)
	}

	// A second turn lands inside the same 2s TTL window. If the handler
	// were re-querying instead of serving from cache, today_usd would
	// jump to 4.00 on the very next call.
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sSeed", Provider: models.ProviderAnthropic,
		Model: "claude-sonnet-4-6", InputTokens: 50_000, OutputTokens: 10_000,
		Timestamp: now.Add(-time.Hour), RequestID: "msg_cachehit_2",
		CostUSD: 3.00,
	}); err != nil {
		t.Fatal(err)
	}
	second := openStatusline(t, server, "")
	if !approx(second.TodayUSD, first.TodayUSD) {
		t.Errorf("cache hit within TTL: today_usd changed (got %v, want unchanged %v) — handler must have re-queried instead of serving the cached response", second.TodayUSD, first.TodayUSD)
	}
	if second.GeneratedAt != first.GeneratedAt {
		t.Errorf("cache hit within TTL: generated_at must stay the ORIGINAL compute time (the staleness signal), got %q want %q", second.GeneratedAt, first.GeneratedAt)
	}
}

// TestHandleStatuslineTile_CacheExpiresAndRecomputes pins the other half
// of the F6 fix: once the TTL elapses, the next request must recompute
// and pick up data that landed while the stale entry was cached. Uses a
// tiny injected TTL (via withStatuslineTTL) so the test doesn't have to
// sleep out the real 2s default.
func TestHandleStatuslineTile_CacheExpiresAndRecomputes(t *testing.T) {
	withStatuslineTTL(t, 10*time.Millisecond)
	st, server := newStatuslineFixture(t)
	// Mutable pinned clock (see TestHandleStatuslineTile_CacheHitWithinTTL):
	// the cache check is `now.Before(entry.expiresAt)`, computed entirely
	// from server.now(). Advancing the fake clock past the TTL — rather
	// than sleeping and relying on the real wall clock advancing under
	// newStatuslineFixture's frozen default — is what actually exercises
	// expiry deterministically.
	current := statuslineTestAnchor
	server.now = func() time.Time { return current }
	now := current
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sSeed", Provider: models.ProviderAnthropic,
		Model: "claude-sonnet-4-6", InputTokens: 50_000, OutputTokens: 10_000,
		Timestamp: now.Add(-time.Hour), RequestID: "msg_cacheexp_1",
		CostUSD: 1.00,
	}); err != nil {
		t.Fatal(err)
	}
	first := openStatusline(t, server, "")
	if !approx(first.TodayUSD, 1.00) {
		t.Fatalf("first call today_usd: got %v want 1.00", first.TodayUSD)
	}

	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sSeed", Provider: models.ProviderAnthropic,
		Model: "claude-sonnet-4-6", InputTokens: 50_000, OutputTokens: 10_000,
		Timestamp: now.Add(-time.Hour), RequestID: "msg_cacheexp_2",
		CostUSD: 3.00,
	}); err != nil {
		t.Fatal(err)
	}
	current = current.Add(20 * time.Millisecond) // well past the 10ms injected TTL
	second := openStatusline(t, server, "")
	if !approx(second.TodayUSD, 4.00) {
		t.Errorf("after TTL expiry today_usd must reflect the new row: got %v want 4.00", second.TodayUSD)
	}
}

// TestHandleStatuslineTile_DatedPricingLadder pins the same recorded →
// dated → undated ladder in statuslineQueryRows, including its perf
// gate (dateAware := engine.HasDatedPricing()): a recorded turn wins
// verbatim, and an unrecorded turn before the synthetic boundary must
// price at the OLD dated rate — which only happens if HasDatedPricing()
// actually reports true and the per-row timestamp gets parsed, proving
// the gate engages rather than silently short-circuiting to the
// current-rate fallback.
func TestHandleStatuslineTile_DatedPricingLadder(t *testing.T) {
	withStatuslineTTL(t, 0)
	st, server := newStatuslineFixture(t)
	now := statuslineTestAnchor
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	// statuslineTestAnchor is fixed at midday UTC, so hourOfDay is always
	// 12h — both rows and the boundary land safely inside "today" without
	// the real-clock near-midnight skip this test used to need.
	hourOfDay := now.Sub(dayStart)
	boundary := dayStart.Add(hourOfDay / 2)

	engine := newLadderTestEngine(t, boundary)
	server.opts.CostEngine = engine

	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sSeed", Provider: models.ProviderAnthropic,
		Model: ladderModel, InputTokens: ladderBundle.Input, OutputTokens: ladderBundle.Output,
		Timestamp: boundary.Add(30 * time.Minute), RequestID: "msg_ladder_recorded",
		CostUSD: 9.99, // recorded — must win verbatim
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sSeed", Provider: models.ProviderAnthropic,
		Model: ladderModel, InputTokens: ladderBundle.Input, OutputTokens: ladderBundle.Output,
		Timestamp: boundary.Add(-30 * time.Minute), RequestID: "msg_ladder_dated",
		CostUSD: 0, // unrecorded, before boundary — must price at the OLD dated rate
	}); err != nil {
		t.Fatal(err)
	}

	if !engine.HasDatedPricing() {
		t.Fatal("test fixture engine must report HasDatedPricing() == true")
	}

	got := openStatusline(t, server, "")
	want := 9.99 + ladderOldCost
	if !approx(got.TodayUSD, want) {
		t.Errorf("today_usd: got %v want %v (recorded=9.99 old=%.2f)", got.TodayUSD, want, ladderOldCost)
	}
}

// BenchmarkHandleStatuslineTile is the query-cost regression guard the
// plan requires (§7 / WP2 AC): a seeded, representative fixture (500
// today rows spread across api_turns + token_usage, plus 200 rows for
// the benchmarked session scattered across a week) exercised through
// the real HTTP handler on every iteration. If a future change grows
// this handler toward handleAnalysisHeadline's breadth (month
// projection, LC decomposition, top-model, ...) this number should
// jump sharply — that's the signal to look here first.
//
// Documented cost bound: this handler must stay well under the <100ms
// end-to-end statusline budget (plan §2.3) on its own — most of that
// budget is reserved for the loopback round trip + CLI startup, not
// server-side query time. ns/op here should read in the sub-millisecond
// to low-single-digit-millisecond range on a seeded fixture this size;
// see the reported ns/op in the benchmark tail for the measured figure.
func BenchmarkHandleStatuslineTile(b *testing.B) {
	path := filepath.Join(b.TempDir(), "d.db")
	database, err := openTestDB(context.Background(), db.Options{Path: path})
	if err != nil {
		b.Fatal(err)
	}
	defer database.Close()
	st := store.New(database)
	root := b.TempDir()
	const benchSession = "sBenchTarget"
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{{
		SourceFile: "f", SourceEventID: "e1", SessionID: "sBenchSeed",
		ProjectRoot: root, Timestamp: time.Now().UTC().Add(-time.Hour),
		Tool: models.ToolClaudeCode, ActionType: models.ActionReadFile,
		Target: "a.go", Success: true,
	}, {
		// token_usage.session_id carries a REAL FOREIGN KEY (unlike
		// api_turns.session_id, which is a bare TEXT column) — the
		// benchmarked session must exist in `sessions` before
		// InsertTokenEvents below will accept rows against it.
		SourceFile: "f", SourceEventID: "e2", SessionID: benchSession,
		ProjectRoot: root, Timestamp: time.Now().UTC().Add(-time.Hour),
		Tool: models.ToolClaudeCode, ActionType: models.ActionReadFile,
		Target: "a.go", Success: true,
	}}, nil, store.IngestOptions{}); err != nil {
		b.Fatal(err)
	}

	now := time.Now().UTC()
	for i := 0; i < 500; i++ {
		if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
			SessionID: "sBenchOther", Provider: models.ProviderAnthropic,
			Model: "claude-sonnet-4-6", InputTokens: 1_000, OutputTokens: 200,
			Timestamp: now.Add(-time.Duration(i) * time.Minute),
			RequestID: "bench_at_" + itoa(i),
		}); err != nil {
			b.Fatal(err)
		}
	}
	for i := 0; i < 200; i++ {
		if _, err := st.InsertTokenEvents(context.Background(), []models.TokenEvent{{
			SourceFile: "bench.jsonl", SourceEventID: "bench_tu_" + itoa(i),
			SessionID: benchSession, Timestamp: now.Add(-time.Duration(i) * time.Hour),
			Tool: models.ToolClaudeCode, Model: "claude-sonnet-4-6",
			InputTokens: 2_000, OutputTokens: 400,
			Source: models.TokenSourceJSONL, Reliability: models.ReliabilityApproximate,
		}}); err != nil {
			b.Fatal(err)
		}
	}

	server, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		b.Fatal(err)
	}
	url := "/api/statusline?session_id=" + benchSession

	// F6 caching fix: every iteration below hits the exact same
	// (day-window, session_id) cache key, so with caching left on this
	// benchmark would measure one real query plus (b.N - 1) cache hits —
	// defeating its purpose as a per-call query-cost regression guard.
	// Disable the cache for the duration of this benchmark so it keeps
	// measuring the uncached cost.
	withStatuslineTTL(b, 0)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, url, nil)
		server.Handler().ServeHTTP(rr, req)
		if rr.Code != 200 {
			b.Fatalf("GET %s: %d body=%s", url, rr.Code, rr.Body.String())
		}
	}
}
