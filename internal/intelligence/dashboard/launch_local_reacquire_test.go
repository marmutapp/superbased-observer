package dashboard

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// localReacquireManager hands out a FRESH recordingWriter on every
// AcquireWriterLocal call (unlike recordingLaunchManager, which pins one), so a
// test can revoke the first lease — simulating a native-terminal reclaim — and
// prove the loopback bridge re-grants control on an {"t":"acquire-writer"}
// frame. The rest of the LaunchManager surface is inherited.
type localReacquireManager struct {
	*recordingLaunchManager
	mu      sync.Mutex
	handed  []*recordingWriter
	acquire int
}

func (m *localReacquireManager) AcquireWriterLocal(string) (LaunchWriter, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := newRecordingWriter()
	m.handed = append(m.handed, w)
	m.acquire++
	return w, nil
}

func (m *localReacquireManager) writerAt(i int) *recordingWriter {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i >= len(m.handed) {
		return nil
	}
	return m.handed[i]
}

// TestLocalReacquireAfterRevokeRegrantsWriter pins the native-reclaim →
// dashboard take-back seam: a loopback /ws/launch seat whose writer lease was
// revoked (native terminal reclaimed control) re-acquires with the same
// acquire-writer control frame the remote path uses — no conjunction, no
// reconnect — and its keystrokes flow again through the NEW lease. Guards two
// regressions at once: the local bridge passing a nil acquire closure (the
// frame would be silently dropped), and the bridge's stale demoted writer
// variable masking the acquire case (writer is only cleared on a failed write,
// and a demoted client stops writing).
func TestLocalReacquireAfterRevokeRegrantsWriter(t *testing.T) {
	lm := &localReacquireManager{recordingLaunchManager: newRecordingLaunchManager(nil)}
	t.Cleanup(func() { close(lm.sub.release) })
	s := newLaunchTestServer(t, lm)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	base := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, base+"/ws/launch/HANDLE-abc", &websocket.DialOptions{
		HTTPHeader: map[string][]string{"Origin": {ts.URL}},
	})
	if err != nil {
		t.Fatalf("same-origin dial: %v", err)
	}
	defer c.CloseNow()

	// The loopback open takes the owner-local writer unconditionally.
	deadline := time.Now().Add(3 * time.Second)
	for lm.writerAt(0) == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	w1 := lm.writerAt(0)
	if w1 == nil {
		t.Fatal("loopback open never took the owner-local writer")
	}

	// A keystroke reaches the initial lease.
	_ = c.Write(ctx, websocket.MessageBinary, []byte("a"))
	select {
	case <-w1.written:
	case <-time.After(3 * time.Second):
		t.Fatal("pre-revoke keystroke never reached the initial writer")
	}

	// Native terminal reclaims: the seat's lease is revoked and the client is
	// told it lost control.
	close(w1.revoked)
	if !waitForControl(t, ctx, c, "control_revoked") {
		t.Fatal("expected control_revoked after the lease was revoked")
	}

	// Take-back: the same acquire-writer frame the remote path uses, with the
	// cap/confirm fields empty (the local closure ignores them).
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"t":"acquire-writer","cap":"","confirm":""}`))
	if !waitForControl(t, ctx, c, "control_granted") {
		t.Fatal("expected control_granted on a local re-acquire after revoke")
	}

	// Keystrokes flow again — through the NEW lease, not the revoked one.
	w2 := lm.writerAt(1)
	if w2 == nil {
		t.Fatal("re-acquire never took a fresh owner-local writer")
	}
	_ = c.Write(ctx, websocket.MessageBinary, []byte("b"))
	select {
	case <-w2.written:
	case <-time.After(3 * time.Second):
		t.Fatal("post-reclaim keystroke never reached the re-acquired writer")
	}
	if got := w1.writes.Load(); got != 1 {
		t.Errorf("revoked writer saw %d writes; want exactly the 1 pre-revoke keystroke", got)
	}
	if lm.remoteCalls.Load() != 0 {
		t.Error("a loopback re-acquire must never route through AcquireWriterRemote")
	}
}
