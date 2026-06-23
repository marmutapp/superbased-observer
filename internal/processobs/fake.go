package processobs

import "context"

// FakeBackend is a deterministic Backend that replays a fixed RawEvent
// sequence then closes the channel. It needs no privileges, so it drives
// the store/dashboard/CLI/guard tests (spec §16.2) and this package's own
// pipeline tests without a real OS backend.
type FakeBackend struct {
	BackendName string
	Events      []RawEvent
	// StartErr, when set, makes Start fail — used to test the degraded-
	// health / fail-open path.
	StartErr error

	closed bool
}

// Name implements Backend.
func (f *FakeBackend) Name() string {
	if f.BackendName == "" {
		return "fake"
	}
	return f.BackendName
}

// Start emits the configured events on a buffered channel and closes it,
// or returns StartErr. The context is honored: emission stops if cancelled.
func (f *FakeBackend) Start(ctx context.Context) (<-chan RawEvent, error) {
	if f.StartErr != nil {
		return nil, f.StartErr
	}
	ch := make(chan RawEvent, len(f.Events)+1)
	go func() {
		defer close(ch)
		for _, ev := range f.Events {
			select {
			case <-ctx.Done():
				return
			case ch <- ev:
			}
		}
	}()
	return ch, nil
}

// Close implements Backend.
func (f *FakeBackend) Close() error { f.closed = true; return nil }

// Closed reports whether Close was called (test helper).
func (f *FakeBackend) Closed() bool { return f.closed }

// SliceSink is an in-memory Sink that records every persisted run, for
// tests. Safe for the single-goroutine Observer loop.
type SliceSink struct {
	Runs []ProcessRun
	Err  error // when set, PersistRuns fails (tests the sink-error path)
}

// PersistRuns implements Sink.
func (s *SliceSink) PersistRuns(_ context.Context, runs []ProcessRun) (int, error) {
	if s.Err != nil {
		return 0, s.Err
	}
	s.Runs = append(s.Runs, runs...)
	return len(runs), nil
}
