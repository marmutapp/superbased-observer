package dashboard

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
)

func openForecastTestDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fc.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	return database, func() { database.Close() }
}

func newForecastTestEngine(t *testing.T) *cost.Engine {
	t.Helper()
	// Build a tiny pricing engine with two known SKUs the
	// forecaster can score against.
	//
	// **Units**: cost.Pricing stores rates as $ per MILLION tokens
	// — matches the production BakedInDefaults shape (e.g.
	// claude-opus-4-1.Input = 15 means $15 per 1M tokens). The
	// cost engine's per-token math (cost/engine.go:288-290)
	// divides by 1_000_000 internally; lookupRates does the same
	// when bridging to cachetrack.RatePair, which the forecaster
	// expects in per-token units. The test fixture had per-token
	// values pre-2026-06-10 — the off-by-1e6 silently held
	// because both ends of the bridge were wrong; the operator-
	// reported $39K switch-cost regression exposed the mismatch.
	// Fixture now matches production shape.
	engine := cost.NewEngine(config.IntelligenceConfig{})
	engine.Table().Merge(map[string]cost.Pricing{
		"claude-opus-4-8": {
			Input: 15, Output: 75,
			CacheRead: 1.5, CacheCreation: 18.75,
			FastMultiplier: 2.0,
		},
		"claude-haiku-4-5-20251001": {
			Input: 0.8, Output: 4,
			CacheRead: 0.08, CacheCreation: 1,
		},
	})
	return engine
}

func seedForecastSession(t *testing.T, database *sql.DB, sessionID, model string, prefixTokens int64, withFast, withGap bool) {
	t.Helper()
	ctx := context.Background()
	// Seed a project row; the sessions schema has project_id NOT NULL.
	var projectID int64
	err := database.QueryRowContext(ctx,
		`INSERT INTO projects (root_path, created_at) VALUES (?, '2026-06-09T00:00:00Z') RETURNING id`,
		"/tmp/fc-"+sessionID).Scan(&projectID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.ExecContext(ctx,
		`INSERT INTO sessions (id, tool, project_id, model, started_at)
		 VALUES (?, 'claude-code', ?, ?, '2026-06-09T00:00:00Z')`,
		sessionID, projectID, model)
	if err != nil {
		t.Fatal(err)
	}
	// Seed a live cache_entries row so PrefixTokens has a value.
	_, err = database.ExecContext(ctx,
		`INSERT INTO cache_entries
		   (model, cache_scope, session_id, prefix_hash, token_count, ttl_tier, tier,
		    created_at, last_refresh_at, expires_at, state)
		 VALUES (?, 'default', ?, 'pfx-1', ?, '5m', 'proxy',
		         '2026-06-09T00:00:00Z', '2026-06-09T00:00:00Z',
		         '2099-06-09T00:00:00Z', 'live')`,
		model, sessionID, prefixTokens)
	if err != nil {
		t.Fatal(err)
	}
	// Seed 3 turns with output tokens + suffix writes.
	baseTS := time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		ts := baseTS.Add(time.Duration(i) * time.Minute)
		if withGap && i == 1 {
			ts = baseTS.Add(20 * time.Minute) // 20-min gap before turn 1
		}
		fast := 0
		if withFast {
			fast = 1
		}
		_, err = database.ExecContext(ctx,
			`INSERT INTO api_turns
			   (session_id, timestamp, provider, model, input_tokens, output_tokens, fast)
			 VALUES (?, ?, 'anthropic', ?, 1000, 500, ?)`,
			sessionID, ts.Format(time.RFC3339Nano), model, fast)
		if err != nil {
			t.Fatal(err)
		}
		_, err = database.ExecContext(ctx,
			`INSERT INTO cache_events
			   (session_id, tier, timestamp, model, kind, cause, tokens_written)
			 VALUES (?, 'proxy', ?, ?, 'write', 'suffix_growth', 200)`,
			sessionID, ts.Format(time.RFC3339Nano), model)
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestHandleSessionCacheForecast_HappyPath seeds a session with
// opus + a 50K cached prefix and asks for haiku. Expects a 200
// with the headline forecast: positive savings/turn, non-zero
// break-even, no never-pays-off warning.
func TestHandleSessionCacheForecast_HappyPath(t *testing.T) {
	t.Parallel()
	database, cleanup := openForecastTestDB(t)
	defer cleanup()
	seedForecastSession(t, database, "sA", "claude-opus-4-8", 50000, false, false)

	srv := &Server{opts: Options{DB: database, CostEngine: newForecastTestEngine(t)}}
	req := httptest.NewRequest(http.MethodGet,
		"/api/session/sA/cache/forecast?model=claude-haiku-4-5-20251001", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp CacheForecastResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.CurrentModel != "claude-opus-4-8" || resp.CandidateModel != "claude-haiku-4-5-20251001" {
		t.Errorf("models = (%q,%q), want (opus,haiku)", resp.CurrentModel, resp.CandidateModel)
	}
	if resp.CurrentPrefixTokens != 50000 {
		t.Errorf("P = %d, want 50000", resp.CurrentPrefixTokens)
	}
	if resp.SavingsPerTurnUSD <= 0 {
		t.Errorf("savings/turn = %v, want > 0 (opus→haiku is cheaper)", resp.SavingsPerTurnUSD)
	}
	if resp.BreakEvenTurns <= 0 {
		t.Errorf("break_even = %d, want > 0", resp.BreakEvenTurns)
	}
	for _, w := range resp.Warnings {
		if w == "switch_never_pays_off" {
			t.Errorf("switch_never_pays_off must not fire on opus→haiku: %+v", resp.Warnings)
		}
	}
}

// TestHandleSessionCacheForecast_MissingModelParam pins the 400
// when ?model= is not provided.
func TestHandleSessionCacheForecast_MissingModelParam(t *testing.T) {
	t.Parallel()
	database, cleanup := openForecastTestDB(t)
	defer cleanup()
	seedForecastSession(t, database, "sA", "claude-opus-4-8", 50000, false, false)

	srv := &Server{opts: Options{DB: database, CostEngine: newForecastTestEngine(t)}}
	req := httptest.NewRequest(http.MethodGet, "/api/session/sA/cache/forecast", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (missing model)", rec.Code)
	}
}

// TestHandleSessionCacheForecast_UnknownCandidate pins the 400
// when the candidate model has no pricing entry.
func TestHandleSessionCacheForecast_UnknownCandidate(t *testing.T) {
	t.Parallel()
	database, cleanup := openForecastTestDB(t)
	defer cleanup()
	seedForecastSession(t, database, "sA", "claude-opus-4-8", 50000, false, false)

	srv := &Server{opts: Options{DB: database, CostEngine: newForecastTestEngine(t)}}
	req := httptest.NewRequest(http.MethodGet,
		"/api/session/sA/cache/forecast?model=made-up-model-999", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (unknown candidate)", rec.Code)
	}
}

// TestHandleSessionCacheForecast_FastDetectAndGap pins the
// secondary signals: when api_turns carries fast=1 in the recent
// window, CurrentFast = true; when there's a 20-min gap between
// turns, HasGapsOver5Min = true; both pulled through to the
// forecaster and surfaced in the warnings.
func TestHandleSessionCacheForecast_FastDetectAndGap(t *testing.T) {
	t.Parallel()
	database, cleanup := openForecastTestDB(t)
	defer cleanup()
	seedForecastSession(t, database, "sFast", "claude-opus-4-8", 50000, true, true)

	srv := &Server{opts: Options{DB: database, CostEngine: newForecastTestEngine(t)}}
	req := httptest.NewRequest(http.MethodGet,
		"/api/session/sFast/cache/forecast?model=claude-haiku-4-5-20251001", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp CacheForecastResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.CurrentFast {
		t.Errorf("CurrentFast = false; want true (fast=1 in recent api_turns)")
	}
	if !resp.HasGapsOver5Min {
		t.Errorf("HasGapsOver5Min = false; want true (20-min gap seeded)")
	}
	var sawFast, sawGap bool
	for _, w := range resp.Warnings {
		if w == "fast_mode_active" {
			sawFast = true
		}
		if w == "try_1h_tier" {
			sawGap = true
		}
	}
	if !sawFast {
		t.Errorf("fast_mode_active warning missing: %+v", resp.Warnings)
	}
	if !sawGap {
		t.Errorf("try_1h_tier warning missing: %+v", resp.Warnings)
	}
}

// TestHandleSessionCacheForecast_404OnUnknownSession pins the
// 404 path when the session id doesn't exist.
func TestHandleSessionCacheForecast_404OnUnknownSession(t *testing.T) {
	t.Parallel()
	database, cleanup := openForecastTestDB(t)
	defer cleanup()

	srv := &Server{opts: Options{DB: database, CostEngine: newForecastTestEngine(t)}}
	req := httptest.NewRequest(http.MethodGet,
		"/api/session/nope/cache/forecast?model=claude-haiku-4-5-20251001", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestLookupRates_PerTokenScale pins the unit conversion in
// lookupRates. cost.Pricing stores rates as $ per MILLION tokens
// (the production BakedInDefaults shape); cachetrack.RatePair
// expects per-token. Off-by-1e6 in this bridge produces dollar
// numbers ~1,000,000× too large in the forecast result — the
// operator-reported 2026-06-10 regression ($39K switch-cost on a
// $8.74 session) traced exactly here.
func TestLookupRates_PerTokenScale(t *testing.T) {
	t.Parallel()
	engine := newForecastTestEngine(t)
	got, ok := lookupRates(engine, "claude-opus-4-8")
	if !ok {
		t.Fatalf("lookupRates: opus not found")
	}
	// Fixture: opus Input=15 ($/M). Expected per-token: 15 / 1e6 = 1.5e-5.
	const eps = 1e-12
	checks := map[string]struct {
		got, want float64
	}{
		"Input":         {got.Input, 15.0 / 1_000_000},
		"Output":        {got.Output, 75.0 / 1_000_000},
		"CacheRead":     {got.CacheRead, 1.5 / 1_000_000},
		"CacheCreation": {got.CacheCreation, 18.75 / 1_000_000},
	}
	for field, c := range checks {
		if abs(c.got-c.want) > eps {
			t.Errorf("RatePair.%s = %v, want %v (per-token; off-by-1e6 means $/M leaked through)",
				field, c.got, c.want)
		}
	}
	// FastMultiplier is unitless — passes through as-is.
	if got.FastMultiplier != 2.0 {
		t.Errorf("FastMultiplier = %v, want 2.0 (unitless, no scaling)", got.FastMultiplier)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// TestHandleSessionCacheForecast_RealisticDollarMagnitudes pins
// that the round-trip from cost.Pricing through lookupRates +
// the forecaster lands in sane-magnitude dollar numbers. Seeds
// a session with a realistic 50K-token prefix on opus and asks
// for a haiku forecast; switch_cost should be sub-dollar (cold
// 50K-prefix write at haiku's $1/M creation rate ≈ $0.05).
// Pre-2026-06-10 fix this would have returned $50,000+ and the
// regression test catches it definitively.
func TestHandleSessionCacheForecast_RealisticDollarMagnitudes(t *testing.T) {
	t.Parallel()
	database, cleanup := openForecastTestDB(t)
	defer cleanup()
	engine := newForecastTestEngine(t)
	const sessionID = "sess-units"
	const prefixTokens = 50_000
	seedForecastSession(t, database, sessionID, "claude-opus-4-8", prefixTokens, false, false)

	srv := &Server{opts: Options{DB: database, CostEngine: engine}}
	req := httptest.NewRequest(http.MethodGet,
		"/api/session/"+sessionID+"/cache/forecast?model=claude-haiku-4-5-20251001", nil)
	rec := httptest.NewRecorder()
	srv.handleSessionCacheForecast(rec, req, sessionID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp CacheForecastResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// Switch cost = 50000 × haiku.CacheCreation ($1/M) / 1e6 = $0.05.
	const expectSwitchCost = 0.05
	if abs(resp.SwitchCostUSD-expectSwitchCost) > 1e-9 {
		t.Errorf("SwitchCostUSD = %v, want ~%v (off-by-1e6 regression)",
			resp.SwitchCostUSD, expectSwitchCost)
	}
	// Sanity cap: any dollar field above $10 on a 50K-token cold-
	// cache forecast indicates a units-leak regression. Catches
	// the prior $39K / $4M shape without pinning the exact
	// downstream value (S + O + T derived from session stats,
	// which the fixture controls but the next reviewer might not).
	const sanityCap = 10.0
	if resp.SwitchCostUSD > sanityCap {
		t.Errorf("SwitchCostUSD = %v exceeds sanity cap $%v — likely $/M units leaked into RatePair",
			resp.SwitchCostUSD, sanityCap)
	}
	if resp.PerTurnBeforeUSD > sanityCap {
		t.Errorf("PerTurnBeforeUSD = %v exceeds sanity cap $%v — units regression",
			resp.PerTurnBeforeUSD, sanityCap)
	}
}

// TestCandidateMinCacheable pins the per-family thresholds.
func TestCandidateMinCacheable(t *testing.T) {
	t.Parallel()
	cases := map[string]int64{
		"claude-haiku-4-5-20251001": 1024,
		"claude-sonnet-4-1":         4096,
		"claude-opus-4-8":           8192,
		"gpt-4o":                    0, // non-Anthropic
		"":                          0,
	}
	for model, want := range cases {
		if got := candidateMinCacheable(model); got != want {
			t.Errorf("candidateMinCacheable(%q) = %d, want %d", model, got, want)
		}
	}
}
