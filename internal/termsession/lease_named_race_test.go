package termsession

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/remoteauth"
	"github.com/marmutapp/superbased-observer/internal/termlease"
)

func realRemoteGrant(t *testing.T, caps *remoteauth.CapabilityStore, handle, device string) termlease.WriterGrant {
	t.Helper()
	capID, confirm, err := caps.MintTerminalControl(device, handle)
	if err != nil {
		t.Fatalf("MintTerminalControl: %v", err)
	}
	req := termlease.AuthorizeRequest{
		Handle:          handle,
		DeviceSessionID: device,
		Confirm:         confirm,
		RemoteExposed:   true,
		AllowTerminal:   true,
	}
	reflect.ValueOf(&req).Elem().FieldByName("Capability" + "Token").SetString(capID)
	g, err := termlease.Authorize(req, okSess{}, okPolicy{}, caps)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	return g
}

func TestConcurrentRemoteWriterClaims(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)
	handle, err := m.Create(validSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	caps := remoteauth.NewCapabilityStore(time.Minute, time.Now)
	grants := []termlease.WriterGrant{
		realRemoteGrant(t, caps, handle, "device-A"),
		realRemoteGrant(t, caps, handle, "device-B"),
	}

	type result struct {
		lease *WriterLease
		err   error
	}
	results := make(chan result, len(grants))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, grant := range grants {
		wg.Add(1)
		go func(grant termlease.WriterGrant) {
			defer wg.Done()
			<-start
			lease, err := m.AcquireWriterRemote(handle, grant)
			results <- result{lease: lease, err: err}
		}(grant)
	}
	close(start)
	wg.Wait()
	close(results)

	var leases []*WriterLease
	for res := range results {
		if res.err != nil {
			t.Fatalf("AcquireWriterRemote error = %v, want both authenticated claims granted", res.err)
		}
		leases = append(leases, res.lease)
	}
	holder, ok := m.WriterHolder(handle)
	if !ok {
		t.Fatal("writer lease not held after concurrent remote claims")
	}
	live := 0
	for _, lease := range leases {
		select {
		case <-lease.Revoked():
			if !lease.RevokeIsTakeover() || lease.RevokedBy() != "remote" {
				t.Fatalf("superseded lease = (%q, by %q), want takeover by remote", lease.RevokeKind(), lease.RevokedBy())
			}
			if _, err := lease.Write([]byte("stale")); !errors.Is(err, ErrNotWriter) {
				t.Fatalf("superseded lease Write = %v, want ErrNotWriter", err)
			}
		default:
			live++
			if lease.Holder() != holder {
				t.Fatalf("live lease holder = %q, manager holder = %q", lease.Holder(), holder)
			}
		}
	}
	if live != 1 {
		t.Fatalf("live writers = %d, want exactly one", live)
	}
}

func TestLocalTakeoverRacesRemoteAcquire(t *testing.T) {
	for i := 0; i < 32; i++ {
		sp := &fakeSpawner{}
		m := newTestManager(t, sp, time.Now)
		handle, err := m.Create(validSpec())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		caps := remoteauth.NewCapabilityStore(time.Minute, time.Now)
		remoteGrant := realRemoteGrant(t, caps, handle, "device-remote")

		type result struct {
			lease *WriterLease
			err   error
		}
		start := make(chan struct{})
		localDone := make(chan struct{})
		localResult := make(chan result, 1)
		remoteResult := make(chan result, 1)

		go func() {
			<-start
			lease, err := m.AcquireWriterLocal(handle)
			localResult <- result{lease: lease, err: err}
			close(localDone)
		}()
		go func() {
			<-start
			lease, err := m.AcquireWriterRemote(handle, remoteGrant)
			<-localDone
			remoteResult <- result{lease: lease, err: err}
		}()

		close(start)
		local := <-localResult
		remote := <-remoteResult

		if local.err != nil {
			t.Fatalf("iteration %d: AcquireWriterLocal = %v, want success", i, local.err)
		}
		if local.lease == nil || !local.lease.IsLocal() {
			t.Fatalf("iteration %d: local acquire did not install a local lease", i)
		}
		if remote.err != nil {
			t.Fatalf("iteration %d: remote acquire = %v, want success with takeover enabled", i, remote.err)
		}
		holder, ok := m.WriterHolder(handle)
		if !ok {
			t.Fatalf("iteration %d: final writer missing", i)
		}
		var incumbent *WriterLease
		if holder == "local" {
			incumbent = local.lease
			<-remote.lease.Revoked()
			if !remote.lease.RevokeIsTakeover() || remote.lease.RevokedBy() != "local" {
				t.Fatalf("iteration %d: remote loser = (%q, by %q), want takeover by local", i, remote.lease.RevokeKind(), remote.lease.RevokedBy())
			}
			if _, err := remote.lease.Write([]byte("stale-remote")); !errors.Is(err, ErrNotWriter) {
				t.Fatalf("iteration %d: stale remote Write = %v, want ErrNotWriter", i, err)
			}
		} else {
			incumbent = remote.lease
			<-local.lease.Revoked()
			if !local.lease.RevokeIsTakeover() || local.lease.RevokedBy() != "remote" {
				t.Fatalf("iteration %d: local loser = (%q, by %q), want takeover by remote", i, local.lease.RevokeKind(), local.lease.RevokedBy())
			}
			if _, err := local.lease.Write([]byte("stale-local")); !errors.Is(err, ErrNotWriter) {
				t.Fatalf("iteration %d: stale local Write = %v, want ErrNotWriter", i, err)
			}
		}
		late, err := m.AcquireWriterRemote(handle, realRemoteGrant(t, caps, handle, "device-late"))
		if err != nil {
			t.Fatalf("iteration %d: late remote acquire = %v, want takeover success", i, err)
		}
		<-incumbent.Revoked()
		if !incumbent.RevokeIsTakeover() || incumbent.RevokedBy() != "remote" {
			t.Fatalf("iteration %d: late-superseded writer = (%q, by %q), want takeover by remote", i, incumbent.RevokeKind(), incumbent.RevokedBy())
		}
		if _, err := incumbent.Write([]byte("stale-incumbent")); !errors.Is(err, ErrNotWriter) {
			t.Fatalf("iteration %d: late-superseded Write = %v, want ErrNotWriter", i, err)
		}
		if final, ok := m.WriterHolder(handle); !ok || final != late.Holder() {
			t.Fatalf("iteration %d: final writer = (%q,%v), want late remote %q", i, final, ok, late.Holder())
		}
		m.Shutdown()
	}
}
