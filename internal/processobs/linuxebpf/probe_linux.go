//go:build linux

package linuxebpf

import (
	"log/slog"

	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
)

// Available is the P0 capability gate: it reports whether THIS process can
// actually load an eBPF program AND attach it to a raw tracepoint — the two
// privilege checks (CAP_BPF for load, CAP_PERFMON/attach for the tracepoint)
// that decide whether the eBPF backend can run. The daemon usually runs
// unprivileged, so this almost always returns false there and the selector
// falls back to the poll backend (fail-open). The probe is definitive rather
// than heuristic: it builds the real ring-buffer map + program and attaches the
// real tracepoint for a microsecond, then tears it all down — so a true here
// means Start will succeed, and we never advertise a capability we don't have.
func Available(logger *slog.Logger) bool {
	if logger == nil {
		logger = slog.Default()
	}
	// Best-effort: unnecessary on memcg-accounted kernels (5.11+), harmless
	// elsewhere. A failure here is not itself disqualifying.
	_ = rlimit.RemoveMemlock()

	off, err := startBoottimeOffset()
	if err != nil {
		logger.Debug("processobs/linuxebpf: cannot resolve task_struct.start_boottime from BTF — eBPF capture disabled, using poll", "err", err)
		return false
	}
	nsDev, nsIno, err := pidnsDevIno()
	if err != nil {
		logger.Debug("processobs/linuxebpf: cannot resolve pid namespace — eBPF capture disabled, using poll", "err", err)
		return false
	}

	rb, err := newRingbufMap()
	if err != nil {
		logger.Debug("processobs/linuxebpf: ring-buffer map unavailable — eBPF capture disabled, using poll", "err", err)
		return false
	}
	defer rb.Close()

	prog, err := buildProgram("sbo_probe", rb, evExec, off, nsDev, nsIno)
	if err != nil {
		logger.Debug("processobs/linuxebpf: program load failed (likely no CAP_BPF) — eBPF capture disabled, using poll", "err", err)
		return false
	}
	defer prog.Close()

	lk, err := link.AttachRawTracepoint(link.RawTracepointOptions{
		Name:    "sched_process_exec",
		Program: prog,
	})
	if err != nil {
		logger.Debug("processobs/linuxebpf: tracepoint attach failed (likely no CAP_PERFMON) — eBPF capture disabled, using poll", "err", err)
		return false
	}
	_ = lk.Close()
	return true
}
