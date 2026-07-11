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
}

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
		for _, re := range tr.handle(ev) {
			select {
			case b.out <- re:
			case <-ctx.Done():
				return
			}
		}
	}
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
