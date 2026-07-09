package main

import (
	"context"
	"testing"

	otelexp "github.com/marmutapp/superbased-observer/internal/exporter/otel"
)

// stubSpanSink records emitted guard events.
type stubSpanSink struct{ events []otelexp.GuardEvent }

func (s *stubSpanSink) ExportGuardEvent(_ context.Context, ev otelexp.GuardEvent) {
	s.events = append(s.events, ev)
}

// TestGuardOTelTail_FirstEnableAnchorsAtTail pins the no-flood
// posture: history that predates the first enable never reaches the
// collector.
func TestGuardOTelTail_FirstEnableAnchorsAtTail(t *testing.T) {
	st := newCloudTestStore(t)
	ctx := context.Background()
	insertGuardEvent(t, st, "s1", "R-001", "high", "deny", "hook") // pre-existing history

	sink := &stubSpanSink{}
	tail := &guardOTelTail{st: st, sink: sink, logger: quietLogger()}
	if err := tail.anchorCursor(ctx); err != nil {
		t.Fatalf("anchor: %v", err)
	}
	tail.sweep(ctx)
	if len(sink.events) != 0 {
		t.Errorf("pre-existing history emitted on first enable: %+v", sink.events)
	}

	// New events after the anchor DO emit, fully mapped.
	id := insertGuardEvent(t, st, "s2", "R-002", "critical", "deny", "proxy")
	tail.sweep(ctx)
	if len(sink.events) != 1 {
		t.Fatalf("got %d emissions, want 1", len(sink.events))
	}
	ev := sink.events[0]
	if ev.ID != id || ev.RuleID != "R-002" || ev.Severity != "critical" ||
		ev.Decision != "deny" || ev.SessionID != "s2" || ev.Source != "proxy" ||
		ev.Tool != "claude-code" || ev.EventKind != "shell_exec" ||
		ev.TargetHash != "th" || ev.ChainHash == "" || ev.Reason != "test verdict" {
		t.Errorf("row mapping wrong: %+v", ev)
	}

	// Idempotence: a sweep with nothing new emits nothing.
	tail.sweep(ctx)
	if len(sink.events) != 1 {
		t.Errorf("re-sweep re-emitted: %d events", len(sink.events))
	}
}

// TestGuardOTelTail_CursorIndependentOfCloud pins that the OTel tail
// and the cloud dispatcher track separate positions over the same
// guard_events tail.
func TestGuardOTelTail_CursorIndependentOfCloud(t *testing.T) {
	st := newCloudTestStore(t)
	ctx := context.Background()

	tail := &guardOTelTail{st: st, sink: &stubSpanSink{}, logger: quietLogger()}
	if err := tail.anchorCursor(ctx); err != nil {
		t.Fatalf("anchor: %v", err)
	}
	insertGuardEvent(t, st, "s1", "R-001", "high", "deny", "hook")
	tail.sweep(ctx)

	otelCur, err := st.LoadGuardOTelCursor(ctx)
	if err != nil || otelCur == 0 {
		t.Fatalf("otel cursor not advanced: %d, %v", otelCur, err)
	}
	cloudCur, err := st.LoadGuardCloudCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cloudCur == otelCur {
		t.Errorf("cloud cursor moved with the otel tail (%d == %d) — consumers must be independent", cloudCur, otelCur)
	}
}
