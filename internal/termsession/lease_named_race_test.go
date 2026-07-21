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

	var wins int
	var held int
	var winnerHolder string
	for res := range results {
		switch {
		case res.err == nil:
			wins++
			winnerHolder = res.lease.Holder()
		case errors.Is(res.err, ErrWriterHeld):
			held++
		default:
			t.Fatalf("AcquireWriterRemote error = %v, want nil or ErrWriterHeld", res.err)
		}
	}
	if wins != 1 {
		t.Fatalf("remote winners = %d, want exactly 1", wins)
	}
	if held != 1 {
		t.Fatalf("remote losers rejected with ErrWriterHeld = %d, want 1", held)
	}
	holder, ok := m.WriterHolder(handle)
	if !ok {
		t.Fatal("writer lease not held after winning remote claim")
	}
	if holder != winnerHolder {
		t.Fatalf("final holder = %q, want winning remote holder %q", holder, winnerHolder)
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
			if err == nil {
				<-localDone
				if _, writeErr := lease.Write([]byte("remote-after-local")); !errors.Is(writeErr, ErrNotWriter) {
					remoteResult <- result{lease: lease, err: writeErr}
					return
				}
			}
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
		switch {
		case remote.err == nil:
			select {
			case <-remote.lease.Revoked():
				if !remote.lease.RevokeIsTakeover() {
					t.Fatalf("iteration %d: remote lease revoked by %q, want local takeover", i, remote.lease.RevokeKind())
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("iteration %d: remote lease was not revoked by local takeover", i)
			}
		case errors.Is(remote.err, ErrHeldLocally):
		default:
			t.Fatalf("iteration %d: remote acquire/write error = %v, want nil or ErrHeldLocally", i, remote.err)
		}
		holder, ok := m.WriterHolder(handle)
		if !ok {
			t.Fatalf("iteration %d: final writer missing", i)
		}
		if holder != "local" {
			t.Fatalf("iteration %d: final holder = %q, want local", i, holder)
		}
		if _, err := m.AcquireWriterRemote(handle, realRemoteGrant(t, caps, handle, "device-late")); !errors.Is(err, ErrHeldLocally) {
			t.Fatalf("iteration %d: late remote acquire = %v, want ErrHeldLocally", i, err)
		}
		m.Shutdown()
	}
}
