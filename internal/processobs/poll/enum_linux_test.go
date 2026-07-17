//go:build linux

package poll

import (
	"os"
	"testing"
	"time"
)

func TestParseStat(t *testing.T) {
	// Synthetic /proc/<pid>/stat lines. Post-comm field indices: 0=state,
	// 1=ppid, 11=utime, 12=stime, 17=num_threads, 19=starttime. utime/stime are
	// USER_HZ (100) ticks → ms = ticks*10.
	cases := []struct {
		name string
		raw  string
		want statFields
		ok   bool
	}{
		{
			name: "simple",
			//        pid comm     st ppid 2 3 4 5 6 7 8 9 10 ut st 13 14 15 16 thr 18 start
			raw:  "1234 (bash) S 1000 2 3 4 5 6 7 8 9 10 50 20 13 14 15 16 7 18 99999",
			want: statFields{ppid: 1000, cpuUserMs: 500, cpuSystemMs: 200, threads: 7, start: 99999},
			ok:   true,
		},
		{
			name: "comm with spaces and parens",
			raw:  "999 (weird ) (name) S 50 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 3 18 7777",
			want: statFields{ppid: 50, cpuUserMs: 110, cpuSystemMs: 120, threads: 3, start: 7777},
			ok:   true,
		},
		{
			name: "zero cpu",
			raw:  "5 (q) R 1 2 3 4 5 6 7 8 9 10 0 0 13 14 15 16 1 18 42",
			want: statFields{ppid: 1, cpuUserMs: 0, cpuSystemMs: 0, threads: 1, start: 42},
			ok:   true,
		},
		{name: "too short", raw: "5 (q) R 1 2 3", ok: false},
		{name: "no paren", raw: "5 q R 1 2 3", ok: false},
		{name: "empty", raw: "", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseStat(tc.raw)
			if ok != tc.ok {
				t.Fatalf("parseStat ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if got != tc.want {
				t.Errorf("parseStat = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestProcIntField(t *testing.T) {
	status := "Name:\tbash\nVmRSS:\t   12345 kB\nVmHWM:\t   20000 kB\nThreads:\t4\n"
	io := "rchar: 100\nwchar: 200\nsyscr: 7\nsyscw: 9\nread_bytes: 4096\nwrite_bytes: 8192\n"
	cases := []struct {
		name   string
		s      string
		prefix string
		want   int64
	}{
		{"vmrss kB", status, "VmRSS:", 12345},
		{"vmhwm kB", status, "VmHWM:", 20000},
		{"threads", status, "Threads:", 4},
		{"read_bytes", io, "read_bytes:", 4096},
		{"write_bytes", io, "write_bytes:", 8192},
		{"syscr", io, "syscr:", 7},
		{"syscw", io, "syscw:", 9},
		{"absent", status, "Nope:", 0},
		{"non-numeric", "Bad:\tnotanumber\n", "Bad:", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := procIntField(tc.s, tc.prefix); got != tc.want {
				t.Errorf("procIntField(%q) = %d, want %d", tc.prefix, got, tc.want)
			}
		})
	}
}

// TestReadProcSelfMetrics exercises the real /proc read path against the test
// process itself. A short CPU burn first guarantees non-zero utime/stime so the
// assertion does not race a too-fast run. Disk byte counters are environment-
// dependent (page cache, WSL/container quirks), so they are logged, not
// asserted — the reliable signals are CPU, working set, peak, and threads.
func TestReadProcSelfMetrics(t *testing.T) {
	deadline := time.Now().Add(50 * time.Millisecond)
	acc := 0
	for time.Now().Before(deadline) {
		acc++
	}
	_ = acc

	p, ok := readProc(os.Getpid())
	if !ok {
		t.Fatal("readProc(self) returned ok=false")
	}
	if !p.HasMetrics {
		t.Error("HasMetrics = false for self, want true")
	}
	if p.WorkingSetBytes <= 0 {
		t.Errorf("WorkingSetBytes = %d, want > 0", p.WorkingSetBytes)
	}
	if p.MaxRSSBytes < p.WorkingSetBytes {
		t.Errorf("MaxRSSBytes (%d) < WorkingSetBytes (%d): peak must be >= current",
			p.MaxRSSBytes, p.WorkingSetBytes)
	}
	if p.ThreadCount < 1 {
		t.Errorf("ThreadCount = %d, want >= 1", p.ThreadCount)
	}
	if p.CPUUserMs+p.CPUSystemMs <= 0 {
		t.Errorf("CPU user+system = %dms, want > 0 after the burn", p.CPUUserMs+p.CPUSystemMs)
	}
	t.Logf("self metrics: cpu(u=%d,s=%d)ms ws=%dB peak=%dB threads=%d disk(r=%dB w=%dB ro=%d wo=%d)",
		p.CPUUserMs, p.CPUSystemMs, p.WorkingSetBytes, p.MaxRSSBytes, p.ThreadCount,
		p.ReadBytes, p.WriteBytes, p.ReadOps, p.WriteOps)
}
