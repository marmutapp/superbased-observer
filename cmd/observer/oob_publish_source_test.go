package main

import (
	"context"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/termoob"
	"github.com/marmutapp/superbased-observer/internal/termrun"
)

// TestPublishOOBSessionSourceMapping pins publishOOB's Session-frame Source →
// correlation Source mapping (the distinct-confidence-source intent + R2-6): an
// empty hint (claude's forced id / codex resume) records at SourceOOB, an
// explicit "discovered" hint (codex discovery) records at SourceDiscovered, and
// any UNKNOWN non-empty hint is REFUSED — the correlation is SKIPPED entirely
// rather than promoted to full OOB confidence for an evidence class this daemon
// doesn't understand. Asserted through the real ptyLauncher.publishOOB via the
// correlate seam, so the mapping table — not a per-test reimplementation — is
// what's under test.
func TestPublishOOBSessionSourceMapping(t *testing.T) {
	cases := []struct {
		name        string
		frameSource string
		want        termrun.Source
		expectSkip  bool // R2-6: unknown class ⇒ no Correlate call at all
	}{
		{"empty hint → oob (known id)", "", termrun.SourceOOB, false},
		{"discovered hint → discovered", termoob.SessionSourceDiscovered, termrun.SourceDiscovered, false},
		{"unknown future hint → skipped (no correlate)", "some-future-source", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				gotID     string
				gotSource termrun.Source
				calls     int
			)
			l := &ptyLauncher{
				correlate: func(_ context.Context, runID, sessionID string, source termrun.Source, _ time.Time) error {
					calls++
					gotID = sessionID
					gotSource = source
					return nil
				},
			}
			l.publishOOB("run-x", "codex", termoob.Frame{
				Type:    termoob.TypeSession,
				Session: &termoob.Session{SessionID: "sess-x", Source: tc.frameSource},
			})
			if tc.expectSkip {
				if calls != 0 {
					t.Fatalf("unknown source class must SKIP correlation, got %d calls (source=%q)", calls, gotSource)
				}
				return
			}
			if calls != 1 {
				t.Fatalf("correlate called %d times, want 1", calls)
			}
			if gotID != "sess-x" {
				t.Fatalf("sessionID = %q, want sess-x", gotID)
			}
			if gotSource != tc.want {
				t.Fatalf("mapped source = %q, want %q", gotSource, tc.want)
			}
		})
	}
}

// TestPublishOOBSessionEmptyIDNoCorrelate pins that a Session frame with no id is
// dropped before the correlate seam regardless of Source hint.
func TestPublishOOBSessionEmptyIDNoCorrelate(t *testing.T) {
	calls := 0
	l := &ptyLauncher{
		correlate: func(context.Context, string, string, termrun.Source, time.Time) error {
			calls++
			return nil
		},
	}
	l.publishOOB("run-x", "codex", termoob.Frame{
		Type:    termoob.TypeSession,
		Session: &termoob.Session{SessionID: "", Source: termoob.SessionSourceDiscovered},
	})
	if calls != 0 {
		t.Fatalf("correlate called %d times for an empty-id frame, want 0", calls)
	}
}
