package handoffsvc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/handoff"
	"github.com/marmutapp/superbased-observer/internal/integration"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

type fakeSubstrate struct {
	sub      store.HandoffSubstrate
	shape    store.PredictShape
	inserted []store.HandoffRecord
}

func (f *fakeSubstrate) LoadHandoffSubstrate(context.Context, string) (store.HandoffSubstrate, error) {
	return f.sub, nil
}

func (f *fakeSubstrate) LoadSessionShape(context.Context, string) (store.PredictShape, error) {
	return f.shape, nil
}

func (f *fakeSubstrate) InsertHandoff(_ context.Context, r store.HandoffRecord) (int64, error) {
	f.inserted = append(f.inserted, r)
	return int64(len(f.inserted)), nil
}

// fakeAdapter implements adapter.Adapter + TranscriptReader for the
// claude-code tool name.
type fakeAdapter struct {
	name string
	msgs []models.TranscriptMessage
}

func (f *fakeAdapter) Name() string              { return f.name }
func (f *fakeAdapter) WatchPaths() []string      { return nil }
func (f *fakeAdapter) IsSessionFile(string) bool { return false }
func (f *fakeAdapter) ParseSessionFile(context.Context, string, int64) (adapter.ParseResult, error) {
	return adapter.ParseResult{}, nil
}

func (f *fakeAdapter) ReadTranscript(context.Context, models.Session, []string) ([]models.TranscriptMessage, error) {
	return f.msgs, nil
}

func testDeps(t *testing.T, root string) (Deps, *fakeSubstrate) {
	t.Helper()
	msgs := []models.TranscriptMessage{
		{Index: 0, Role: models.TranscriptUser, Time: time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC), Text: "build it"},
		{Index: 1, Role: models.TranscriptAssistant, Time: time.Date(2026, 7, 3, 10, 1, 0, 0, time.UTC), Text: "built", ToolCalls: []models.ToolCallRef{{ID: "t1", Name: "Edit", Resolved: true, ResultExcerpt: "ok"}}},
	}
	fs := &fakeSubstrate{
		sub: store.HandoffSubstrate{
			Session:     models.Session{ID: "s1", Tool: "claude-code", Model: "opus-4-8"},
			ProjectRoot: root,
			Files:       []handoff.FileFact{{Path: "a.go", Edits: 1}},
		},
		shape: store.PredictShape{Model: "opus-4-8", PrefixTokens: 50_000},
	}
	deps := Deps{
		Store:    fs,
		Cfg:      config.Default().Handoff,
		Adapters: []adapter.Adapter{&fakeAdapter{name: "claude-code", msgs: msgs}},
		Price:    func(_ string, tok int64) float64 { return float64(tok) / 100_000 },
		Now:      func() time.Time { return time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC) },
	}
	return deps, fs
}

func TestBuild_EndToEnd(t *testing.T) {
	root := t.TempDir()
	deps, fs := testDeps(t, root)
	res, err := Build(context.Background(), deps, Request{SessionID: "s1", TargetTool: "codex"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.CarryUsed != handoff.CarryDistilledTail {
		t.Errorf("default carry = %s", res.CarryUsed)
	}
	if !strings.Contains(res.Doc, "superbased-handoff "+res.ShortID) {
		t.Error("doc must carry the short-id marker")
	}
	if !strings.Contains(res.Doc, "build it") {
		t.Error("doc must carry the mission quote")
	}
	if res.DocPath == "" || !strings.HasPrefix(res.DocPath, root) {
		t.Errorf("doc path = %q, want under project root", res.DocPath)
	}
	if _, err := os.Stat(res.DocPath); err != nil {
		t.Errorf("doc file must exist: %v", err)
	}
	if len(fs.inserted) != 1 {
		t.Fatalf("want 1 handoffs row, got %d", len(fs.inserted))
	}
	rec := fs.inserted[0]
	if rec.TargetTool != "codex" || rec.Delivery != "file" || rec.ForkAnchorHash == "" {
		t.Errorf("record = %+v", rec)
	}
	// Estimate: 4 rows incl. fork-scaled full (context tokens known).
	var sawFull bool
	for _, r := range res.Estimate.Rows {
		if r.Mode == handoff.CarryFull {
			sawFull = true
			if r.Tokens != 50_000 {
				t.Errorf("full tokens = %d, want 50000", r.Tokens)
			}
		}
	}
	if !sawFull {
		t.Error("estimate must include the full row when context tokens are known")
	}
}

func TestBuild_HookDeliveryArms(t *testing.T) {
	root := t.TempDir()
	deps, fs := testDeps(t, root)
	// claude-code is the only tool that declares the inject_hook lane; the
	// source session in testDeps is already claude-code.
	res, err := Build(context.Background(), deps, Request{
		SessionID:  "s1",
		TargetTool: "claude-code",
		Delivery:   integration.InjectHook,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Delivery != integration.InjectHook {
		t.Errorf("result delivery = %q, want hook", res.Delivery)
	}
	// TTL default 240m off the injected Now (2026-07-04T00:00Z).
	want := time.Date(2026, 7, 4, 4, 0, 0, 0, time.UTC)
	if !res.HookExpiresAt.Equal(want) {
		t.Errorf("hook expiry = %v, want %v", res.HookExpiresAt, want)
	}
	if len(fs.inserted) != 1 {
		t.Fatalf("want 1 row, got %d", len(fs.inserted))
	}
	rec := fs.inserted[0]
	if rec.Delivery != "hook" || rec.HookExpiresAt.IsZero() || rec.ProjectRoot != root {
		t.Errorf("armed row wrong: %+v", rec)
	}
	if rec.DeliveryRef != res.DocPath || res.DocPath == "" {
		t.Errorf("delivery_ref must be the on-disk doc path: %q vs %q", rec.DeliveryRef, res.DocPath)
	}
}

func TestBuild_HookDeliveryUnsupportedLaneErrors(t *testing.T) {
	deps, _ := testDeps(t, t.TempDir())
	// codex does not declare the inject_hook lane → honest error.
	_, err := Build(context.Background(), deps, Request{
		SessionID:  "s1",
		TargetTool: "codex",
		Delivery:   integration.InjectHook,
	})
	if err == nil || !strings.Contains(err.Error(), "does not support") {
		t.Fatalf("unsupported hook lane must error naming the gap, got %v", err)
	}
}

func TestBuild_DryRunWritesNothing(t *testing.T) {
	root := t.TempDir()
	deps, fs := testDeps(t, root)
	res, err := Build(context.Background(), deps, Request{SessionID: "s1", DryRun: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.DocPath != "" || len(fs.inserted) != 0 {
		t.Errorf("dry run must not write: path=%q rows=%d", res.DocPath, len(fs.inserted))
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Errorf("dry run left files: %v", entries)
	}
}

// TestBuild_DegradesToMetadata pins the honest downgrade: a tool with no
// reader (cowork is classified full but its reader is a P2 tranche) falls
// back to metadata carry with a reason naming the gap.
func TestBuild_DegradesToMetadata(t *testing.T) {
	root := t.TempDir()
	deps, fs := testDeps(t, root)
	fs.sub.Session.Tool = "cowork"
	deps.Adapters = nil
	res, err := Build(context.Background(), deps, Request{SessionID: "s1"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.CarryUsed != handoff.CarryMetadata {
		t.Errorf("carry = %s, want metadata", res.CarryUsed)
	}
	if !strings.Contains(res.DegradeReason, "not implemented") {
		t.Errorf("degrade reason must name the missing capability: %q", res.DegradeReason)
	}
}

func TestBuild_ForkMidHistory(t *testing.T) {
	root := t.TempDir()
	deps, _ := testDeps(t, root)
	res, err := Build(context.Background(), deps, Request{
		SessionID: "s1",
		Fork:      handoff.ForkPoint{Kind: handoff.ForkMessageIndex, MessageIndex: 2},
		DryRun:    true,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Fork.ResolvedIndex != 2 {
		t.Errorf("fork resolved = %d", res.Fork.ResolvedIndex)
	}
}

func TestBuild_GitignoreHint(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	deps, _ := testDeps(t, root)
	res, err := Build(context.Background(), deps, Request{SessionID: "s1"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !res.GitignoreHint {
		t.Error("repo without a HANDOFF gitignore pattern must set the hint")
	}
}

func TestBuild_Disabled(t *testing.T) {
	deps, _ := testDeps(t, t.TempDir())
	deps.Cfg.Enabled = false
	if _, err := Build(context.Background(), deps, Request{SessionID: "s1"}); err == nil {
		t.Fatal("disabled config must error")
	}
}

func TestBuild_IncludeBoundaries(t *testing.T) {
	root := t.TempDir()
	deps, _ := testDeps(t, root)
	deps.Adapters[0].(*fakeAdapter).msgs[0].Text = "build it token=hunter2secret"
	deps.Scrub = func(s string) string { return strings.ReplaceAll(s, "hunter2secret", "[REDACTED]") }
	res, err := Build(context.Background(), deps, Request{SessionID: "s1", DryRun: true, IncludeBoundaries: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Boundaries) != 2 {
		t.Fatalf("boundaries = %d, want 2", len(res.Boundaries))
	}
	if res.Boundaries[0].Stable || !res.Boundaries[1].Stable {
		t.Errorf("stability = %v/%v, want false/true", res.Boundaries[0].Stable, res.Boundaries[1].Stable)
	}
	if strings.Contains(res.Boundaries[0].Preview, "hunter2secret") {
		t.Error("boundary preview must be scrubbed")
	}
	if !strings.Contains(res.Boundaries[0].Preview, "[REDACTED]") {
		t.Errorf("scrub did not run on preview: %q", res.Boundaries[0].Preview)
	}
	// Without the flag, no boundaries ride along.
	res2, err := Build(context.Background(), deps, Request{SessionID: "s1", DryRun: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res2.Boundaries != nil {
		t.Error("boundaries must be nil unless requested")
	}
}

func TestBuild_ResolveTargetModel(t *testing.T) {
	root := t.TempDir()

	// Resolver set + no explicit target-model → estimate prices at the
	// resolved (tier-matched) model, not the source model.
	deps, _ := testDeps(t, root)
	deps.ResolveTargetModel = func(sourceModel, targetTool string) string {
		if sourceModel != "opus-4-8" || targetTool != "codex" {
			t.Errorf("resolver got (%q, %q)", sourceModel, targetTool)
		}
		return "gpt-5.5"
	}
	res, err := Build(context.Background(), deps, Request{SessionID: "s1", TargetTool: "codex", DryRun: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.TargetModel != "gpt-5.5" {
		t.Errorf("resolved target model = %q, want gpt-5.5", res.TargetModel)
	}

	// Explicit --target-model always wins over the resolver.
	res2, _ := Build(context.Background(), deps, Request{SessionID: "s1", TargetTool: "codex", TargetModel: "gpt-5.4", DryRun: true})
	if res2.TargetModel != "gpt-5.4" {
		t.Errorf("explicit target model = %q, want gpt-5.4", res2.TargetModel)
	}

	// Resolver returns "" (ungrounded) → honest source-model fallback.
	deps.ResolveTargetModel = func(string, string) string { return "" }
	res3, _ := Build(context.Background(), deps, Request{SessionID: "s1", TargetTool: "codex", DryRun: true})
	if res3.TargetModel != "opus-4-8" {
		t.Errorf("empty-resolver target model = %q, want source opus-4-8", res3.TargetModel)
	}

	// Nil resolver → existing behavior preserved (source model).
	deps.ResolveTargetModel = nil
	res4, _ := Build(context.Background(), deps, Request{SessionID: "s1", TargetTool: "codex", DryRun: true})
	if res4.TargetModel != "opus-4-8" {
		t.Errorf("nil-resolver target model = %q, want source opus-4-8", res4.TargetModel)
	}
}

func TestBuild_StayOption(t *testing.T) {
	root := t.TempDir()
	deps, _ := testDeps(t, root)
	deps.Stay = func(_ context.Context, sessionID string) (handoff.StayEstimate, bool) {
		if sessionID != "s1" {
			t.Errorf("stay resolver got session %q", sessionID)
		}
		return handoff.StayEstimate{HasBand: true, NextMessageMidUSD: 0.42}, true
	}
	res, err := Build(context.Background(), deps, Request{SessionID: "s1", DryRun: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Estimate.Stay == nil || !res.Estimate.Stay.HasBand || res.Estimate.Stay.NextMessageMidUSD != 0.42 {
		t.Errorf("stay = %+v", res.Estimate.Stay)
	}
	// An ungrounded resolver omits the row.
	deps.Stay = func(context.Context, string) (handoff.StayEstimate, bool) { return handoff.StayEstimate{}, false }
	res2, _ := Build(context.Background(), deps, Request{SessionID: "s1", DryRun: true})
	if res2.Estimate.Stay != nil {
		t.Error("ungrounded stay must stay nil")
	}
}
