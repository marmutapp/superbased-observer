//go:build linux

package linuxebpf

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"

	"github.com/marmutapp/superbased-observer/internal/processobs"
	"github.com/marmutapp/superbased-observer/internal/processobs/poll"
)

// backend is the Linux eBPF Backend. It loads two programs (one per
// tracepoint) writing to a shared ring buffer, then drains the ring buffer on a
// single goroutine through the pure translator. All privileged setup happens in
// Start and fails open: any error returns to the Observer as degraded health
// (spec §15) and the selector has already fallen back to poll when Available
// said no.
type backend struct {
	opts Options
	out  chan processobs.RawEvent

	mu       sync.Mutex
	closed   bool
	rb       *ebpf.Map
	execProg *ebpf.Program
	exitProg *ebpf.Program
	execLink link.Link
	exitLink link.Link
	reader   *ringbuf.Reader

	// netMu guards the network-accounting state, which the ring-buffer loop
	// writes (exec/exit maintenance) and the metric sampler reads
	// (NetworkBytes) from another goroutine.
	netMu    sync.Mutex
	net      *netAccounting
	netFinal map[int]netTotals // last counters of recently-exited pids
	netOrder []int             // insertion order for the bounded netFinal cache
}

// netTotals is one pid's cumulative (rx, tx) byte pair.
type netTotals struct{ rx, tx int64 }

// netFinalCacheMax bounds the recently-exited counter cache. A process's final
// counters must outlive the kernel-map entry, because the poll backend only
// notices the exit up to one poll interval later — without this the LAST point
// of a chart would drop to zero and look like a counter reset.
const netFinalCacheMax = 1024

// New builds the eBPF Backend. The returned value always satisfies
// processobs.Backend; whether it can actually capture is decided in Start.
func New(opts Options) processobs.Backend { return &backend{opts: opts} }

// Name implements processobs.Backend.
func (b *backend) Name() string { return "linux_ebpf" }

// Start loads the programs, attaches the tracepoints, and streams events until
// ctx is cancelled or Close is called. Every failure tears down what it built
// and returns an error (fail-open). On success the returned channel closes once
// the ring-buffer loop drains.
func (b *backend) Start(ctx context.Context) (<-chan processobs.RawEvent, error) {
	b.opts.withDefaults()
	if b.opts.BootID == "" {
		b.opts.BootID = poll.PlatformBootID()
	}
	enrich := b.opts.enrich
	if enrich == nil {
		enrich = poll.ReadProcInfo
	}

	_ = rlimit.RemoveMemlock() // best-effort, see probe_linux.go

	startOff, err := startBoottimeOffset()
	if err != nil {
		return nil, fmt.Errorf("linuxebpf: resolve task_struct.start_boottime from BTF: %w", err)
	}
	nsDev, nsIno, err := pidnsDevIno()
	if err != nil {
		return nil, fmt.Errorf("linuxebpf: resolve pid namespace: %w", err)
	}

	rb, err := newRingbufMap()
	if err != nil {
		return nil, fmt.Errorf("linuxebpf: ring-buffer map: %w", err)
	}
	execProg, err := buildProgram("sbo_exec", rb, evExec, startOff, nsDev, nsIno)
	if err != nil {
		_ = rb.Close()
		return nil, fmt.Errorf("linuxebpf: load exec program (missing CAP_BPF?): %w", err)
	}
	exitProg, err := buildProgram("sbo_exit", rb, evExit, startOff, nsDev, nsIno)
	if err != nil {
		_ = execProg.Close()
		_ = rb.Close()
		return nil, fmt.Errorf("linuxebpf: load exit program: %w", err)
	}
	execLink, err := link.AttachRawTracepoint(link.RawTracepointOptions{Name: "sched_process_exec", Program: execProg})
	if err != nil {
		_ = exitProg.Close()
		_ = execProg.Close()
		_ = rb.Close()
		return nil, fmt.Errorf("linuxebpf: attach sched_process_exec (missing CAP_PERFMON?): %w", err)
	}
	exitLink, err := link.AttachRawTracepoint(link.RawTracepointOptions{Name: "sched_process_exit", Program: exitProg})
	if err != nil {
		_ = execLink.Close()
		_ = exitProg.Close()
		_ = execProg.Close()
		_ = rb.Close()
		return nil, fmt.Errorf("linuxebpf: attach sched_process_exit: %w", err)
	}
	reader, err := ringbuf.NewReader(rb)
	if err != nil {
		_ = exitLink.Close()
		_ = execLink.Close()
		_ = exitProg.Close()
		_ = execProg.Close()
		_ = rb.Close()
		return nil, fmt.Errorf("linuxebpf: ring-buffer reader: %w", err)
	}

	b.rb, b.execProg, b.exitProg = rb, execProg, exitProg
	b.execLink, b.exitLink, b.reader = execLink, exitLink, reader
	b.out = make(chan processobs.RawEvent, 1024)

	// Per-process network byte accounting is a SEPARATE, optional attach: if
	// it fails we keep lifecycle capture exactly as before and report the
	// degradation honestly, rather than failing Start or logging on a loop.
	b.startNetwork(nsDev, nsIno)

	tr := newTranslator(b.opts.BootID, b.opts.Now, enrich)
	go b.loop(ctx, tr)
	// Closing the reader is what unblocks reader.Read (ErrClosed); do it when
	// the context ends so the loop can drain and close the out channel.
	go func() {
		<-ctx.Done()
		_ = b.Close()
	}()
	return b.out, nil
}

// loop drains the ring buffer, decoding each record and folding it through the
// translator. It closes the out channel on exit (reader closed or ctx done).
func (b *backend) loop(ctx context.Context, tr *translator) {
	defer close(b.out)
	for {
		rec, err := b.reader.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				return
			}
			continue // transient read error; keep draining
		}
		ev, ok := decodeEvent(rec.RawSample)
		if !ok {
			continue
		}
		b.maintainNetwork(ev)
		for _, re := range tr.handle(ev) {
			select {
			case b.out <- re:
			case <-ctx.Done():
				return
			}
		}
	}
}

// startNetwork brings up per-process network byte accounting when it was
// requested. Failure is NOT fatal: the status handle records "unavailable"
// plus the reason (so the UI can distinguish unmeasured from zero), one DEBUG
// line is emitted, and lifecycle capture continues untouched.
func (b *backend) startNetwork(nsDev, nsIno uint64) {
	if !b.opts.NetworkAccounting {
		b.opts.NetworkStatus.Set(processobs.NetworkAccountingOff, "not enabled ([observer.process.network].process_bytes)")
		return
	}
	na, err := startNetAccounting(nsDev, nsIno)
	if err != nil {
		b.opts.NetworkStatus.Set(processobs.NetworkAccountingUnavailable, err.Error())
		b.opts.Logger.Debug("processobs/linuxebpf: per-process network accounting unavailable — bytes reported as UNMEASURED, lifecycle capture unaffected", "err", err)
		return
	}
	b.netMu.Lock()
	b.net = na
	b.netFinal = make(map[int]netTotals, netFinalCacheMax)
	b.netMu.Unlock()
	b.opts.NetworkStatus.Set(processobs.NetworkAccountingTCP, "")
	b.opts.Logger.Info("processobs/linuxebpf: per-process network accounting active (TCP payload bytes only; UDP/QUIC and Windows-side processes are not counted)")
}

// maintainNetwork keeps the per-pid counter map honest across the process
// lifecycle: an EXEC clears any leftover totals so a REUSED pid never inherits
// the previous occupant's bytes, and an EXIT caches the final totals (so the
// next metric sample still sees them) before dropping the kernel entry.
func (b *backend) maintainNetwork(ev kernelEvent) {
	b.netMu.Lock()
	defer b.netMu.Unlock()
	if b.net == nil {
		return
	}
	switch ev.Type {
	case evExec:
		delete(b.netFinal, ev.PID)
		b.net.forget(ev.PID)
	case evExit:
		if rx, tx, ok := b.net.lookup(ev.PID); ok {
			b.rememberFinalLocked(ev.PID, netTotals{rx: rx, tx: tx})
		}
		b.net.forget(ev.PID)
	}
}

// rememberFinalLocked inserts into the bounded recently-exited cache, evicting
// oldest-first. Caller holds netMu.
func (b *backend) rememberFinalLocked(pid int, t netTotals) {
	if _, dup := b.netFinal[pid]; !dup {
		b.netOrder = append(b.netOrder, pid)
	}
	b.netFinal[pid] = t
	for len(b.netOrder) > netFinalCacheMax {
		delete(b.netFinal, b.netOrder[0])
		b.netOrder = b.netOrder[1:]
	}
}

// NetworkBytes implements processobs.NetworkSampler: cumulative TCP payload
// bytes received/sent by a pid since the probes attached.
//
// ok=false means accounting is NOT LIVE — the caller must record the sample as
// unmeasured rather than zero. ok=true with (0,0) means the process is measured
// and simply has not moved any TCP bytes, which is a real observation.
func (b *backend) NetworkBytes(pid int) (in, out int64, ok bool) {
	b.netMu.Lock()
	defer b.netMu.Unlock()
	if b.net == nil {
		return 0, 0, false
	}
	if rx, tx, hit := b.net.lookup(pid); hit {
		return rx, tx, true
	}
	if t, hit := b.netFinal[pid]; hit {
		return t.rx, t.tx, true
	}
	return 0, 0, true // measured, no TCP bytes yet
}

// Close detaches the tracepoints and releases the BPF objects. Idempotent and
// safe to call after a Start error (all handles nil-checked) and from both the
// ctx watcher and the Observer's defer.
func (b *backend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	// Tear down in reverse order of setup: reader first (unblocks Read with
	// ErrClosed), then detach the tracepoints, then the programs, then the map.
	var errs []error
	b.netMu.Lock()
	if b.net != nil {
		b.net.close()
		b.net = nil
		b.opts.NetworkStatus.Set(processobs.NetworkAccountingOff, "capture stopped")
	}
	b.netMu.Unlock()
	if b.reader != nil {
		errs = append(errs, b.reader.Close())
	}
	if b.execLink != nil {
		errs = append(errs, b.execLink.Close())
	}
	if b.exitLink != nil {
		errs = append(errs, b.exitLink.Close())
	}
	if b.execProg != nil {
		errs = append(errs, b.execProg.Close())
	}
	if b.exitProg != nil {
		errs = append(errs, b.exitProg.Close())
	}
	if b.rb != nil {
		errs = append(errs, b.rb.Close())
	}
	return errors.Join(errs...)
}
