package termsession

import (
	"testing"
	"time"
)

// TestOnExitFiresOnce pins the Phase-0 session-exit seam (remote-dashboard-
// access plan §7 Phase 0): the OnExit callback fires exactly once when a
// session's process exits, carrying content-free metadata (session id,
// subcommand, exit code). A nil callback (the default) must be a no-op — the
// classic loopback launch path is unchanged.
func TestOnExitFiresOnce(t *testing.T) {
	got := make(chan SessionExit, 4)
	sp := &fakeSpawner{}
	m := NewManager(Options{
		Spawner:      sp,
		ReapInterval: time.Hour,
		IdleTimeout:  30 * time.Minute,
		ExitLinger:   10 * time.Millisecond,
		OnExit:       func(se SessionExit) { got <- se },
	})
	t.Cleanup(m.Shutdown)

	if _, err := m.Create(validSpec()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.last().exit(7)

	select {
	case se := <-got:
		if se.SessionID != "abc123" || se.Subcommand != "claude" || se.ExitCode != 7 {
			t.Errorf("SessionExit = %+v, want session=abc123 sub=claude code=7", se)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnExit did not fire within 2s")
	}

	// No second fire.
	select {
	case se := <-got:
		t.Fatalf("OnExit fired twice: %+v", se)
	case <-time.After(200 * time.Millisecond):
	}
}
