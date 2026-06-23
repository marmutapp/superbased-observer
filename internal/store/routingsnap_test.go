package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/routing"
)

// seedTurn inserts one api_turn with the routing-relevant fields.
func seedTurn(t *testing.T, s *Store, sid, model string, ts time.Time, costUSD float64, status int, totalMS, cacheRead int64) {
	t.Helper()
	_, err := s.InsertAPITurn(context.Background(), models.APITurn{
		SessionID: sid, Timestamp: ts, Provider: "anthropic", Model: model,
		InputTokens: 1000, OutputTokens: 100, CacheReadTokens: cacheRead,
		CostUSD: costUSD, HTTPStatus: status, TotalResponseMS: totalMS,
	})
	if err != nil {
		t.Fatalf("InsertAPITurn: %v", err)
	}
}

func testRefresher(t *testing.T, s *Store, p routing.Policy, now time.Time) *RoutingRefresher {
	t.Helper()
	r := NewRoutingRefresher(s, p, routing.NewTierResolver(), nil)
	r.now = func() time.Time { return now }
	return r
}

// TestRoutingRefresher_BudgetBurnScopes pins §R14 scope aggregation:
// global sums everything in-window; tier scopes resolve through the
// tier table; out-of-window spend is excluded.
func TestRoutingRefresher_BudgetBurnScopes(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	now := time.Now().UTC()
	seedTurn(t, s, "s1", "claude-opus-4-8", now.Add(-time.Hour), 4.0, 0, 0, 0)
	seedTurn(t, s, "s1", "claude-haiku-4-5", now.Add(-2*time.Hour), 1.0, 0, 0, 0)
	seedTurn(t, s, "s1", "claude-opus-4-8", now.Add(-48*time.Hour), 100.0, 0, 0, 0) // outside day window

	p := routing.Policy{
		BudgetScopes: []routing.BudgetScope{
			{Scope: "global", LimitUSD: 10, Window: "day", Bands: routing.DefaultBudgetBands},
			{Scope: "tier:opus-class", LimitUSD: 5, Window: "day", Bands: routing.DefaultBudgetBands},
		},
	}
	r := testRefresher(t, s, p, now)
	if err := r.RefreshNow(context.Background()); err != nil {
		t.Fatalf("RefreshNow: %v", err)
	}
	snap := r.Current()
	if snap == nil || len(snap.BudgetBurn) != 2 {
		t.Fatalf("snapshot burn = %+v", snap)
	}
	if got := snap.BudgetBurn[0].SpentUSD; got < 4.99 || got > 5.01 {
		t.Errorf("global day spend = %v, want 5.0 (48h-old row excluded)", got)
	}
	if got := snap.BudgetBurn[1].SpentUSD; got < 3.99 || got > 4.01 {
		t.Errorf("opus-tier day spend = %v, want 4.0", got)
	}
}

// TestRoutingRefresher_BreakerLifecycle pins the §R12.3 state machine:
// an error storm opens the breaker, cooldown half-opens it, a natural
// success after opening closes it.
func TestRoutingRefresher_BreakerLifecycle(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	base := time.Now().UTC()
	for i := 0; i < 4; i++ {
		seedTurn(t, s, "s1", "claude-haiku-4-5", base.Add(-time.Duration(i)*time.Minute), 0, 429, 0, 0)
	}

	r := testRefresher(t, s, routing.Policy{}, base)
	if err := r.RefreshNow(context.Background()); err != nil {
		t.Fatalf("RefreshNow: %v", err)
	}
	if got := r.Current().Health["claude-haiku-4-5"]; got != routing.HealthOpen {
		t.Fatalf("after 429 storm: health = %q, want open", got)
	}

	// Past the cooldown with no new traffic: half-open (awaiting the
	// natural probe request).
	r.now = func() time.Time { return base.Add(2 * breakerCooldown) }
	if err := r.RefreshNow(context.Background()); err != nil {
		t.Fatalf("RefreshNow: %v", err)
	}
	if got := r.Current().Health["claude-haiku-4-5"]; got != routing.HealthHalfOpen {
		t.Fatalf("after cooldown: health = %q, want half_open", got)
	}

	// A success lands after the breaker opened: closed (healthy = no
	// entry in the map).
	seedTurn(t, s, "s1", "claude-haiku-4-5", base.Add(2*breakerCooldown), 0, 200, 0, 0)
	r.now = func() time.Time { return base.Add(3 * breakerCooldown) }
	if err := r.RefreshNow(context.Background()); err != nil {
		t.Fatalf("RefreshNow: %v", err)
	}
	if got, present := r.Current().Health["claude-haiku-4-5"]; present {
		t.Fatalf("after successful probe: health = %q, want absent (healthy)", got)
	}
}

// TestRoutingRefresher_DegradedBelowOpenRate pins the degraded band:
// error rate in [0.25, 0.5) marks degraded, not open.
func TestRoutingRefresher_DegradedBelowOpenRate(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	now := time.Now().UTC()
	seedTurn(t, s, "s1", "claude-opus-4-8", now.Add(-time.Minute), 0, 529, 0, 0)
	for i := 0; i < 3; i++ {
		seedTurn(t, s, "s1", "claude-opus-4-8", now.Add(-time.Duration(i+2)*time.Minute), 0, 200, 0, 0)
	}
	r := testRefresher(t, s, routing.Policy{}, now)
	if err := r.RefreshNow(context.Background()); err != nil {
		t.Fatalf("RefreshNow: %v", err)
	}
	if got := r.Current().Health["claude-opus-4-8"]; got != routing.HealthDegraded {
		t.Errorf("health = %q, want degraded (1/4 errors)", got)
	}
}

// TestRoutingRefresher_LatencyP75AndCacheReads pins the remaining
// fast/slow signals: per-model p75 and the most recent per-session
// warm-prefix size.
func TestRoutingRefresher_LatencyP75AndCacheReads(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	now := time.Now().UTC()
	for i, ms := range []int64{100, 200, 300, 400} {
		seedTurn(t, s, "s1", "claude-opus-4-8", now.Add(-time.Duration(i+1)*time.Minute), 0, 0, ms, 0)
	}
	seedTurn(t, s, "s2", "claude-haiku-4-5", now.Add(-10*time.Minute), 0, 0, 0, 111)
	seedTurn(t, s, "s2", "claude-haiku-4-5", now.Add(-5*time.Minute), 0, 0, 0, 999)

	r := testRefresher(t, s, routing.Policy{}, now)
	if err := r.RefreshNow(context.Background()); err != nil {
		t.Fatalf("RefreshNow: %v", err)
	}
	snap := r.Current()
	if got := snap.LatencyP75Ms["claude-opus-4-8"]; got != 400 {
		t.Errorf("p75 = %d, want 400 (index 3 of sorted [100 200 300 400])", got)
	}
	if got := snap.SessionCacheRead["s2"]; got != 999 {
		t.Errorf("session cache read = %d, want the most recent turn's 999", got)
	}
}

// TestRoutingRefresher_WindowPressure pins the §R15 heuristic: nil when
// the feature is off; a recent 429 marks projected exhaustion.
func TestRoutingRefresher_WindowPressure(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	now := time.Now().UTC()
	seedTurn(t, s, "s1", "claude-opus-4-8", now.Add(-5*time.Minute), 0, 429, 0, 0)

	off := testRefresher(t, s, routing.Policy{}, now)
	if err := off.RefreshNow(context.Background()); err != nil {
		t.Fatalf("RefreshNow: %v", err)
	}
	if off.Current().Window != nil {
		t.Error("window state present with the feature off")
	}

	on := testRefresher(t, s, routing.Policy{
		RateLimit: routing.RateLimitPolicy{Enabled: true, HeadroomPct: 15},
	}, now)
	if err := on.RefreshNow(context.Background()); err != nil {
		t.Fatalf("RefreshNow: %v", err)
	}
	win := on.Current().Window
	if win == nil || !win.ProjectedExhaustion {
		t.Errorf("window = %+v, want projected exhaustion after a 5-minute-old 429", win)
	}
}

// TestRoutingRefresher_StalenessFailsOpen pins §R9.2: a snapshot older
// than the staleness horizon comes back Stale, and the engine fails
// open on it.
func TestRoutingRefresher_StalenessFailsOpen(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	now := time.Now().UTC()
	r := testRefresher(t, s, routing.Policy{}, now)
	if err := r.RefreshNow(context.Background()); err != nil {
		t.Fatalf("RefreshNow: %v", err)
	}
	if snap := r.Current(); snap.Stale {
		t.Fatal("fresh snapshot marked stale")
	}
	r.now = func() time.Time { return now.Add(2 * r.staleAfter) }
	snap := r.Current()
	if !snap.Stale {
		t.Fatal("aged snapshot not marked stale")
	}
	p, _ := routing.TemplateByName("value")
	d := routing.Decide(p, snap, routing.DecisionInput{Shape: routing.TurnShape{Model: "claude-opus-4-8"}})
	if d.Changed || len(d.ReasonCodes) != 1 || d.ReasonCodes[0] != routing.ReasonFailOpen {
		t.Errorf("decision on stale snapshot = %+v, want fail_open no-change", d)
	}
}

// TestRoutingRefresher_NilBeforeFirstRefresh pins the cold-start
// contract: Current() is nil until the first publish (the proxy seam
// fails open on nil).
func TestRoutingRefresher_NilBeforeFirstRefresh(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	r := testRefresher(t, s, routing.Policy{}, time.Now().UTC())
	if r.Current() != nil {
		t.Error("Current() non-nil before first refresh")
	}
}

// seedAction inserts one action row for the activity tests.
func seedAction(t *testing.T, s *Store, sid string, pid int64, actionType, target string, ts time.Time, sidechain bool) {
	t.Helper()
	side := int64(0)
	if sidechain {
		side = 1
	}
	_ = side
	_, err := s.InsertActions(context.Background(), []models.Action{{
		SessionID: sid, ProjectID: pid, Timestamp: ts, ActionType: actionType, Target: target,
		Success: true, IsSidechain: sidechain, Tool: models.ToolClaudeCode,
		SourceFile: "f.jsonl", SourceEventID: sid + actionType + ts.Format(time.RFC3339Nano),
	}})
	if err != nil {
		t.Fatalf("InsertActions: %v", err)
	}
}

// TestRoutingRefresher_SessionActivity pins the §R8.1 boundary
// resolution: recent-action windows with command classes, the plan
// phase marker, path-class hit FLAGS (+ hash, never paths), scope
// keys, and the §R8.3 lag detection.
func TestRoutingRefresher_SessionActivity(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	pid, _ := s.UpsertProject(ctx, "/home/u/projects/acme-api", "")
	if err := s.UpsertSession(ctx, models.Session{
		ID: "act-1", ProjectID: pid, Tool: models.ToolClaudeCode, StartedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	seedAction(t, s, "act-1", pid, models.ActionReadFile, "infra/deploy.tf", now.Add(-4*time.Minute), false)
	seedAction(t, s, "act-1", pid, models.ActionRunCommand, "go test ./...", now.Add(-3*time.Minute), false)
	seedAction(t, s, "act-1", pid, models.ActionPermissionMode, "plan", now.Add(-2*time.Minute), false)
	seedTurn(t, s, "act-1", "claude-opus-4-8", now.Add(-90*time.Second), 0, 0, 0, 0)

	p := routing.Policy{PathClasses: map[string][]string{"secrets": {"**/.env*", "infra/**"}}}
	r := testRefresher(t, s, p, now)
	if err := r.RefreshNow(ctx); err != nil {
		t.Fatalf("RefreshNow: %v", err)
	}
	act, ok := r.Current().Sessions["act-1"]
	if !ok {
		t.Fatal("session activity missing")
	}
	if len(act.RecentActions) != 3 {
		t.Fatalf("recent actions = %d, want 3", len(act.RecentActions))
	}
	if act.RecentActions[1].CommandClass != routing.CommandTest {
		t.Errorf("command class = %q, want test", act.RecentActions[1].CommandClass)
	}
	if act.ClientPhase != "plan" {
		t.Errorf("phase = %q, want plan", act.ClientPhase)
	}
	if len(act.PathClassHits) != 1 || act.PathClassHits[0] != "secrets" {
		t.Errorf("path-class hits = %v, want [secrets]", act.PathClassHits)
	}
	if act.PathClassHitsHash == "" {
		t.Error("path-class hit hash empty")
	}
	for _, hit := range act.PathClassHits {
		if strings.Contains(hit, "/") || strings.Contains(hit, ".tf") {
			t.Errorf("path content leaked into hits: %q", hit)
		}
	}
	if act.Project != "acme-api" {
		t.Errorf("project = %q, want acme-api (basename)", act.Project)
	}
	wantScopes := map[string]bool{"global": true, "project:acme-api": true, "tool:claude-code": true}
	for _, k := range act.ScopeKeys {
		if !wantScopes[k] {
			t.Errorf("unexpected scope key %q", k)
		}
	}
	if act.ActionsLagged {
		t.Error("fresh window marked lagged")
	}
	if act.TurnCount != 1 {
		t.Errorf("turn count = %d, want 1", act.TurnCount)
	}
}

// TestRoutingRefresher_LagDetection pins §R8.3: a session whose newest
// turn is far newer than its newest action marks lagged.
func TestRoutingRefresher_LagDetection(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	pid, _ := s.UpsertProject(ctx, "/tmp/lagged", "")
	if err := s.UpsertSession(ctx, models.Session{
		ID: "lag-1", ProjectID: pid, Tool: models.ToolClaudeCode, StartedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	seedAction(t, s, "lag-1", pid, models.ActionReadFile, "a.go", now.Add(-20*time.Minute), false)
	seedTurn(t, s, "lag-1", "claude-opus-4-8", now.Add(-time.Minute), 0, 0, 0, 0)

	r := testRefresher(t, s, routing.Policy{}, now)
	if err := r.RefreshNow(ctx); err != nil {
		t.Fatalf("RefreshNow: %v", err)
	}
	act := r.Current().Sessions["lag-1"]
	if !act.ActionsLagged {
		t.Error("19-minute action lag not detected")
	}
	// The lagged window drives the classifier to unknown → no routing.
	p, _ := routing.TemplateByName("value")
	d := routing.Decide(p, r.Current(), routing.DecisionInput{
		Shape:   routing.TurnShape{Model: "claude-opus-4-8"},
		Session: routing.SessionState{RecentActions: act.RecentActions, ActionsLagged: act.ActionsLagged},
	})
	if d.Changed || d.TurnKind != routing.TurnUnknown {
		t.Errorf("lagged session decision = %+v, want unknown no-change", d)
	}
}

// TestRouterDecisionReadAndPrune pins the §R17 read seam + the
// decision-log retention sweep: insert → select (joined project/tool)
// → by-id → prune old.
func TestRouterDecisionReadAndPrune(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	pid, _ := s.UpsertProject(ctx, "/tmp/dec-read", "")
	if err := s.UpsertSession(ctx, models.Session{
		ID: "dec-1", ProjectID: pid, Tool: models.ToolClaudeCode, StartedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	rows := []RouterDecisionRow{
		{
			SessionID: "dec-1", Timestamp: now.Add(-time.Minute), Mode: "advise", Channel: "B",
			OriginalModel: "claude-opus-4-8", SelectedModel: "claude-haiku-4-5",
			TurnKind: "read_only", PolicyName: "value", PolicyHash: "abc",
			ReasonCodes: []string{"overpowered_read"}, EstSavingsUSD: 0.4, EstimateVersion: "p1-v1",
		},
		{
			SessionID: "dec-1", Timestamp: now.AddDate(0, 0, -200), Mode: "advise", Channel: "B",
			OriginalModel: "claude-opus-4-8", SelectedModel: "claude-opus-4-8",
			TurnKind: "edit", PolicyName: "value", PolicyHash: "abc", EstimateVersion: "p1-v1",
		},
	}
	if err := s.InsertRouterDecisions(ctx, rows); err != nil {
		t.Fatalf("InsertRouterDecisions: %v", err)
	}

	got, err := s.SelectRouterDecisions(ctx, now.AddDate(0, 0, -365), 0)
	if err != nil || len(got) != 2 {
		t.Fatalf("SelectRouterDecisions = %d rows, err=%v", len(got), err)
	}
	if got[0].ProjectRoot != "/tmp/dec-read" || got[0].Tool != models.ToolClaudeCode {
		t.Errorf("join fields: %+v", got[0])
	}
	if len(got[0].ReasonCodes) != 1 || got[0].ReasonCodes[0] != "overpowered_read" {
		t.Errorf("reason codes: %v", got[0].ReasonCodes)
	}

	byID, ok, err := s.SelectRouterDecisionByID(ctx, got[0].ID)
	if err != nil || !ok || byID.SelectedModel != "claude-haiku-4-5" {
		t.Fatalf("ByID = %+v ok=%v err=%v", byID, ok, err)
	}
	if _, ok, _ := s.SelectRouterDecisionByID(ctx, 99999); ok {
		t.Error("phantom decision found")
	}

	n, err := s.PruneRouterDecisions(ctx, 180)
	if err != nil || n != 1 {
		t.Fatalf("prune = %d err=%v, want the 200-day-old row", n, err)
	}
	if n, _ := s.PruneRouterDecisions(ctx, 180); n != 0 {
		t.Errorf("second prune deleted %d, want idempotent 0", n)
	}
	if n, _ := s.PruneRouterDecisions(ctx, 0); n != 0 {
		t.Errorf("retention 0 deleted %d, want disabled no-op", n)
	}
}

// TestBuildAdviseShadowReport pins the §R18.2 promotion surface:
// would-have aggregation, parity-vs-flag split, hold attribution, the
// nil-evidence maximum-caution rule, and the mechanical gate read.
func TestBuildAdviseShadowReport(t *testing.T) {
	t.Parallel()
	tiers := routing.NewTierResolver().Table()
	rows := []RouterDecisionDetail{
		{
			Mode: "advise", OriginalModel: "claude-opus-4-8", SelectedModel: "claude-haiku-4-5",
			TurnKind: "read_only", EstSavingsUSD: 0.4, CacheForfeitUSD: 0.1,
		},
		{
			Mode: "advise", OriginalModel: "claude-opus-4-8", SelectedModel: "claude-sonnet-4-6",
			TurnKind: "subagent", EstSavingsUSD: 0.2,
		},
		{
			Mode: "advise", OriginalModel: "claude-opus-4-8", SelectedModel: "claude-opus-4-8",
			TurnKind: "edit", ReasonCodes: []string{"cache_hold"},
		},
		{
			Mode: "enforce", Applied: true, OriginalModel: "claude-opus-4-8",
			SelectedModel: "claude-haiku-4-5", TurnKind: "read_only", EstSavingsUSD: 9.9,
		},
	}
	evidence := map[routing.EvidenceKey]string{
		{Kind: routing.TurnReadOnly, Tier: routing.TierHaikuClass}: routing.EvidenceParity,
	}
	rep := BuildAdviseShadowReport(rows, evidence, tiers, 30)
	if rep.AdviseDecisions != 3 || rep.WouldReroute != 2 {
		t.Fatalf("counts: %+v", rep)
	}
	if rep.WouldSaveUSD < 0.599 || rep.WouldSaveUSD > 0.601 || rep.CacheForfeitUSD != 0.1 {
		t.Errorf("dollars: %+v", rep)
	}
	if rep.EvidenceBackedMoves != 1 || rep.QualityFlags != 1 || rep.QualityByKind["subagent"] != 1 {
		t.Errorf("evidence split: %+v", rep)
	}
	if rep.HoldsByReason["cache_hold"] != 1 {
		t.Errorf("holds: %+v", rep.HoldsByReason)
	}
	if rep.ReadyToPromote {
		t.Error("gate read true with quality flags present and n<50")
	}
	if rep.MinDecisions != shadowMinDecisions {
		t.Errorf("min_decisions = %d, want the gate's floor %d (the dashboard ladder renders it)", rep.MinDecisions, shadowMinDecisions)
	}

	// Nil evidence flags every reroute — maximum caution.
	repNil := BuildAdviseShadowReport(rows, nil, tiers, 30)
	if repNil.QualityFlags != 2 || repNil.EvidenceBackedMoves != 0 {
		t.Errorf("nil-evidence caution: %+v", repNil)
	}
}
