package processobs

import (
	"strings"
	"testing"
	"time"
)

// evClock hands out monotonically increasing timestamps so duration math
// is deterministic.
type evClock struct{ t time.Time }

func (c *evClock) next() time.Time { c.t = c.t.Add(time.Second); return c.t }

func bridgeSeed(rootPID int, sess, tool string, project int64) SeedLookup {
	return func(pid int) (Seed, bool) {
		if pid == rootPID {
			return Seed{SessionID: sess, Tool: tool, ProjectID: project}, true
		}
		return Seed{}, false
	}
}

func forkEv(boot string, pid, ppid int, start int64, ts time.Time) RawEvent {
	return RawEvent{Type: EventFork, BootID: boot, PID: pid, PPID: ppid, StartTimeTicks: start, HasStartTime: true, Timestamp: ts}
}

func execEv(boot string, pid, ppid int, start int64, exe string, argv []string, ts time.Time) RawEvent {
	return RawEvent{Type: EventExec, BootID: boot, PID: pid, PPID: ppid, StartTimeTicks: start, HasStartTime: true, ExePath: exe, Argv: argv, CWD: "/proj", Timestamp: ts}
}

func exitEv(boot string, pid int, start int64, code int, ts time.Time) RawEvent {
	return RawEvent{Type: EventExit, BootID: boot, PID: pid, StartTimeTicks: start, HasStartTime: true, ExitCode: code, Timestamp: ts}
}

// TestAttributionInheritsThroughDescendants pins §9.2.1–2: a direct bridge
// hit on the AI-tool root is high-confidence, and forked descendants
// inherit it (as AttrInherited) even when they exec into other binaries
// that the bridge knows nothing about.
func TestAttributionInheritsThroughDescendants(t *testing.T) {
	t.Parallel()
	clk := &evClock{t: time.Unix(1_700_000_000, 0).UTC()}
	a := NewAttributor(bridgeSeed(100, "sess-1", "claude-code", 7), &FieldScrubber{ArgvMode: "preview", MaxPreviewBytes: 512}, nil)

	// Root claude-code (pid 100) execs — bridge hit.
	root, ch := a.Observe(execEv("b", 100, 1, 1000, "/usr/bin/claude", []string{"claude"}, clk.next()), nil)
	if ch != ChangeCreated || root.Attribution.Source != AttrBridge || root.Attribution.Confidence != ConfHigh {
		t.Fatalf("root attribution = %+v change=%v", root.Attribution, ch)
	}
	if root.Attribution.SessionID != "sess-1" || root.Attribution.ProjectID != 7 {
		t.Fatalf("root session/project not seeded: %+v", root.Attribution)
	}

	// Root forks a shell (pid 200), which execs bash.
	a.Observe(forkEv("b", 200, 100, 2000, clk.next()), nil)
	shell, _ := a.Observe(execEv("b", 200, 100, 2000, "/bin/bash", []string{"bash", "-c", "npm test"}, clk.next()), nil)
	if !shell.Attributed() || shell.Attribution.Source != AttrInherited || shell.Attribution.SessionID != "sess-1" {
		t.Fatalf("shell did not inherit: %+v", shell.Attribution)
	}

	// Shell forks node (pid 300) — second-level descendant still inherits.
	a.Observe(forkEv("b", 300, 200, 3000, clk.next()), nil)
	node, _ := a.Observe(execEv("b", 300, 200, 3000, "/usr/bin/node", []string{"node", "x.js"}, clk.next()), nil)
	if node.Attribution.SessionID != "sess-1" || node.Attribution.Source != AttrInherited {
		t.Fatalf("grandchild did not inherit: %+v", node.Attribution)
	}
	// Argv was captured + scrubbed on exec.
	if node.ArgvArgc != 2 || node.ArgvHash == "" {
		t.Errorf("argv not captured: argc=%d hash=%q", node.ArgvArgc, node.ArgvHash)
	}
}

// TestBoundaryStopsInheritance pins §9.2.6: an init/systemd/WSL-relay
// process is an attribution boundary — it does not inherit, and its
// descendants do not inherit through it.
func TestBoundaryStopsInheritance(t *testing.T) {
	t.Parallel()
	clk := &evClock{t: time.Unix(1_700_000_000, 0).UTC()}
	a := NewAttributor(bridgeSeed(100, "sess-1", "claude-code", 7), nil, nil)

	a.Observe(execEv("b", 100, 1, 1000, "/usr/bin/claude", []string{"claude"}, clk.next()), nil)
	a.Observe(forkEv("b", 200, 100, 2000, clk.next()), nil)
	// pid 200 execs into a WSL relay — a boundary, even though its parent
	// is attributed.
	relay, _ := a.Observe(execEv("b", 200, 100, 2000, "/init", []string{"wsl"}, clk.next()), nil)
	if !relay.IsBoundary {
		t.Fatalf("relay should be a boundary, got %+v", relay)
	}
	if relay.Attributed() {
		t.Errorf("boundary must not be attributed: %+v", relay.Attribution)
	}

	// A child of the relay must NOT inherit the session.
	a.Observe(forkEv("b", 300, 200, 3000, clk.next()), nil)
	child, _ := a.Observe(execEv("b", 300, 200, 3000, "/bin/sh", []string{"sh"}, clk.next()), nil)
	if child.Attributed() {
		t.Errorf("descendant through a boundary must be unattributed: %+v", child.Attribution)
	}
}

// TestExitFinalizesAndDetaches pins exit semantics: the run gets exit
// fields + a positive duration, the change is ChangeUpdated, and the tree
// stops tracking it (memory bound).
func TestExitFinalizesAndDetaches(t *testing.T) {
	t.Parallel()
	clk := &evClock{t: time.Unix(1_700_000_000, 0).UTC()}
	a := NewAttributor(bridgeSeed(100, "s", "claude-code", 1), nil, nil)

	a.Observe(execEv("b", 100, 1, 1000, "/usr/bin/claude", []string{"claude"}, clk.next()), nil)
	if a.Tracked() != 1 {
		t.Fatalf("expected 1 tracked, got %d", a.Tracked())
	}
	run, ch := a.Observe(exitEv("b", 100, 1000, 3, clk.next()), nil)
	if ch != ChangeUpdated {
		t.Fatalf("exit change = %v want ChangeUpdated", ch)
	}
	if !run.Exited || run.ExitCode != 3 || run.DurationMs <= 0 {
		t.Errorf("exit not finalized: exited=%v code=%d dur=%d", run.Exited, run.ExitCode, run.DurationMs)
	}
	if a.Tracked() != 0 {
		t.Errorf("exited run not detached: %d still tracked", a.Tracked())
	}
}

// TestPIDReuseNoStaleLinkage pins §9.3 at the tree level: after a pid's
// process exits, a NEW process reusing that pid (new start time) gets a
// fresh key and does NOT inherit the dead process's attribution.
func TestPIDReuseNoStaleLinkage(t *testing.T) {
	t.Parallel()
	clk := &evClock{t: time.Unix(1_700_000_000, 0).UTC()}
	// Bridge maps NOTHING — so any attribution could only come from stale
	// tree state. The first pid-100 process is force-attributed by hand.
	a := NewAttributor(nil, nil, nil)

	first, _ := a.Observe(execEv("b", 100, 1, 1000, "/bin/bash", []string{"bash"}, clk.next()), nil)
	first.Attribution = Attribution{SessionID: "ghost", Source: AttrBridge, Confidence: ConfHigh}
	firstKey := first.ProcessKey
	a.Observe(exitEv("b", 100, 1000, 0, clk.next()), nil)

	// pid 100 reused, different start time → different key, no inheritance.
	second, _ := a.Observe(execEv("b", 100, 1, 2000, "/bin/bash", []string{"bash"}, clk.next()), nil)
	if second.ProcessKey == firstKey {
		t.Fatal("reused pid produced the same process key — PID-reuse refusal broken")
	}
	if second.Attributed() {
		t.Errorf("reused-pid process inherited stale attribution: %+v", second.Attribution)
	}
}

// TestExecCopiesSecurityPosture pins P4: the §8 security/isolation fields
// flow from the (enriched) RawEvent onto the run, and the cgroup PATH is
// reduced to a hash — the raw path never lands on the run.
func TestExecCopiesSecurityPosture(t *testing.T) {
	t.Parallel()
	a := NewAttributor(bridgeSeed(100, "s", "claude-code", 1), nil, nil)
	ev := execEv("b", 100, 1, 1000, "/usr/bin/claude", []string{"claude"}, time.Unix(1_700_000_000, 0).UTC())
	ev.SeccompMode = "filter"
	ev.CapabilitiesEff = "00000000a80425fb"
	ev.AppArmorLabel = "unconfined"
	ev.ContainerID = "abc123def456"
	ev.PIDNamespace = "4026531836"
	ev.MountNamespace = "4026531840"
	ev.NetNamespace = "4026531992"
	ev.CgroupPath = "/system.slice/docker-abc123def456.scope"

	run, _ := a.Observe(ev, nil)
	if run.SeccompMode != "filter" || run.CapabilitiesEff != "00000000a80425fb" {
		t.Errorf("seccomp/caps not copied: %+v", run)
	}
	if run.AppArmorLabel != "unconfined" {
		t.Errorf("apparmor = %q", run.AppArmorLabel)
	}
	if run.ContainerID != "abc123def456" || run.PIDNamespace != "4026531836" || run.NetNamespace != "4026531992" {
		t.Errorf("container/namespace not copied: %+v", run)
	}
	// cgroup path is HASHED, never stored raw.
	if !strings.HasPrefix(run.CgroupHash, "sha256:") {
		t.Errorf("cgroup hash = %q, want sha256: prefix", run.CgroupHash)
	}
	if strings.Contains(run.CgroupHash, "system.slice") {
		t.Error("raw cgroup path leaked into the hash field")
	}
}

// TestForkWithoutStartTimeIsDropped pins §9.3: an unkeyable fork/exec is
// not tracked.
func TestForkWithoutStartTimeIsDropped(t *testing.T) {
	t.Parallel()
	a := NewAttributor(nil, nil, nil)
	ev := RawEvent{Type: EventFork, BootID: "b", PID: 200, PPID: 100, HasStartTime: false, Timestamp: time.Now()}
	if run, ch := a.Observe(ev, nil); run != nil || ch != ChangeNone {
		t.Errorf("expected drop, got run=%v change=%v", run, ch)
	}
	if a.Tracked() != 0 {
		t.Errorf("unkeyable event was tracked: %d", a.Tracked())
	}
}

func tokenSeedLookup(wantToken, sess, tool string, project int64) TokenLookup {
	return func(token string) (Seed, bool) {
		if token == wantToken {
			return Seed{SessionID: sess, Tool: tool, ProjectID: project}, true
		}
		return Seed{}, false
	}
}

func execEvTok(boot string, pid, ppid int, start int64, exe string, argv []string, token string, ts time.Time) RawEvent {
	ev := execEv(boot, pid, ppid, start, exe, argv, ts)
	ev.SessionToken = token
	return ev
}

// TestAttributionEnvTokenHighConfidence pins §5.5 P-B6 EV: a process whose
// captured session-id env token resolves to an existing session is attributed
// env_token / high with the session's tool+project, with NO pid seed (the
// cross-OS topology). Because every descendant inherits the env var, each
// re-resolves to env_token/high DIRECTLY at its own exec (not merely inherited)
// — the whole subtree is high-confidence.
func TestAttributionEnvTokenHighConfidence(t *testing.T) {
	t.Parallel()
	clk := &evClock{t: time.Unix(1_700_000_000, 0).UTC()}
	a := NewAttributor(nil, &FieldScrubber{ArgvMode: "preview", MaxPreviewBytes: 512}, nil)
	a.SetTokenLookup(tokenSeedLookup("19f16087", "19f16087", "claude-code", 7))

	root, ch := a.Observe(execEvTok("b", 100, 1, 1000, `C:\claude.exe`, []string{"claude"}, "19f16087", clk.next()), nil)
	if ch != ChangeCreated || root.Attribution.Source != AttrEnvToken || root.Attribution.Confidence != ConfHigh {
		t.Fatalf("root EV attribution = %+v change=%v", root.Attribution, ch)
	}
	if root.Attribution.SessionID != "19f16087" || root.Attribution.Tool != "claude-code" || root.Attribution.ProjectID != 7 {
		t.Fatalf("root EV session/tool/project = %+v", root.Attribution)
	}

	// A forked child that execs another binary still carries the token (env
	// inheritance) → re-resolves to env_token/high DIRECTLY, not just inherited.
	a.Observe(forkEv("b", 200, 100, 2000, clk.next()), nil)
	child, _ := a.Observe(execEvTok("b", 200, 100, 2000, `C:\node.exe`, []string{"node", "x.js"}, "19f16087", clk.next()), nil)
	if child.Attribution.Source != AttrEnvToken || child.Attribution.Confidence != ConfHigh || child.Attribution.SessionID != "19f16087" {
		t.Fatalf("child should re-resolve EV directly: %+v", child.Attribution)
	}
}

// TestAttributionEnvTokenMissUnattributed pins that a token which does NOT
// resolve to an existing session leaves the run unattributed (it then falls
// back to the deferred medium CorrelateCrossOS pass downstream).
func TestAttributionEnvTokenMissUnattributed(t *testing.T) {
	t.Parallel()
	clk := &evClock{t: time.Unix(1_700_000_000, 0).UTC()}
	a := NewAttributor(nil, nil, nil)
	a.SetTokenLookup(tokenSeedLookup("known", "known", "claude-code", 1))

	run, _ := a.Observe(execEvTok("b", 100, 1, 1000, `C:\x.exe`, []string{"x"}, "unknown-session", clk.next()), nil)
	if run.Attributed() {
		t.Errorf("unresolvable token must not attribute: %+v", run.Attribution)
	}
}

// TestAttributionEnvTokenPreemptsCollidingPIDSeed pins the resolve order: the
// namespace-independent env token wins over a pid seed that (across the OS
// boundary) numerically collides with an unrelated WSL pidbridge entry. The
// process is attributed to the token's session, not the colliding seed's.
func TestAttributionEnvTokenPreemptsCollidingPIDSeed(t *testing.T) {
	t.Parallel()
	clk := &evClock{t: time.Unix(1_700_000_000, 0).UTC()}
	a := NewAttributor(bridgeSeed(100, "wsl-collision", "codex", 9), nil, nil)
	a.SetTokenLookup(tokenSeedLookup("real-claude", "real-claude", "claude-code", 7))

	run, _ := a.Observe(execEvTok("b", 100, 1, 1000, `C:\claude.exe`, []string{"claude"}, "real-claude", clk.next()), nil)
	if run.Attribution.Source != AttrEnvToken || run.Attribution.SessionID != "real-claude" || run.Attribution.Tool != "claude-code" {
		t.Fatalf("env token must preempt a colliding pid seed: %+v", run.Attribution)
	}
}

// TestAttributionEnvTokenDisabledByDefault pins the additive contract: with no
// SetTokenLookup installed, a captured SessionToken is ignored and the existing
// pid-seed path governs unchanged.
func TestAttributionEnvTokenDisabledByDefault(t *testing.T) {
	t.Parallel()
	clk := &evClock{t: time.Unix(1_700_000_000, 0).UTC()}
	a := NewAttributor(bridgeSeed(100, "sess-1", "claude-code", 7), nil, nil)

	run, _ := a.Observe(execEvTok("b", 100, 1, 1000, `C:\claude.exe`, []string{"claude"}, "sess-1", clk.next()), nil)
	if run.Attribution.Source != AttrBridge {
		t.Fatalf("with no token lookup, the pid seed governs: %+v", run.Attribution)
	}
}

// TestAttributionMetricsRefresh pins EventMetrics: a metrics refresh updates a
// tracked run's resource counters + appends a sparkline sample WITHOUT changing
// its attribution; a refresh for an unknown process is a no-op.
func TestAttributionMetricsRefresh(t *testing.T) {
	t.Parallel()
	clk := &evClock{t: time.Unix(1_700_000_000, 0).UTC()}
	a := NewAttributor(bridgeSeed(100, "s", "claude-code", 1), nil, nil)

	run, ch := a.Observe(execEv("b", 100, 1, 1000, "/usr/bin/claude", []string{"claude"}, clk.next()), nil)
	if ch != ChangeCreated {
		t.Fatalf("exec change = %v", ch)
	}
	src := run.Attribution.Source

	mev := RawEvent{
		Type: EventMetrics, BootID: "b", PID: 100, StartTimeTicks: 1000,
		HasStartTime: true, HasMetrics: true, Timestamp: clk.next(),
		CPUUserMs: 50, CPUSystemMs: 10, WorkingSetBytes: 4096, MaxRSSBytes: 8192, ReadBytes: 100,
	}
	run2, ch2 := a.Observe(mev, nil)
	if ch2 != ChangeUpdated || run2 == nil {
		t.Fatalf("metrics change = %v run=%v", ch2, run2)
	}
	if run2.CPUUserMs != 50 || run2.WorkingSetBytes != 4096 || run2.MaxRSSBytes != 8192 {
		t.Errorf("metrics not applied: %+v", run2)
	}
	if run2.Attribution.Source != src {
		t.Errorf("metrics refresh changed attribution: %v -> %v", src, run2.Attribution.Source)
	}
	if len(run2.MetricSamples) != 1 || run2.MetricSamples[0].CPUMs != 60 {
		t.Errorf("sparkline sample not appended: %+v", run2.MetricSamples)
	}

	// Refresh for an untracked process is a clean no-op.
	if _, ch3 := a.Observe(RawEvent{
		Type: EventMetrics, BootID: "b", PID: 999, StartTimeTicks: 1,
		HasStartTime: true, HasMetrics: true, Timestamp: clk.next(),
	}, nil); ch3 != ChangeNone {
		t.Errorf("metrics for unknown pid = %v, want ChangeNone", ch3)
	}
}

// TestAttributionMetricsSparklineThrottle pins the ring-buffer throttle: refreshes
// within metricSampleInterval update the current point in place (values change,
// count stays), and only a refresh PAST the interval appends a new point — so a
// sparkline accrues one point per interval, not one per poll.
func TestAttributionMetricsSparklineThrottle(t *testing.T) {
	t.Parallel()
	base := time.Unix(1_700_000_000, 0).UTC()
	a := NewAttributor(bridgeSeed(100, "s", "claude-code", 1), nil, nil)

	mev := func(ts time.Time, ws int64) RawEvent {
		return RawEvent{
			Type: EventMetrics, BootID: "b", PID: 100, StartTimeTicks: 1000,
			HasStartTime: true, HasMetrics: true, Timestamp: ts, WorkingSetBytes: ws,
		}
	}
	a.Observe(RawEvent{
		Type: EventExec, BootID: "b", PID: 100, PPID: 1, StartTimeTicks: 1000,
		HasStartTime: true, HasMetrics: true, Timestamp: base,
		ExePath: "/usr/bin/claude", WorkingSetBytes: 100,
	}, nil) // first sample

	a.Observe(mev(base.Add(5*time.Second), 200), nil)
	run, _ := a.Observe(mev(base.Add(10*time.Second), 300), nil)
	if len(run.MetricSamples) != 1 {
		t.Fatalf("within-interval refreshes must not append: got %d samples", len(run.MetricSamples))
	}
	if run.MetricSamples[0].WorkingSet != 300 {
		t.Errorf("in-place refresh should update values, got ws=%d", run.MetricSamples[0].WorkingSet)
	}

	run, _ = a.Observe(mev(base.Add(20*time.Second), 400), nil)
	if len(run.MetricSamples) != 2 {
		t.Fatalf("refresh past the interval should append: got %d samples", len(run.MetricSamples))
	}
}

// TestEvictOldestLive pins the memory bound for never-exiting processes.
func TestEvictOldestLive(t *testing.T) {
	t.Parallel()
	clk := &evClock{t: time.Unix(1_700_000_000, 0).UTC()}
	a := NewAttributor(nil, nil, nil)
	for i := 0; i < 5; i++ {
		a.Observe(execEv("b", 100+i, 1, int64(1000+i), "/bin/x", []string{"x"}, clk.next()), nil)
	}
	if a.Tracked() != 5 {
		t.Fatalf("tracked=%d", a.Tracked())
	}
	if n := a.EvictOldestLive(3); n != 2 {
		t.Errorf("evicted=%d want 2", n)
	}
	if a.Tracked() != 3 {
		t.Errorf("tracked after evict=%d want 3", a.Tracked())
	}
	if a.EvictOldestLive(0) != 0 {
		t.Error("max<=0 must not evict")
	}
}
