package linuxebpf

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/processobs"
	"github.com/marmutapp/superbased-observer/internal/processobs/poll"
)

// encodeRecord builds a wire record exactly as the BPF program would.
func encodeRecord(typ evType, pid int, startNs uint64, comm string) []byte {
	b := make([]byte, recordSize)
	binary.LittleEndian.PutUint32(b[offType:offType+4], uint32(typ))
	binary.LittleEndian.PutUint32(b[offPID:offPID+4], uint32(pid))
	binary.LittleEndian.PutUint64(b[offStartBoottime:offStartBoottime+8], startNs)
	copy(b[offComm:offComm+commLen], comm)
	return b
}

func TestDecodeEvent(t *testing.T) {
	tests := []struct {
		name    string
		in      []byte
		wantOK  bool
		wantTyp evType
		wantPID int
		wantNs  uint64
		wantCom string
	}{
		{"exec", encodeRecord(evExec, 4242, 70_000_000, "go"), true, evExec, 4242, 70_000_000, "go"},
		{"exit", encodeRecord(evExit, 99, 0, "rg"), true, evExit, 99, 0, "rg"},
		{"comm-fills-16", encodeRecord(evExec, 7, 1, "abcdefghijklmnop"), true, evExec, 7, 1, "abcdefghijklmnop"},
		{"short", make([]byte, recordSize-1), false, 0, 0, 0, ""},
		{"empty", nil, false, 0, 0, 0, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok := decodeEvent(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if ev.Type != tc.wantTyp || ev.PID != tc.wantPID || ev.StartBoottimeNs != tc.wantNs || ev.Comm != tc.wantCom {
				t.Fatalf("decoded = %+v, want type=%d pid=%d ns=%d comm=%q", ev, tc.wantTyp, tc.wantPID, tc.wantNs, tc.wantCom)
			}
		})
	}
}

func TestStartTicksMatchesProcConvention(t *testing.T) {
	// /proc/<pid>/stat starttime = nsec_to_clock_t(start_boottime) = ns / 1e7.
	ev := kernelEvent{StartBoottimeNs: 55_550_000_000} // 5555.0 s of boot-time
	if got, want := ev.StartTicks(), int64(5555); got != want {
		t.Fatalf("StartTicks = %d, want %d (ns/1e7, matching /proc)", got, want)
	}
}

func TestTranslatorExecEnrichedEmitsForkThenExec(t *testing.T) {
	const boot = "boot-xyz"
	fixed := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	const startNs = 9_990_000_000 // → 999 ticks
	proc := poll.ProcInfo{
		PID: 1000, PPID: 7, StartTicks: 999, HasStart: true,
		ExePath: "/usr/bin/rg", Argv: []string{"rg", "foo"}, CWD: "/home/u/proj",
	}
	tr := newTranslator(boot, func() time.Time { return fixed }, func(pid int) (poll.ProcInfo, bool) {
		if pid == 1000 {
			return proc, true
		}
		return poll.ProcInfo{}, false
	})

	out := tr.handle(kernelEvent{Type: evExec, PID: 1000, StartBoottimeNs: startNs, Comm: "rg"})
	if len(out) != 2 || out[0].Type != processobs.EventFork || out[1].Type != processobs.EventExec {
		t.Fatalf("want fork+exec, got %+v", out)
	}
	ex := out[1]
	if ex.ExePath != "/usr/bin/rg" || ex.CWD != "/home/u/proj" || len(ex.Argv) != 2 {
		t.Fatalf("exec not enriched: %+v", ex)
	}
	// Key comes from the KERNEL start-time (999 ticks), and must equal the poll
	// ProcessKey for the same process so the backends dedup.
	if ex.StartTimeTicks != 999 || !ex.HasStartTime {
		t.Fatalf("exec start ticks = %d (hasStart %v), want 999/true from kernel", ex.StartTimeTicks, ex.HasStartTime)
	}
	want := processobs.ProcessKey(boot, 1000, 999)
	got := processobs.ProcessKey(ex.BootID, ex.PID, ex.StartTimeTicks)
	if got != want {
		t.Fatalf("ProcessKey mismatch: %s vs poll %s", got, want)
	}
}

func TestTranslatorExecVanishedIsStillKeyed(t *testing.T) {
	// The whole point of the in-kernel start-time: a process gone before the
	// /proc read is STILL keyed (existence recorded), just without argv/cwd.
	tr := newTranslator("b", time.Now, func(int) (poll.ProcInfo, bool) {
		return poll.ProcInfo{}, false
	})
	out := tr.handle(kernelEvent{Type: evExec, PID: 42, StartBoottimeNs: 30_000_000, Comm: "date"})
	if len(out) != 1 || out[0].Type != processobs.EventExec {
		t.Fatalf("want one exec, got %+v", out)
	}
	ex := out[0]
	if !ex.HasStartTime || ex.StartTimeTicks != 3 {
		t.Fatalf("vanished exec must be keyed from kernel start: %+v", ex)
	}
	if ex.PID != 42 || ex.ExePath != "date" {
		t.Fatalf("vanished exec lost identity (comm→ExePath): %+v", ex)
	}
}

func TestTranslatorExitSelfKeys(t *testing.T) {
	const boot = "b"
	tr := newTranslator(boot, time.Now, func(int) (poll.ProcInfo, bool) {
		t.Fatal("enrich must not be called for exit")
		return poll.ProcInfo{}, false
	})
	out := tr.handle(kernelEvent{Type: evExit, PID: 500, StartBoottimeNs: 90_010_000_000, Comm: "git"})
	if len(out) != 1 || out[0].Type != processobs.EventExit {
		t.Fatalf("want one exit, got %+v", out)
	}
	// Exit keys from its OWN kernel start-time (no exec→exit shared map) and must
	// match the exec key for the same process.
	if out[0].StartTimeTicks != 9001 || !out[0].HasStartTime {
		t.Fatalf("exit not self-keyed: %+v", out[0])
	}
	if k1, k2 := processobs.ProcessKey(boot, 500, 9001), processobs.ProcessKey(out[0].BootID, out[0].PID, out[0].StartTimeTicks); k1 != k2 {
		t.Fatalf("exit key %s != exec key %s", k2, k1)
	}
}
