package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/intelligence/dashboard"
	"github.com/marmutapp/superbased-observer/internal/remoteauth"
	"github.com/marmutapp/superbased-observer/internal/termfeed"
	"github.com/marmutapp/superbased-observer/internal/termlease"
	"github.com/marmutapp/superbased-observer/internal/termsession"
	"github.com/marmutapp/superbased-observer/internal/termsvc"
)

// standingStubAuthz is a TerminalControlAuthorizer + StandingTerminalVerifier
// stub whose standing verify outcome and generation are test-controlled — it
// lets the finding-1 TOCTOU (a revoke landing DURING the slow argon2 verify) be
// reproduced deterministically: bumpDuringVerify simulates the admin transition
// firing mid-verify, after the verify snapshot but before the lease install.
type standingStubAuthz struct {
	gen         atomic.Uint64
	verifyOK    bool
	consumeOK   bool
	verifyCalls atomic.Int32
	// bumpDuringVerify simulates a secret revoke/rotate landing mid-verify
	// (generation moves while argon2 grinds). invalidateSessionDuring* simulate
	// a device revoke/logout/rotate landing mid-verify/consume WITHOUT bumping
	// the generation — the finding-1 SESSION-lifecycle residual.
	bumpDuringVerify              bool
	invalidateSessionDuringVerify bool
	invalidateSessionDuringCons   bool
	sessionValid                  atomic.Bool
	// allowTerm is the LIVE allow_terminal gate the acquire path reads (via the
	// gate closure wired into wireRemoteExecute). flipAllowTermDuringVerify /
	// flipAllowTermDuringCons simulate an allow_terminal→false flip (+ its
	// RevokeAllRemoteWriters sweep) landing DURING the credential leg — after the
	// gate read but before the lease install — the finding-3 install-time race:
	// the sweep misses the not-yet-installed lease and only the recheck's LIVE
	// gate re-read catches it.
	allowTerm                 atomic.Bool
	flipAllowTermDuringVerify bool
	flipAllowTermDuringCons   bool
}

func (s *standingStubAuthz) Validate(string) error {
	if s.sessionValid.Load() {
		return nil
	}
	return errors.New("no such session")
}

func (s *standingStubAuthz) ConsumeTerminalControl(string, string, string, string) bool {
	if s.invalidateSessionDuringCons {
		s.sessionValid.Store(false) // device revoked while the (fast) consume ran
	}
	if s.flipAllowTermDuringCons {
		s.allowTerm.Store(false) // allow_terminal→false lands while the consume ran
	}
	return s.consumeOK
}

func (s *standingStubAuthz) MintTerminalControl(string, string) (string, string, error) {
	return "", "", errors.New("not used")
}

func (s *standingStubAuthz) VerifyStandingTerminalControl(string, string, string) bool {
	s.verifyCalls.Add(1)
	if s.bumpDuringVerify {
		// Secret revoke/rotate races the in-flight verify: the generation moves
		// while argon2 is still grinding (the writer-kill sweep, which runs
		// AFTER the bump, sees no lease to kill yet).
		s.gen.Add(1)
	}
	if s.invalidateSessionDuringVerify {
		// Device revoke/logout/rotate races the verify — the SESSION dies but
		// the standing generation is UNCHANGED (session lifecycle ≠ secret
		// lifecycle). Only a session re-validation at install time catches this.
		s.sessionValid.Store(false)
	}
	if s.flipAllowTermDuringVerify {
		// allow_terminal→false races the verify — neither the secret generation
		// nor the session moves; only the recheck's LIVE allow_terminal re-read
		// catches this (finding-3 install-time residual).
		s.allowTerm.Store(false)
	}
	return s.verifyOK
}

func (s *standingStubAuthz) StandingTerminalGeneration() uint64 { return s.gen.Load() }

// newStandingStub returns a stub whose session starts VALID (the acquire is
// otherwise refused at the session-validate leg).
func newStandingStub() *standingStubAuthz {
	s := &standingStubAuthz{}
	s.sessionValid.Store(true)
	s.allowTerm.Store(true) // the acquire is otherwise refused at the allow_terminal gate
	return s
}

func requireControlDenial(t *testing.T, err error, want dashboard.ControlDenialReason) {
	t.Helper()
	var denial *dashboard.ControlDeniedError
	if !errors.As(err, &denial) {
		t.Fatalf("want *dashboard.ControlDeniedError with reason %q, got %T: %v", want, err, err)
	}
	if denial.Reason != want {
		t.Fatalf("control denial reason = %q, want %q", denial.Reason, want)
	}
}

// newStandingExecAdapter assembles the process-free adapter + a live
// termsvc-tracked handle (the same shape as newExecuteHarness) wired to the
// given standing stub.
func newStandingExecAdapter(t *testing.T, stub dashboard.TerminalControlAuthorizer) (*launchManagerAdapter, *termsession.Manager, string) {
	t.Helper()
	return newStandingExecAdapterGate(t, stub, func() bool { return true })
}

// newStandingExecAdapterGate is newStandingExecAdapter with a caller-supplied
// LIVE allow_terminal gate closure, so a test can flip allow_terminal mid-verify
// (the finding-3 install-time race) rather than pin it true.
func newStandingExecAdapterGate(t *testing.T, stub dashboard.TerminalControlAuthorizer, allowTerminal func() bool) (*launchManagerAdapter, *termsession.Manager, string) {
	t.Helper()
	mgr := termsession.NewManager(termsession.Options{Spawner: fakeSpawner{}})
	t.Cleanup(mgr.Shutdown)
	svc := termsvc.New(termsvc.Options{
		Recorder: nopRunRecorder{},
		Launcher: mgrLauncher{mgr: mgr},
		Feed:     termfeed.New(termfeed.Options{}),
	})
	res, err := svc.LaunchHandoff(context.Background(), termsvc.HandoffRequest{
		Tool: "claude-code", Subcommand: "claude", SessionID: "src-session",
	})
	if err != nil {
		t.Fatalf("LaunchHandoff: %v", err)
	}
	adapter := &launchManagerAdapter{svc: svc, mgr: mgr}
	adapter.wireRemoteExecute(stub, allowTerminal)
	return adapter, mgr, res.Handle
}

// standingReq builds a remote acquire request whose credential is a standing
// secret (the `standing.` prefix routes it to AuthorizeStanding).
func standingReq(handle string) dashboard.RemoteWriterRequest {
	r := dashboard.RemoteWriterRequest{
		Handle:          handle,
		DeviceSessionID: "device-session-raw",
		RemoteExposed:   true,
	}
	r.CapabilityToken = "standing.test-secret-value"
	return r
}

// TestStandingSecretGrantsWriterViaAdapter: the standing path grants a real
// writer lease through the SAME manager boundary as the single-use path.
func TestStandingSecretGrantsWriterViaAdapter(t *testing.T) {
	stub := newStandingStub()
	stub.verifyOK = true
	adapter, mgr, handle := newStandingExecAdapter(t, stub)
	w, err := adapter.AcquireWriterRemote(standingReq(handle))
	if err != nil || w == nil {
		t.Fatalf("standing acquire want grant, got writer=%v err=%v", w, err)
	}
	if _, held := mgr.WriterHolder(handle); !held {
		t.Fatal("no writer lease after a granted standing acquire")
	}
	if stub.verifyCalls.Load() != 1 {
		t.Fatalf("verify calls = %d, want 1 (one attempt per acquire)", stub.verifyCalls.Load())
	}
}

// TestStandingRejectedNoWriter: a refused standing verify denies with the
// typed auth reason and installs nothing.
func TestStandingRejectedNoWriter(t *testing.T) {
	stub := newStandingStub()
	adapter, mgr, handle := newStandingExecAdapter(t, stub)
	_, err := adapter.AcquireWriterRemote(standingReq(handle))
	requireControlDenial(t, err, dashboard.ControlDenialAuth)
	if _, held := mgr.WriterHolder(handle); held {
		t.Fatal("a refused standing acquire left a writer lease installed")
	}
}

// TestStandingRevokeDuringVerifyCannotInstallWriter reproduces the finding-1
// TOCTOU: the standing verify passes against the pre-revoke state, but the
// revoke bumped the standing generation while the verify was in flight (before
// the lease install). The install-time generation recheck must tear the lease
// down — the acquire fails and NO writer survives, even though the kill sweep
// ran before the lease existed.
func TestStandingRevokeDuringVerifyCannotInstallWriter(t *testing.T) {
	stub := newStandingStub()
	stub.verifyOK = true
	stub.bumpDuringVerify = true
	adapter, mgr, handle := newStandingExecAdapter(t, stub)
	_, err := adapter.AcquireWriterRemote(standingReq(handle))
	requireControlDenial(t, err, dashboard.ControlDenialAuth)
	if holder, held := mgr.WriterHolder(handle); held {
		t.Fatalf("a verify that raced a revoke installed a SURVIVING writer (holder %q) — the TOCTOU is open", holder)
	}
}

// TestStandingRealControllerAcquireAndRevoke drives the standing path through
// the REAL remote controller end-to-end: a paired device presents the encoded
// standing secret, acquires a writer, and after a live revoke-style reload
// (generation bump + hash clear, exactly what the dashboard revoke handler does
// FIRST) a fresh acquire with the same secret is refused.
func TestStandingRealControllerAcquireAndRevoke(t *testing.T) {
	// Real controller with a known standing secret + a paired device.
	rawStanding, encStanding, err := remoteauth.GenerateStandingSecret()
	if err != nil {
		t.Fatalf("GenerateStandingSecret: %v", err)
	}
	standingHash, err := remoteauth.HashSecret(rawStanding)
	if err != nil {
		t.Fatalf("HashSecret: %v", err)
	}
	rawPair, encPair, err := remoteauth.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	pairHash, err := remoteauth.HashSecret(rawPair)
	if err != nil {
		t.Fatalf("HashSecret: %v", err)
	}
	rc := dashboard.NewRemoteController(dashboard.RemoteOptions{
		HashedSecret:               pairHash,
		AllowedHosts:               []string{"remote.example:8443"},
		RateLimitPerMin:            60,
		StandingTerminalSecretHash: standingHash,
		StandingTerminalEnabled:    true,
		Session:                    remoteauth.SessionParams{TTL: time.Hour, Idle: time.Hour, Max: 5},
	})
	device := pairDevice(t, rc, encPair)

	authz, ok := rc.(dashboard.TerminalControlAuthorizer)
	if !ok {
		t.Fatal("controller lacks TerminalControlAuthorizer")
	}
	adapter, mgr, handle := newStandingExecAdapter(t, authz)

	req := dashboard.RemoteWriterRequest{Handle: handle, DeviceSessionID: device, RemoteExposed: true}
	req.CapabilityToken = encStanding
	w, err := adapter.AcquireWriterRemote(req)
	if err != nil || w == nil {
		t.Fatalf("real-controller standing acquire want grant, got %v / %v", w, err)
	}
	w.Release()
	mgr.RevokeWriter(handle, "test-reset")

	// Live revoke: hash cleared + generation bumped (the dashboard revoke
	// handler's FIRST step). The same secret must now be refused.
	rl, ok := rc.(interface{ ReloadStandingTerminalSecret(string, bool) })
	if !ok {
		t.Fatal("controller lacks ReloadStandingTerminalSecret")
	}
	rl.ReloadStandingTerminalSecret("", false)
	_, err = adapter.AcquireWriterRemote(req)
	requireControlDenial(t, err, dashboard.ControlDenialAuth)
	if _, held := mgr.WriterHolder(handle); held {
		t.Fatal("post-revoke standing acquire left a writer lease")
	}
}

// TestStandingSessionRevokeDuringVerifyCannotInstallWriter reproduces the
// finding-1 SESSION-lifecycle residual: the standing verify passes and the
// standing GENERATION is UNCHANGED, but the device SESSION was revoked while
// argon2 ran. The generation fence alone would miss this; the install-time
// SESSION re-validation must tear the just-installed lease down.
func TestStandingSessionRevokeDuringVerifyCannotInstallWriter(t *testing.T) {
	stub := newStandingStub()
	stub.verifyOK = true
	stub.invalidateSessionDuringVerify = true // session dies mid-verify; gen unchanged
	adapter, mgr, handle := newStandingExecAdapter(t, stub)
	_, err := adapter.AcquireWriterRemote(standingReq(handle))
	requireControlDenial(t, err, dashboard.ControlDenialSessionInvalid)
	if holder, held := mgr.WriterHolder(handle); held {
		t.Fatalf("a verify that raced a SESSION revoke installed a surviving writer (holder %q)", holder)
	}
	if stub.gen.Load() != 0 {
		t.Fatalf("this test must exercise the session leg, not the generation leg (gen moved to %d)", stub.gen.Load())
	}
}

// TestSingleUseSessionRevokeDuringConsumeCannotInstallWriter reproduces the same
// residual for the SINGLE-USE path: a capability consume succeeds but the device
// session is revoked in the same window; the install-time session re-validation
// must reject, so no writer survives here either.
func TestSingleUseSessionRevokeDuringConsumeCannotInstallWriter(t *testing.T) {
	stub := newStandingStub()
	stub.consumeOK = true
	stub.invalidateSessionDuringCons = true
	adapter, mgr, handle := newStandingExecAdapter(t, stub)
	// A one-time credential (NOT a standing secret — no prefix) routes to the
	// single-use Authorize path.
	req := dashboard.RemoteWriterRequest{Handle: handle, DeviceSessionID: "dev", RemoteExposed: true}
	req.CapabilityToken = "cap-" + "onetime"
	req.Confirm = "conf-" + "irm"
	_, err := adapter.AcquireWriterRemote(req)
	requireControlDenial(t, err, dashboard.ControlDenialSessionInvalid)
	if _, held := mgr.WriterHolder(handle); held {
		t.Fatal("single-use acquire that raced a session revoke left a surviving writer")
	}
}

// TestLiveAllowTerminalRefusesSingleUseAfterFlip reproduces the finding-3
// residual: wired via wireRemoteExecuteTier (the PRODUCTION assembly, which now
// reads allow_terminal LIVE from the controller), a single-use acquire that
// would grant is REFUSED once the live allow_terminal gate is flipped off —
// without a restart. Proves allow_terminal=false stops the single-use path too.
func TestLiveAllowTerminalRefusesSingleUseAfterFlip(t *testing.T) {
	h := newExecuteHarness(t)
	// Re-wire through the PRODUCTION tier so allow_terminal is read from the
	// live controller (h.rc), not the harness's own atomic override.
	wireRemoteExecuteTier(config.Config{}, h.adapter, h.rc)

	rl, ok := h.rc.(interface{ ReloadAllowTerminal(bool) })
	if !ok {
		t.Fatal("controller lacks ReloadAllowTerminal")
	}
	rl.ReloadAllowTerminal(true) // live ON
	if w, err := h.adapter.AcquireWriterRemote(h.validReq(t)); err != nil || w == nil {
		t.Fatalf("with live allow_terminal ON, single-use acquire must grant: %v", err)
	}
	h.resetWriter()

	rl.ReloadAllowTerminal(false) // live OFF — no restart
	if _, err := h.adapter.AcquireWriterRemote(h.validReq(t)); !errors.Is(err, termlease.ErrTerminalDisabled) {
		t.Fatalf("with live allow_terminal OFF, single-use acquire must be refused ErrTerminalDisabled, got %v", err)
	}
	if _, held := h.mgr.WriterHolder(h.handle); held {
		t.Fatal("single-use acquire granted after a live allow_terminal→false flip")
	}
}

// TestStandingAllowTerminalFlipDuringVerifyCannotInstallWriter reproduces the
// finding-3 INSTALL-TIME race for the STANDING path: allow_terminal is true at
// gate-read (so the conjunction passes and the lease installs), but flips false
// DURING the slow argon2 verify — after the gate read, and its
// RevokeAllRemoteWriters sweep completes before this not-yet-installed lease
// exists. The secret generation and the session are BOTH unchanged, so only the
// recheck's LIVE allow_terminal re-read can tear the lease down. Without the
// gate in the recheck this would leave a SURVIVING writer past the disable.
func TestStandingAllowTerminalFlipDuringVerifyCannotInstallWriter(t *testing.T) {
	stub := newStandingStub()
	stub.verifyOK = true
	stub.flipAllowTermDuringVerify = true // allow_terminal→false mid-verify; gen + session unchanged
	adapter, mgr, handle := newStandingExecAdapterGate(t, stub, stub.allowTerm.Load)
	_, err := adapter.AcquireWriterRemote(standingReq(handle))
	requireControlDenial(t, err, dashboard.ControlDenialTerminalDisabled)
	if holder, held := mgr.WriterHolder(handle); held {
		t.Fatalf("a standing verify that raced an allow_terminal→false flip installed a SURVIVING writer (holder %q) — the finding-3 install race is open", holder)
	}
	if stub.gen.Load() != 0 {
		t.Fatalf("this test must exercise the allow_terminal leg, not the generation leg (gen moved to %d)", stub.gen.Load())
	}
	if !stub.sessionValid.Load() {
		t.Fatal("this test must exercise the allow_terminal leg, not the session leg (session was invalidated)")
	}
}

// TestSingleUseAllowTerminalFlipDuringConsumeCannotInstallWriter reproduces the
// same finding-3 install-time race for the SINGLE-USE path: the capability
// consume succeeds but allow_terminal flips false in the same window (after the
// gate read, before the lease install). The install-time recheck's LIVE gate
// re-read must reject, so no writer survives here either.
func TestSingleUseAllowTerminalFlipDuringConsumeCannotInstallWriter(t *testing.T) {
	stub := newStandingStub()
	stub.consumeOK = true
	stub.flipAllowTermDuringCons = true
	adapter, mgr, handle := newStandingExecAdapterGate(t, stub, stub.allowTerm.Load)
	// A one-time credential (NOT a standing secret — no prefix) routes to the
	// single-use Authorize path.
	req := dashboard.RemoteWriterRequest{Handle: handle, DeviceSessionID: "dev", RemoteExposed: true}
	req.CapabilityToken = "one" + "time" // any non-standing credential; consumeOK ignores its value
	req.Confirm = "conf" + "irm"
	_, err := adapter.AcquireWriterRemote(req)
	requireControlDenial(t, err, dashboard.ControlDenialTerminalDisabled)
	if _, held := mgr.WriterHolder(handle); held {
		t.Fatal("single-use acquire that raced an allow_terminal→false flip left a surviving writer")
	}
}
