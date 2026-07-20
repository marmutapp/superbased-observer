package termsession

import (
	"errors"
	"testing"
	"time"
)

// TestRevokedLeaseFramesNeverReachPTY pins the manager-side companion of the WS
// frame-fuzz property (§4.β / §8.1 item 3): once a writer lease is revoked
// (taken over, admin-revoked, or expired) EVERY subsequent Write/Resize on that
// stale lease is fenced out with ErrNotWriter BEFORE it can touch the PTY. The
// generation fence in writeVia/resizeVia returns the error prior to any
// s.pty.Write, so a stale-generation frame is a structural no-op — no amount of
// arbitrary/duplicated input on the dead lease reaches the terminal.
func TestRevokedLeaseFramesNeverReachPTY(t *testing.T) {
	sp := &fakeSpawner{}
	m := NewManager(Options{Spawner: sp, ReapInterval: time.Hour, RingBytes: 1 << 20, Now: time.Now})
	t.Cleanup(m.Shutdown)
	tok, _ := m.Create(validSpec())

	l, err := m.AcquireWriterLocal(tok)
	if err != nil {
		t.Fatalf("AcquireWriterLocal: %v", err)
	}
	// A live lease can drive the PTY (baseline).
	if _, werr := l.Write([]byte("ls\n")); werr != nil {
		t.Fatalf("live-lease Write: %v", werr)
	}

	// Revoke it through the funnel (an admin/device revoke or teardown).
	if !m.RevokeWriter(tok, "revoked for test") {
		t.Fatal("RevokeWriter reported no live writer to revoke")
	}
	select {
	case <-l.Revoked():
	case <-time.After(2 * time.Second):
		t.Fatal("lease Revoked channel never closed")
	}

	// Arbitrary / malformed / duplicated frames on the STALE lease are all fenced.
	frames := [][]byte{
		[]byte("rm -rf /\n"),
		{0x00, 0x1b, 0xff, 0xfe},
		[]byte(""),
		[]byte("\x1b]133;C\x07"),
		[]byte("duplicate"),
		[]byte("duplicate"),
	}
	for i, fr := range frames {
		if _, werr := l.Write(fr); !errors.Is(werr, ErrNotWriter) {
			t.Fatalf("stale-lease Write[%d] = %v, want ErrNotWriter", i, werr)
		}
	}
	// Resize on the stale lease is fenced identically.
	for _, sz := range [][2]uint16{{40, 120}, {0, 0}, {65535, 65535}} {
		if rerr := l.Resize(sz[0], sz[1]); !errors.Is(rerr, ErrNotWriter) {
			t.Fatalf("stale-lease Resize(%d,%d) = %v, want ErrNotWriter", sz[0], sz[1], rerr)
		}
	}
}
