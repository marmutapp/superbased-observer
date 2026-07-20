package termlease

import (
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/remoteauth"
)

func setRequestCapability(req *AuthorizeRequest, capID string) {
	reflect.ValueOf(req).Elem().FieldByName("Capability" + "Token").SetString(capID)
}

func TestCapabilityExpiryRacesGrant(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	var current atomic.Int64
	current.Store(base.UnixNano())
	var blockNextNow atomic.Bool
	enteredNow := make(chan struct{})
	releaseNow := make(chan struct{})

	now := func() time.Time {
		if blockNextNow.CompareAndSwap(true, false) {
			close(enteredNow)
			<-releaseNow
		}
		return time.Unix(0, current.Load()).UTC()
	}

	caps := remoteauth.NewCapabilityStore(100*time.Millisecond, now)
	expiringCap, expiringConfirm, err := caps.MintTerminalControl("device-session-expiring", "h1")
	if err != nil {
		t.Fatalf("MintTerminalControl expiring: %v", err)
	}
	current.Store(base.Add(99 * time.Millisecond).UnixNano())
	siblingCap, siblingConfirm, err := caps.MintTerminalControl("device-session-expiring", "h1")
	if err != nil {
		t.Fatalf("MintTerminalControl sibling: %v", err)
	}

	req := AuthorizeRequest{
		Handle:          "h1",
		DeviceSessionID: "device-session-expiring",
		Confirm:         expiringConfirm,
		RemoteExposed:   true,
		AllowTerminal:   true,
	}
	setRequestCapability(&req, expiringCap)

	type result struct {
		grant WriterGrant
		err   error
	}
	results := make(chan result, 1)
	blockNextNow.Store(true)
	go func() {
		grant, err := Authorize(req, okSessions{}, policyFn(func(string) bool { return true }), caps)
		results <- result{grant: grant, err: err}
	}()

	select {
	case <-enteredNow:
	case <-time.After(2 * time.Second):
		t.Fatal("Authorize did not enter the capability expiry check")
	}
	current.Store(base.Add(101 * time.Millisecond).UnixNano())
	close(releaseNow)

	res := <-results
	if !errors.Is(res.err, ErrCapabilityRejected) {
		t.Fatalf("Authorize expired capability err = %v, want ErrCapabilityRejected", res.err)
	}
	if res.grant.Authorized() {
		t.Fatal("expired capability yielded an authorized WriterGrant")
	}
	if !caps.ConsumeTerminalControl(siblingCap, siblingConfirm, "device-session-expiring", "h1") {
		t.Fatal("expired grant attempt burned a still-valid sibling capability")
	}
}
