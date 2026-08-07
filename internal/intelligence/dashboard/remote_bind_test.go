package dashboard

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/remoteauth"
)

// readyController is a minimal RemoteController that reports Ready() = true so
// the fail-closed predicate tests can exercise the "substrate present" branch
// without standing up the full Phase-1 stack.
type readyController struct{ ready bool }

func (r readyController) Ready() bool                        { return r.ready }
func (r readyController) AllowedHosts() []string             { return []string{"host.example"} }
func (r readyController) Principal(*http.Request) Capability { return CapabilityPublic }
func (r readyController) AllowTerminal() bool                { return false }
func (r readyController) Routes() []ExtraRoute               { return nil }
func (r readyController) Sessions() []remoteauth.SessionInfo { return nil }
func (r readyController) RevokeSession(string) bool          { return false }
func (r readyController) RotateSessions() error              { return nil }
func (r readyController) ReloadSecret(string)                {}

func newBindTestServer(t *testing.T, rc RemoteController) *Server {
	t.Helper()
	database, err := openTestDB(context.Background(), db.Options{Path: filepath.Join(t.TempDir(), "d.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	s, err := New(Options{DB: database, Remote: rc})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// TestNonLoopbackBindRefusedWithoutAuth pins the plan §4.6 atomic-safety rule:
// with no remote substrate (the default), a non-loopback bind FAILS CLOSED —
// ListenAndServe returns an error immediately rather than exposing an
// unauthenticated surface. Loopback binds are unaffected.
func TestNonLoopbackBindRefusedWithoutAuth(t *testing.T) {
	s := newBindTestServer(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, addr := range []string{"0.0.0.0:8080", "10.0.0.5:8081", "192.168.1.20:8080", "[::]:8080"} {
		err := s.ListenAndServe(ctx, addr)
		if err == nil {
			t.Fatalf("non-loopback bind %q was ALLOWED without auth — the §4.6 fail-closed rule regressed", addr)
		}
		if !strings.Contains(err.Error(), "refusing to bind") {
			t.Errorf("bind %q refusal message = %q, want the actionable §4.6 message", addr, err.Error())
		}
	}
}

// TestNoSilentZeroZero specifically pins that a bare 0.0.0.0 bind is never
// silently accepted (the historical `--addr 0.0.0.0` unauthenticated-RCE
// hole). It also confirms a ready substrate + explicit allow-list re-permits a
// non-loopback bind, so the refusal is conditioned on the §4.6 predicate and
// not a blanket ban.
func TestNoSilentZeroZero(t *testing.T) {
	// No substrate → refused.
	if err := remoteExposureAllowed("0.0.0.0:8080", nil); err == nil {
		t.Fatal("0.0.0.0 bind permitted with a nil controller — silent bind-all regression")
	}
	// A non-ready controller is treated the same as none (fail closed).
	if err := remoteExposureAllowed("0.0.0.0:8080", readyController{ready: false}); err == nil {
		t.Fatal("0.0.0.0 bind permitted with a not-ready controller — the predicate must require Ready()")
	}
	// A ready controller re-permits it (Phase 2 will actually wire this).
	if err := remoteExposureAllowed("0.0.0.0:8080", readyController{ready: true}); err != nil {
		t.Fatalf("ready substrate should permit a non-loopback bind: %v", err)
	}
}

// TestLoopbackBindUnchanged is the behavioural-parity guard: every loopback
// form is permitted with no controller, so the operator's running daemon
// (127.0.0.1) is untouched by the hardening.
func TestLoopbackBindUnchanged(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "localhost:8081", "[::1]:8080", "127.0.0.2:9000"} {
		if err := remoteExposureAllowed(addr, nil); err != nil {
			t.Errorf("loopback bind %q was refused — the local single-user path must be unchanged: %v", addr, err)
		}
	}
	// A loopback bind must serve (start + immediate ctx-cancel returns nil).
	s := newBindTestServer(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if err := s.ListenAndServe(ctx, "127.0.0.1:0"); err != nil {
		t.Errorf("loopback ListenAndServe returned %v, want nil on ctx-cancel", err)
	}
}
