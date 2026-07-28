package termsession

import (
	"errors"
	"testing"
	"time"
)

// TestWriterLeaseReapMatrix is the table-driven pin for the §4.α.2c lifetime
// sweep after the 2026-07-25 mobile terminal-continuity arc widened it.
//
// The rule set the reaper now implements:
//
//	holder / provenance      idle expiry?   hard cap?
//	local                    no             no
//	remote, single-use cap   YES            YES
//	remote, standing secret  no             YES
//
// The standing-idle exemption is the change: a standing secret is a reusable,
// non-expiring credential whose holder silently re-acquires on every fresh
// socket, so idle-expiring the lease it backs reduced no authority — it only
// tore down the remote websocket underneath a user who had walked away for five
// minutes. The HARD CAP deliberately still applies to it: that is the periodic
// re-run of the whole §4.δ conjunction an operator revoke relies on.
func TestWriterLeaseReapMatrix(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	const (
		idle = 5 * time.Minute
		max  = 30 * time.Minute
	)

	tests := []struct {
		name string
		// standing/local select which acquire path mints the lease.
		local    bool
		standing bool
		// advance is how far the clock moves before the reap tick, with no
		// intervening write (so both clocks age together unless noted).
		advance     time.Duration
		wantRevoked bool
	}{
		{name: "local lease past idle and hard cap", local: true, advance: 24 * time.Hour, wantRevoked: false},
		{name: "single-use remote lease past idle", advance: 6 * time.Minute, wantRevoked: true},
		{name: "single-use remote lease inside idle", advance: 4 * time.Minute, wantRevoked: false},
		{name: "standing remote lease past idle", standing: true, advance: 6 * time.Minute, wantRevoked: false},
		{name: "standing remote lease far past idle", standing: true, advance: 25 * time.Minute, wantRevoked: false},
		{name: "standing remote lease past hard cap", standing: true, advance: 31 * time.Minute, wantRevoked: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nowP := &atomicTime{}
			nowP.set(base)
			m := NewManager(Options{
				Spawner:         &fakeSpawner{},
				ReapInterval:    time.Hour, // manual reapOnce only
				WriterLeaseIdle: idle,
				WriterLeaseMax:  max,
				Now:             nowP.get,
			})
			t.Cleanup(m.Shutdown)
			tok, err := m.Create(validSpec())
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			var l *WriterLease
			switch {
			case tc.local:
				l, err = m.AcquireWriterLocal(tok)
			case tc.standing:
				l, err = m.AcquireWriterRemote(tok, standingGrant(t, tok, "device-s"))
			default:
				l, err = m.AcquireWriterRemote(tok, remoteGrant(t, tok, "device-x"))
			}
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}

			nowP.set(base.Add(tc.advance))
			m.reapOnce(nowP.get())

			revoked := false
			select {
			case <-l.Revoked():
				revoked = true
			case <-time.After(time.Second):
			}
			if revoked != tc.wantRevoked {
				t.Fatalf("lease revoked = %v, want %v", revoked, tc.wantRevoked)
			}
			if !tc.wantRevoked {
				if _, werr := l.Write([]byte("x")); werr != nil {
					t.Fatalf("surviving lease Write = %v, want success", werr)
				}
				return
			}
			if _, werr := l.Write([]byte("x")); !errors.Is(werr, ErrNotWriter) {
				t.Fatalf("expired-lease Write = %v, want ErrNotWriter", werr)
			}
		})
	}
}

// TestWriterLeaseExpiryIsClassifiedAsExpiry pins that the reaper marks an
// aged-out lease LeaseExpired (RevokeIsExpiry) rather than LeaseRevoked
// (RevokeIsTakeover=false, RevokeIsExpiry=false). The remote websocket bridge
// branches on exactly this: an expiry DEMOTES the client to a read-only viewer
// so it can silently re-acquire, whereas a trust-withdrawing revoke closes the
// socket. Mis-classifying expiry as revoke is what closed a remote terminal
// after 30 minutes and forced the user to re-establish it.
func TestWriterLeaseExpiryIsClassifiedAsExpiry(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	nowP := &atomicTime{}
	nowP.set(base)
	m := NewManager(Options{
		Spawner:         &fakeSpawner{},
		ReapInterval:    time.Hour,
		WriterLeaseIdle: 5 * time.Minute,
		WriterLeaseMax:  30 * time.Minute,
		Now:             nowP.get,
	})
	t.Cleanup(m.Shutdown)
	tok, err := m.Create(validSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	l, err := m.AcquireWriterRemote(tok, remoteGrant(t, tok, "device-x"))
	if err != nil {
		t.Fatalf("AcquireWriterRemote: %v", err)
	}

	nowP.set(base.Add(6 * time.Minute))
	m.reapOnce(nowP.get())
	select {
	case <-l.Revoked():
	case <-time.After(2 * time.Second):
		t.Fatal("idle remote writer lease not revoked by reaper")
	}
	if got := l.RevokeKind(); got != LeaseExpired {
		t.Fatalf("RevokeKind = %q, want %q", got, LeaseExpired)
	}
	if !l.RevokeIsExpiry() {
		t.Fatal("RevokeIsExpiry = false for an aged-out lease")
	}
	if l.RevokeIsTakeover() {
		t.Fatal("an expiry must not masquerade as a takeover (it would corrupt the audit taxonomy)")
	}
}

// TestExplicitRevokeIsNotClassifiedAsExpiry is the companion negative: an
// operator/admin revoke keeps reporting LeaseRevoked, so the bridge still closes
// the socket for a device that lost trust. The expiry carve-out must not leak
// into the security-relevant revocation paths.
func TestExplicitRevokeIsNotClassifiedAsExpiry(t *testing.T) {
	m := newTestManager(t, &fakeSpawner{}, time.Now)
	tok, err := m.Create(validSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	l, err := m.AcquireWriterRemote(tok, remoteGrant(t, tok, "device-x"))
	if err != nil {
		t.Fatalf("AcquireWriterRemote: %v", err)
	}
	if n := m.RevokeAllRemoteWriters("admin disable"); n != 1 {
		t.Fatalf("RevokeAllRemoteWriters = %d, want 1", n)
	}
	select {
	case <-l.Revoked():
	case <-time.After(2 * time.Second):
		t.Fatal("admin revoke did not terminate the remote lease")
	}
	if l.RevokeIsExpiry() {
		t.Fatal("an admin revoke must NOT be classified as an expiry — the socket must still close")
	}
	if got := l.RevokeKind(); got != LeaseRevoked {
		t.Fatalf("RevokeKind = %q, want %q", got, LeaseRevoked)
	}
}
