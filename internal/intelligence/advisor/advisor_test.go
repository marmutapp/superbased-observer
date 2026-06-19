package advisor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// flatPrice prices every bucket at simple flat rates so detector arithmetic
// is hand-checkable: input $5/M, output $25/M, cache read $0.50/M, cache
// write $6.25/M (1h $10/M), 2× when Fast.
func flatPrice(_ string, b cost.TokenBundle) (float64, bool) {
	cc1 := b.CacheCreation1h
	if cc1 > b.CacheCreation {
		cc1 = b.CacheCreation
	}
	cc5 := b.CacheCreation - cc1
	usd := (float64(b.Input)*5 + float64(b.Output)*25 + float64(b.CacheRead)*0.5 +
		float64(cc5)*6.25 + float64(cc1)*10) / 1e6
	if b.Fast {
		usd *= 2
	}
	return usd, true
}

func testFacts(sessions ...SessionFacts) *Facts {
	return &Facts{
		WindowDays:  14,
		Now:         time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC),
		Sessions:    sessions,
		Price:       flatPrice,
		LCThreshold: func(string) int64 { return 0 },
	}
}

// growingSession builds n rows whose window grows by growth tokens per
// turn, one minute apart, constant output.
func growingSession(id string, n int, start, growth int64) SessionFacts {
	t0 := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	s := SessionFacts{ID: id, Tool: "claude-code", Model: "claude-opus-4-7"}
	for i := 0; i < n; i++ {
		win := start + int64(i)*growth
		s.Rows = append(s.Rows, TurnFact{
			TS: t0.Add(time.Duration(i) * time.Minute), Model: "claude-opus-4-7",
			Input: 1000, Output: 500, CacheRead: win - 1000 - 2000, CacheCreation: 2000,
			Source: "jsonl",
		})
	}
	return s
}

func TestDetectSessionBalloon(t *testing.T) {
	th := DefaultThresholds()
	cases := []struct {
		name    string
		facts   *Facts
		wantN   int
		wantKey string
		minSave float64
	}{
		{
			name:    "ballooned session fires with positive savings",
			facts:   testFacts(growingSession("sess-big", 80, 40_000, 8_000)),
			wantN:   1,
			wantKey: "session_balloon|sess-big",
			minSave: 1.0,
		},
		{
			name:  "below threshold never fires",
			facts: testFacts(growingSession("sess-small", 80, 10_000, 500)), // max ~50K vs 100K floor
			wantN: 0,
		},
		{
			// 25 rows, 90K→114K: crosses the 100K floor at ~row 11,
			// leaving only ~14 rows after (< BalloonMinRowsAfter 20).
			name:  "too few rows after balloon point",
			facts: testFacts(growingSession("sess-short", 25, 90_000, 1_000)),
			wantN: 0,
		},
		{
			// A 200K-context session living at 60% of its window: fires
			// under fraction semantics (threshold 100K) — the case the
			// old fixed-150K threshold MISSED for standard-context models.
			name:    "standard-context session past 50% fires",
			facts:   testFacts(growingSession("sess-std-ctx", 60, 80_000, 800)), // max ~127K, ctx 200K, threshold 100K
			wantN:   1,
			wantKey: "session_balloon|sess-std-ctx",
			minSave: 0.0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectSessionBalloon(tc.facts, th)
			if len(got) != tc.wantN {
				t.Fatalf("suggestions = %d, want %d (%+v)", len(got), tc.wantN, got)
			}
			if tc.wantN == 0 {
				return
			}
			s := got[0]
			if s.DedupKey != tc.wantKey {
				t.Errorf("DedupKey = %q, want %q", s.DedupKey, tc.wantKey)
			}
			if s.SavingsUSD < tc.minSave {
				t.Errorf("SavingsUSD = %.2f, want >= %.2f", s.SavingsUSD, tc.minSave)
			}
			if s.Evidence.Math == "" || s.Evidence.Numbers["actual_usd"] <= 0 {
				t.Errorf("evidence incomplete: %+v", s.Evidence)
			}
		})
	}
}

// TestContextWindowOf pins the per-session context inference (operator
// feedback 2026-06-10): [1m] tag wins, gpt-5 family = 272K, plain claude =
// 200K, and an untagged session whose OBSERVED window exceeds the model-tag
// base snaps up to the smallest standard tier that contains it (claude-code
// 1M-beta transcripts record the plain model id).
func TestContextWindowOf(t *testing.T) {
	mk := func(model string, maxWin int64) *SessionFacts {
		return &SessionFacts{Model: model, Rows: []TurnFact{{Model: model, Input: maxWin}}}
	}
	cases := []struct {
		name string
		s    *SessionFacts
		want int64
	}{
		{"explicit 1m tag", mk("claude-opus-4-8[1m]", 50_000), 1_000_000},
		{"gpt-5 family", mk("gpt-5.4", 50_000), 272_000},
		{"plain claude small", mk("claude-opus-4-7", 120_000), 200_000},
		{"untagged 1M-beta snaps up", mk("claude-opus-4-7", 646_000), 1_000_000},
		{"untagged mid snaps to 272K", mk("claude-sonnet-4-6", 250_000), 272_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := contextWindowOf(tc.s); got != tc.want {
				t.Errorf("contextWindowOf = %d, want %d", got, tc.want)
			}
		})
	}
	// Threshold composition: 1M ctx × 0.5 = 500K; floor guards small ctx.
	th := DefaultThresholds()
	if got := balloonThresholdFor(mk("claude-opus-4-7", 646_000), th); got != 500_000 {
		t.Errorf("balloon threshold for inferred-1M session = %d, want 500000", got)
	}
	if got := balloonThresholdFor(mk("claude-opus-4-7", 50_000), th); got != 100_000 {
		t.Errorf("balloon threshold for 200K session = %d, want 100000 (0.5×200K)", got)
	}
}

func TestDetectSessionBalloon_FastSessionPricedThroughPriceFn(t *testing.T) {
	// Two identical balloon sessions, one entirely fast: the fast session's
	// savings must be ~2× the standard one's (calibration T2 — the price fn
	// owns the multiplier, the detector never re-derives rates).
	std := growingSession("sess-std", 80, 40_000, 8_000)
	fast := growingSession("sess-fast", 80, 40_000, 8_000)
	for i := range fast.Rows {
		fast.Rows[i].Fast = true
	}
	got := detectSessionBalloon(testFacts(std, fast), DefaultThresholds())
	if len(got) != 2 {
		t.Fatalf("suggestions = %d, want 2", len(got))
	}
	bySess := map[string]float64{}
	for _, s := range got {
		bySess[s.ScopeID] = s.SavingsUSD
	}
	ratio := bySess["sess-fast"] / bySess["sess-std"]
	if ratio < 1.9 || ratio > 2.1 {
		t.Errorf("fast/std savings ratio = %.2f, want ~2.0 (fast flag must flow through PriceFn)", ratio)
	}
}

func TestDetectIdleRecache(t *testing.T) {
	t0 := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	s := SessionFacts{ID: "sess-idle", Model: "claude-opus-4-7"}
	// Row 0, then a 10-minute gap into a full re-cache (cr=0, cc=400K),
	// then another, then a warm row (cr>0) after a gap — NOT an event.
	s.Rows = []TurnFact{
		{TS: t0, Input: 1000, Output: 100, CacheRead: 50_000},
		{TS: t0.Add(10 * time.Minute), Input: 1000, Output: 100, CacheCreation: 400_000},
		{TS: t0.Add(25 * time.Minute), Input: 1000, Output: 100, CacheCreation: 400_000},
		{TS: t0.Add(40 * time.Minute), Input: 1000, Output: 100, CacheRead: 400_000, CacheCreation: 2000},
		{TS: t0.Add(41 * time.Minute), Input: 1000, Output: 100, CacheRead: 400_000},
	}
	got := detectIdleRecache(testFacts(s), DefaultThresholds())
	if len(got) != 1 {
		t.Fatalf("suggestions = %d, want 1", len(got))
	}
	if n := got[0].Evidence.Numbers["recache_events"]; n != 2 {
		t.Errorf("recache_events = %v, want 2 (warm post-gap row must not count)", n)
	}
	// 2 × 400K × $6.25/M = $5.00 writes; savings = ×0.9 = $4.50
	if got[0].SavingsUSD < 4.4 || got[0].SavingsUSD > 4.6 {
		t.Errorf("SavingsUSD = %.2f, want ~4.50", got[0].SavingsUSD)
	}
}

func TestDetectTrivialModelOverprovisioning(t *testing.T) {
	t0 := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	mk := func(id, model string, in, out int64) SessionFacts {
		s := SessionFacts{ID: id, Model: model}
		for i := 0; i < 5; i++ {
			s.Rows = append(s.Rows, TurnFact{TS: t0.Add(time.Duration(i) * time.Minute), Model: model, Input: in / 5, Output: out / 5})
		}
		return s
	}
	// price fn: opus expensive, sonnet cheap (model-sensitive — flatPrice
	// is model-blind, so use a custom fn here).
	price := func(model string, b cost.TokenBundle) (float64, bool) {
		rate := 5.0
		if strings.Contains(model, "sonnet") {
			rate = 1.0
		}
		return (float64(b.Input)*rate + float64(b.Output)*rate*5) / 1e6, true
	}
	f := testFacts(
		mk("sess-trivial-1", "claude-opus-4-7", 10_000, 1_000),
		mk("sess-trivial-2", "claude-opus-4-8", 8_000, 800),
		mk("sess-big-opus", "claude-opus-4-7", 500_000, 50_000), // not trivial
		mk("sess-sonnet", "claude-sonnet-4-6", 9_000, 900),      // not opus
	)
	f.Price = price
	got := detectTrivialModelOverprovisioning(f, DefaultThresholds())
	if len(got) != 1 {
		t.Fatalf("suggestions = %d, want 1 aggregate", len(got))
	}
	if n := got[0].Evidence.Numbers["sessions"]; n != 2 {
		t.Errorf("sessions = %v, want 2 (big-opus and sonnet sessions excluded)", n)
	}
	if got[0].Scope != ScopeGlobal {
		t.Errorf("Scope = %q, want global aggregate", got[0].Scope)
	}
}

func TestRunFiltersAndRanks(t *testing.T) {
	// Engine floors: a suggestion below MinSavingsUSD is dropped; survivors
	// sort by savings desc. Use the real Run against a seeded DB.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(ctx, db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	st := store.New(database)
	if _, err := st.UpsertProject(ctx, "/tmp/proj", ""); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.UpsertSession(ctx, models.Session{ID: "sess-bal", ProjectID: 1, Tool: "claude-code", Model: "claude-opus-4-7", StartedAt: now.Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// 60 growing turns crossing 150K with plenty after.
	var evs []models.TokenEvent
	for i := 0; i < 60; i++ {
		win := int64(60_000 + i*12_000)
		evs = append(evs, models.TokenEvent{
			SourceFile: "/x/t.jsonl", SourceEventID: "e" + string(rune('A'+i/26)) + string(rune('a'+i%26)),
			SessionID: "sess-bal", Timestamp: now.Add(time.Duration(i-70) * time.Minute),
			Tool: "claude-code", Model: "claude-opus-4-7",
			InputTokens: 1000, OutputTokens: 800, CacheReadTokens: win - 3000, CacheCreationTokens: 2000,
			Source: models.TokenSourceJSONL, Reliability: models.ReliabilityApproximate,
		})
	}
	if _, err := st.InsertTokenEvents(ctx, evs); err != nil {
		t.Fatal(err)
	}

	rep, err := Run(ctx, database, Options{WindowDays: 14, CostEngine: cost.NewEngine(config.IntelligenceConfig{})})
	if err != nil {
		t.Fatal(err)
	}
	if rep.SessionsScanned != 1 {
		t.Fatalf("SessionsScanned = %d, want 1", rep.SessionsScanned)
	}
	if len(rep.Suggestions) == 0 {
		t.Fatal("want at least the balloon suggestion")
	}
	if rep.Suggestions[0].Detector != "session_balloon" {
		t.Errorf("top suggestion = %q, want session_balloon", rep.Suggestions[0].Detector)
	}
	for i := 1; i < len(rep.Suggestions); i++ {
		if rep.Suggestions[i].SavingsUSD > rep.Suggestions[i-1].SavingsUSD {
			t.Errorf("suggestions not sorted by savings desc at %d", i)
		}
	}
	for _, s := range rep.Suggestions {
		if s.SavingsMin == 0 && s.SavingsUSD < 1.0 {
			t.Errorf("floor violated: %+v", s)
		}
	}
}

func TestLoadFacts_ProxyJSONLDedup(t *testing.T) {
	// A codex-shaped session captured by BOTH proxy and JSONL: the JSONL
	// twin (different event id, same shape) must be dropped; a unique JSONL
	// row survives.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(ctx, db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	st := store.New(database)
	if _, err := st.UpsertProject(ctx, "/tmp/proj", ""); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.UpsertSession(ctx, models.Session{ID: "sess-dx", ProjectID: 1, Tool: "codex", Model: "gpt-5.4", StartedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertAPITurn(ctx, models.APITurn{
		SessionID: "sess-dx", ProjectID: 1, Timestamp: now.Add(-30 * time.Minute),
		Provider: "openai", Model: "gpt-5.4", RequestID: "resp_abc",
		InputTokens: 7510, OutputTokens: 66, CacheReadTokens: 4480,
	}); err != nil {
		t.Fatal(err)
	}
	evs := []models.TokenEvent{
		{ // shape twin of the proxy turn — must be deduped
			SourceFile: "/x/r.jsonl", SourceEventID: "tk:r:L1", SessionID: "sess-dx",
			Timestamp: now.Add(-30*time.Minute + 10*time.Second), Tool: "codex", Model: "gpt-5.4",
			InputTokens: 7510, OutputTokens: 66, CacheReadTokens: 4480,
			Source: models.TokenSourceJSONL, Reliability: models.ReliabilityApproximate,
		},
		{ // unique JSONL turn — must survive
			SourceFile: "/x/r.jsonl", SourceEventID: "tk:r:L2", SessionID: "sess-dx",
			Timestamp: now.Add(-20 * time.Minute), Tool: "codex", Model: "gpt-5.4",
			InputTokens: 9000, OutputTokens: 120, CacheReadTokens: 6000,
			Source: models.TokenSourceJSONL, Reliability: models.ReliabilityApproximate,
		},
		// padding so the session passes minSessionRows
		{SourceFile: "/x/r.jsonl", SourceEventID: "tk:r:L3", SessionID: "sess-dx", Timestamp: now.Add(-19 * time.Minute), Tool: "codex", Model: "gpt-5.4", InputTokens: 100, OutputTokens: 10, Source: models.TokenSourceJSONL, Reliability: models.ReliabilityApproximate},
		{SourceFile: "/x/r.jsonl", SourceEventID: "tk:r:L4", SessionID: "sess-dx", Timestamp: now.Add(-18 * time.Minute), Tool: "codex", Model: "gpt-5.4", InputTokens: 110, OutputTokens: 11, Source: models.TokenSourceJSONL, Reliability: models.ReliabilityApproximate},
		{SourceFile: "/x/r.jsonl", SourceEventID: "tk:r:L5", SessionID: "sess-dx", Timestamp: now.Add(-17 * time.Minute), Tool: "codex", Model: "gpt-5.4", InputTokens: 120, OutputTokens: 12, Source: models.TokenSourceJSONL, Reliability: models.ReliabilityApproximate},
	}
	if _, err := st.InsertTokenEvents(ctx, evs); err != nil {
		t.Fatal(err)
	}

	f, err := LoadFacts(ctx, database, Options{WindowDays: 14}.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(f.Sessions))
	}
	rows := f.Sessions[0].Rows
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5 (1 proxy + 4 jsonl; shape twin deduped): %+v", len(rows), rows)
	}
	proxyN, in7510 := 0, 0
	for _, r := range rows {
		if r.Source == "proxy" {
			proxyN++
		}
		if r.Input == 7510 {
			in7510++
		}
	}
	if proxyN != 1 || in7510 != 1 {
		t.Errorf("proxy rows = %d (want 1), rows with input 7510 = %d (want 1 — the twin must be dropped)", proxyN, in7510)
	}
}
