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
	if err := st.EndTerminalRun(ctx, "run-abc", ended, 0, EndReasonChildExit); err != nil {
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
	if got2.EndReason != EndReasonChildExit {
		t.Errorf("endReason = %q, want %q", got2.EndReason, EndReasonChildExit)
	}
}

// TestLiveRunForSession pins the durable double-spawn AUTHORITY (round-5 finding
// 1): the query reports a conflict for a LIVE (ended_at NULL) run correlated to a
// session at or above the confidence gate, and abstains for an ended run, a
// below-gate weak correlation, and an uncorrelated session — kind-agnostically.
func TestLiveRunForSession(t *testing.T) {
	t.Parallel()
	st, ctx := openTermRunTestStore(t)
	ended := time.Now().UTC()

	mk := func(id, kind string) {
		if err := st.InsertTerminalRun(ctx, TerminalRun{RunID: id, Tool: "claude-code", Kind: kind}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	correlate := func(id, sess string, conf float64, src string) {
		if err := st.UpsertCorrelation(ctx, TerminalCorrelation{RunID: id, SessionID: sess, Confidence: conf, Source: src}); err != nil {
			t.Fatalf("correlate %s: %v", id, err)
		}
	}

	// (1) live + strongly correlated → a conflict. kind='resume' proves the query
	//     is kind-agnostic (a dashboard resume is not KindAttach).
	mk("live", "resume")
	correlate("live", "sess-live", 0.95, "oob")
	// (2) correlated but the run ENDED → not a live conflict.
	mk("ended", "attach")
	if err := st.EndTerminalRun(ctx, "ended", ended, 0, EndReasonChildExit); err != nil {
		t.Fatalf("end: %v", err)
	}
	correlate("ended", "sess-ended", 0.95, "oob")
	// (3) live but only WEAKLY correlated (below the gate) → abstain.
	mk("weak", "attach")
	correlate("weak", "sess-weak", 0.40, "heuristic")

	cases := []struct {
		session string
		want    bool
	}{
		{"sess-live", true},   // live + established correlation
		{"sess-ended", false}, // correlated but the run ended
		{"sess-weak", false},  // live but below the confidence gate
		{"sess-none", false},  // nothing correlated at all
		{"", false},           // empty session id is never a conflict
	}
	for _, tc := range cases {
		got, err := st.LiveRunForSession(ctx, tc.session, 0.50)
		if err != nil {
			t.Fatalf("LiveRunForSession(%q): %v", tc.session, err)
		}
		if got != tc.want {
			t.Errorf("LiveRunForSession(%q) = %v, want %v", tc.session, got, tc.want)
		}
	}
}

// TestLiveRunForSessionExcluding pins the review finding-1 fix (self-blocking
// authority): the durable authority must NOT count a conclusively-dead orphan
// (end_reason 'resumed'/'daemon_shutdown', ended_at NULL) as live, and must
// exclude the caller-supplied predecessor run ids — so a crash-orphan auto-resume
// never self-blocks on its own predecessor — while a GENUINELY distinct live run
// for the same session still conflicts.
func TestLiveRunForSessionExcluding(t *testing.T) {
	t.Parallel()
	st, ctx := openTermRunTestStore(t)

	mk := func(id, kind, reason string) {
		if err := st.InsertTerminalRun(ctx, TerminalRun{RunID: id, Tool: "claude-code", Kind: kind}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
		if reason != "" {
			// Stamp the durable reason WITHOUT setting ended_at (mirrors the
			// shutdown/resumed sweeps: reason changes, ended_at stays NULL).
			if _, err := st.db.ExecContext(ctx,
				`UPDATE terminal_run SET end_reason = ? WHERE run_id = ?`, reason, id); err != nil {
				t.Fatalf("stamp %s: %v", id, err)
			}
		}
	}
	correlate := func(id, sess string) {
		if err := st.UpsertCorrelation(ctx, TerminalCorrelation{RunID: id, SessionID: sess, Confidence: 0.95, Source: "oob"}); err != nil {
			t.Fatalf("correlate %s: %v", id, err)
		}
	}

	// (t3) A lone already-superseded orphan: end_reason='resumed', ended_at NULL,
	// correlated. It must NOT be a conflict (dead state).
	mk("resumed-orphan", "attach", EndReasonResumed)
	correlate("resumed-orphan", "sess-resumed")
	// A lone shutdown orphan (daemon_shutdown, ended_at NULL) is likewise dead.
	mk("shutdown-orphan", "attach", EndReasonDaemonShutdown)
	correlate("shutdown-orphan", "sess-shutdown")

	// A crash orphan (end_reason='', ended_at NULL), correlated — the predecessor
	// rediscovery offers for auto-resume. With NO exclusion it looks live (SQL
	// cannot tell it from a real live run); excluding its run id retires it.
	mk("crash-orphan", "attach", "")
	correlate("crash-orphan", "sess-crash")

	// A genuinely distinct live run for the SAME session as the crash orphan (a
	// dashboard run): end_reason='', ended_at NULL, NOT a predecessor. It must
	// still conflict even when the crash orphan is excluded.
	mk("dash-live", "resume", "")
	correlate("dash-live", "sess-crash")

	cases := []struct {
		name    string
		session string
		exclude []string
		want    bool
	}{
		{"resumed orphan alone → no conflict", "sess-resumed", nil, false},
		{"shutdown orphan alone → no conflict", "sess-shutdown", nil, false},
		{"crash orphan excluded → no self-block", "sess-crash", []string{"crash-orphan", "dash-live"}, false},
		{"crash orphan excluded but distinct live run remains → conflict", "sess-crash", []string{"crash-orphan"}, true},
		{"no exclusion, live run present → conflict", "sess-crash", nil, true},
	}
	for _, tc := range cases {
		got, err := st.LiveRunForSessionExcluding(ctx, tc.session, 0.50, tc.exclude)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestEndReasonGuardedAndSweeps pins the H2 end-reason semantics: EndTerminalRun
// never downgrades an existing reason, the graceful-shutdown sweep stamps only
// live attach runs, and the resumed-supersede targets a session's orphan(s).
func TestEndReasonGuardedAndSweeps(t *testing.T) {
	t.Parallel()
	st, ctx := openTermRunTestStore(t)
	base := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)

	mk := func(id string) {
		if err := st.InsertTerminalRun(ctx, TerminalRun{
			RunID: id, Tool: "claude-code", Kind: "attach", LaunchedAt: base,
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	// live1: live attach run correlated to a session (shutdown orphan candidate).
	mk("live1")
	if err := st.UpsertCorrelation(ctx, TerminalCorrelation{
		RunID: "live1", SessionID: "sess-1", Confidence: 0.95, Source: "oob", ObservedAt: base,
	}); err != nil {
		t.Fatalf("correlate: %v", err)
	}
	// exited1: already recorded a natural child exit.
	mk("exited1")
	if err := st.EndTerminalRun(ctx, "exited1", base.Add(time.Minute), 0, EndReasonChildExit); err != nil {
		t.Fatalf("end exited1: %v", err)
	}

	// Shutdown sweep: only live1 gets stamped (exited1 already ended).
	n, err := st.StampLiveAttachRunsShutdown(ctx)
	if err != nil {
		t.Fatalf("StampLiveAttachRunsShutdown: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d rows, want 1 (only the live attach run)", n)
	}
	r, _, _ := st.LoadTerminalRun(ctx, "live1")
	if r.EndReason != EndReasonDaemonShutdown {
		t.Fatalf("live1 end_reason = %q, want daemon_shutdown", r.EndReason)
	}
	if r.EndedAt != nil {
		t.Fatalf("shutdown stamp must NOT set ended_at, got %v", r.EndedAt)
	}

	// A racing natural OnExit now records ended_at but MUST NOT downgrade the
	// daemon_shutdown reason (guarded update).
	if err := st.EndTerminalRun(ctx, "live1", base.Add(2*time.Minute), 0, EndReasonChildExit); err != nil {
		t.Fatalf("racing end: %v", err)
	}
	r, _, _ = st.LoadTerminalRun(ctx, "live1")
	if r.EndReason != EndReasonDaemonShutdown {
		t.Fatalf("racing OnExit downgraded reason to %q — must stay daemon_shutdown", r.EndReason)
	}
	if r.EndedAt == nil {
		t.Fatal("racing OnExit must still record ended_at")
	}

	// A FRESH replacement run correlates to the SAME session via OOB (as the
	// codex resume announce does immediately) BEFORE the supersede stamp runs.
	// This is the wrong-run-supersede race: a by-session stamp would wrongly
	// mark this live row 'resumed'. StampResumedByRunID targets the predecessor
	// by exact id, so this new run must be LEFT untouched.
	mk("new1")
	if err := st.UpsertCorrelation(ctx, TerminalCorrelation{
		RunID: "new1", SessionID: "sess-1", Confidence: 0.95, Source: "oob", ObservedAt: base,
	}); err != nil {
		t.Fatalf("correlate new1: %v", err)
	}

	// Supersede by RUN ID: stamps the predecessor live1 (daemon_shutdown →
	// resumed) and NOTHING else.
	if err := st.StampResumedByRunID(ctx, "live1"); err != nil {
		t.Fatalf("StampResumedByRunID: %v", err)
	}
	r, _, _ = st.LoadTerminalRun(ctx, "live1")
	if r.EndReason != EndReasonResumed {
		t.Fatalf("after supersede live1 end_reason = %q, want resumed", r.EndReason)
	}
	// The concurrently-correlated fresh run must NOT have been stamped — its
	// live row stays end_reason='' so the shutdown sweep + its own restart offer
	// still apply.
	rn, _, _ := st.LoadTerminalRun(ctx, "new1")
	if rn.EndReason != "" {
		t.Fatalf("fresh replacement run end_reason = %q, want '' (must not be superseded)", rn.EndReason)
	}
}

// TestStampLiveNonAttachRunsShutdown pins the FIX-2 sibling sweep: a live
// non-attach run (resume/fresh/handoff) is stamped 'daemon_shutdown' at graceful
// shutdown so the runs-history view stops showing it as live (the 2026-07-20
// orphaned resume-kind run), while the attach sweep stays DISJOINT (kind) and the
// attach resume-offer inputs are provably untouched.
func TestStampLiveNonAttachRunsShutdown(t *testing.T) {
	t.Parallel()
	st, ctx := openTermRunTestStore(t)
	base := time.Date(2026, 7, 20, 15, 24, 33, 0, time.UTC)

	mk := func(id, kind string) {
		if err := st.InsertTerminalRun(ctx, TerminalRun{
			RunID: id, Tool: "claude-code", Kind: kind, LaunchedAt: base,
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	// A live attach run correlated to a session (offer candidate), a live
	// resume-kind run (the orphan), and an already-exited resume run.
	mk("attach-live", "attach")
	if err := st.UpsertCorrelation(ctx, TerminalCorrelation{
		RunID: "attach-live", SessionID: "sess-A", Confidence: 0.95, Source: "oob", ObservedAt: base,
	}); err != nil {
		t.Fatalf("correlate: %v", err)
	}
	mk("resume-live", "resume")
	mk("fresh-live", "fresh")
	mk("resume-done", "resume")
	if err := st.EndTerminalRun(ctx, "resume-done", base.Add(time.Minute), 0, EndReasonChildExit); err != nil {
		t.Fatalf("end resume-done: %v", err)
	}

	// The attach sweep must NOT touch the non-attach kinds (disjoint by kind).
	nAttach, err := st.StampLiveAttachRunsShutdown(ctx)
	if err != nil {
		t.Fatalf("StampLiveAttachRunsShutdown: %v", err)
	}
	if nAttach != 1 {
		t.Fatalf("attach sweep stamped %d rows, want 1 (only the attach run)", nAttach)
	}
	if r, _, _ := st.LoadTerminalRun(ctx, "resume-live"); r.EndReason != "" {
		t.Fatalf("attach sweep leaked onto resume-live: end_reason=%q, want '' (disjoint by kind)", r.EndReason)
	}

	// The sibling sweep stamps exactly the two live NON-attach runs.
	nNon, err := st.StampLiveNonAttachRunsShutdown(ctx)
	if err != nil {
		t.Fatalf("StampLiveNonAttachRunsShutdown: %v", err)
	}
	if nNon != 2 {
		t.Fatalf("non-attach sweep stamped %d rows, want 2 (resume-live + fresh-live)", nNon)
	}
	for _, id := range []string{"resume-live", "fresh-live"} {
		r, _, _ := st.LoadTerminalRun(ctx, id)
		if r.EndReason != EndReasonDaemonShutdown {
			t.Fatalf("%s end_reason = %q, want daemon_shutdown", id, r.EndReason)
		}
		// Finding 4: the non-attach sweep MUST set ended_at so
		// /api/terminal/runs (running == ended_at==nil) stops rendering the dead
		// run as live — that was the exact bug this sweep was meant to fix.
		if r.EndedAt == nil {
			t.Fatalf("%s: sibling sweep must SET ended_at (running-view fix), got nil", id)
		}
	}
	// The already-exited resume run keeps its child_exit reason (excluded).
	if r, _, _ := st.LoadTerminalRun(ctx, "resume-done"); r.EndReason != EndReasonChildExit {
		t.Fatalf("resume-done end_reason = %q, want child_exit (already ended, excluded)", r.EndReason)
	}
	// Offerability of the attach run is UNCHANGED: it carries the same
	// daemon_shutdown reason it would have without the sibling sweep, so the
	// kind='attach'-scoped resume-offer path still classifies it resumable.
	if r, _, _ := st.LoadTerminalRun(ctx, "attach-live"); r.EndReason != EndReasonDaemonShutdown {
		t.Fatalf("attach-live end_reason = %q, want daemon_shutdown (offer path untouched)", r.EndReason)
	} else if r.EndedAt != nil {
		// The attach sweep MUST leave ended_at NULL — the resume-offer path keys
		// daemon-death orphan detection off it. The non-attach sweep setting
		// ended_at must not have leaked onto the attach kind.
		t.Fatalf("attach-live ended_at = %v, want nil (attach sweep leaves ended_at NULL for the resume-offer flow)", r.EndedAt)
	}
	// Idempotent: a second sibling sweep stamps nothing.
	if n, err := st.StampLiveNonAttachRunsShutdown(ctx); err != nil || n != 0 {
		t.Fatalf("second sibling sweep n=%d err=%v, want 0,nil (idempotent)", n, err)
	}
}

// TestStampResumedByRunIDs is the round-4 multi-orphan supersede: a single
// successful auto-resume stamps EVERY eligible predecessor orphan of the session
// (historical duplicates / prior stamp failures), not just the newest, applying
// StampResumedByRunID's guarded per-id semantics so a child_exit row is left
// untouched.
func TestStampResumedByRunIDs(t *testing.T) {
	t.Parallel()
	st, ctx := openTermRunTestStore(t)
	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	mk := func(id string) {
		if err := st.InsertTerminalRun(ctx, TerminalRun{
			RunID: id, Tool: "claude-code", Kind: "attach", LaunchedAt: base,
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	// Two eligible crash orphans (reason '') for one session, plus a natural
	// child-exit that must be left untouched by the guarded stamp.
	mk("orphan-new")
	mk("orphan-old")
	mk("done")
	if err := st.EndTerminalRun(ctx, "done", base.Add(time.Minute), 0, EndReasonChildExit); err != nil {
		t.Fatalf("end done: %v", err)
	}

	// Stamp all three ids in one call (as the supersede does for the session's
	// predecessor list); "done" is included to prove the guard leaves it alone.
	if err := st.StampResumedByRunIDs(ctx, []string{"orphan-new", "orphan-old", "done"}); err != nil {
		t.Fatalf("StampResumedByRunIDs: %v", err)
	}
	for _, id := range []string{"orphan-new", "orphan-old"} {
		r, _, _ := st.LoadTerminalRun(ctx, id)
		if r.EndReason != EndReasonResumed {
			t.Fatalf("%s end_reason = %q, want resumed", id, r.EndReason)
		}
	}
	r, _, _ := st.LoadTerminalRun(ctx, "done")
	if r.EndReason != EndReasonChildExit {
		t.Fatalf("done end_reason = %q, want child_exit (guarded, untouched)", r.EndReason)
	}
	// Empty input is a no-op (not an error).
	if err := st.StampResumedByRunIDs(ctx, nil); err != nil {
		t.Fatalf("empty StampResumedByRunIDs: %v", err)
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
