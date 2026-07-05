package bridge

import "testing"

func TestBackendName(t *testing.T) {
	if got := New(Options{}).Name(); got != "bridge" {
		t.Fatalf("Name() = %q, want bridge", got)
	}
}

func TestBackendCloseIdempotent(t *testing.T) {
	b := New(Options{})
	if err := b.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("second Close (must be a no-op): %v", err)
	}
}

func TestStatsZeroValue(t *testing.T) {
	if s := New(Options{}).Stats(); s.Events != 0 || s.DecodeErrs != 0 || s.Respawns != 0 || s.LastErr != "" {
		t.Fatalf("fresh Stats not zero: %+v", s)
	}
}
