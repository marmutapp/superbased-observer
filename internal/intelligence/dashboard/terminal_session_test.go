package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/db"
)

// newTerminalSessionServer builds a dashboard Server with the given
// token→session-link resolver and optional remote controller (for the
// allow_terminal_view gate).
func newTerminalSessionServer(t *testing.T, resolver func(string) (TerminalSessionLink, bool), remote RemoteController) *Server {
	t.Helper()
	database, err := openTestDB(context.Background(), db.Options{Path: filepath.Join(t.TempDir(), "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	opts := Options{DB: database, SessionResolver: resolver}
	if remote != nil {
		opts.Remote = remote
	}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// fixedSessionResolver returns a resolver backed by a static token→link map.
func fixedSessionResolver(m map[string]TerminalSessionLink) func(string) (TerminalSessionLink, bool) {
	return func(token string) (TerminalSessionLink, bool) {
		if v, ok := m[token]; ok {
			return v, true
		}
		return TerminalSessionLink{}, false
	}
}

func TestTerminalSessionNilResolverDisabled(t *testing.T) {
	s := newTerminalSessionServer(t, nil, nil)
	// A known-shape path must 404 when the seam is nil (endpoint existence
	// unconfirmable).
	rec := doGet(t, s, "/api/terminal/session/LIVE", false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404 (cockpit disabled)", rec.Code)
	}
}

func TestTerminalSessionTokenGates(t *testing.T) {
	resolver := fixedSessionResolver(map[string]TerminalSessionLink{
		"CORR": {RunID: "run-1", Kind: "attach", Tool: "claude-code", SessionID: "sess-9", Confidence: 0.95},
		"BARE": {RunID: "run-2", Kind: "fresh", Tool: "codex"}, // live, uncorrelated
	})
	s := newTerminalSessionServer(t, resolver, nil)

	t.Run("unknown token 404", func(t *testing.T) {
		rec := doGet(t, s, "/api/terminal/session/GHOST", false)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", rec.Code)
		}
		if got := errorCode(t, rec); got != "unknown_token" {
			t.Fatalf("error = %q, want unknown_token", got)
		}
	})

	t.Run("empty token 404", func(t *testing.T) {
		rec := doGet(t, s, "/api/terminal/session/", false)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", rec.Code)
		}
		if got := errorCode(t, rec); got != "unknown_token" {
			t.Fatalf("error = %q, want unknown_token", got)
		}
	})

	t.Run("method not allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/terminal/session/CORR", nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("code = %d, want 405", rec.Code)
		}
		if got := errorCode(t, rec); got != "method_not_allowed" {
			t.Fatalf("error = %q, want method_not_allowed", got)
		}
	})

	t.Run("correlated 200 shape", func(t *testing.T) {
		rec := doGet(t, s, "/api/terminal/session/CORR", false)
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
		}
		var resp terminalSessionResp
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		want := terminalSessionResp{
			RunID: "run-1", Kind: "attach", Tool: "claude-code",
			Correlated: true, SessionID: "sess-9", Confidence: 0.95,
		}
		if resp != want {
			t.Fatalf("resp = %+v, want %+v", resp, want)
		}
	})

	t.Run("uncorrelated 200 shape", func(t *testing.T) {
		rec := doGet(t, s, "/api/terminal/session/BARE", false)
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d", rec.Code)
		}
		var resp terminalSessionResp
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		want := terminalSessionResp{
			RunID: "run-2", Kind: "fresh", Tool: "codex",
			Correlated: false, SessionID: "", Confidence: 0,
		}
		if resp != want {
			t.Fatalf("resp = %+v, want %+v (uncorrelated: correlated:false, empty id, 0 confidence)", resp, want)
		}
	})
}

// TestTerminalSessionRemoteGate is the token-oracle test: a remote-exposed
// caller without allow_terminal_view is refused with an IDENTICAL 403 for a
// correlated, an uncorrelated, AND an unknown token — the gate runs BEFORE the
// resolver so the response can't discriminate token state. Mirrors
// TestProjectPanelRemoteGate.
func TestTerminalSessionRemoteGate(t *testing.T) {
	base := fixedSessionResolver(map[string]TerminalSessionLink{
		"CORR": {RunID: "run-1", Kind: "attach", Tool: "claude-code", SessionID: "sess-9", Confidence: 0.95},
		"BARE": {RunID: "run-2", Kind: "fresh", Tool: "codex"},
		// "GHOST" is absent → unknown.
	})
	// Counting resolver: the ZERO-invocation assertion (below) is what makes this
	// a real oracle test. Identical 403s alone are insufficient — a regression
	// that resolved the token BEFORE gating would still return the same body. A
	// call counter proves the gate short-circuits ahead of the resolver. Requests
	// are synchronous (httptest ServeHTTP runs in-goroutine), so a plain int is
	// race-free.
	var calls int
	resolver := func(tok string) (TerminalSessionLink, bool) {
		calls++
		return base(tok)
	}

	// view disabled: every token state gets an identical 403 AND never reaches
	// the resolver.
	sOff := newTerminalSessionServer(t, resolver, NewRemoteController(RemoteOptions{AllowTerminalView: false}))
	for _, token := range []string{"CORR", "BARE", "GHOST"} {
		rec := doGet(t, sOff, "/api/terminal/session/"+token, true)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("remote view-off token=%s: code = %d, want 403", token, rec.Code)
		}
		if got := errorCode(t, rec); got != "remote_view_disabled" {
			t.Fatalf("remote view-off token=%s: error = %q, want remote_view_disabled", token, got)
		}
	}

	// Every remotely-gated request above (correlated, uncorrelated, unknown) must
	// have short-circuited at the gate BEFORE the resolver — the load-bearing
	// hardening. The counter is monotonic, so calls==0 after all three proves
	// each made zero resolver invocations.
	if calls != 0 {
		t.Fatalf("resolver invoked %d times across the gated requests, want 0 (the gate MUST precede the token oracle)", calls)
	}

	// Local (non-remote) caller is never gated — and DOES reach the resolver,
	// proving the counter is live (a dead counter would trivially pass the
	// zero-assertion above).
	if rec := doGet(t, sOff, "/api/terminal/session/CORR", false); rec.Code != http.StatusOK {
		t.Fatalf("local caller code = %d, want 200", rec.Code)
	}
	if calls != 1 {
		t.Fatalf("local caller: resolver invoked %d times, want 1 (an ungated request must reach the resolver)", calls)
	}

	// view enabled: the remote caller passes the gate (and reaches the resolver).
	sOn := newTerminalSessionServer(t, resolver, NewRemoteController(RemoteOptions{AllowTerminalView: true}))
	if rec := doGet(t, sOn, "/api/terminal/session/CORR", true); rec.Code != http.StatusOK {
		t.Fatalf("remote view-on code = %d, want 200", rec.Code)
	}
	if calls != 2 {
		t.Fatalf("remote view-on: resolver invoked %d times total, want 2 (gated caller reaches resolver once view is allowed)", calls)
	}
}
