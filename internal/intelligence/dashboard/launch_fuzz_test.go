package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/marmutapp/superbased-observer/internal/db"
)

// newLaunchServerTB is the testing.TB flavour of newLaunchTestServer so a fuzz
// harness (which holds *testing.F) can build a server once, outside f.Fuzz.
func newLaunchServerTB(tb testing.TB, lm LaunchManager) *Server {
	tb.Helper()
	database, err := openTestDB(context.Background(), db.Options{Path: filepath.Join(tb.TempDir(), "d.db")})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = database.Close() })
	s, err := New(Options{DB: database, LaunchManager: lm})
	if err != nil {
		tb.Fatal(err)
	}
	return s
}

// FuzzLaunchBridgeViewerFrames feeds arbitrary / malformed / mixed-type viewer
// frames into the /ws/launch bridge on a REMOTE-exposed connection that never
// holds a writer lease, and asserts the §4.β / §8.1-item-3 invariant at the
// MANAGER side-effect boundary: NO viewer frame — binary keystroke, resize,
// arbitrary text control, duplicated-field JSON, client "oob", or rejected
// acquire — ever reaches Write / Resize / a granted lease. The recording manager
// grants NO remote writer (remoteWriter == nil), so a lease can never exist; the
// owner-local writer must never be taken for a remote request. The stale/revoked
// -lease half of item 3 is proven at the same boundary by
// termsession.TestRevokedLeaseFramesNeverReachPTY.
func FuzzLaunchBridgeViewerFrames(f *testing.F) {
	lm := newRecordingLaunchManager(nil) // nil remoteWriter ⇒ acquire always denied
	f.Cleanup(func() { close(lm.sub.release) })
	s := newLaunchServerTB(f, lm)
	inner := s.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(w, r.WithContext(withRemoteExposed(r.Context())))
	}))
	f.Cleanup(ts.Close)
	base := "ws" + strings.TrimPrefix(ts.URL, "http")

	// Seed the corpus with the tricky control-frame shapes.
	seeds := []string{
		`{"t":"resize","rows":40,"cols":120}`,            // valid resize, no lease ⇒ dropped
		`{"t":"resize","rows":99999,"cols":1}`,           // out-of-range dims
		`{"t":`,                                          // truncated JSON
		`{"t":"oob","data":"forged"}`,                    // client "oob" is not a real frame
		`{"t":"acquire-writer","cap":"x"}`,               // acquire missing confirm
		`{"t":"resize","t":"acquire-writer"}`,            // duplicated key
		`{"t":123,"rows":"not-a-number"}`,                // mixed types
		`{"t":"control_granted"}`,                        // client forging a server->client control
		`{"t":"exit","code":0}`,                          // client forging an exit
		"",                                               // empty
		"\x00\x1b\xff\xfe not json at all",               // arbitrary bytes
		`{"t":"acquire-writer","cap":"a","confirm":"b"}`, // rejected acquire (remoteWriter nil)
	}
	for _, sd := range seeds {
		f.Add([]byte(sd))
	}
	f.Add([]byte("\x1b]133;C\x07rm -rf /")) // an OSC-laced keystroke blob

	f.Fuzz(func(t *testing.T, data []byte) {
		// Keep frames under the 1 MiB server read cap — oversized rejection is
		// covered separately (TestOversizedViewerFrameRejected); here we probe the
		// dispatch path, not the cap.
		if len(data) > 256*1024 {
			data = data[:256*1024]
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, _, err := websocket.Dial(ctx, base+"/ws/launch/HANDLE-abc", &websocket.DialOptions{
			HTTPHeader: http.Header{"Origin": {ts.URL}},
		})
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.CloseNow()

		// Feed the fuzz bytes down BOTH frame paths: as a binary keystroke blob
		// and as an attempted text control frame.
		_ = c.Write(ctx, websocket.MessageBinary, data)
		_ = c.Write(ctx, websocket.MessageText, data)

		// Barrier: a final rejected acquire whose control_denied response proves
		// the server's FIFO read loop processed every earlier frame.
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"t":"acquire-writer","cap":"barrier","confirm":"barrier"}`))
		// Drain up to a few control frames looking for the denial; a read error
		// (e.g. the server closed on a malformed frame) also means the frames were
		// consumed and the socket torn down — either way no side effect is allowed.
		for i := 0; i < 8; i++ {
			rctx, rcancel := context.WithTimeout(ctx, 2*time.Second)
			typ, msg, rerr := c.Read(rctx)
			rcancel()
			if rerr != nil {
				break
			}
			if typ == websocket.MessageText && strings.Contains(string(msg), "control_denied") {
				break
			}
		}

		// The MANAGER side-effect boundary: nothing reached the PTY, and the
		// owner-local writer was never taken for a remote request.
		if n := lm.localCalls.Load(); n != 0 {
			t.Fatalf("owner-local writer acquired on a REMOTE request (%d)", n)
		}
		if w, rz := lm.localWriter.writes.Load(), lm.localWriter.resizes.Load(); w != 0 || rz != 0 {
			t.Fatalf("viewer frame reached the PTY without a lease (writes=%d resizes=%d)", w, rz)
		}
	})
}
