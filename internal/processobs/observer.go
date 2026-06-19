package processobs

import (
	"context"
	"maps"
	"sync"
	"time"
)

// Backend is the OS event source. Implementations (linuxebpf / etw /
// endpointsec / poll) live in their own packages and own all privileged
// code; this package only consumes the channel.
type Backend interface {
	// Name identifies the backend for health/doctor (e.g. "linux_ebpf").
	Name() string
	// Start begins capture and returns the event channel, which the backend
	// closes when capture ends. An error means the backend is unavailable
	// (missing privileges, unsupported kernel, …) — the Observer reports
	// degraded health and the daemon continues (fail-open, spec §15).
	Start(ctx context.Context) (<-chan RawEvent, error)
	// Close releases backend resources. Safe to call after a Start error.
	Close() error
}

// UnattributedCapturer is an OPTIONAL Backend capability. A backend that
// implements it and returns true produces events that cannot be attributed at
// capture time — the cross-OS bridge (§5.5) is the case: the pidbridge holds
// WSL-side pids, so a Windows bridge event never gets a direct hit and arrives
// AttrNone. The daemon must then capture unattributed runs (so they persist)
// and rely on the deferred CorrelateCrossOS pass to join them to a session.
// Branching on this capability — not on the backend's name — keeps the wiring
// rule-compliant (CLAUDE.md: branch on capabilities, never source identity).
type UnattributedCapturer interface {
	RequiresUnattributedCapture() bool
}

// Enricher fills OS-specific envelope fields on a RawEvent in place (e.g.
// the Linux backend reads /proc/<pid>/...) and returns the process
// environment for posture capture. Optional: a nil Enricher means events
// arrive already enriched and no env is captured.
type Enricher interface {
	Enrich(ev *RawEvent) (env map[string]string, err error)
}

// DeepEnricher performs a targeted, per-NEW-process enrichment AFTER
// attribution has decided a run will be persisted. It reads the sensitive or
// expensive data the cheap whole-table enrichment deliberately skips — the
// process environment for posture (spec §8.1) and the executable content for
// hashing (§8 Executable / §19 Q6) — exactly ONCE per persisted run, never
// across the whole process table every poll. The OS-specific implementation
// (Linux reads /proc/<pid>/environ + hashes /proc/<pid>/exe) lives outside
// this pure package and is injected; a nil DeepEnricher means no deep
// enrichment. It mutates the run in place and MUST be best-effort and
// fail-open: a process that already exited or a file it cannot read simply
// leaves the field empty. Distinct from Enricher, which runs per-event and
// pre-attribution over every observed process.
type DeepEnricher interface {
	DeepEnrich(run *ProcessRun)
}

// Sink persists finished runs. The store implements it and translates
// ProcessRun into its own SQL row type at the boundary; process_key is
// UNIQUE, so PersistRuns upserts (insert at exec, update at exit).
type Sink interface {
	PersistRuns(ctx context.Context, runs []ProcessRun) (int, error)
}

// Options configures an Observer.
type Options struct {
	Backend             Backend
	Enricher            Enricher     // optional
	DeepEnricher        DeepEnricher // optional, post-attribution per-new-run
	Attributor          *Attributor  // required
	Sink                Sink         // required
	CaptureUnattributed bool         // §9.2.7 / D5
	BatchSize           int          // store batch (spec §15: 100–500); default 250
	MaxTracked          int          // live-tree cap for never-exiting procs; 0 = unbounded
	FlushInterval       time.Duration
	Now                 func() time.Time
}

// Observer is the orchestrator: it drains the backend, enriches, attributes,
// applies the capture policy, batches to the sink, and tracks health. The
// run loop is single-goroutine, so the Attributor needs no locking.
type Observer struct {
	backend             Backend
	enricher            Enricher
	deepEnricher        DeepEnricher
	attr                *Attributor
	sink                Sink
	captureUnattributed bool
	batchSize           int
	maxTracked          int
	flushInterval       time.Duration
	now                 func() time.Time
	health              *Health
}

// NewObserver builds an Observer from Options, applying defaults.
func NewObserver(opts Options) *Observer {
	if opts.BatchSize <= 0 {
		opts.BatchSize = 250
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = 2 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	name := ""
	if opts.Backend != nil {
		name = opts.Backend.Name()
	}
	return &Observer{
		backend:             opts.Backend,
		enricher:            opts.Enricher,
		deepEnricher:        opts.DeepEnricher,
		attr:                opts.Attributor,
		sink:                opts.Sink,
		captureUnattributed: opts.CaptureUnattributed,
		batchSize:           opts.BatchSize,
		maxTracked:          opts.MaxTracked,
		flushInterval:       opts.FlushInterval,
		now:                 opts.Now,
		health:              newHealth(name),
	}
}

// Health exposes the live counters for metrics/doctor.
func (o *Observer) Health() *Health { return o.health }

// Run drains the backend until the channel closes or ctx is cancelled,
// flushing batches as it goes. A backend Start error is returned after
// recording degraded health; callers log it and keep the daemon running.
func (o *Observer) Run(ctx context.Context) error {
	ch, err := o.backend.Start(ctx)
	if err != nil {
		o.health.setBackendUp(false)
		o.health.setError(err.Error())
		return err
	}
	o.health.setBackendUp(true)
	defer func() {
		o.health.setBackendUp(false)
		_ = o.backend.Close()
	}()

	ticker := time.NewTicker(o.flushInterval)
	defer ticker.Stop()

	batch := make([]ProcessRun, 0, o.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		// Best-effort: a sink error is recorded as a drop, never fatal
		// (fail-open). The backend keeps draining.
		if _, err := o.sink.PersistRuns(ctx, batch); err != nil {
			o.health.addDropped(DropReason("sink_error"), int64(len(batch)))
		}
		batch = batch[:0]
		o.attr.EvictOldestLive(o.maxTracked)
	}

	for {
		select {
		case <-ctx.Done():
			flush()
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				flush()
				return nil
			}
			o.health.setQueueDepth(int64(len(ch)))
			if run, change := o.handle(&ev); change != ChangeNone && run != nil {
				batch = append(batch, *run)
				if len(batch) >= o.batchSize {
					flush()
				}
			}
		case <-ticker.C:
			flush()
		}
	}
}

// handle folds one event through enrich → attribute → capture policy and
// returns the run to persist (or ChangeNone to skip). Exposed-ish via Run;
// kept separate so tests can drive it deterministically.
func (o *Observer) handle(ev *RawEvent) (*ProcessRun, Change) {
	o.health.incEvent(ev.Type)

	// fork/exec need a start time for a stable key (§9.3); exit can fall
	// back to the live-pid map, so it is not dropped here.
	if (ev.Type == EventFork || ev.Type == EventExec) && !ev.HasStartTime {
		o.health.addDropped(DropNoStartTime, 1)
		return nil, ChangeNone
	}

	var env map[string]string
	if o.enricher != nil {
		e, err := o.enricher.Enrich(ev)
		if err != nil {
			o.health.addDropped(DropEnrichFailed, 1)
			return nil, ChangeNone
		}
		env = e
	}

	run, change := o.attr.Observe(*ev, env)
	if change == ChangeNone || run == nil {
		return nil, ChangeNone
	}
	if !run.Attributed() && !o.captureUnattributed {
		o.health.addDropped(DropUnattributed, 1)
		return nil, ChangeNone
	}
	if run.Attributed() {
		o.health.incAttributed(run.Attribution.Tool)
	} else {
		o.health.incUnattributed()
	}
	// Deep enrichment runs ONCE per persisted run, at the create (exec) point
	// only — the process is freshly observed and still alive, so /proc reads
	// succeed; the exit update reuses the same tracked pointer and inherits
	// these fields. Fail-open: a nil enricher or an unreadable process is a
	// no-op (spec §8.1 env posture, §8 executable hashing, §19 Q6).
	if change == ChangeCreated && o.deepEnricher != nil {
		o.deepEnricher.DeepEnrich(run)
	}
	return run, change
}

// Health holds the live process-observability counters that back the spec
// §15 metrics and the doctor health check. Guarded by a mutex: the Run loop
// writes from one goroutine, metrics/doctor read from another.
type Health struct {
	mu               sync.Mutex
	backendName      string
	backendUp        bool
	lastError        string
	queueDepth       int64
	unattributed     int64
	eventsTotal      map[EventType]int64
	dropped          map[DropReason]int64
	attributedByTool map[string]int64
}

func newHealth(backendName string) *Health {
	return &Health{
		backendName:      backendName,
		eventsTotal:      make(map[EventType]int64),
		dropped:          make(map[DropReason]int64),
		attributedByTool: make(map[string]int64),
	}
}

func (h *Health) setBackendUp(up bool)  { h.mu.Lock(); h.backendUp = up; h.mu.Unlock() }
func (h *Health) setError(e string)     { h.mu.Lock(); h.lastError = e; h.mu.Unlock() }
func (h *Health) setQueueDepth(d int64) { h.mu.Lock(); h.queueDepth = d; h.mu.Unlock() }
func (h *Health) incEvent(t EventType)  { h.mu.Lock(); h.eventsTotal[t]++; h.mu.Unlock() }
func (h *Health) incUnattributed()      { h.mu.Lock(); h.unattributed++; h.mu.Unlock() }
func (h *Health) incAttributed(tool string) {
	h.mu.Lock()
	if tool == "" {
		tool = "unknown"
	}
	h.attributedByTool[tool]++
	h.mu.Unlock()
}

func (h *Health) addDropped(reason DropReason, n int64) {
	h.mu.Lock()
	h.dropped[reason] += n
	h.mu.Unlock()
}

// HealthSnapshot is an immutable copy of the counters for metrics/doctor.
type HealthSnapshot struct {
	BackendName      string
	BackendUp        bool
	LastError        string
	QueueDepth       int64
	Unattributed     int64
	EventsTotal      map[EventType]int64
	Dropped          map[DropReason]int64
	AttributedByTool map[string]int64
}

// Snapshot returns a deep copy of the current counters.
func (h *Health) Snapshot() HealthSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := HealthSnapshot{
		BackendName:      h.backendName,
		BackendUp:        h.backendUp,
		LastError:        h.lastError,
		QueueDepth:       h.queueDepth,
		Unattributed:     h.unattributed,
		EventsTotal:      make(map[EventType]int64, len(h.eventsTotal)),
		Dropped:          make(map[DropReason]int64, len(h.dropped)),
		AttributedByTool: make(map[string]int64, len(h.attributedByTool)),
	}
	maps.Copy(s.EventsTotal, h.eventsTotal)
	maps.Copy(s.Dropped, h.dropped)
	maps.Copy(s.AttributedByTool, h.attributedByTool)
	return s
}
