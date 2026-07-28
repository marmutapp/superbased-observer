//go:build linux

package linuxebpf

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// TestNetworkBytesUnmeasuredWhenNotAttached pins the degradation contract: a
// backend whose probes never attached reports ok=false, so every sample it
// feeds is recorded as UNMEASURED. It must NEVER report (0,0,true) — that
// would draw a flat zero line for traffic nobody measured.
func TestNetworkBytesUnmeasuredWhenNotAttached(t *testing.T) {
	t.Parallel()
	b := &backend{}
	if in, out, ok := b.NetworkBytes(1234); ok || in != 0 || out != 0 {
		t.Fatalf("NetworkBytes with no probes = (%d,%d,%v), want (0,0,false)", in, out, ok)
	}
}

// TestStartNetworkDisabledReportsOff pins the config-gated path: with
// NetworkAccounting off, no probe is attempted and the status reads "off"
// (distinct from "unavailable", which means we tried and could not).
func TestStartNetworkDisabledReportsOff(t *testing.T) {
	t.Parallel()
	status := &processobs.NetworkAccounting{}
	b := &backend{opts: Options{NetworkStatus: status}}
	b.opts.withDefaults()
	b.startNetwork(0, 0)
	if mode, _ := status.Status(); mode != processobs.NetworkAccountingOff {
		t.Fatalf("status = %q, want %q", mode, processobs.NetworkAccountingOff)
	}
	if b.net != nil {
		t.Error("no accounting objects should have been created")
	}
}

// TestStartNetworkUnprivilegedDegrades pins the fail-open path when the probes
// are requested but cannot attach (the normal case for the unprivileged
// daemon): the status becomes "unavailable" WITH a reason, and lifecycle
// capture is untouched. When the test host CAN load BPF the probes attach and
// the status is "tcp" — both outcomes are correct, neither is an error.
func TestStartNetworkUnprivilegedDegrades(t *testing.T) {
	status := &processobs.NetworkAccounting{}
	b := &backend{opts: Options{NetworkAccounting: true, NetworkStatus: status}}
	b.opts.withDefaults()
	nsDev, nsIno, err := pidnsDevIno()
	if err != nil {
		t.Skipf("cannot resolve pid namespace: %v", err)
	}
	b.startNetwork(nsDev, nsIno)
	defer func() { b.netMu.Lock(); b.net.close(); b.netMu.Unlock() }()

	mode, reason := status.Status()
	t.Logf("network accounting on this host: mode=%q reason=%q", mode, reason)
	switch mode {
	case processobs.NetworkAccountingTCP:
		if b.net == nil {
			t.Fatal("status says live but no objects were kept")
		}
	case processobs.NetworkAccountingUnavailable:
		if reason == "" {
			t.Error("degraded status must carry a reason so the UI can explain it")
		}
		if b.net != nil {
			t.Error("degraded status must leave no half-attached objects")
		}
		if _, _, ok := b.NetworkBytes(1); ok {
			t.Error("degraded backend must report bytes as unmeasured")
		}
	default:
		t.Fatalf("unexpected status %q", mode)
	}
}

// TestNetFinalCacheBounded pins the recently-exited counter cache: it survives
// long enough for the next poll to read a process's FINAL bytes, evicts
// oldest-first, and never grows past its cap.
func TestNetFinalCacheBounded(t *testing.T) {
	t.Parallel()
	b := &backend{netFinal: make(map[int]netTotals)}
	for pid := 1; pid <= netFinalCacheMax+50; pid++ {
		b.rememberFinalLocked(pid, netTotals{rx: int64(pid), tx: int64(pid) * 2})
	}
	if len(b.netFinal) != netFinalCacheMax || len(b.netOrder) != netFinalCacheMax {
		t.Fatalf("cache not bounded: map=%d order=%d cap=%d", len(b.netFinal), len(b.netOrder), netFinalCacheMax)
	}
	if _, ok := b.netFinal[1]; ok {
		t.Error("oldest entry should have been evicted first")
	}
	newest := netFinalCacheMax + 50
	if got, ok := b.netFinal[newest]; !ok || got.rx != int64(newest) {
		t.Errorf("newest entry missing or wrong: %+v ok=%v", got, ok)
	}
	// Re-remembering an existing pid must not double-count the order slice.
	before := len(b.netOrder)
	b.rememberFinalLocked(newest, netTotals{rx: 1, tx: 1})
	if len(b.netOrder) != before {
		t.Errorf("duplicate insert grew the order slice: %d -> %d", before, len(b.netOrder))
	}
}

// TestNetProbeTableIsCoherent pins the probe table's invariants — the offsets
// are what make the programs read the right kernel value, and a typo here is a
// silently wrong counter rather than a load failure.
func TestNetProbeTableIsCoherent(t *testing.T) {
	t.Parallel()
	if len(netProbes) == 0 {
		t.Fatal("probe table is empty")
	}
	seenVal := map[int16]bool{}
	for _, p := range netProbes {
		if p.name == "" || len(p.name) > 15 {
			t.Errorf("%q: program name must be 1..15 chars (kernel limit)", p.name)
		}
		if p.symbol == "" {
			t.Errorf("%q: missing attach symbol", p.name)
		}
		if p.ctxOff%8 != 0 || p.ctxOff < 0 {
			t.Errorf("%q: ctxOff %d is not a valid trampoline slot", p.name, p.ctxOff)
		}
		if p.valOff != netValRxOff && p.valOff != netValTxOff {
			t.Errorf("%q: valOff %d is outside the 16-byte value", p.name, p.valOff)
		}
		seenVal[p.valOff] = true
	}
	if !seenVal[netValRxOff] || !seenVal[netValTxOff] {
		t.Error("both directions must be covered, or the series is half a lie")
	}
}

// TestNetProgramInstructionsAssemble is the strongest check available WITHOUT
// CAP_BPF: every probe's instruction stream must resolve its labels ("out",
// "init") and marshal to valid bytecode. It cannot exercise the kernel
// verifier — that needs privileges this test process does not have — so a pass
// here means "well-formed", NOT "verified by the kernel".
func TestNetProgramInstructionsAssemble(t *testing.T) {
	t.Parallel()
	for _, p := range netProbes {
		insns := netProgramInstructions(p, 3 /* plausible fd */, 1, 2)
		var buf bytes.Buffer
		if err := insns.Marshal(&buf, binary.LittleEndian); err != nil {
			t.Fatalf("%s: marshal: %v", p.name, err)
		}
		if buf.Len()%8 != 0 || buf.Len() == 0 {
			t.Fatalf("%s: bytecode length %d is not a positive multiple of 8", p.name, buf.Len())
		}
	}
}
