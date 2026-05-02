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

// headlineResponse mirrors the JSON shape /api/analysis/headline returns.
// Kept private to the test so the public JSON contract can stay flexible
// without breaking other tests.
type headlineResponse struct {
	Days   int `json:"days"`
	Period struct {
		CostUSD      float64 `json:"cost_usd"`
		PriorCostUSD float64 `json:"prior_cost_usd"`
		PriorIsZero  bool    `json:"prior_is_zero"`
		DeltaPct     float64 `json:"delta_pct"`
	} `json:"period"`
	Month struct {
		ToDateUSD     float64 `json:"to_date_usd"`
		ProjectionUSD float64 `json:"projection_usd"`
		BudgetUSD     float64 `json:"budget_usd"`
		BudgetPct     float64 `json:"budget_pct"`
		DaysElapsed   int     `json:"days_elapsed"`
		DaysInMonth   int     `json:"days_in_month"`
	} `json:"month"`
	Effective struct {
		Tokens         int64   `json:"tokens"`
		RatePerMillion float64 `json:"rate_per_million"`
	} `json:"effective"`
	Cache struct {
		ReadTokens  int64   `json:"read_tokens"`
		WriteTokens int64   `json:"write_tokens"`
		Efficacy    float64 `json:"efficacy"`
	} `json:"cache"`
	LongContext struct {
		Turns        int64   `json:"turns"`
		SurchargeUSD float64 `json:"surcharge_usd"`
	} `json:"long_context"`
	Waste struct {
		USD                   float64 `json:"usd"`
		Tokens                int64   `json:"tokens"`
		BlendedRatePerMillion float64 `json:"blended_rate_per_million"`
	} `json:"waste"`
}

func openHeadline(t *testing.T, server *Server, days int) headlineResponse {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/analysis/headline?days="+itoa(days), nil)
	server.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("GET /api/analysis/headline: %d body=%s", rr.Code, rr.Body.String())
	}
	var got headlineResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestAnalysisHeadline_EmptyDB returns zeros for every metric without
// erroring. The handler must tolerate a fresh install with no api_turns
// or token_usage rows.
func TestAnalysisHeadline_EmptyDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	s, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	got := openHeadline(t, s, 30)
	if got.Period.CostUSD != 0 || got.Period.PriorCostUSD != 0 {
		t.Errorf("empty DB cost: %+v", got.Period)
	}
	if !got.Period.PriorIsZero {
		t.Errorf("empty DB should report prior_is_zero=true")
	}
	if got.LongContext.Turns != 0 || got.LongContext.SurchargeUSD != 0 {
		t.Errorf("empty DB LC: %+v", got.LongContext)
	}
	if got.Cache.Efficacy != 0 {
		t.Errorf("empty DB cache efficacy: %v", got.Cache.Efficacy)
	}
	if got.Days != 30 {
		t.Errorf("days echo: got %d want 30", got.Days)
	}
}

// TestAnalysisHeadline_PeriodVsPrior splits one batch of turns across
// the current and prior windows to verify period-over-period
// accounting. Each turn is small enough to stay below the LC threshold
// so cost computes at standard rates.
func TestAnalysisHeadline_PeriodVsPrior(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)
	root := t.TempDir()
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{{
		SourceFile: "f", SourceEventID: "e1", SessionID: "sX",
		ProjectRoot: root, Timestamp: time.Now().UTC().Add(-time.Hour),
		Tool: models.ToolClaudeCode, ActionType: models.ActionReadFile,
		Target: "a.go", Success: true,
	}}, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	// One turn 1 day ago (in current 7d window).
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sX", Provider: models.ProviderAnthropic,
		Model: "claude-sonnet-4-6", InputTokens: 50_000, OutputTokens: 10_000,
		Timestamp: now.Add(-24 * time.Hour), RequestID: "msg_p_1",
	}); err != nil {
		t.Fatal(err)
	}
	// Two turns 10 days ago (in prior 7d window: [-14, -7] days from now).
	for i := 0; i < 2; i++ {
		if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
			SessionID: "sX", Provider: models.ProviderAnthropic,
			Model: "claude-sonnet-4-6", InputTokens: 50_000, OutputTokens: 10_000,
			Timestamp: now.Add(-10 * 24 * time.Hour),
			RequestID: "msg_q_" + itoa(i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	server, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	got := openHeadline(t, server, 7)

	// Sonnet 4.6 standard: input $3/M, output $15/M. Each turn:
	//   50K × $3 + 10K × $15 / 1M = 0.15 + 0.15 = 0.30
	const perTurn = 0.30
	wantPeriod := perTurn    // 1 turn in current period
	wantPrior := perTurn * 2 // 2 turns in prior period
	if approx(got.Period.CostUSD, wantPeriod) != true {
		t.Errorf("period cost: got %v want %v", got.Period.CostUSD, wantPeriod)
	}
	if approx(got.Period.PriorCostUSD, wantPrior) != true {
		t.Errorf("prior cost: got %v want %v", got.Period.PriorCostUSD, wantPrior)
	}
	if got.Period.PriorIsZero {
		t.Errorf("prior_is_zero must be false when prior had turns")
	}
	// delta = (period - prior) / prior * 100 = (0.30 - 0.60) / 0.60 = -50%
	if got.Period.DeltaPct < -50.5 || got.Period.DeltaPct > -49.5 {
		t.Errorf("delta_pct: got %v want ≈ -50", got.Period.DeltaPct)
	}
}

// TestAnalysisHeadline_LCSurchargeAttribution pins LC dispatch in the
// headline. One Sonnet 4 turn at 100K (under 200K threshold, standard
// rates) and one at 250K (LC tier kicks in). The surcharge attribution
// = LC turn cost - what it would have cost at standard rates.
func TestAnalysisHeadline_LCSurchargeAttribution(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)
	root := t.TempDir()
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{{
		SourceFile: "f", SourceEventID: "e1", SessionID: "sLC",
		ProjectRoot: root, Timestamp: time.Now().UTC().Add(-time.Hour),
		Tool: models.ToolClaudeCode, ActionType: models.ActionReadFile,
		Target: "a.go", Success: true,
	}}, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	// Sub-threshold turn — should NOT add to surcharge.
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sLC", Provider: models.ProviderAnthropic,
		Model: "claude-sonnet-4-20250514", InputTokens: 100_000, OutputTokens: 5_000,
		Timestamp: now.Add(-2 * 24 * time.Hour), RequestID: "msg_lc_under",
	}); err != nil {
		t.Fatal(err)
	}
	// Over-threshold turn (250K) — LC pricing kicks in. Standard cost:
	//   250K × $3 + 5K × $15 / 1M = 0.75 + 0.075 = 0.825
	// LC cost: 250K × $6 + 5K × $22.50 / 1M = 1.50 + 0.1125 = 1.6125
	// Surcharge = 1.6125 - 0.825 = 0.7875
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sLC", Provider: models.ProviderAnthropic,
		Model: "claude-sonnet-4-20250514", InputTokens: 250_000, OutputTokens: 5_000,
		Timestamp: now.Add(-1 * 24 * time.Hour), RequestID: "msg_lc_over",
	}); err != nil {
		t.Fatal(err)
	}

	server, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	got := openHeadline(t, server, 7)
	if got.LongContext.Turns != 1 {
		t.Errorf("LC turns: got %d want 1 (one over-threshold turn)", got.LongContext.Turns)
	}
	const wantSurcharge = 0.7875
	if !approx(got.LongContext.SurchargeUSD, wantSurcharge) {
		t.Errorf("LC surcharge: got %v want %v", got.LongContext.SurchargeUSD, wantSurcharge)
	}
}

// TestAnalysisHeadline_CacheEfficacy verifies cache_read / (cache_read +
// cache_creation) over the current period.
func TestAnalysisHeadline_CacheEfficacy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)
	root := t.TempDir()
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{{
		SourceFile: "f", SourceEventID: "e1", SessionID: "sCache",
		ProjectRoot: root, Timestamp: time.Now().UTC().Add(-time.Hour),
		Tool: models.ToolClaudeCode, ActionType: models.ActionReadFile,
		Target: "a.go", Success: true,
	}}, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sCache", Provider: models.ProviderAnthropic,
		Model:           "claude-sonnet-4-6",
		CacheReadTokens: 800_000, CacheCreationTokens: 200_000,
		Timestamp: now.Add(-2 * 24 * time.Hour), RequestID: "msg_cache_1",
	}); err != nil {
		t.Fatal(err)
	}

	server, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	got := openHeadline(t, server, 7)
	// 800K / (800K + 200K) = 0.80
	if got.Cache.Efficacy < 0.799 || got.Cache.Efficacy > 0.801 {
		t.Errorf("cache efficacy: got %v want 0.80", got.Cache.Efficacy)
	}
	if got.Cache.ReadTokens != 800_000 || got.Cache.WriteTokens != 200_000 {
		t.Errorf("cache tokens: read=%d write=%d", got.Cache.ReadTokens, got.Cache.WriteTokens)
	}
}

// TestAnalysisHeadline_BudgetTile pins MTD + projection + budget %
// computation when MonthlyBudgetUSD is set on the server options.
func TestAnalysisHeadline_BudgetTile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)
	root := t.TempDir()
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{{
		SourceFile: "f", SourceEventID: "e1", SessionID: "sB",
		ProjectRoot: root, Timestamp: time.Now().UTC().Add(-time.Hour),
		Tool: models.ToolClaudeCode, ActionType: models.ActionReadFile,
		Target: "a.go", Success: true,
	}}, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sB", Provider: models.ProviderAnthropic,
		Model: "claude-sonnet-4-6", InputTokens: 100_000, OutputTokens: 20_000,
		Timestamp: monthStart.Add(time.Hour), RequestID: "msg_b_1",
	}); err != nil {
		t.Fatal(err)
	}

	server, err := New(Options{DB: database, DBPath: path, MonthlyBudgetUSD: 100})
	if err != nil {
		t.Fatal(err)
	}
	got := openHeadline(t, server, 30)
	// MTD cost: 100K × $3 + 20K × $15 / 1M = 0.30 + 0.30 = 0.60
	if !approx(got.Month.ToDateUSD, 0.60) {
		t.Errorf("MTD: got %v want 0.60", got.Month.ToDateUSD)
	}
	if got.Month.BudgetUSD != 100 {
		t.Errorf("budget echo: got %v want 100", got.Month.BudgetUSD)
	}
	// budget_pct = MTD / budget * 100 = 0.60%
	if got.Month.BudgetPct < 0.59 || got.Month.BudgetPct > 0.61 {
		t.Errorf("budget pct: got %v want ≈ 0.60", got.Month.BudgetPct)
	}
	if got.Month.DaysInMonth < 28 || got.Month.DaysInMonth > 31 {
		t.Errorf("days_in_month sanity: %v", got.Month.DaysInMonth)
	}
}

// TestAnalysisHeadline_NoBudgetSetHidesPct when MonthlyBudgetUSD is zero,
// budget_pct stays zero so the JS knows to render only the projection.
func TestAnalysisHeadline_NoBudgetSetHidesPct(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	server, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	got := openHeadline(t, server, 30)
	if got.Month.BudgetUSD != 0 || got.Month.BudgetPct != 0 {
		t.Errorf("no budget should yield zeros: %+v", got.Month)
	}
}

func approx(got, want float64) bool {
	d := got - want
	return d < 1e-6 && d > -1e-6
}

// routingResponse mirrors /api/analysis/routing-suggestions.
type routingResponse struct {
	Days        int     `json:"days"`
	TotalSaving float64 `json:"total_savings_usd"`
	Suggestions []struct {
		SessionID        string   `json:"session_id"`
		CurrentModel     string   `json:"current_model"`
		SuggestedModel   string   `json:"suggested_model"`
		CurrentCostUSD   float64  `json:"current_cost_usd"`
		SuggestedCostUSD float64  `json:"suggested_cost_usd"`
		SavingsUSD       float64  `json:"savings_usd"`
		Reasons          []string `json:"reasons"`
	} `json:"suggestions"`
}

func openRouting(t *testing.T, server *Server, days int) routingResponse {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/analysis/routing-suggestions?days="+itoa(days), nil)
	server.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("GET /api/analysis/routing-suggestions: %d body=%s", rr.Code, rr.Body.String())
	}
	var got routingResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// TestAnalysisRoutingSuggestions_TrivialOpusFlagged: a single-turn
// Opus session with small prompt + low output gets flagged with a
// Sonnet 4.6 alternative. The savings reflect the 5× rate gap
// between Opus 4.7 ($5/$25) and Sonnet 4.6 ($3/$15).
func TestAnalysisRoutingSuggestions_TrivialOpusFlagged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)
	root := t.TempDir()
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{{
		SourceFile: "f", SourceEventID: "e1", SessionID: "sTrivial",
		ProjectRoot: root, Timestamp: time.Now().UTC().Add(-time.Hour),
		Tool: models.ToolClaudeCode, ActionType: models.ActionReadFile,
		Target: "a.go", Success: true,
	}}, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sTrivial", Provider: models.ProviderAnthropic,
		Model: "claude-opus-4-7", InputTokens: 20_000, OutputTokens: 3_000,
		Timestamp: time.Now().UTC().Add(-24 * time.Hour), RequestID: "msg_t",
	}); err != nil {
		t.Fatal(err)
	}

	server, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	got := openRouting(t, server, 7)
	if len(got.Suggestions) != 1 {
		t.Fatalf("expected 1 suggestion, got %d", len(got.Suggestions))
	}
	s := got.Suggestions[0]
	if s.SessionID != "sTrivial" || s.CurrentModel != "claude-opus-4-7" {
		t.Errorf("session/model: %+v", s)
	}
	if s.SuggestedModel != "claude-sonnet-4-6" {
		t.Errorf("suggested model: got %q want claude-sonnet-4-6", s.SuggestedModel)
	}
	// Opus 4.7: 20K × $5 + 3K × $25 / 1M = 0.100 + 0.075 = 0.175
	// Sonnet 4.6: 20K × $3 + 3K × $15 / 1M = 0.060 + 0.045 = 0.105
	// Savings = 0.07 — clears the 0.05 floor.
	if !approx(s.SavingsUSD, 0.07) {
		t.Errorf("savings: got %v want 0.07", s.SavingsUSD)
	}
	if !approx(s.CurrentCostUSD, 0.175) {
		t.Errorf("current cost: got %v want 0.175", s.CurrentCostUSD)
	}
}

// TestAnalysisRoutingSuggestions_HeavyOpusSkipped: an Opus session
// with a large prompt or LC-tier turn is NOT flagged — heavy work
// could legitimately need Opus. Two sessions; both should fail the
// trivial filter, neither should appear in the suggestions list.
func TestAnalysisRoutingSuggestions_HeavyOpusSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)
	root := t.TempDir()

	// Heavy-prompt Opus — 200K input. Should be skipped.
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{{
		SourceFile: "f", SourceEventID: "eH", SessionID: "sHeavy",
		ProjectRoot: root, Timestamp: time.Now().UTC().Add(-time.Hour),
		Tool: models.ToolClaudeCode, ActionType: models.ActionReadFile,
		Target: "a.go", Success: true,
	}}, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sHeavy", Provider: models.ProviderAnthropic,
		Model: "claude-opus-4-7", InputTokens: 200_000, OutputTokens: 1_000,
		Timestamp: time.Now().UTC().Add(-24 * time.Hour), RequestID: "msg_h",
	}); err != nil {
		t.Fatal(err)
	}

	// High-output Opus — 10K output. Should be skipped (output >= 5K).
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{{
		SourceFile: "f", SourceEventID: "eO", SessionID: "sHighOutput",
		ProjectRoot: root, Timestamp: time.Now().UTC().Add(-time.Hour),
		Tool: models.ToolClaudeCode, ActionType: models.ActionReadFile,
		Target: "b.go", Success: true,
	}}, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sHighOutput", Provider: models.ProviderAnthropic,
		Model: "claude-opus-4-7", InputTokens: 10_000, OutputTokens: 10_000,
		Timestamp: time.Now().UTC().Add(-24 * time.Hour), RequestID: "msg_o",
	}); err != nil {
		t.Fatal(err)
	}

	server, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	got := openRouting(t, server, 7)
	for _, s := range got.Suggestions {
		if s.SessionID == "sHeavy" || s.SessionID == "sHighOutput" {
			t.Errorf("expected skip but got suggestion for %s: %+v", s.SessionID, s)
		}
	}
}

// TestAnalysisRoutingSuggestions_MixedModelSkipped: a session that
// mixed Opus and Sonnet is intentional routing — leave it alone.
func TestAnalysisRoutingSuggestions_MixedModelSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)
	root := t.TempDir()
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{{
		SourceFile: "f", SourceEventID: "eM", SessionID: "sMixed",
		ProjectRoot: root, Timestamp: time.Now().UTC().Add(-time.Hour),
		Tool: models.ToolClaudeCode, ActionType: models.ActionReadFile,
		Target: "a.go", Success: true,
	}}, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}
	for i, model := range []string{"claude-opus-4-7", "claude-sonnet-4-6"} {
		if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
			SessionID: "sMixed", Provider: models.ProviderAnthropic,
			Model: model, InputTokens: 5_000, OutputTokens: 1_000,
			Timestamp: time.Now().UTC().Add(-time.Duration(i+1) * 24 * time.Hour),
			RequestID: "msg_m_" + itoa(i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	server, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	got := openRouting(t, server, 7)
	for _, s := range got.Suggestions {
		if s.SessionID == "sMixed" {
			t.Errorf("mixed-model session should NOT be flagged: %+v", s)
		}
	}
}

// TestAnalysisRoutingSuggestions_EmptyDB returns an empty list, not 500.
func TestAnalysisRoutingSuggestions_EmptyDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	server, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	got := openRouting(t, server, 30)
	if len(got.Suggestions) != 0 || got.TotalSaving != 0 {
		t.Errorf("empty DB: %+v", got)
	}
}

// trendResponse mirrors /api/analysis/trend's JSON shape for tests.
type trendResponse struct {
	Metric string `json:"metric"`
	Dim    string `json:"dim"`
	Bucket string `json:"bucket"`
	Days   int    `json:"days"`
	Series []struct {
		Bucket      string  `json:"bucket"`
		Key         string  `json:"key"`
		TotalTokens int64   `json:"total_tokens"`
		CostUSD     float64 `json:"cost_usd"`
		TurnCount   int     `json:"turn_count"`
	} `json:"series"`
}

func openTrend(t *testing.T, server *Server, dim string, days int) trendResponse {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/analysis/trend?dim="+dim+"&days="+itoa(days), nil)
	server.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("GET /api/analysis/trend?dim=%s: %d body=%s", dim, rr.Code, rr.Body.String())
	}
	var got trendResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// TestAnalysisTrend_EmptyAndUnknownDim verifies the empty-DB path
// returns an empty series, and that an invalid dim is rejected.
func TestAnalysisTrend_EmptyAndUnknownDim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	server, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	got := openTrend(t, server, "model", 30)
	if got.Dim != "model" || got.Days != 30 {
		t.Errorf("echo: dim=%q days=%d", got.Dim, got.Days)
	}
	if len(got.Series) != 0 {
		t.Errorf("empty DB series: got %d points", len(got.Series))
	}

	// Unknown dim → 400.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/analysis/trend?dim=invalid", nil)
	server.Handler().ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Errorf("unknown dim: got %d want 400", rr.Code)
	}
}

// TestAnalysisTrend_GroupsByDimension seeds turns from two projects
// running two tools across two days, then verifies each dimension
// rolls up correctly. The dim toggle on the page rides this contract.
func TestAnalysisTrend_GroupsByDimension(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)

	now := time.Now().UTC()
	day1 := now.Add(-2 * 24 * time.Hour)
	day2 := now.Add(-1 * 24 * time.Hour)
	repoA := t.TempDir()
	repoB := t.TempDir()

	// Two sessions, different projects, different tools. Token counts
	// kept small so cost stays at standard rates (no LC dispatch).
	for _, ev := range []struct {
		sid, eid, ts string
		root         string
		tool         string
	}{
		{"sA", "eA", day1.Format(time.RFC3339Nano), repoA, models.ToolClaudeCode},
		{"sB", "eB", day2.Format(time.RFC3339Nano), repoB, models.ToolCodex},
	} {
		ts, _ := time.Parse(time.RFC3339Nano, ev.ts)
		if _, err := st.Ingest(context.Background(), []models.ToolEvent{{
			SourceFile: "f", SourceEventID: ev.eid, SessionID: ev.sid,
			ProjectRoot: ev.root, Timestamp: ts, Tool: ev.tool,
			ActionType: models.ActionReadFile, Target: "x.go", Success: true,
		}}, nil, store.IngestOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	// Two proxy turns, one per session. 10K input + 5K output × Sonnet
	// 4.6 = $0.10 each. Different days, different (project, tool).
	// Look up each session's project_id so the api_turns row joins back
	// to a project root_path — without it the GroupByDayProject rollup
	// groups everything under "<no-project>".
	projectIDFor := func(sessionID string) int64 {
		var pid int64
		if err := database.QueryRow(
			`SELECT project_id FROM sessions WHERE id = ?`, sessionID,
		).Scan(&pid); err != nil {
			t.Fatalf("project_id for %s: %v", sessionID, err)
		}
		return pid
	}
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sA", ProjectID: projectIDFor("sA"),
		Provider: models.ProviderAnthropic,
		Model:    "claude-sonnet-4-6", InputTokens: 10_000, OutputTokens: 5_000,
		Timestamp: day1, RequestID: "msg_A",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sB", ProjectID: projectIDFor("sB"),
		Provider: models.ProviderOpenAI,
		Model:    "gpt-5.1", InputTokens: 10_000, OutputTokens: 5_000,
		Timestamp: day2, RequestID: "msg_B",
	}); err != nil {
		t.Fatal(err)
	}

	server, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}

	// dim=model → 2 series points (one per model, on different days).
	got := openTrend(t, server, "model", 7)
	if len(got.Series) != 2 {
		t.Fatalf("model series: got %d want 2", len(got.Series))
	}
	models := map[string]bool{}
	for _, p := range got.Series {
		models[p.Key] = true
	}
	if !models["claude-sonnet-4-6"] || !models["gpt-5.1"] {
		t.Errorf("model keys: got %v want claude-sonnet-4-6 + gpt-5.1", models)
	}

	// dim=project → 2 series points (one per project root).
	got = openTrend(t, server, "project", 7)
	if len(got.Series) != 2 {
		t.Fatalf("project series: got %d want 2 (one per project)", len(got.Series))
	}
	projects := map[string]bool{}
	for _, p := range got.Series {
		projects[p.Key] = true
	}
	if !projects[repoA] || !projects[repoB] {
		t.Errorf("project keys: got %v want %s + %s", projects, repoA, repoB)
	}

	// dim=tool → 2 series points (claude-code + codex).
	got = openTrend(t, server, "tool", 7)
	if len(got.Series) != 2 {
		t.Fatalf("tool series: got %d want 2 (one per tool)", len(got.Series))
	}
	tools := map[string]bool{}
	for _, p := range got.Series {
		tools[p.Key] = true
	}
	if !tools["claude-code"] || !tools["codex"] {
		t.Errorf("tool keys: got %v want claude-code + codex", tools)
	}
}

// moversResponse mirrors /api/analysis/movers's JSON for tests.
type moversResponse struct {
	Dim         string         `json:"dim"`
	Days        int            `json:"days"`
	Increases   []moverEntry   `json:"increases"`
	Decreases   []moverEntry   `json:"decreases"`
	NewEntrants []entrantEntry `json:"new_entrants"`
}
type moverEntry struct {
	Key        string  `json:"key"`
	PriorUSD   float64 `json:"prior_usd"`
	CurrentUSD float64 `json:"current_usd"`
	DeltaUSD   float64 `json:"delta_usd"`
	DeltaPct   float64 `json:"delta_pct"`
}
type entrantEntry struct {
	Key        string  `json:"key"`
	CurrentUSD float64 `json:"current_usd"`
}

func openMovers(t *testing.T, server *Server, dim string, days int) moversResponse {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/analysis/movers?dim="+dim+"&days="+itoa(days), nil)
	server.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("GET /api/analysis/movers?dim=%s: %d body=%s", dim, rr.Code, rr.Body.String())
	}
	var got moversResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// TestAnalysisMovers_PeriodOverPeriod pins the diff math: a model whose
// cost rises gets surfaced in increases; one that falls in decreases;
// one new this period in new_entrants; one only in prior period appears
// as a -100% mover with current=0.
func TestAnalysisMovers_PeriodOverPeriod(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)
	root := t.TempDir()
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{{
		SourceFile: "f", SourceEventID: "e1", SessionID: "sM",
		ProjectRoot: root, Timestamp: time.Now().UTC().Add(-time.Hour),
		Tool: models.ToolClaudeCode, ActionType: models.ActionReadFile,
		Target: "a.go", Success: true,
	}}, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	priorTs := now.Add(-10 * 24 * time.Hour)
	currentTs := now.Add(-1 * 24 * time.Hour)

	type fix struct {
		ts      time.Time
		model   string
		in, out int64
		reqID   string
	}
	for i, f := range []fix{
		// "rising" — prior $0.30, current $0.60 → +$0.30
		{priorTs, "claude-sonnet-4-6", 50_000, 10_000, "msg_rise_p"},
		{currentTs, "claude-sonnet-4-6", 100_000, 20_000, "msg_rise_c"},
		// "falling" — prior $0.60, current $0.30 → -$0.30
		{priorTs, "claude-haiku-4-5", 200_000, 100_000, "msg_fall_p"},  // 200K×$1 + 100K×$5 = 0.20+0.50=0.70
		{currentTs, "claude-haiku-4-5", 100_000, 50_000, "msg_fall_c"}, // 0.10+0.25=0.35
		// "new entrant" — only in current period
		{currentTs, "gpt-5.1", 50_000, 10_000, "msg_new_c"},
		// "departed" — only in prior period
		{priorTs, "claude-opus-4-7", 10_000, 1_000, "msg_dep_p"},
	} {
		_ = i
		if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
			SessionID: "sM", Provider: models.ProviderAnthropic,
			Model: f.model, InputTokens: f.in, OutputTokens: f.out,
			Timestamp: f.ts, RequestID: f.reqID,
		}); err != nil {
			t.Fatal(err)
		}
	}

	server, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	got := openMovers(t, server, "model", 7)

	// Increases: should include claude-sonnet-4-6 (Δ +0.30).
	foundRise := false
	for _, m := range got.Increases {
		if m.Key == "claude-sonnet-4-6" {
			foundRise = true
			if !approx(m.DeltaUSD, 0.30) {
				t.Errorf("rise delta: got %v want 0.30", m.DeltaUSD)
			}
		}
	}
	if !foundRise {
		t.Errorf("increases missing claude-sonnet-4-6: %+v", got.Increases)
	}

	// Decreases: should include claude-haiku-4-5 (Δ -0.35) AND
	// claude-opus-4-7 (the departed model — current=0, delta=-prior).
	foundFall, foundDep := false, false
	for _, m := range got.Decreases {
		if m.Key == "claude-haiku-4-5" {
			foundFall = true
		}
		if m.Key == "claude-opus-4-7" {
			foundDep = true
			if m.CurrentUSD != 0 || m.DeltaPct != -100 {
				t.Errorf("departed model: got %+v want current=0 deltaPct=-100", m)
			}
		}
	}
	if !foundFall {
		t.Errorf("decreases missing claude-haiku-4-5: %+v", got.Decreases)
	}
	if !foundDep {
		t.Errorf("decreases missing claude-opus-4-7 (departed): %+v", got.Decreases)
	}

	// New entrants: gpt-5.1 only.
	if len(got.NewEntrants) != 1 || got.NewEntrants[0].Key != "gpt-5.1" {
		t.Errorf("new entrants: got %+v want [gpt-5.1]", got.NewEntrants)
	}
}

// TestAnalysisMovers_EmptyAndUnknownDim handles fresh DB + invalid dim.
func TestAnalysisMovers_EmptyAndUnknownDim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	server, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	got := openMovers(t, server, "model", 30)
	if len(got.Increases) != 0 || len(got.Decreases) != 0 || len(got.NewEntrants) != 0 {
		t.Errorf("empty DB movers: %+v", got)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/analysis/movers?dim=invalid", nil)
	server.Handler().ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Errorf("unknown dim: got %d want 400", rr.Code)
	}
}

// topSessionsResponse mirrors /api/analysis/top-sessions's JSON for tests.
type topSessionsResponse struct {
	Days     int `json:"days"`
	Limit    int `json:"limit"`
	Sessions []struct {
		ID        string   `json:"id"`
		Tool      string   `json:"tool"`
		Models    []string `json:"models"`
		Turns     int      `json:"turns"`
		MaxPrompt int64    `json:"max_prompt_tokens"`
		LCTurns   int      `json:"lc_turn_count"`
		CostUSD   float64  `json:"cost_usd"`
		Badges    []string `json:"badges"`
	} `json:"sessions"`
}

func openTopSessions(t *testing.T, server *Server, days, limit int) topSessionsResponse {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/analysis/top-sessions?days="+itoa(days)+"&limit="+itoa(limit), nil)
	server.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("GET /api/analysis/top-sessions: %d body=%s", rr.Code, rr.Body.String())
	}
	var got topSessionsResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// TestAnalysisTopSessions_RankingAndBadges sets up four sessions with
// distinct cost profiles and verifies (a) cost-ordering, (b) badge
// attribution, (c) tool/started_at enrichment from the sessions table.
func TestAnalysisTopSessions_RankingAndBadges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)
	root := t.TempDir()
	now := time.Now().UTC()

	// Each session needs an action row so it lands in the sessions
	// table; that's where tool / started_at come from for enrichment.
	seedSession := func(sid, tool string, ts time.Time) {
		t.Helper()
		if _, err := st.Ingest(context.Background(), []models.ToolEvent{{
			SourceFile: "f", SourceEventID: "e_" + sid, SessionID: sid,
			ProjectRoot: root, Timestamp: ts, Tool: tool,
			ActionType: models.ActionReadFile, Target: "x.go", Success: true,
		}}, nil, store.IngestOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	// Session A — Opus, big single turn, $4.00 → top of the list,
	// badges: opus, large_prompt (250K > 100K).
	seedSession("sA", models.ToolClaudeCode, now.Add(-2*24*time.Hour))
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sA", Provider: models.ProviderAnthropic,
		Model: "claude-opus-4-7", InputTokens: 250_000, OutputTokens: 100_000,
		Timestamp: now.Add(-2 * 24 * time.Hour), RequestID: "msg_A",
	}); err != nil {
		t.Fatal(err)
	}

	// Session B — Sonnet 4 LC tier turn (250K) → cost $1.6125,
	// badges: lc_tier, large_prompt.
	seedSession("sB", models.ToolClaudeCode, now.Add(-1*24*time.Hour))
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sB", Provider: models.ProviderAnthropic,
		Model: "claude-sonnet-4-20250514", InputTokens: 250_000, OutputTokens: 5_000,
		Timestamp: now.Add(-1 * 24 * time.Hour), RequestID: "msg_B",
	}); err != nil {
		t.Fatal(err)
	}

	// Session C — many small Sonnet 4.6 turns (35 turns) → many_turns badge.
	seedSession("sC", models.ToolCodex, now.Add(-3*24*time.Hour))
	for i := 0; i < 35; i++ {
		if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
			SessionID: "sC", Provider: models.ProviderAnthropic,
			Model: "claude-sonnet-4-6", InputTokens: 5_000, OutputTokens: 1_000,
			Timestamp: now.Add(-3 * 24 * time.Hour),
			RequestID: "msg_C_" + itoa(i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Session D — small, no badges. Should rank last.
	seedSession("sD", models.ToolClaudeCode, now.Add(-4*24*time.Hour))
	if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: "sD", Provider: models.ProviderAnthropic,
		Model: "claude-haiku-4-5", InputTokens: 1_000, OutputTokens: 500,
		Timestamp: now.Add(-4 * 24 * time.Hour), RequestID: "msg_D",
	}); err != nil {
		t.Fatal(err)
	}

	server, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	got := openTopSessions(t, server, 7, 10)
	if len(got.Sessions) != 4 {
		t.Fatalf("session count: got %d want 4", len(got.Sessions))
	}

	// Cost ranking: A > B > C > D.
	if got.Sessions[0].ID != "sA" {
		t.Errorf("rank 1: got %q want sA", got.Sessions[0].ID)
	}
	if got.Sessions[1].ID != "sB" {
		t.Errorf("rank 2: got %q want sB", got.Sessions[1].ID)
	}
	if got.Sessions[3].ID != "sD" {
		t.Errorf("rank last: got %q want sD", got.Sessions[3].ID)
	}

	// Badges per session.
	badgeOf := func(sid string) []string {
		for _, s := range got.Sessions {
			if s.ID == sid {
				return s.Badges
			}
		}
		return nil
	}
	hasBadge := func(badges []string, want string) bool {
		for _, b := range badges {
			if b == want {
				return true
			}
		}
		return false
	}

	bA := badgeOf("sA")
	if !hasBadge(bA, "opus") || !hasBadge(bA, "large_prompt") {
		t.Errorf("sA badges: got %v want opus + large_prompt", bA)
	}
	bB := badgeOf("sB")
	if !hasBadge(bB, "lc_tier") || !hasBadge(bB, "large_prompt") {
		t.Errorf("sB badges: got %v want lc_tier + large_prompt", bB)
	}
	bC := badgeOf("sC")
	if !hasBadge(bC, "many_turns") {
		t.Errorf("sC badges: got %v want many_turns", bC)
	}
	if hasBadge(bC, "opus") {
		t.Errorf("sC must NOT have opus badge: %v", bC)
	}
	bD := badgeOf("sD")
	if len(bD) != 0 {
		t.Errorf("sD should have no badges: got %v", bD)
	}

	// Tool enrichment.
	for _, s := range got.Sessions {
		if s.ID == "sC" && s.Tool != models.ToolCodex {
			t.Errorf("sC tool: got %q want codex", s.Tool)
		}
		if s.ID == "sA" && s.Tool != models.ToolClaudeCode {
			t.Errorf("sA tool: got %q want claude-code", s.Tool)
		}
	}
}

// TestAnalysisTopSessions_LimitAndEmpty pins the limit param and the
// empty-DB path.
func TestAnalysisTopSessions_LimitAndEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	server, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	got := openTopSessions(t, server, 30, 5)
	if got.Limit != 5 || len(got.Sessions) != 0 {
		t.Errorf("empty DB: got limit=%d sessions=%d", got.Limit, len(got.Sessions))
	}
}

// TestAnalysisTrend_ChronologicalOrder pins the rendering contract:
// chart axes read left-to-right by day. Cost engine returns rows by
// cost DESC so the handler must re-sort before serializing.
func TestAnalysisTrend_ChronologicalOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)
	root := t.TempDir()

	now := time.Now().UTC()
	// Three days, smallest cost first chronologically (so the
	// cost-DESC default would put it last in the wire).
	for i, in := range []int64{1_000, 100_000, 10_000} {
		ts := now.Add(-time.Duration(3-i) * 24 * time.Hour)
		if _, err := st.Ingest(context.Background(), []models.ToolEvent{{
			SourceFile: "f", SourceEventID: "e" + itoa(i), SessionID: "s" + itoa(i),
			ProjectRoot: root, Timestamp: ts, Tool: models.ToolClaudeCode,
			ActionType: models.ActionReadFile, Target: "x.go", Success: true,
		}}, nil, store.IngestOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.InsertAPITurn(context.Background(), models.APITurn{
			SessionID: "s" + itoa(i), Provider: models.ProviderAnthropic,
			Model: "claude-sonnet-4-6", InputTokens: in,
			Timestamp: ts, RequestID: "msg_" + itoa(i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	server, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	got := openTrend(t, server, "model", 7)
	for i := 1; i < len(got.Series); i++ {
		if got.Series[i].Bucket < got.Series[i-1].Bucket {
			t.Errorf("series not chronological at %d: %s < %s",
				i, got.Series[i].Bucket, got.Series[i-1].Bucket)
		}
	}
}
