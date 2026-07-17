package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
)

func openTermRunTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	database, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "termrun_test.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return New(database), ctx
}

func TestTerminalRunRoundTrip(t *testing.T) {
	t.Parallel()
	st, ctx := openTermRunTestStore(t)

	launched := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	run := TerminalRun{
		RunID:                "run-abc",
		Tool:                 "claude-code",
		Kind:                 "handoff",
		SourceSessionID:      "sess-source",
		ProjectRootHash:      "deadbeef",
		CorrelationTokenHash: "cafef00d",
		LaunchedAt:           launched,
	}
	if err := st.InsertTerminalRun(ctx, run); err != nil {
		t.Fatalf("InsertTerminalRun: %v", err)
	}

	got, ok, err := st.LoadTerminalRun(ctx, "run-abc")
	if err != nil || !ok {
		t.Fatalf("LoadTerminalRun ok=%v err=%v", ok, err)
	}
	if got.Tool != "claude-code" || got.Kind != "handoff" || got.SourceSessionID != "sess-source" {
		t.Errorf("run = %+v", got)
	}
	if got.ProjectRootHash != "deadbeef" || got.CorrelationTokenHash != "cafef00d" {
		t.Errorf("hashes not round-tripped: %+v", got)
	}
	if !got.LaunchedAt.Equal(launched) {
		t.Errorf("launchedAt = %v, want %v", got.LaunchedAt, launched)
	}
	if got.EndedAt != nil || got.ExitCode != nil {
		t.Errorf("fresh run must have nil ended_at/exit_code, got %+v", got)
	}

	// End the run.
	ended := launched.Add(5 * time.Minute)
	if err := st.EndTerminalRun(ctx, "run-abc", ended, 0); err != nil {
		t.Fatalf("EndTerminalRun: %v", err)
	}
	got2, _, err := st.LoadTerminalRun(ctx, "run-abc")
	if err != nil {
		t.Fatalf("LoadTerminalRun after end: %v", err)
	}
	if got2.EndedAt == nil || !got2.EndedAt.Equal(ended) {
		t.Errorf("endedAt = %v, want %v", got2.EndedAt, ended)
	}
	if got2.ExitCode == nil || *got2.ExitCode != 0 {
		t.Errorf("exitCode = %v, want 0", got2.ExitCode)
	}
}

func TestListTerminalRuns(t *testing.T) {
	t.Parallel()
	st, ctx := openTermRunTestStore(t)

	base := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	// Three runs, launched oldest→newest so ORDER BY DESC is observable.
	for i, id := range []string{"run-old", "run-mid", "run-new"} {
		if err := st.InsertTerminalRun(ctx, TerminalRun{
			RunID:      id,
			Tool:       "claude-code",
			Kind:       "fresh",
			LaunchedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("InsertTerminalRun %s: %v", id, err)
		}
	}
	// Correlate run-mid with two sessions; the stronger one must win.
	if err := st.UpsertCorrelation(ctx, TerminalCorrelation{RunID: "run-mid", SessionID: "sess-weak", Confidence: 0.4, Source: "heuristic"}); err != nil {
		t.Fatalf("UpsertCorrelation weak: %v", err)
	}
	if err := st.UpsertCorrelation(ctx, TerminalCorrelation{RunID: "run-mid", SessionID: "sess-strong", Confidence: 0.9, Source: "oob"}); err != nil {
		t.Fatalf("UpsertCorrelation strong: %v", err)
	}

	got, err := st.ListTerminalRuns(ctx, 10)
	if err != nil {
		t.Fatalf("ListTerminalRuns: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 runs, got %d", len(got))
	}
	// Newest first.
	if got[0].RunID != "run-new" || got[2].RunID != "run-old" {
		t.Errorf("order = %s..%s, want run-new..run-old", got[0].RunID, got[2].RunID)
	}
	// run-mid folds in the strongest correlation.
	var mid *TerminalRunSummary
	for i := range got {
		if got[i].RunID == "run-mid" {
			mid = &got[i]
		}
	}
	if mid == nil {
		t.Fatal("run-mid missing")
	}
	if mid.BestSessionID != "sess-strong" || mid.BestConfidence != 0.9 {
		t.Errorf("best correlation = %q/%v, want sess-strong/0.9", mid.BestSessionID, mid.BestConfidence)
	}
	// A run with no correlation reports an empty session.
	for _, r := range got {
		if r.RunID == "run-new" && (r.BestSessionID != "" || r.BestConfidence != 0) {
			t.Errorf("run-new should have no correlation, got %q/%v", r.BestSessionID, r.BestConfidence)
		}
	}

	// Limit is honored.
	limited, err := st.ListTerminalRuns(ctx, 1)
	if err != nil {
		t.Fatalf("ListTerminalRuns limit: %v", err)
	}
	if len(limited) != 1 || limited[0].RunID != "run-new" {
		t.Errorf("limit 1 = %+v, want [run-new]", limited)
	}
}

func TestLoadTerminalRunAbsent(t *testing.T) {
	t.Parallel()
	st, ctx := openTermRunTestStore(t)
	_, ok, err := st.LoadTerminalRun(ctx, "nope")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for absent run")
	}
}

func TestUpsertCorrelationKeepsStrongest(t *testing.T) {
	t.Parallel()
	st, ctx := openTermRunTestStore(t)
	if err := st.InsertTerminalRun(ctx, TerminalRun{RunID: "run-1", Tool: "codex", Kind: "fresh"}); err != nil {
		t.Fatalf("InsertTerminalRun: %v", err)
	}

	// Weak heuristic first.
	if err := st.UpsertCorrelation(ctx, TerminalCorrelation{
		RunID: "run-1", SessionID: "sess-x", Confidence: 0.40, Source: "heuristic",
	}); err != nil {
		t.Fatalf("upsert heuristic: %v", err)
	}
	// Strong OOB upgrades it.
	if err := st.UpsertCorrelation(ctx, TerminalCorrelation{
		RunID: "run-1", SessionID: "sess-x", Confidence: 0.95, Source: "oob",
	}); err != nil {
		t.Fatalf("upsert oob: %v", err)
	}
	// A later weak heuristic must NOT downgrade.
	if err := st.UpsertCorrelation(ctx, TerminalCorrelation{
		RunID: "run-1", SessionID: "sess-x", Confidence: 0.40, Source: "heuristic",
	}); err != nil {
		t.Fatalf("upsert heuristic 2: %v", err)
	}

	corrs, err := st.LoadCorrelations(ctx, "run-1")
	if err != nil {
		t.Fatalf("LoadCorrelations: %v", err)
	}
	if len(corrs) != 1 {
		t.Fatalf("expected 1 correlation (unique per run,session), got %d", len(corrs))
	}
	if corrs[0].Confidence != 0.95 || corrs[0].Source != "oob" {
		t.Errorf("correlation = %+v, want confidence=0.95 source=oob", corrs[0])
	}
}

func TestResolveRunForSession(t *testing.T) {
	t.Parallel()
	st, ctx := openTermRunTestStore(t)
	for _, r := range []string{"run-a", "run-b"} {
		if err := st.InsertTerminalRun(ctx, TerminalRun{RunID: r, Tool: "claude-code", Kind: "fresh"}); err != nil {
			t.Fatalf("InsertTerminalRun %s: %v", r, err)
		}
	}
	// Two runs both claim sess-shared; the stronger wins the reverse lookup.
	_ = st.UpsertCorrelation(ctx, TerminalCorrelation{RunID: "run-a", SessionID: "sess-shared", Confidence: 0.40, Source: "heuristic"})
	_ = st.UpsertCorrelation(ctx, TerminalCorrelation{RunID: "run-b", SessionID: "sess-shared", Confidence: 0.95, Source: "oob"})

	runID, conf, ok, err := st.ResolveRunForSession(ctx, "sess-shared")
	if err != nil || !ok {
		t.Fatalf("ResolveRunForSession ok=%v err=%v", ok, err)
	}
	if runID != "run-b" || conf != 0.95 {
		t.Errorf("resolved = (%q, %v), want (run-b, 0.95)", runID, conf)
	}

	// Absent session → ok=false.
	_, _, ok, err = st.ResolveRunForSession(ctx, "sess-none")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for uncorrelated session")
	}
}
