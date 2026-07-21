package main

import (
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/termlease"
	"github.com/marmutapp/superbased-observer/internal/termsession"
)

// reclaimLivePTY is a PTY stub that stays alive until Kill (Read/Wait block), so
// a session's writer leases stay live for the reclaim test.
type reclaimLivePTY struct {
	done     chan struct{}
	killOnce sync.Once
}

func (p *reclaimLivePTY) Read([]byte) (int, error)    { <-p.done; return 0, io.EOF }
func (p *reclaimLivePTY) Write(b []byte) (int, error) { return len(b), nil }
func (p *reclaimLivePTY) Resize(uint16, uint16) error { return nil }
func (p *reclaimLivePTY) Wait() (int, error)          { <-p.done; return 0, nil }
func (p *reclaimLivePTY) Kill() error                 { p.killOnce.Do(func() { close(p.done) }); return nil }
func (p *reclaimLivePTY) Close() error                { return nil }

type reclaimLiveSpawner struct{}

func (reclaimLiveSpawner) Spawn(termsession.Spec) (termsession.PTY, error) {
	return &reclaimLivePTY{done: make(chan struct{})}, nil
}

// The §4.δ conjunction stubs a standing grant needs (all admit). The standing
// verifier ignores the credential, so the AuthorizeRequest carries none.
type okReclaimSess struct{}

func (okReclaimSess) Validate(string) error { return nil }

type okReclaimPolicy struct{}

func (okReclaimPolicy) Allowed(string) bool { return true }

type okReclaimStanding struct{}

func (okReclaimStanding) VerifyStandingTerminalControl(cred, dev, handle string) bool { return true }

// standingGrantForReclaim mints a standing WriterGrant through the real
// AuthorizeStanding path so its provenance (Standing()==true) is genuine.
func standingGrantForReclaim(t *testing.T, handle string) termlease.WriterGrant {
	t.Helper()
	req := termlease.AuthorizeRequest{
		Handle:          handle,
		DeviceSessionID: "device-standing",
		RemoteExposed:   true,
		AllowTerminal:   true,
	}
	g, err := termlease.AuthorizeStanding(req, okReclaimSess{}, okReclaimPolicy{}, okReclaimStanding{})
	if err != nil {
		t.Fatalf("AuthorizeStanding: %v", err)
	}
	if !g.Standing() {
		t.Fatal("grant must report Standing()==true")
	}
	return g
}

// TestReclaimSupersedesStandingRemoteFiresHook proves Feature 1's reclaim routes
// through the SAME AcquireWriterLocal funnel the dashboard uses, so a reclaim that
// supersedes a STANDING remote writer still fires SetOnStandingLocalTakeover —
// the opt-in revoke-standing-on-takeover policy keeps working under reclaim.
func TestReclaimSupersedesStandingRemoteFiresHook(t *testing.T) {
	mgr := termsession.NewManager(termsession.Options{
		Spawner:      reclaimLiveSpawner{},
		ReapInterval: time.Hour,
		Now:          time.Now,
	})
	t.Cleanup(mgr.Shutdown)

	fired := make(chan [2]string, 1)
	mgr.SetOnStandingLocalTakeover(func(handle, revokedHolder string) {
		fired <- [2]string{handle, revokedHolder}
	})

	handle, err := mgr.Create(termsession.Spec{BinPath: "observer", Subcommand: "claude", SessionID: "s1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	grant := standingGrantForReclaim(t, handle)
	remoteLease, err := mgr.AcquireWriterRemote(handle, grant)
	if err != nil {
		t.Fatalf("AcquireWriterRemote(standing): %v", err)
	}

	// The reclaim path: reacquire == AcquireWriterLocal (the exact wiring
	// LaunchAttachable installs when reclaim_on_input is on).
	sess := &attachSession{
		handle: handle,
		lease:  remoteLease,
		reacquire: func() (attachLease, error) {
			l, aerr := mgr.AcquireWriterLocal(handle)
			if aerr != nil {
				return nil, aerr
			}
			return l, nil
		},
	}
	if err := sess.ReclaimWriter(); err != nil {
		t.Fatalf("ReclaimWriter: %v", err)
	}

	select {
	case got := <-fired:
		if got[0] != handle {
			t.Errorf("hook handle = %q, want %q", got[0], handle)
		}
		if got[1] != grant.HolderKey() {
			t.Errorf("hook revokedHolder = %q, want the superseded standing holder key %q", got[1], grant.HolderKey())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("standing-takeover hook did not fire when reclaim superseded the standing remote writer")
	}
}

// TestReclaimDisabledReturnsSentinel pins the disabled path: a nil reacquire hook
// (reclaim_on_input=false) makes ReclaimWriter report errReclaimDisabled, so the
// server keeps the fence-and-notify behavior.
func TestReclaimDisabledReturnsSentinel(t *testing.T) {
	sess := &attachSession{handle: "h"}
	if err := sess.ReclaimWriter(); !errors.Is(err, errReclaimDisabled) {
		t.Fatalf("ReclaimWriter with nil reacquire = %v, want errReclaimDisabled", err)
	}
}
