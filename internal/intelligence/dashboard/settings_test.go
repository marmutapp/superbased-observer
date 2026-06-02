package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
)

// TestHandleConfig_GetReturnsFullStruct verifies that GET /api/config
// surfaces the full live config plus the editable_sections capability
// list. Settings UI uses this to render every section.
func TestHandleConfig_GetReturnsFullStruct(t *testing.T) {
	tdir := t.TempDir()
	cfgPath := filepath.Join(tdir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(`
[observer]
log_level = "warn"

[intelligence]
monthly_budget_usd = 75
`), 0o644); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(tdir, "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	server, err := New(Options{DB: database, ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		ConfigPath       string        `json:"config_path"`
		Config           config.Config `json:"config"`
		EditableSections []string      `json:"editable_sections"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.ConfigPath != cfgPath {
		t.Errorf("config_path: got %q want %q", got.ConfigPath, cfgPath)
	}
	if got.Config.Observer.LogLevel != "warn" {
		t.Errorf("log_level not loaded: %q", got.Config.Observer.LogLevel)
	}
	if got.Config.Intelligence.MonthlyBudgetUSD != 75 {
		t.Errorf("monthly_budget_usd: got %v want 75", got.Config.Intelligence.MonthlyBudgetUSD)
	}
	foundPricing := false
	for _, s := range got.EditableSections {
		if s == "pricing" {
			foundPricing = true
		}
	}
	if !foundPricing {
		t.Errorf("editable_sections must include pricing: %v", got.EditableSections)
	}
}

// TestHandleConfig_NoFileReturnsDefaults — fresh install path. No
// config.toml on disk yet, GET /api/config still works and returns the
// baked-in defaults so the Settings UI has something to render.
func TestHandleConfig_NoFileReturnsDefaults(t *testing.T) {
	tdir := t.TempDir()
	cfgPath := filepath.Join(tdir, "missing.toml") // never created
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(tdir, "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	server, err := New(Options{DB: database, ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestHandleConfigPricing_SaveWritesFileAndReloadsEngine pins the full
// hot-reload path: PUT /api/config/pricing writes the new model rates
// to disk, creates a .bak of the previous file, and the cost engine
// reloads in-place so subsequent Compute calls see the new rate.
func TestHandleConfigPricing_SaveWritesFileAndReloadsEngine(t *testing.T) {
	tdir := t.TempDir()
	cfgPath := filepath.Join(tdir, "config.toml")
	const originalToml = `
[observer]
log_level = "info"
`
	if err := os.WriteFile(cfgPath, []byte(originalToml), 0o644); err != nil {
		t.Fatal(err)
	}

	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(tdir, "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	engine := cost.NewEngine(config.IntelligenceConfig{})
	server, err := New(Options{
		DB:         database,
		CostEngine: engine,
		ConfigPath: cfgPath,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Sanity: pre-save the engine returns baked-in Sonnet 4.6 rates.
	pre, ok := engine.Lookup("claude-sonnet-4-6")
	if !ok || pre.Input != 3 {
		t.Fatalf("baseline lookup: %+v ok=%v", pre, ok)
	}

	// PUT new pricing override: bump claude-sonnet-4-6 input to $99.
	body := `{"models":{"claude-sonnet-4-6":{"input":99,"output":999,"cache_read":9.9}}}`
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPut, "/api/config/pricing", strings.NewReader(body)))
	if rr.Code != 200 {
		t.Fatalf("PUT status: %d body=%s", rr.Code, rr.Body.String())
	}
	var saveResp struct {
		Saved      bool   `json:"saved"`
		BackupPath string `json:"backup_path"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&saveResp); err != nil {
		t.Fatal(err)
	}
	if !saveResp.Saved {
		t.Errorf("response did not report saved=true")
	}
	if saveResp.BackupPath != cfgPath+".bak" {
		t.Errorf("backup path: got %q want %q", saveResp.BackupPath, cfgPath+".bak")
	}

	// .bak preserves the prior content (Option A — comments are lost
	// on save but the prior version is recoverable).
	bak, err := os.ReadFile(cfgPath + ".bak")
	if err != nil {
		t.Fatalf("read .bak: %v", err)
	}
	if string(bak) != originalToml {
		t.Errorf(".bak contents drifted: got %q want %q", bak, originalToml)
	}

	// The new file parses back through config.Load with the override applied.
	reloaded, err := config.Load(config.LoadOptions{GlobalPath: cfgPath})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	mp, ok := reloaded.Intelligence.Pricing.Models["claude-sonnet-4-6"]
	if !ok || mp.Input != 99 || mp.Output != 999 {
		t.Errorf("override not persisted: %+v ok=%v", mp, ok)
	}

	// Engine reloaded in place: same instance now returns the new rates.
	post, ok := engine.Lookup("claude-sonnet-4-6")
	if !ok || post.Input != 99 || post.Output != 999 {
		t.Errorf("engine not reloaded: %+v ok=%v", post, ok)
	}
}

// TestHandleConfigPricing_NoConfigPath rejects saves when the server
// was started without a ConfigPath option (the dashboard subcommand is
// always given one, but tests / future ephemeral modes may not).
func TestHandleConfigPricing_NoConfigPath(t *testing.T) {
	tdir := t.TempDir()
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(tdir, "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	server, err := New(Options{DB: database})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPut, "/api/config/pricing", strings.NewReader(`{}`)))
	if rr.Code != http.StatusConflict {
		t.Errorf("status: got %d want 409", rr.Code)
	}
}

// TestHandleConfigPricingDefaults_ShapeAndCoverage pins the baked-in
// pricing table that the dashboard renders as a reference list. When
// pricing-reference.md drifts (new models, rate changes), this test
// catches if the matching defaultPricing entry didn't get updated.
func TestHandleConfigPricingDefaults_ShapeAndCoverage(t *testing.T) {
	tdir := t.TempDir()
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(tdir, "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	server, err := New(Options{DB: database})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/config/pricing/defaults", nil))
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Defaults map[string]struct {
			Input                      float64 `json:"input"`
			Output                     float64 `json:"output"`
			CacheRead                  float64 `json:"cache_read"`
			CacheCreation              float64 `json:"cache_creation"`
			CacheCreation1h            float64 `json:"cache_creation_1h"`
			LongContextThreshold       int64   `json:"long_context_threshold,omitempty"`
			LongContextInput           float64 `json:"long_context_input,omitempty"`
			LongContextOutput          float64 `json:"long_context_output,omitempty"`
			LongContextCacheRead       float64 `json:"long_context_cache_read,omitempty"`
			LongContextCacheCreation   float64 `json:"long_context_cache_creation,omitempty"`
			LongContextCacheCreation1h float64 `json:"long_context_cache_creation_1h,omitempty"`
		} `json:"defaults"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	// Sanity: every provider's flagship is present with non-trivial rates.
	for _, model := range []string{
		"claude-sonnet-4-6", "claude-opus-4-7", "claude-haiku-4-5",
		"gpt-5", "gpt-5.4", "gemini-2.5-pro", "grok-4-20", "kimi-k2-5",
	} {
		entry, ok := got.Defaults[model]
		if !ok {
			t.Errorf("default missing: %s", model)
			continue
		}
		if entry.Input <= 0 && entry.Output <= 0 {
			t.Errorf("%s has zero rates: %+v", model, entry)
		}
	}
	// LC tier round-trip: claude-sonnet-4-5 should expose its 200K threshold.
	if s := got.Defaults["claude-sonnet-4-5"]; s.LongContextThreshold != 200_000 {
		t.Errorf("claude-sonnet-4-5 LC threshold: got %v want 200000", s.LongContextThreshold)
	}
	// Gross size sanity: there are 60+ baked-in models per pricing-reference.md.
	if len(got.Defaults) < 50 {
		t.Errorf("default count: got %d want >= 50", len(got.Defaults))
	}
}

// TestHandleConfigSection_SaveRetention pins the per-section save
// path: PUT /api/config/section/retention writes a new RetentionConfig,
// the file persists the change, and the response sets
// restart_required=true so the UI surfaces the restart banner.
func TestHandleConfigSection_SaveRetention(t *testing.T) {
	tdir := t.TempDir()
	cfgPath := filepath.Join(tdir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(`[observer]
log_level = "info"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(tdir, "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	server, err := New(Options{DB: database, ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}

	body := `{"MaxAgeDays":365,"MaxDBSizeMB":2000,"PruneOnStartup":false,"ObserverLogMaxAgeDays":60}`
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPut, "/api/config/section/retention", strings.NewReader(body)))
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Saved           bool   `json:"saved"`
		Section         string `json:"section"`
		RestartRequired bool   `json:"restart_required"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Saved {
		t.Errorf("response did not report saved=true")
	}
	if resp.Section != "retention" {
		t.Errorf("section echo: got %q want retention", resp.Section)
	}
	if !resp.RestartRequired {
		t.Errorf("restart_required must be true for non-pricing saves")
	}

	// Verify persistence — the new file parses with the override applied.
	reloaded, err := config.Load(config.LoadOptions{GlobalPath: cfgPath})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Observer.Retention.MaxAgeDays != 365 {
		t.Errorf("MaxAgeDays not persisted: got %v want 365", reloaded.Observer.Retention.MaxAgeDays)
	}
	if reloaded.Observer.Retention.MaxDBSizeMB != 2000 {
		t.Errorf("MaxDBSizeMB: got %v want 2000", reloaded.Observer.Retention.MaxDBSizeMB)
	}
}

// TestHandleConfigSection_PreservesPricingOnIntelligenceSave guards a
// subtle interaction: the intelligence section's PUT handler decodes
// only the editable subset (CodeGraph / APIKeyEnv / SummaryModel /
// MonthlyBudgetUSD) and must NOT clobber the pricing overrides that
// /api/config/pricing manages separately.
func TestHandleConfigSection_PreservesPricingOnIntelligenceSave(t *testing.T) {
	tdir := t.TempDir()
	cfgPath := filepath.Join(tdir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(`
[intelligence.pricing.models."claude-sonnet-4-6"]
input = 99
output = 999
`), 0o644); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(tdir, "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	server, err := New(Options{DB: database, ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}

	// Save intelligence section with a new monthly budget but NO
	// pricing in the body.
	body := `{"MonthlyBudgetUSD":250,"APIKeyEnv":"OBSERVER_API_KEY","SummaryModel":"haiku-4-5"}`
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPut, "/api/config/section/intelligence", strings.NewReader(body)))
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}

	reloaded, err := config.Load(config.LoadOptions{GlobalPath: cfgPath})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Intelligence.MonthlyBudgetUSD != 250 {
		t.Errorf("budget not saved: %v", reloaded.Intelligence.MonthlyBudgetUSD)
	}
	if reloaded.Intelligence.SummaryModel != "haiku-4-5" {
		t.Errorf("summary model not saved: %q", reloaded.Intelligence.SummaryModel)
	}
	mp, ok := reloaded.Intelligence.Pricing.Models["claude-sonnet-4-6"]
	if !ok || mp.Input != 99 {
		t.Errorf("pricing override clobbered by intelligence save: %+v ok=%v", mp, ok)
	}
}

// TestHandleConfigSection_UnknownSection rejects unknown section names
// with 400 — guards typos and prevents arbitrary JSON from landing
// somewhere unexpected if the route mux changes.
func TestHandleConfigSection_UnknownSection(t *testing.T) {
	tdir := t.TempDir()
	cfgPath := filepath.Join(tdir, "config.toml")
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(tdir, "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	server, err := New(Options{DB: database, ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPut, "/api/config/section/bogus", strings.NewReader(`{}`)))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status: got %d want 400", rr.Code)
	}

	// Pricing through the section path is also rejected — that's a
	// different endpoint with hot-reload semantics.
	rr = httptest.NewRecorder()
	server.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPut, "/api/config/section/pricing", strings.NewReader(`{}`)))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("pricing-via-section path should 400: got %d", rr.Code)
	}
}

// TestHandleAdminRestart_ScheduledResponse verifies the endpoint
// returns 200 with the scheduled flag. The actual os.Exit is fired
// async after a delay; we don't invoke it in tests because that would
// kill the test runner. The 500ms delay path is exercised by hand with
// the smoke-test recipe in docs/.
func TestHandleAdminRestart_ScheduledResponse(t *testing.T) {
	t.Skip("os.Exit invocation tested manually — would kill the test runner")
}

// TestHandleBackfillStatus pins the per-mode shape: every documented
// flag surfaces, SQL-checkable modes get a non-negative count, file-walk
// modes report -1 with a "needs scan" note.
func TestHandleBackfillStatus(t *testing.T) {
	tdir := t.TempDir()
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(tdir, "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	server, err := New(Options{DB: database})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/backfill/status", nil))
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Modes []struct {
			Mode           string `json:"mode"`
			Flag           string `json:"flag"`
			Description    string `json:"description"`
			Candidates     int64  `json:"candidates"`
			CandidatesNote string `json:"candidates_note"`
		} `json:"modes"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Modes) < 14 {
		t.Errorf("mode count: got %d want >= 14 (3 SQL + 12 file-walk after v1.4.20 api-errors mode)", len(got.Modes))
	}
	byMode := map[string]int64{}
	noteByMode := map[string]string{}
	for _, m := range got.Modes {
		byMode[m.Mode] = m.Candidates
		noteByMode[m.Mode] = m.CandidatesNote
	}
	// SQL modes return a non-negative count (empty DB = 0).
	for _, sqlMode := range []string{"is-sidechain", "cache-tier", "message-id"} {
		if byMode[sqlMode] < 0 {
			t.Errorf("SQL mode %q must have non-negative candidates: %d", sqlMode, byMode[sqlMode])
		}
	}
	// File-walk modes report -1 + a note.
	for _, fwMode := range []string{"openclaw-model", "cursor-model", "claudecode-user-prompts"} {
		if byMode[fwMode] != -1 {
			t.Errorf("file-walk mode %q should report -1, got %d", fwMode, byMode[fwMode])
		}
		if noteByMode[fwMode] == "" {
			t.Errorf("file-walk mode %q missing note", fwMode)
		}
	}
}
