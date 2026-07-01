package poll

import (
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// TestExecEventCarriesCWD pins the cross-OS plumbing: a ProcInfo's CWD (filled
// by the Linux /proc/<pid>/cwd readlink or the Windows PEB read) must reach the
// exec RawEvent, since cross-OS attribution (§5.5) matches it against a
// session's project_root. Pure + Windows-host-safe (injected enumerate).
func TestExecEventCarriesCWD(t *testing.T) {
	b := New(Options{
		Enumerate: func() ([]ProcInfo, error) { return nil, nil },
		BootID:    "boot",
		Now:       func() time.Time { return time.Unix(1000, 0) },
	})
	p := ProcInfo{
		PID: 4242, PPID: 1, StartTicks: 99, HasStart: true,
		ExePath: `C:\Program Files\nodejs\node.exe`,
		Argv:    []string{"node.exe", "server.js"},
		CWD:     `C:\Users\marmu\proj`,
	}
	ev := b.execEvent(&p, time.Unix(1000, 0))
	if ev.Type != processobs.EventExec {
		t.Fatalf("event type = %q, want exec", ev.Type)
	}
	if ev.CWD != `C:\Users\marmu\proj` {
		t.Fatalf("exec event CWD = %q, want the ProcInfo CWD", ev.CWD)
	}
}
