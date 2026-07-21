package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestProxy_PrewarmFiresHEADPerTarget pins the V6-3 mitigation:
// Prewarm fires a HEAD against every configured target so the
// http.Transport connection pool warms up before the first real
// proxy request lands. Verified by counting requests served by a
// httptest server registered as the target.
func TestProxy_PrewarmFiresHEADPerTarget(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Method != http.MethodHead {
			t.Errorf("expected HEAD, got %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	sink := &fakeSink{}
	p, err := New(Options{
		AnthropicUpstream: "https://api.anthropic.com",
		Sink:              sink,
		PrewarmTargets:    []string{srv.URL, srv.URL + "/path"},
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	p.Prewarm(context.Background(), p.prewarmTargets)
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("prewarm hits = %d, want 2", got)
	}
}

// TestProxy_PrewarmTolerates5xxAndTimeouts pins that prewarm
// failures are non-fatal. The function returns even if every target
// errors; logs are best-effort.
func TestProxy_PrewarmTolerates5xxAndTimeouts(t *testing.T) {
	srv500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv500.Close()

	srvSlow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer srvSlow.Close()

	sink := &fakeSink{}
	p, err := New(Options{
		AnthropicUpstream: "https://api.anthropic.com",
		Sink:              sink,
		PrewarmTargets:    []string{srv500.URL, srvSlow.URL, "http://unreachable.invalid"},
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		p.Prewarm(ctx, p.prewarmTargets)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Prewarm hung past per-target timeout — should be ~5s per target plus ctx cap")
	}
}

// TestProxy_PrewarmEmptyTargetsSilent pins the trivial guard.
func TestProxy_PrewarmEmptyTargetsSilent(t *testing.T) {
	sink := &fakeSink{}
	p, err := New(Options{
		AnthropicUpstream: "https://api.anthropic.com",
		Sink:              sink,
		PrewarmTargets:    []string{}, // explicit-empty disables prewarm
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	if len(p.prewarmTargets) != 0 {
		t.Errorf("explicit-empty PrewarmTargets should disable prewarm, got %v", p.prewarmTargets)
	}
	// Should not panic / not block.
	p.Prewarm(context.Background(), p.prewarmTargets)
}

// TestProxy_PrewarmNilTargetsUsesDefault pins that a nil
// PrewarmTargets falls back to the chatgpt.com + api.openai.com
// defaults. Construction-time only; we don't actually exercise the
// network here.
func TestProxy_PrewarmNilTargetsUsesDefault(t *testing.T) {
	sink := &fakeSink{}
	p, err := New(Options{
		AnthropicUpstream: "https://api.anthropic.com",
		Sink:              sink,
		// PrewarmTargets left nil
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	if len(p.prewarmTargets) != 2 {
		t.Errorf("default PrewarmTargets count = %d, want 2", len(p.prewarmTargets))
	}
}

// TestProxy_ServeFlushesAfterWriteHeader pins the V6-3 defense-in-
// depth flush: after WriteHeader, the proxy explicitly calls
// flusher.Flush() so codex's inner pipe sees status+headers within
// RTT, before the upstream body composes the first SSE event.
//
// The test stands up a slow-body upstream that delays N ms after
// returning headers, and verifies a flushCount-tracking
// ResponseWriter records at least one flush before that delay would
// have ended.
func TestProxy_ServeFlushesAfterWriteHeader(t *testing.T) {
	const headerDelay = 200 * time.Millisecond

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Sleep AFTER sending headers so the proxy returns from
		// doWithRetry, calls WriteHeader (+ our flush), but the SSE
		// body hasn't arrived yet.
		time.Sleep(headerDelay)
		_, _ = w.Write([]byte("data: ping\n\n"))
	}))
	defer upstream.Close()

	sink := &fakeSink{}
	p, err := New(Options{
		AnthropicUpstream: upstream.URL,
		Sink:              sink,
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		io.NopCloser(stringReader(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Content-Type", "application/json")

	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	p.serve(rec, req)

	if got := atomic.LoadInt32(&rec.flushes); got < 1 {
		t.Errorf("expected at least 1 flush (V6-3 hygiene), got %d", got)
	}
}

// flushRecorder wraps httptest.ResponseRecorder and counts Flush
// calls. Implements http.Flusher so the proxy's `if flusher, ok :=
// w.(http.Flusher); ok` type-assertion succeeds.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushes int32
}

func (f *flushRecorder) Flush() {
	atomic.AddInt32(&f.flushes, 1)
	f.ResponseRecorder.Flush()
}

// stringReader is a tiny io.Reader over a string so the tests don't
// need to pull in strings.NewReader semantics from package strings
// at the test call sites.
type stringReader string

func (s stringReader) Read(p []byte) (int, error) {
	n := copy(p, s)
	if n < len(s) {
		return n, nil
	}
	return n, io.EOF
}
