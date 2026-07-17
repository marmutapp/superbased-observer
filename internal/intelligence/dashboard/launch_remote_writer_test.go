package dashboard

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// recordingWriter is a LaunchWriter that counts every PTY side effect so a test
// can assert the §4.β boundary: a viewer's frames must never reach Write/Resize.
type recordingWriter struct {
	revoked chan struct{}
	writes  atomic.Int32
	resizes atomic.Int32
	written chan struct{} // signalled on the first Write (positive-path sync)
}

func newRecordingWriter() *recordingWriter {
	return &recordingWriter{revoked: make(chan struct{}), written: make(chan struct{}, 1)}
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.writes.Add(1)
	select {
	case w.written <- struct{}{}:
	default:
	}
	return len(p), nil
}
func (w *recordingWriter) Resize(uint16, uint16) error { w.resizes.Add(1); return nil }
func (w *recordingWriter) Revoked() <-chan struct{}    { return w.revoked }
func (w *recordingWriter) Release()                    {}
func (w *recordingWriter) Holder() string              { return "device-fp" }
func (w *recordingWriter) RevokeIsTakeover() bool      { return false }

// recordingLaunchManager records which writer-acquire path was taken and hands
// back instrumented writers so a test can prove a remote-exposed WS never takes
// the owner-local writer and never mutates the PTY without a granted lease. All
// cross-goroutine state is atomic so the -race detector stays clean.
type recordingLaunchManager struct {
	sub               *fakeSubscription
	localWriter       *recordingWriter // pre-created; AcquireWriterLocal never reassigns
	remoteWriter      *recordingWriter // nil ⇒ AcquireWriterRemote denies
	localCalls        atomic.Int32
	remoteCalls       atomic.Int32
	remoteExposedSeen atomic.Bool
	lastRemoteReq     atomic.Pointer[RemoteWriterRequest] // last AcquireWriterRemote input (side-channel test)

	// Setup-confidentiality instrumentation (FIX 1): setupHandles marks which
	// handles are SpecSetup; the two counters record which subscribe path the WS
	// bridge took; snapshot is what Snapshot() returns (so the redaction test can
	// assert a setup handle never appears for a remote caller).
	setupHandles         map[string]bool
	subscribeLocalCalls  atomic.Int32
	subscribeRemoteCalls atomic.Int32
	snapshot             []LaunchInfo
}

func newRecordingLaunchManager(remoteWriter *recordingWriter) *recordingLaunchManager {
	return &recordingLaunchManager{
		sub:          newFakeSubscription(),
		localWriter:  newRecordingWriter(),
		remoteWriter: remoteWriter,
	}
}

func (m *recordingLaunchManager) Create(LaunchSpec) (string, error)           { return "H", nil }
func (m *recordingLaunchManager) CreateFresh(FreshLaunchSpec) (string, error) { return "H", nil }
func (m *recordingLaunchManager) CreateSetup(SetupSpec) (string, error)       { return "H", nil }
func (m *recordingLaunchManager) Subscribe(string) (LaunchSubscription, error) {
	m.subscribeLocalCalls.Add(1)
	return m.sub, nil
}

func (m *recordingLaunchManager) SubscribeRemote(handle string) (LaunchSubscription, error) {
	m.subscribeRemoteCalls.Add(1)
	if m.setupHandles[handle] {
		// A setup session is local-only for reads — refuse the remote subscribe
		// (the manager returns ErrSetupSessionLocalOnly; the dashboard maps any
		// error to a policy-violation close).
		return nil, ErrLaunchExecuteUnavailable
	}
	return m.sub, nil
}

func (m *recordingLaunchManager) IsSetupSession(handle string) bool { return m.setupHandles[handle] }
func (m *recordingLaunchManager) Unsubscribe(LaunchSubscription)    {}
func (m *recordingLaunchManager) AcquireWriterLocal(string) (LaunchWriter, error) {
	m.localCalls.Add(1)
	return m.localWriter, nil
}

func (m *recordingLaunchManager) AcquireWriterRemote(req RemoteWriterRequest) (LaunchWriter, error) {
	m.remoteCalls.Add(1)
	m.remoteExposedSeen.Store(req.RemoteExposed)
	r := req
	m.lastRemoteReq.Store(&r)
	if m.remoteWriter == nil {
		return nil, ErrLaunchExecuteUnavailable
	}
	return m.remoteWriter, nil
}
func (m *recordingLaunchManager) Close(string)                                   {}
func (m *recordingLaunchManager) Snapshot() []LaunchInfo                         { return m.snapshot }
func (m *recordingLaunchManager) RevokeAllRemoteWriters(string) int              { return 0 }
func (m *recordingLaunchManager) RevokeRemoteWriterByHolder(string, string) bool { return false }

// remoteExposedWSServer serves the dashboard mux with the remote-exposed
// provenance marker forced on — simulating a request that arrived through the
// remoteAuthz chain — so handleLaunchWS takes the remote writer path.
func remoteExposedWSServer(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	h := s.Handler()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(withRemoteExposed(r.Context())))
	}))
}

// TestViewerFrameNeverReachesPTYWithoutLease pins §4.β at the manager side-effect
// boundary: on a remote-exposed /ws/launch bridge with NO granted writer lease,
// a viewer's binary keystrokes, resize, arbitrary control, and client-"oob"
// frames are ALL dropped — none reaches Write/Resize — and the owner-local
// writer path is NEVER taken for a remote request.
func TestViewerFrameNeverReachesPTYWithoutLease(t *testing.T) {
	lm := newRecordingLaunchManager(nil) // remoteWriter nil ⇒ acquire denied
	t.Cleanup(func() { close(lm.sub.release) })
	s := newLaunchTestServer(t, lm)
	ts := remoteExposedWSServer(t, s)
	defer ts.Close()

	base := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, base+"/ws/launch/HANDLE-abc", &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {ts.URL}},
	})
	if err != nil {
		t.Fatalf("same-origin dial: %v", err)
	}
	defer c.CloseNow()

	// Every one of these must be dropped without a live lease.
	_ = c.Write(ctx, websocket.MessageBinary, []byte("rm -rf / #keystroke"))
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"t":"resize","rows":40,"cols":120}`))
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"t":"bogus-control"}`))
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"t":"oob","data":"forged"}`))
	// An acquire-writer frame whose capability is rejected must also grant nothing.
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"t":"acquire-writer","cap":"bad","confirm":"bad"}`))

	// Give the server's read loop time to process every frame.
	time.Sleep(200 * time.Millisecond)

	if n := lm.localCalls.Load(); n != 0 {
		t.Errorf("AcquireWriterLocal called %d times on a REMOTE-exposed request — a remote principal must never take the owner-local writer", n)
	}
	if lm.remoteCalls.Load() == 0 {
		t.Error("expected the rejected acquire-writer frame to attempt AcquireWriterRemote (defence-in-depth)")
	}
	if lm.localWriter.writes.Load() != 0 || lm.localWriter.resizes.Load() != 0 {
		t.Errorf("owner-local writer was driven on a remote request (writes=%d resizes=%d)", lm.localWriter.writes.Load(), lm.localWriter.resizes.Load())
	}
	// remoteWriter is nil (denied), so there is no lease to have driven at all.
	if !lm.remoteExposedSeen.Load() {
		t.Error("AcquireWriterRemote must be called with RemoteExposed=true (boundary provenance)")
	}
}

// TestRemoteWriterAcquireGrantsWriterThenRoutesInput proves the positive half:
// once an acquire-writer frame yields a granted lease, subsequent keystrokes
// DO reach the writer — and a prior keystroke (before the grant) did not.
func TestRemoteWriterAcquireGrantsWriterThenRoutesInput(t *testing.T) {
	rw := newRecordingWriter()
	lm := newRecordingLaunchManager(rw)
	t.Cleanup(func() { close(lm.sub.release) })
	s := newLaunchTestServer(t, lm)
	ts := remoteExposedWSServer(t, s)
	defer ts.Close()

	base := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, base+"/ws/launch/HANDLE-abc", &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {ts.URL}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	// Keystroke BEFORE any grant → dropped.
	_ = c.Write(ctx, websocket.MessageBinary, []byte("early"))
	time.Sleep(100 * time.Millisecond)
	if rw.writes.Load() != 0 {
		t.Fatalf("keystroke before grant reached the writer (writes=%d)", rw.writes.Load())
	}

	// Acquire the writer; wait for the control_granted ack.
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"t":"acquire-writer","cap":"good","confirm":"good"}`))
	if !waitForControl(t, ctx, c, "control_granted") {
		t.Fatal("expected control_granted after a successful acquire")
	}

	// Now keystrokes reach the granted writer.
	_ = c.Write(ctx, websocket.MessageBinary, []byte("ls"))
	select {
	case <-rw.written:
	case <-time.After(2 * time.Second):
		t.Fatal("keystroke after grant never reached the writer")
	}
	if lm.localCalls.Load() != 0 {
		t.Errorf("AcquireWriterLocal was taken on a remote request (%d)", lm.localCalls.Load())
	}
}

// TestRemotePrincipalCannotReadOrSeeSetupSession is the critical FIX-1 pin: a
// remote-exposed principal (View/Execute — the /ws/launch bridge never grants a
// setup writer anyway) can NEITHER subscribe to / read a SpecSetup session's
// output NOR see its handle in the remote session snapshot. The local owner
// still sees the handle (the in-dashboard xterm embed needs it). It complements
// the manager-level TestSetupSessionRefusesRemoteWriter (write side) with the
// read + snapshot side.
func TestRemotePrincipalCannotReadOrSeeSetupSession(t *testing.T) {
	lm := newRecordingLaunchManager(nil) // no remote writer would be granted anyway
	lm.setupHandles = map[string]bool{"SETUP-1": true}
	lm.snapshot = []LaunchInfo{
		{ID: "SETUP-1", Setup: true},
		{ID: "REG-1"},
	}
	t.Cleanup(func() { close(lm.sub.release) })
	s := newLaunchTestServer(t, lm)

	// (1) A REMOTE WS attach to the setup handle is refused before any output.
	ts := remoteExposedWSServer(t, s)
	defer ts.Close()
	base := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, base+"/ws/launch/SETUP-1", &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {ts.URL}},
	})
	if err == nil {
		// Handshake accepted; the server then closes with a policy violation
		// BEFORE streaming any bytes, so a read must fail.
		if _, _, rerr := c.Read(ctx); rerr == nil {
			t.Error("remote WS to a setup session streamed data — it must be refused before any output")
		}
		_ = c.CloseNow()
	}
	if lm.subscribeRemoteCalls.Load() == 0 {
		t.Error("remote WS must route through SubscribeRemote (the setup confidentiality pin), not Subscribe")
	}
	if n := lm.subscribeLocalCalls.Load(); n != 0 {
		t.Errorf("remote WS used the owner-local Subscribe path %d times for a setup session", n)
	}
	if n := lm.localCalls.Load(); n != 0 {
		t.Errorf("owner-local writer path taken %d times on a refused remote setup subscribe", n)
	}

	// (2) The REMOTE snapshot must not even reveal the setup handle.
	remoteBody := httpGetBody(t, ts.URL+"/api/launch/sessions")
	if strings.Contains(remoteBody, "SETUP-1") {
		t.Errorf("remote snapshot leaked the setup handle: %s", remoteBody)
	}
	if !strings.Contains(remoteBody, "REG-1") {
		t.Errorf("remote snapshot dropped the regular session: %s", remoteBody)
	}

	// (3) The LOCAL (owner-loopback) snapshot still shows it — the owner drives
	// the setup PTY from the in-dashboard xterm.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/launch/sessions", nil)
	s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "SETUP-1") {
		t.Errorf("local snapshot must still show the setup handle to the owner: %s", rec.Body.String())
	}
}

// httpGetBody GETs url and returns the response body as a string (test helper).
func httpGetBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// waitForControl reads text control frames until it sees one of type want (or
// the context/timeout expires). Binary output frames are ignored.
func waitForControl(t *testing.T, ctx context.Context, c *websocket.Conn, want string) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rctx, cancel := context.WithTimeout(ctx, 1*time.Second)
		typ, data, err := c.Read(rctx)
		cancel()
		if err != nil {
			return false
		}
		if typ == websocket.MessageText && strings.Contains(string(data), `"`+want+`"`) {
			return true
		}
	}
	return false
}
