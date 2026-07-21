package poll

import (
	"context"
	"errors"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

var tPoll = time.Date(2026, 6, 17, 9, 0, 0, 0, time.UTC)

func newBackend(enum EnumerateFunc) *Backend {
	return New(Options{
		Interval:  time.Hour, // ticker never fires during a focused test
		Enumerate: enum,
		BootID:    "boot-1",
		Now:       func() time.Time { return tPoll },
	})
}

func pi(pid, ppid int, start int64, exe string) ProcInfo {
	return ProcInfo{PID: pid, PPID: ppid, StartTicks: start, HasStart: true, ExePath: exe}
}

// TestStartEmitsInitialExecOnly pins the baseline behavior: the first
// snapshot emits exec (not fork) for every process, parents before children,
// stamped with the boot id.
func TestStartEmitsInitialExecOnly(t *testing.T) {
	t.Parallel()
	// Deliberately unsorted: child (200) before parent (100).
	snap := []ProcInfo{pi(200, 100, 2000, "/bin/child"), pi(100, 1, 1000, "/bin/root")}
	b := newBackend(func() ([]ProcInfo, error) { return snap, nil })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := b.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	e1 := <-ch
	e2 := <-ch
	cancel()

	if e1.Type != processobs.EventExec || e1.PID != 100 {
		t.Errorf("first event = %s pid=%d, want exec pid=100 (parent first)", e1.Type, e1.PID)
	}
	if e2.Type != processobs.EventExec || e2.PID != 200 {
		t.Errorf("second event = %s pid=%d, want exec pid=200", e2.Type, e2.PID)
	}
	if e1.BootID != "boot-1" || !e1.HasStartTime || e1.StartTimeTicks != 1000 {
		t.Errorf("ProcessKey inputs not stamped: %+v", e1)
	}
	if e1.ExePath != "/bin/root" {
		t.Errorf("exe not carried: %q", e1.ExePath)
	}
}

func TestDiffForkExecAndExit(t *testing.T) {
	t.Parallel()
	b := newBackend(func() ([]ProcInfo, error) { return nil, nil })
	prev := index([]ProcInfo{pi(100, 1, 1000, "/bin/root"), pi(200, 100, 2000, "/bin/old")})
	cur := index([]ProcInfo{pi(100, 1, 1000, "/bin/root"), pi(300, 100, 3000, "/bin/new")})

	evs := b.diff(prev, cur)
	// 100 survives (no event); 300 appeared (fork+exec); 200 vanished (exit).
	var fork, exec, exit *processobs.RawEvent
	for i := range evs {
		switch evs[i].Type {
		case processobs.EventFork:
			fork = &evs[i]
		case processobs.EventExec:
			exec = &evs[i]
		case processobs.EventExit:
			exit = &evs[i]
		}
	}
	if fork == nil || fork.PID != 300 || fork.PPID != 100 {
		t.Errorf("fork = %+v, want pid300 ppid100", fork)
	}
	if exec == nil || exec.PID != 300 || exec.ExePath != "/bin/new" {
		t.Errorf("exec = %+v, want pid300 /bin/new", exec)
	}
	if exit == nil || exit.PID != 200 || exit.StartTimeTicks != 2000 {
		t.Errorf("exit = %+v, want pid200 start2000", exit)
	}
	// fork must precede exec for the same new process (tree before envelope).
	var fi, ei int = -1, -1
	for i := range evs {
		if evs[i].Type == processobs.EventFork && evs[i].PID == 300 {
			fi = i
		}
		if evs[i].Type == processobs.EventExec && evs[i].PID == 300 {
			ei = i
		}
	}
	if fi < 0 || ei < 0 || fi > ei {
		t.Errorf("fork(%d) must precede exec(%d) for pid 300", fi, ei)
	}
}

// TestDiffPIDReuse pins §9.3 at the poll layer: a pid whose start time
// changed between snapshots is a reused pid — the old process exits and the
// new one forks+execs, never a survivor.
func TestDiffPIDReuse(t *testing.T) {
	t.Parallel()
	b := newBackend(func() ([]ProcInfo, error) { return nil, nil })
	prev := index([]ProcInfo{pi(100, 1, 1000, "/bin/old")})
	cur := index([]ProcInfo{pi(100, 1, 5000, "/bin/new")}) // same pid, new start

	evs := b.diff(prev, cur)
	if len(evs) != 3 {
		t.Fatalf("reuse should yield exit+fork+exec (3 events), got %d: %+v", len(evs), evs)
	}
	var sawOldExit, sawNewExec bool
	for _, e := range evs {
		if e.Type == processobs.EventExit && e.StartTimeTicks == 1000 {
			sawOldExit = true
		}
		if e.Type == processobs.EventExec && e.StartTimeTicks == 5000 && e.ExePath == "/bin/new" {
			sawNewExec = true
		}
	}
	if !sawOldExit {
		t.Error("missing exit for the old (pid 100, start 1000) process")
	}
	if !sawNewExec {
		t.Error("missing exec for the new (pid 100, start 5000) process")
	}
}

func TestDiffNoChangeNoEvents(t *testing.T) {
	t.Parallel()
	b := newBackend(func() ([]ProcInfo, error) { return nil, nil })
	same := index([]ProcInfo{pi(100, 1, 1000, "/bin/x"), pi(200, 100, 2000, "/bin/y")})
	if evs := b.diff(same, same); len(evs) != 0 {
		t.Errorf("identical snapshots must yield no events, got %d", len(evs))
	}
}

// TestStartProbeFailsFailOpen pins the fail-open contract: if the platform
// enumerate is unavailable, Start returns the error so the Observer reports
// degraded health rather than the daemon crashing.
func TestStartProbeFailsFailOpen(t *testing.T) {
	t.Parallel()
	b := newBackend(func() ([]ProcInfo, error) { return nil, ErrUnsupported })
	if _, err := b.Start(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Start err = %v, want ErrUnsupported", err)
	}
}

// TestRefreshSet pins the item-3 refresh-eligibility logic: a distinctive
// AI-tool launcher (codex.exe) and its whole subtree are eligible — INCLUDING
// generically-named children (cmd.exe, node.exe) reached by lineage — plus any
// token-bearing process; while a lone generic process and a system process are
// not.
func TestRefreshSet(t *testing.T) {
	t.Parallel()
	b := newBackend(func() ([]ProcInfo, error) { return nil, nil })

	codex := pi(100, 1, 1000, `C:\Users\me\AppData\codex.exe`)       // launcher root
	codexChild := pi(200, 100, 2000, `C:\Windows\System32\cmd.exe`)  // eligible via lineage
	codexGrand := pi(300, 200, 3000, `C:\Windows\System32\node.exe`) // generic name, still in subtree
	tokened := pi(400, 1, 4000, `C:\Users\me\node.exe`)              // generic; eligible only via token
	loneNode := pi(500, 1, 5000, `C:\Users\me\node.exe`)             // generic, no token, no AI parent
	sysProc := pi(600, 1, 6000, `C:\Windows\System32\svchost.exe`)   // unrelated system process

	cur := index([]ProcInfo{codex, codexChild, codexGrand, tokened, loneNode, sysProc})
	b.tokened[procKey{pid: 400, start: 4000}] = true // EV-tagged at new-process read

	elig := b.refreshSet(cur)

	for _, k := range []procKey{{pid: 100, start: 1000}, {pid: 200, start: 2000}, {pid: 300, start: 3000}, {pid: 400, start: 4000}} {
		if !elig[k] {
			t.Errorf("pid %d should be refresh-eligible", k.pid)
		}
	}
	for _, k := range []procKey{{pid: 500, start: 5000}, {pid: 600, start: 6000}} {
		if elig[k] {
			t.Errorf("pid %d should NOT be refresh-eligible", k.pid)
		}
	}
	if len(elig) != 4 {
		t.Errorf("eligible set size = %d, want 4: %v", len(elig), elig)
	}
}

// TestDiffEmitsMetricsForAISurvivor confirms diff emits a metrics-refresh for
// survivors in an AI-launcher subtree (item 3) and not for unrelated survivors.
func TestDiffEmitsMetricsForAISurvivor(t *testing.T) {
	t.Parallel()
	b := newBackend(func() ([]ProcInfo, error) { return nil, nil })
	root := pi(100, 1, 1000, `C:\tools\codex.exe`)
	child := pi(200, 100, 2000, `C:\Program Files\nodejs\node.exe`) // generic, in subtree
	sys := pi(300, 1, 3000, `C:\Windows\System32\svchost.exe`)
	snap := index([]ProcInfo{root, child, sys})

	evs := b.diff(snap, snap) // all survive
	gotMetrics := map[int]bool{}
	for _, e := range evs {
		if e.Type == processobs.EventMetrics {
			gotMetrics[e.PID] = true
		}
	}
	if !gotMetrics[100] || !gotMetrics[200] {
		t.Errorf("expected metrics refresh for the codex subtree (100,200), got %v", gotMetrics)
	}
	if gotMetrics[300] {
		t.Error("unrelated svchost (300) should not get a metrics refresh")
	}
}

// TestPlatformEnumerate is the cross-platform smoke test: Linux (/proc) and
// Windows (ToolHelp) both enumerate the live table and must find this test
// process with a usable start time + a boot id; every other OS (macOS, …) has
// no enumerate yet and must report ErrUnsupported (fail-open). The detailed
// Windows path is covered in enum_windows_test.go.
func TestPlatformEnumerate(t *testing.T) {
	t.Parallel()
	procs, err := platformEnumerate()

	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("unsupported-OS enumerate err = %v, want ErrUnsupported", err)
		}
		return
	}

	if err != nil {
		t.Fatalf("%s enumerate: %v", runtime.GOOS, err)
	}
	self := os.Getpid()
	var found *ProcInfo
	for i := range procs {
		if procs[i].PID == self {
			found = &procs[i]
		}
	}
	if found == nil {
		t.Fatalf("enumerate did not find self (pid %d) among %d procs", self, len(procs))
	}
	if !found.HasStart || found.StartTicks == 0 {
		t.Errorf("self has no usable start time: %+v", found)
	}
	if platformBootID() == "" {
		t.Error("boot id should be readable")
	}
	if runtime.GOOS == "linux" {
		// P4 posture: self's namespaces + cgroup are always readable for its
		// own process (no extra privilege needed). Linux-only.
		if found.PIDNamespace == "" || found.NetNamespace == "" {
			t.Errorf("self namespaces not read: pid=%q net=%q", found.PIDNamespace, found.NetNamespace)
		}
		if found.CgroupPath == "" {
			t.Error("self cgroup path not read")
		}
	}
}
