package processobs

import (
	"context"
	"sync"
	"testing"
	"time"
)

// countingSeed wraps a pid→Seed table with a call counter, so the tests can
// assert the WORK the pass does, not just its outcome.
type countingSeed struct {
	table map[int]Seed
	calls int
	pids  []int
}

func (c *countingSeed) lookup(pid int) (Seed, bool) {
	c.calls++
	c.pids = append(c.pids, pid)
	s, ok := c.table[pid]
	return s, ok
}

// lateSeedTree builds an Attributor with a controllable seed table and returns
// both. The seed table starts EMPTY — the whole point is that the seed appears
// later, after exec-time resolution has already run and lost.
func lateSeedTree(t *testing.T) (*Attributor, *countingSeed) {
	t.Helper()
	cs := &countingSeed{table: map[int]Seed{}}
	return NewAttributor(cs.lookup, &FieldScrubber{ArgvMode: "preview", MaxPreviewBytes: 512}, nil), cs
}

func keyOf(pid int, start int64) string { return ProcessKey("b", pid, start) }

// TestLateSeedRulesTable pins the candidate decision table row by row — the
// ordered rule set is data (CLAUDE.md rule 5), so each row gets a case.
func TestLateSeedRulesTable(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	pol := DefaultLateSeedPolicy()

	cases := []struct {
		name string
		run  ProcessRun
		want lateSeedVerdict
	}{
		{
			name: "live recent unattributed run is probed",
			run:  ProcessRun{PID: 100, StartedAt: now.Add(-30 * time.Second)},
			want: lateSeedProbe,
		},
		{
			name: "exited run is skipped (row is final, pid is recyclable)",
			run:  ProcessRun{PID: 100, StartedAt: now.Add(-30 * time.Second), Exited: true},
			want: lateSeedSkipExited,
		},
		{
			name: "run without an OS pid is skipped (synthesized action-correlation row)",
			run:  ProcessRun{PID: 0, StartedAt: now.Add(-30 * time.Second)},
			want: lateSeedSkipNoPID,
		},
		{
			name: "already high-confidence run is authoritative — never re-probed",
			run: ProcessRun{PID: 100, StartedAt: now.Add(-30 * time.Second), Attribution: Attribution{
				SessionID: "s", Source: AttrEnvToken, Confidence: ConfHigh,
			}},
			want: lateSeedSkipAuthoritative,
		},
		{
			name: "medium-confidence run is below a direct seed — still a candidate",
			run: ProcessRun{PID: 100, StartedAt: now.Add(-30 * time.Second), Attribution: Attribution{
				SessionID: "s", Source: AttrCrossOSCorrelation, Confidence: ConfMedium,
			}},
			want: lateSeedProbe,
		},
		{
			name: "older than MaxAge is skipped",
			run:  ProcessRun{PID: 100, StartedAt: now.Add(-2 * DefaultLateSeedMaxAge)},
			want: lateSeedSkipStale,
		},
		{
			name: "266s — the worst observed correlation lag — is still in window",
			run:  ProcessRun{PID: 100, StartedAt: now.Add(-267 * time.Second)},
			want: lateSeedProbe,
		},
		{
			name: "a boundary is probed, mirroring resolveAttribution's fall-through",
			run:  ProcessRun{PID: 100, StartedAt: now.Add(-30 * time.Second), IsBoundary: true},
			want: lateSeedProbe,
		},
		{
			name: "unknown start time never counts as stale",
			run:  ProcessRun{PID: 100},
			want: lateSeedProbe,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := tc.run
			got := classifyLateSeed(lateSeedCandidate{
				Run: &run, Now: now, Policy: pol, SeedConfidence: ConfHigh,
			})
			if got != tc.want {
				t.Fatalf("verdict = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestConfidencePrecedenceIsAMaxUpgrade pins the precedence order the pass is
// built on — derived from resolveAttribution (direct identity = high) and the
// CorrelateCrossOS store guard (`attribution_confidence != 'high'`).
func TestConfidencePrecedenceIsAMaxUpgrade(t *testing.T) {
	t.Parallel()
	cases := []struct {
		candidate, current Confidence
		want               bool
	}{
		{ConfHigh, ConfNone, true},
		{ConfHigh, "", true},
		{ConfHigh, ConfLow, true},
		{ConfHigh, ConfMedium, true},
		{ConfHigh, ConfHigh, false}, // never re-write an equal attribution
		{ConfMedium, ConfHigh, false},
		{ConfLow, ConfMedium, false},
		{ConfNone, ConfNone, false},
	}
	for _, tc := range cases {
		if got := outranks(tc.candidate, tc.current); got != tc.want {
			t.Errorf("outranks(%q, %q) = %v, want %v", tc.candidate, tc.current, got, tc.want)
		}
	}
}

// TestLateSeedUpgradesUnattributedRunAndSubtree is the headline case: the
// 30–266s `discovered` correlation lag. The tool subtree execs, resolves
// unattributed (no seed exists yet), and only much later does the terminal
// seeder write the pidbridge row. The late pass must recover the whole subtree.
func TestLateSeedUpgradesUnattributedRunAndSubtree(t *testing.T) {
	t.Parallel()
	a, cs := lateSeedTree(t)
	clk := &evClock{t: time.Unix(1_700_000_000, 0).UTC()}

	// t=0: `observer kilo` launches under the dashboard terminal. No seed yet.
	a.Observe(execEv("b", 100, 1, 1000, "/usr/bin/observer", []string{"observer", "kilo"}, clk.next()), nil)
	a.Observe(forkEv("b", 200, 100, 2000, clk.next()), nil)
	a.Observe(execEv("b", 200, 100, 2000, "/usr/bin/kilo", []string{"kilo"}, clk.next()), nil)
	a.Observe(forkEv("b", 300, 200, 3000, clk.next()), nil)
	a.Observe(execEv("b", 300, 200, 3000, "/usr/bin/node", []string{"node", "x.js"}, clk.next()), nil)

	for _, pid := range []int{100, 200, 300} {
		if run := a.runs[keyOf(pid, int64(pid*10))]; run != nil && run.Attributed() {
			t.Fatalf("pid %d should be unattributed at exec: %+v", pid, run.Attribution)
		}
	}

	// t=+266s: correlation names the session and the seeder writes the row.
	cs.table[100] = Seed{SessionID: "sess-kilo", Tool: "kilo-code-cli", ProjectID: 9}
	now := clk.t.Add(266 * time.Second)

	res := a.ReconcileLateSeeds(now, DefaultLateSeedPolicy())
	if res.Roots != 1 {
		t.Fatalf("Roots = %d, want 1 (%+v)", res.Roots, res)
	}
	if res.Reinherited != 2 {
		t.Fatalf("Reinherited = %d, want 2 (the kilo child + its node grandchild)", res.Reinherited)
	}
	if len(res.Upgraded) != 3 {
		t.Fatalf("Upgraded = %d runs, want 3", len(res.Upgraded))
	}

	root := a.runs[keyOf(100, 1000)]
	if root.Attribution.Source != AttrBridge || root.Attribution.Confidence != ConfHigh {
		t.Errorf("root attribution = %+v, want bridge/high", root.Attribution)
	}
	for _, pid := range []int{200, 300} {
		run := a.runs[keyOf(pid, int64(pid*10))]
		if run.Attribution.SessionID != "sess-kilo" || run.Attribution.Tool != "kilo-code-cli" {
			t.Errorf("pid %d = %+v, want sess-kilo/kilo-code-cli", pid, run.Attribution)
		}
		if run.Attribution.Source != AttrInherited || run.Attribution.Confidence != ConfHigh {
			t.Errorf("pid %d source/conf = %+v, want inherited/high", pid, run.Attribution)
		}
		if run.Attribution.ProjectID != 9 {
			t.Errorf("pid %d project = %d, want 9", pid, run.Attribution.ProjectID)
		}
	}
}

// TestLateSeedNeverDowngradesAHigherConfidenceRun pins the MAX-upgrade rule:
// a run already attributed at high (env-token, or an earlier direct seed) is
// untouched — and is not even probed, so it costs nothing.
func TestLateSeedNeverDowngradesAHigherConfidenceRun(t *testing.T) {
	t.Parallel()
	a, cs := lateSeedTree(t)
	clk := &evClock{t: time.Unix(1_700_000_000, 0).UTC()}
	a.SetTokenLookup(func(tok string) (Seed, bool) {
		if tok == "sess-env" {
			return Seed{SessionID: "sess-env", Tool: "claude-code", ProjectID: 1, Source: AttrEnvToken, Confidence: ConfHigh}, true
		}
		return Seed{}, false
	})

	ev := execEv("b", 100, 1, 1000, "/usr/bin/claude", []string{"claude"}, clk.next())
	ev.SessionToken = "sess-" + "env"
	a.Observe(ev, nil)
	before := a.runs[keyOf(100, 1000)].Attribution
	if before.Source != AttrEnvToken {
		t.Fatalf("setup: want env_token attribution, got %+v", before)
	}

	// A LATER, contradictory pid seed for the same pid must not re-home it.
	cs.table[100] = Seed{SessionID: "sess-other", Tool: "codex", ProjectID: 2}
	res := a.ReconcileLateSeeds(clk.t.Add(time.Minute), DefaultLateSeedPolicy())

	if res.Roots != 0 || len(res.Upgraded) != 0 {
		t.Fatalf("high-confidence run was modified: %+v", res)
	}
	if cs.calls != 0 {
		t.Errorf("SeedLookup called %d times for an authoritative run; want 0 (rule-table short-circuit)", cs.calls)
	}
	if got := a.runs[keyOf(100, 1000)].Attribution; got != before {
		t.Errorf("attribution changed: %+v -> %+v", before, got)
	}
}

// TestLateSeedUpgradesMediumConfidence pins that a run below high (e.g. one a
// cwd-correlation pass placed at medium) IS upgraded by a direct pid seed —
// the seed is the stronger signal, matching the confidence ladder.
func TestLateSeedUpgradesMediumConfidence(t *testing.T) {
	t.Parallel()
	a, cs := lateSeedTree(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	key := keyOf(100, 1000)
	a.runs[key] = &ProcessRun{
		ProcessKey: key, BootID: "b", PID: 100, StartTimeTicks: 1000, StartedAt: now.Add(-time.Minute),
		Attribution: Attribution{SessionID: "guessed", Tool: "codex", Source: AttrCrossOSCorrelation, Confidence: ConfMedium},
	}
	a.livePID[100] = key
	cs.table[100] = Seed{SessionID: "real", Tool: "codex", ProjectID: 4}

	res := a.ReconcileLateSeeds(now, DefaultLateSeedPolicy())
	if res.Roots != 1 {
		t.Fatalf("Roots = %d, want 1", res.Roots)
	}
	got := a.runs[key].Attribution
	if got.SessionID != "real" || got.Source != AttrBridge || got.Confidence != ConfHigh {
		t.Errorf("attribution = %+v, want real/bridge/high", got)
	}
}

// TestLateSeedSecondPassIsANoOp pins idempotence: the upgrade lands the subtree
// at high, and `outranks` is strict, so a re-run classifies every one of those
// runs as authoritative before any lookup — no double-write, no source churn.
func TestLateSeedSecondPassIsANoOp(t *testing.T) {
	t.Parallel()
	a, cs := lateSeedTree(t)
	clk := &evClock{t: time.Unix(1_700_000_000, 0).UTC()}
	a.Observe(execEv("b", 100, 1, 1000, "/usr/bin/observer", []string{"observer", "codex"}, clk.next()), nil)
	a.Observe(forkEv("b", 200, 100, 2000, clk.next()), nil)
	a.Observe(execEv("b", 200, 100, 2000, "/usr/bin/codex", []string{"codex"}, clk.next()), nil)
	cs.table[100] = Seed{SessionID: "sess-1", Tool: "codex", ProjectID: 3}
	now := clk.t.Add(30 * time.Second)

	first := a.ReconcileLateSeeds(now, DefaultLateSeedPolicy())
	if len(first.Upgraded) != 2 {
		t.Fatalf("first pass upgraded %d, want 2", len(first.Upgraded))
	}
	snapshot := map[string]Attribution{}
	for k, r := range a.runs {
		snapshot[k] = r.Attribution
	}
	callsAfterFirst := cs.calls

	second := a.ReconcileLateSeeds(now, DefaultLateSeedPolicy())
	if len(second.Upgraded) != 0 || second.Roots != 0 || second.Reinherited != 0 {
		t.Fatalf("second pass was not a no-op: %+v", second)
	}
	if second.Candidates != 0 || second.Probed != 0 {
		t.Errorf("second pass still probed: candidates=%d probed=%d", second.Candidates, second.Probed)
	}
	if cs.calls != callsAfterFirst {
		t.Errorf("SeedLookup calls grew %d -> %d on an idempotent re-run", callsAfterFirst, cs.calls)
	}
	for k, want := range snapshot {
		if got := a.runs[k].Attribution; got != want {
			t.Errorf("run %s churned: %+v -> %+v", k[:8], want, got)
		}
	}
}

// TestLateSeedSubtreeSkipsExitedAndUntrackedDescendants pins that the
// re-inheritance walk touches only live, tracked processes: a descendant flagged
// Exited is skipped, and a subtree orphaned by an exited middle node is not
// resurrected through the dead parent.
func TestLateSeedSubtreeSkipsExitedAndUntrackedDescendants(t *testing.T) {
	t.Parallel()
	a, cs := lateSeedTree(t)
	clk := &evClock{t: time.Unix(1_700_000_000, 0).UTC()}
	a.Observe(execEv("b", 100, 1, 1000, "/usr/bin/observer", []string{"observer", "grok"}, clk.next()), nil)
	// Live child.
	a.Observe(forkEv("b", 200, 100, 2000, clk.next()), nil)
	a.Observe(execEv("b", 200, 100, 2000, "/usr/bin/grok", []string{"grok"}, clk.next()), nil)
	// Middle child that exits (exit() detaches it from the tree) — and its own
	// child, which stays tracked but is now orphaned from the root.
	a.Observe(forkEv("b", 300, 100, 3000, clk.next()), nil)
	a.Observe(execEv("b", 300, 100, 3000, "/bin/sh", []string{"sh"}, clk.next()), nil)
	a.Observe(forkEv("b", 400, 300, 4000, clk.next()), nil)
	a.Observe(execEv("b", 400, 300, 4000, "/usr/bin/git", []string{"git", "status"}, clk.next()), nil)
	a.Observe(exitEv("b", 300, 3000, 0, clk.next()), nil)
	// A descendant still in the map but already flagged finished (defensive:
	// the normal exit path detaches, this pins the guard regardless).
	zombieKey := keyOf(500, 5000)
	a.runs[zombieKey] = &ProcessRun{
		ProcessKey: zombieKey, BootID: "b", PID: 500, StartTimeTicks: 5000,
		ParentProcessKey: keyOf(100, 1000), StartedAt: clk.t, Exited: true,
	}

	cs.table[100] = Seed{SessionID: "sess-1", Tool: "grok", ProjectID: 5}
	res := a.ReconcileLateSeeds(clk.t.Add(time.Minute), DefaultLateSeedPolicy())

	if res.Roots != 1 || res.Reinherited != 1 {
		t.Fatalf("want 1 root + 1 reinherited (only the live grok child), got %+v", res)
	}
	if got := a.runs[zombieKey].Attribution; got.SessionID != "" {
		t.Errorf("exited descendant was attributed: %+v", got)
	}
	if orphan := a.runs[keyOf(400, 4000)]; orphan.Attributed() {
		t.Errorf("orphaned subtree resurrected through an exited parent: %+v", orphan.Attribution)
	}
	if _, still := a.runs[keyOf(300, 3000)]; still {
		t.Error("exited middle node should have been detached by exit()")
	}
}

// TestLateSeedSubtreeStopsAtBoundaryAndSelfOwningChild pins the two stop
// conditions of the walk (§9.2.6 boundaries, and a descendant that already owns
// a >= confident attribution of its own).
func TestLateSeedSubtreeStopsAtBoundaryAndSelfOwningChild(t *testing.T) {
	t.Parallel()
	a, cs := lateSeedTree(t)
	clk := &evClock{t: time.Unix(1_700_000_000, 0).UTC()}
	a.Observe(execEv("b", 100, 1, 1000, "/usr/bin/observer", []string{"observer", "codex"}, clk.next()), nil)
	// A WSL relay boundary under the root, with a child of its own.
	a.Observe(forkEv("b", 200, 100, 2000, clk.next()), nil)
	a.Observe(execEv("b", 200, 100, 2000, "/init", []string{"wsl"}, clk.next()), nil)
	a.Observe(forkEv("b", 300, 200, 3000, clk.next()), nil)
	a.Observe(execEv("b", 300, 200, 3000, "/bin/sh", []string{"sh"}, clk.next()), nil)
	// A self-owning child: already high-confidence to a DIFFERENT session.
	a.Observe(forkEv("b", 400, 100, 4000, clk.next()), nil)
	a.Observe(execEv("b", 400, 100, 4000, "/usr/bin/claude", []string{"claude"}, clk.next()), nil)
	own := a.runs[keyOf(400, 4000)]
	own.Attribution = Attribution{SessionID: "other", Tool: "claude-code", Source: AttrEnvToken, Confidence: ConfHigh}
	a.Observe(forkEv("b", 500, 400, 5000, clk.next()), nil)
	a.Observe(execEv("b", 500, 400, 5000, "/bin/bash", []string{"bash"}, clk.next()), nil)

	cs.table[100] = Seed{SessionID: "sess-1", Tool: "codex", ProjectID: 3}
	res := a.ReconcileLateSeeds(clk.t.Add(time.Minute), DefaultLateSeedPolicy())

	if res.Roots != 1 || res.Reinherited != 0 {
		t.Fatalf("walk should have changed nothing below the root: %+v", res)
	}
	if a.runs[keyOf(200, 2000)].Attributed() {
		t.Error("a boundary must never be attributed by the walk")
	}
	if a.runs[keyOf(300, 3000)].Attributed() {
		t.Error("attribution leaked through a boundary")
	}
	if got := a.runs[keyOf(400, 4000)].Attribution; got.SessionID != "other" {
		t.Errorf("self-owning child was re-homed: %+v", got)
	}
	if got := a.runs[keyOf(500, 5000)].Attribution; got.SessionID != "other" {
		t.Errorf("subtree of a self-owning child was re-homed: %+v", got)
	}
}

// TestLateSeedSubtreeSurvivesACyclicParentChain pins termination on corrupt
// tree data: a parent-pointer cycle must not spin the walk.
func TestLateSeedSubtreeSurvivesACyclicParentChain(t *testing.T) {
	t.Parallel()
	a, cs := lateSeedTree(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	ka, kb, kc := keyOf(100, 1000), keyOf(200, 2000), keyOf(300, 3000)
	// A <- C <- B <- A : every node is some node's child, so the parent chain
	// closes on itself.
	a.runs[ka] = &ProcessRun{ProcessKey: ka, PID: 100, StartedAt: now, ParentProcessKey: kb}
	a.runs[kb] = &ProcessRun{ProcessKey: kb, PID: 200, StartedAt: now, ParentProcessKey: kc}
	a.runs[kc] = &ProcessRun{ProcessKey: kc, PID: 300, StartedAt: now, ParentProcessKey: ka}
	// A self-parenting node must not become its own child either.
	kd := keyOf(400, 4000)
	a.runs[kd] = &ProcessRun{ProcessKey: kd, PID: 400, StartedAt: now, ParentProcessKey: kd}
	for pid, k := range map[int]string{100: ka, 200: kb, 300: kc, 400: kd} {
		a.livePID[pid] = k
	}
	cs.table[100] = Seed{SessionID: "sess-1", Tool: "codex", ProjectID: 3}

	done := make(chan LateSeedResult, 1)
	go func() { done <- a.ReconcileLateSeeds(now, DefaultLateSeedPolicy()) }()
	select {
	case res := <-done:
		if res.Roots != 1 || res.Reinherited != 2 {
			t.Fatalf("cycle walk = %+v, want 1 root + 2 reinherited", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReconcileLateSeeds did not terminate on a cyclic parent chain")
	}
	if a.runs[kd].Attributed() {
		t.Error("a self-parenting node must not be reached")
	}
}

// TestLateSeedWorkIsBounded pins the cost contract: the pass examines the live
// tree but PROBES only the small candidate slice, and MaxLookups is a hard
// ceiling. It must never behave like a full-table re-resolution.
func TestLateSeedWorkIsBounded(t *testing.T) {
	t.Parallel()
	a, cs := lateSeedTree(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	// 200 already-high runs (the steady state on a claude-code box), 200 stale
	// ones, and 3 genuine candidates.
	add := func(pid int, started time.Time, attr Attribution) {
		k := keyOf(pid, int64(pid))
		a.runs[k] = &ProcessRun{ProcessKey: k, PID: pid, StartTimeTicks: int64(pid), StartedAt: started, Attribution: attr}
		a.livePID[pid] = k
	}
	high := Attribution{SessionID: "s", Tool: "claude-code", Source: AttrInherited, Confidence: ConfHigh}
	for pid := 1000; pid < 1200; pid++ {
		add(pid, now.Add(-time.Minute), high)
	}
	for pid := 2000; pid < 2200; pid++ {
		add(pid, now.Add(-time.Hour), Attribution{})
	}
	for pid := 3000; pid < 3003; pid++ {
		add(pid, now.Add(-time.Minute), Attribution{})
	}

	res := a.ReconcileLateSeeds(now, DefaultLateSeedPolicy())
	if res.Examined != 403 {
		t.Fatalf("Examined = %d, want 403", res.Examined)
	}
	if res.Candidates != 3 || res.Probed != 3 {
		t.Fatalf("candidates=%d probed=%d, want 3/3 — the pass must not scan everything", res.Candidates, res.Probed)
	}
	if cs.calls != 3 {
		t.Fatalf("SeedLookup calls = %d, want 3", cs.calls)
	}
	for _, pid := range cs.pids {
		if pid < 3000 {
			t.Errorf("probed pid %d, which is neither recent nor below high confidence", pid)
		}
	}

	// MaxLookups clips, deterministically newest-first.
	cs.calls, cs.pids = 0, nil
	clipped := a.ReconcileLateSeeds(now, LateSeedPolicy{MaxLookups: 2})
	if !clipped.Truncated || clipped.Probed != 2 || cs.calls != 2 {
		t.Fatalf("MaxLookups not enforced: %+v (calls=%d)", clipped, cs.calls)
	}
}

// TestLateSeedDisabledAndNoSeedLookup pins the opt-out and the nil-seam path.
func TestLateSeedDisabledAndNoSeedLookup(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()

	a, cs := lateSeedTree(t)
	k := keyOf(100, 1000)
	a.runs[k] = &ProcessRun{ProcessKey: k, PID: 100, StartedAt: now}
	cs.table[100] = Seed{SessionID: "s", Tool: "codex"}
	if res := a.ReconcileLateSeeds(now, LateSeedPolicy{Interval: -1}); res.Probed != 0 || len(res.Upgraded) != 0 {
		t.Errorf("negative Interval did not disable the pass: %+v", res)
	}
	if cs.calls != 0 {
		t.Errorf("disabled pass still called SeedLookup %d times", cs.calls)
	}

	noSeed := NewAttributor(nil, nil, nil)
	noSeed.runs[k] = &ProcessRun{ProcessKey: k, PID: 100, StartedAt: now}
	if res := noSeed.ReconcileLateSeeds(now, DefaultLateSeedPolicy()); len(res.Upgraded) != 0 {
		t.Errorf("nil SeedLookup should be a no-op: %+v", res)
	}
}

// TestLateSeedLiftsSelfExcludedLauncher pins the ExcludeOwnBasenames
// interaction. `observer <tool>` is the PTY child the terminal seeder records,
// so it is BOTH the self-excluded basename and the seeded pid. While
// unattributed it is dropped (self_excluded); once a direct seed names it, it
// must persist — exactly as it would have if the seed had existed at exec.
func TestLateSeedLiftsSelfExcludedLauncher(t *testing.T) {
	t.Parallel()
	cs := &countingSeed{table: map[int]Seed{}}
	attr := NewAttributor(cs.lookup, &FieldScrubber{ArgvMode: "preview", MaxPreviewBytes: 512}, nil)
	sink := &SliceSink{}
	o := NewObserver(Options{
		Backend:             &FakeBackend{},
		Attributor:          attr,
		Sink:                sink,
		ExcludeOwnBasenames: []string{"observer"},
		Now:                 func() time.Time { return time.Unix(1_700_000_100, 0).UTC() },
	})

	// Exec while unattributed: dropped as self_excluded, but still TRACKED.
	ev := execEv("b", 100, 1, 1000, "/usr/local/bin/observer", []string{"observer", "kilo"}, time.Unix(1_700_000_000, 0).UTC())
	if run, change := o.handle(&ev); change != ChangeNone || run != nil {
		t.Fatalf("self-excluded unattributed run should be dropped, got change=%v", change)
	}
	if o.Health().Snapshot().Dropped[DropSelfExcluded] != 1 {
		t.Fatalf("expected a self_excluded drop, got %+v", o.Health().Snapshot().Dropped)
	}
	if attr.Tracked() != 1 {
		t.Fatalf("dropped-from-persist run must still be tracked, Tracked()=%d", attr.Tracked())
	}

	// The seed arrives late.
	cs.table[100] = Seed{SessionID: "sess-kilo", Tool: "kilo-code-cli", ProjectID: 9}
	upgraded := o.lateSeedPass()
	if len(upgraded) != 1 {
		t.Fatalf("late pass upgraded %d runs, want 1", len(upgraded))
	}
	if upgraded[0].Attribution.SessionID != "sess-kilo" {
		t.Fatalf("upgraded attribution = %+v", upgraded[0].Attribution)
	}
	h := o.Health().Snapshot()
	if h.LateSeedRoots != 1 || h.AttributedByTool["kilo-code-cli"] != 1 {
		t.Errorf("health not recorded: roots=%d byTool=%+v", h.LateSeedRoots, h.AttributedByTool)
	}
	// The self-exclusion drop count did NOT grow — the pass upgrades, it does
	// not re-run the capture policy.
	if h.Dropped[DropSelfExcluded] != 1 {
		t.Errorf("self_excluded drops = %d, want 1", h.Dropped[DropSelfExcluded])
	}
}

// armedSeed is a SeedLookup that misses until it is armed — the late-arriving
// pidbridge row. Concurrency-safe: the Run goroutine reads it while the test
// arms it.
type armedSeed struct {
	mu    sync.Mutex
	on    bool
	seed  Seed
	calls int
}

func (a *armedSeed) arm() { a.mu.Lock(); a.on = true; a.mu.Unlock() }

func (a *armedSeed) lookup(int) (Seed, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if !a.on {
		return Seed{}, false
	}
	return a.seed, true
}

// heldBackend emits its events and then keeps the channel open until ctx is
// cancelled, so a ticker inside Observer.Run can actually fire.
type heldBackend struct{ events []RawEvent }

func (h *heldBackend) Name() string { return "held" }
func (h *heldBackend) Close() error { return nil }
func (h *heldBackend) Start(ctx context.Context) (<-chan RawEvent, error) {
	ch := make(chan RawEvent, len(h.events)+1)
	go func() {
		defer close(ch)
		for _, ev := range h.events {
			select {
			case <-ctx.Done():
				return
			case ch <- ev:
			}
		}
		<-ctx.Done()
	}()
	return ch, nil
}

// TestObserverRunPersistsLateSeedUpgrades pins the trigger: the pass is driven
// by its own ticker on the Run goroutine and its upgrades reach the Sink.
func TestObserverRunPersistsLateSeedUpgrades(t *testing.T) {
	t.Parallel()
	// The seed is ARMED only after the exec has been observed — the exact
	// `discovered`-lag shape: exec-time resolution runs first and finds nothing.
	gate := &armedSeed{seed: Seed{SessionID: "sess-codex", Tool: "codex", ProjectID: 8}}
	attr := NewAttributor(gate.lookup, &FieldScrubber{ArgvMode: "preview", MaxPreviewBytes: 512}, nil)
	sink := &SliceSink{}
	o := NewObserver(Options{
		Backend: &heldBackend{events: []RawEvent{
			execEv("b", 100, 1, 1000, "/usr/bin/codex", []string{"codex"}, time.Unix(1_700_000_000, 0).UTC()),
		}},
		Attributor:                   attr,
		Sink:                         sink,
		CaptureUnattributedAISubtree: true,
		BatchSize:                    100,
		FlushInterval:                5 * time.Millisecond,
		LateSeed:                     LateSeedPolicy{Interval: 5 * time.Millisecond},
		Now:                          func() time.Time { return time.Unix(1_700_000_030, 0).UTC() },
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = o.Run(ctx) }()

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.After(2 * time.Second)
		for !cond() {
			select {
			case <-deadline:
				t.Fatalf("timed out waiting for %s", what)
			case <-time.After(2 * time.Millisecond):
			}
		}
	}
	waitFor("the exec to be observed", func() bool {
		return o.Health().Snapshot().EventsTotal[EventExec] > 0
	})
	if got := o.Health().Snapshot().AttributedByTool["codex"]; got != 0 {
		t.Fatalf("run was attributed at exec (%d) — the test is not exercising the late path", got)
	}
	gate.arm()
	waitFor("the late-seed ticker to fire", func() bool {
		return o.Health().Snapshot().LateSeedRoots > 0
	})
	cancel()
	<-done

	var attributed int
	for _, r := range sink.Runs {
		if r.Attribution.SessionID == "sess-codex" && r.Attribution.Source == AttrBridge {
			attributed++
		}
	}
	if attributed == 0 {
		t.Fatalf("no late-seed upgrade reached the sink; persisted %d runs", len(sink.Runs))
	}
}
