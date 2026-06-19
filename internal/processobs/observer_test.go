package processobs

import (
	"context"
	"errors"
	"testing"
	"time"
)

func ts(n int) time.Time { return time.Unix(1_700_000_000+int64(n), 0).UTC() }

// attributedSequence: root claude (pid 100, bridged) forks+execs bash
// (200), which exits, then claude exits.
func attributedSequence() []RawEvent {
	return []RawEvent{
		execEv("b", 100, 1, 1000, "/usr/bin/claude", []string{"claude"}, ts(1)),
		forkEv("b", 200, 100, 2000, ts(2)),
		execEv("b", 200, 100, 2000, "/bin/bash", []string{"bash", "-c", "npm test"}, ts(3)),
		exitEv("b", 200, 2000, 0, ts(4)),
		exitEv("b", 100, 1000, 0, ts(5)),
	}
}

func runObserver(t *testing.T, opts Options) (*SliceSink, *Observer) {
	t.Helper()
	sink := opts.Sink.(*SliceSink)
	o := NewObserver(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := o.Run(ctx); err != nil && opts.Backend.(*FakeBackend).StartErr == nil {
		t.Fatalf("Run: %v", err)
	}
	return sink, o
}

func TestObserverPipelineEndToEnd(t *testing.T) {
	t.Parallel()
	be := &FakeBackend{BackendName: "fake", Events: attributedSequence()}
	sink := &SliceSink{}
	attr := NewAttributor(bridgeSeed(100, "sess-1", "claude-code", 7), &FieldScrubber{ArgvMode: "preview", MaxPreviewBytes: 512}, nil)
	_, o := runObserver(t, Options{Backend: be, Attributor: attr, Sink: sink, BatchSize: 100, FlushInterval: time.Hour})

	// Persisted: exec(100) create, exec(200) create, exit(200) update,
	// exit(100) update = 4 run rows. Fork(200) does not persist.
	if len(sink.Runs) != 4 {
		t.Fatalf("persisted %d runs, want 4: %+v", len(sink.Runs), sink.Runs)
	}
	// Every persisted run is attributed to the session.
	for _, r := range sink.Runs {
		if r.Attribution.SessionID != "sess-1" {
			t.Errorf("run %s not attributed: %+v", r.ExeBasename, r.Attribution)
		}
	}
	// The last two are the exit updates (Exited=true).
	if !sink.Runs[2].Exited || !sink.Runs[3].Exited {
		t.Error("expected the two exit updates to carry Exited=true")
	}

	h := o.Health().Snapshot()
	if !h.BackendUp && h.BackendName != "fake" {
		// BackendUp is flipped false again by Run's deferred cleanup; that's
		// expected. We only assert the name here.
		t.Errorf("backend name = %q", h.BackendName)
	}
	if h.EventsTotal[EventExec] != 2 || h.EventsTotal[EventFork] != 1 || h.EventsTotal[EventExit] != 2 {
		t.Errorf("event counts = %+v", h.EventsTotal)
	}
	if h.AttributedByTool["claude-code"] != 4 {
		t.Errorf("attributed-by-tool = %+v, want claude-code:4", h.AttributedByTool)
	}
	if !be.Closed() {
		t.Error("backend Close not called")
	}
}

func TestObserverDropsUnattributedByDefault(t *testing.T) {
	t.Parallel()
	// No seed → nothing is attributed.
	events := []RawEvent{
		execEv("b", 500, 1, 1000, "/bin/bash", []string{"bash"}, ts(1)),
		exitEv("b", 500, 1000, 0, ts(2)),
	}

	// Default: unattributed dropped.
	sink := &SliceSink{}
	attr := NewAttributor(nil, nil, nil)
	_, o := runObserver(t, Options{Backend: &FakeBackend{Events: events}, Attributor: attr, Sink: sink, FlushInterval: time.Hour})
	if len(sink.Runs) != 0 {
		t.Errorf("unattributed runs leaked: %d", len(sink.Runs))
	}
	if o.Health().Snapshot().Dropped[DropUnattributed] == 0 {
		t.Error("expected a dropped-unattributed counter")
	}

	// capture_unattributed = true: now they persist.
	sink2 := &SliceSink{}
	attr2 := NewAttributor(nil, nil, nil)
	_, _ = runObserver(t, Options{Backend: &FakeBackend{Events: events}, Attributor: attr2, Sink: sink2, CaptureUnattributed: true, FlushInterval: time.Hour})
	if len(sink2.Runs) == 0 {
		t.Error("capture_unattributed=true should persist unattributed runs")
	}
}

func TestObserverBackendStartErrorFailsOpen(t *testing.T) {
	t.Parallel()
	be := &FakeBackend{StartErr: errors.New("missing CAP_BPF")}
	sink := &SliceSink{}
	o := NewObserver(Options{Backend: be, Attributor: NewAttributor(nil, nil, nil), Sink: sink})
	err := o.Run(context.Background())
	if err == nil {
		t.Fatal("expected Start error to propagate so the caller can log degraded health")
	}
	h := o.Health().Snapshot()
	if h.BackendUp {
		t.Error("backend must report down after a Start error")
	}
	if h.LastError == "" {
		t.Error("Start error should be recorded for doctor")
	}
}

func TestObserverDropsNoStartTime(t *testing.T) {
	t.Parallel()
	events := []RawEvent{
		{Type: EventExec, BootID: "b", PID: 100, PPID: 1, HasStartTime: false, ExePath: "/bin/x", Timestamp: ts(1)},
	}
	sink := &SliceSink{}
	o := NewObserver(Options{Backend: &FakeBackend{Events: events}, Attributor: NewAttributor(bridgeSeed(100, "s", "claude-code", 1), nil, nil), Sink: sink, FlushInterval: time.Hour})
	_ = o.Run(context.Background())
	if len(sink.Runs) != 0 {
		t.Errorf("unkeyable exec persisted: %d", len(sink.Runs))
	}
	if o.Health().Snapshot().Dropped[DropNoStartTime] == 0 {
		t.Error("expected a no_start_time drop counter")
	}
}

// fakeDeepEnricher records the pids it was asked to enrich and stamps a
// sentinel onto each run, so a test can assert WHICH runs reached the
// post-attribution seam and that the stamp survives to persistence.
type fakeDeepEnricher struct {
	pids  []int
	stamp func(*ProcessRun)
}

func (f *fakeDeepEnricher) DeepEnrich(run *ProcessRun) {
	f.pids = append(f.pids, run.PID)
	if f.stamp != nil {
		f.stamp(run)
	}
}

func TestObserverDeepEnrichRunsOncePerPersistedCreate(t *testing.T) {
	t.Parallel()
	de := &fakeDeepEnricher{stamp: func(r *ProcessRun) {
		r.ExeHash = "sha256:deep"
		r.EnvPosture = map[string]string{"DEEP": "1"}
	}}
	be := &FakeBackend{Events: attributedSequence()}
	sink := &SliceSink{}
	attr := NewAttributor(bridgeSeed(100, "sess-1", "claude-code", 7), &FieldScrubber{ArgvMode: "preview", MaxPreviewBytes: 512}, nil)
	_, _ = runObserver(t, Options{Backend: be, Attributor: attr, Sink: sink, DeepEnricher: de, BatchSize: 100, FlushInterval: time.Hour})

	// Called exactly at the two exec (ChangeCreated) points — pid 100 and 200
	// — and NOT on the two exit updates (those reuse the tracked run).
	if len(de.pids) != 2 {
		t.Fatalf("DeepEnrich called %d times, want 2 (one per persisted exec): pids=%v", len(de.pids), de.pids)
	}
	got := map[int]bool{}
	for _, p := range de.pids {
		got[p] = true
	}
	if !got[100] || !got[200] {
		t.Errorf("DeepEnrich pids = %v, want {100,200}", de.pids)
	}
	// The stamp survives onto every persisted copy, including the exit updates.
	for _, r := range sink.Runs {
		if r.ExeHash != "sha256:deep" || r.EnvPosture["DEEP"] != "1" {
			t.Errorf("run pid=%d missing deep-enrich stamp: hash=%q env=%v", r.PID, r.ExeHash, r.EnvPosture)
		}
	}
}

func TestObserverDeepEnrichGatedByCapturePolicy(t *testing.T) {
	t.Parallel()
	events := []RawEvent{
		execEv("b", 500, 1, 1000, "/bin/bash", []string{"bash"}, ts(1)),
		exitEv("b", 500, 1000, 0, ts(2)),
	}
	// Unattributed + capture_unattributed=false → dropped, so the expensive
	// deep enrichment must NOT run.
	de := &fakeDeepEnricher{}
	_, _ = runObserver(t, Options{Backend: &FakeBackend{Events: events}, Attributor: NewAttributor(nil, nil, nil), Sink: &SliceSink{}, DeepEnricher: de, FlushInterval: time.Hour})
	if len(de.pids) != 0 {
		t.Errorf("DeepEnrich ran on a dropped unattributed run: pids=%v", de.pids)
	}

	// capture_unattributed=true → the unattributed exec persists, so it IS
	// enriched (once, at exec).
	de2 := &fakeDeepEnricher{}
	_, _ = runObserver(t, Options{Backend: &FakeBackend{Events: events}, Attributor: NewAttributor(nil, nil, nil), Sink: &SliceSink{}, DeepEnricher: de2, CaptureUnattributed: true, FlushInterval: time.Hour})
	if len(de2.pids) != 1 || de2.pids[0] != 500 {
		t.Errorf("DeepEnrich on captured-unattributed = %v, want [500]", de2.pids)
	}
}

func TestObserverSinkErrorIsNonFatal(t *testing.T) {
	t.Parallel()
	be := &FakeBackend{Events: attributedSequence()}
	sink := &SliceSink{Err: errors.New("db locked")}
	o := NewObserver(Options{Backend: be, Attributor: NewAttributor(bridgeSeed(100, "s", "claude-code", 1), nil, nil), Sink: sink, FlushInterval: time.Hour})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("sink error must not fail Run: %v", err)
	}
	if o.Health().Snapshot().Dropped["sink_error"] == 0 {
		t.Error("sink error should be recorded as a drop, not crash the daemon")
	}
}
