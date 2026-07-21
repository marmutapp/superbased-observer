package termlease

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// hashPrefix8 mirrors termlease.fingerprint: the first 8 hex chars of the
// sha256 of a raw device-session id. Kept in the test so a regression in the
// production helper is caught rather than mirrored.
func hashPrefix8(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])[:8]
}

func TestDecidePolicyTable(t *testing.T) {
	tests := []struct {
		name       string
		req        Requester
		current    HolderKind
		wantGrant  bool
		wantRevoke bool
	}{
		{"local acquire, none held", RequesterLocal, HolderNone, true, false},
		{"local re-acquire self", RequesterLocal, HolderLocal, true, true},
		{"local takeover of remote", RequesterLocal, HolderRemote, true, true},
		{"remote acquire, none held", RequesterRemote, HolderNone, true, false},
		{"remote refused while local holds", RequesterRemote, HolderLocal, false, false},
		{"remote refused while remote holds", RequesterRemote, HolderRemote, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := Decide(tc.req, tc.current)
			if out.Granted() != tc.wantGrant {
				t.Fatalf("Granted()=%v want %v (reason=%q)", out.Granted(), tc.wantGrant, out.Reason)
			}
			if out.RevokeCurrent != tc.wantRevoke {
				t.Fatalf("RevokeCurrent=%v want %v", out.RevokeCurrent, tc.wantRevoke)
			}
		})
	}
}

func TestDecideLocalNeverRefused(t *testing.T) {
	for _, h := range []HolderKind{HolderNone, HolderLocal, HolderRemote} {
		if !Decide(RequesterLocal, h).Granted() {
			t.Fatalf("local acquire refused with current=%v — local must never be refused", h)
		}
	}
}

// --- Authorize conjunction test doubles ---

type okSessions struct{ err error }

func (o okSessions) Validate(string) error { return o.err }

type policyFn func(string) bool

func (p policyFn) Allowed(h string) bool { return p(h) }

type capFn func(token, confirm, dev, handle string) bool

func (c capFn) ConsumeTerminalControl(token, confirm, dev, handle string) bool {
	return c(token, confirm, dev, handle)
}

func validReq() AuthorizeRequest {
	return AuthorizeRequest{
		Handle:          "h1",
		DeviceSessionID: "device-session-abcdef012345",
		CapabilityToken: "cap",
		Confirm:         "conf",
		RemoteExposed:   true,
		AllowTerminal:   true,
	}
}

func allowAll() (SessionValidator, LaunchPolicy, CapabilityConsumer) {
	return okSessions{}, policyFn(func(string) bool { return true }), capFn(func(_, _, _, _ string) bool { return true })
}

func TestAuthorizeHappyPath(t *testing.T) {
	s, p, c := allowAll()
	g, err := Authorize(validReq(), s, p, c)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !g.Authorized() {
		t.Fatal("grant not authorized on happy path")
	}
	if g.Handle() != "h1" {
		t.Fatalf("handle=%q", g.Handle())
	}
	// The holder is sha256(DeviceSessionID)[:8] (a hash prefix), NEVER a prefix
	// of the raw session id — it must match the dashboard's deviceFingerprint.
	rawID := "device-session-abcdef012345"
	wantFP := hashPrefix8(rawID)
	if g.Holder() != wantFP {
		t.Fatalf("holder fingerprint=%q, want sha256 hash prefix %q (never the raw id)", g.Holder(), wantFP)
	}
	if strings.HasPrefix(rawID, g.Holder()) {
		t.Fatalf("holder %q is a prefix of the RAW session id — the fingerprint must be a hash", g.Holder())
	}
}

func TestZeroGrantNeverAuthorized(t *testing.T) {
	var g WriterGrant
	if g.Authorized() {
		t.Fatal("zero-value WriterGrant must never be authorized")
	}
}

// TestAuthorizeConjunctionLegs exercises each leg of the §4.δ conjunction and
// asserts the distinct fail-closed sentinel, plus that the capability is
// consumed ONLY when every earlier leg passed (a failed earlier leg burns
// nothing).
func TestAuthorizeConjunctionLegs(t *testing.T) {
	tests := []struct {
		name        string
		mut         func(*AuthorizeRequest)
		sessErr     error
		policyOK    bool
		capOK       bool
		wantErr     error
		wantConsume bool
	}{
		{"not remote exposed", func(r *AuthorizeRequest) { r.RemoteExposed = false }, nil, true, true, ErrNotRemoteExposed, false},
		{"allow_terminal false", func(r *AuthorizeRequest) { r.AllowTerminal = false }, nil, true, true, ErrTerminalDisabled, false},
		{"bad device session", nil, errors.New("no session"), true, true, ErrNoDeviceSession, false},
		{"policy denied", nil, nil, false, true, ErrPolicyDenied, false},
		{"capability rejected", nil, nil, true, false, ErrCapabilityRejected, true},
		{"missing handle", func(r *AuthorizeRequest) { r.Handle = "" }, nil, true, true, ErrMissingField, false},
		{"missing device id", func(r *AuthorizeRequest) { r.DeviceSessionID = "" }, nil, true, true, ErrMissingField, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := validReq()
			if tc.mut != nil {
				tc.mut(&req)
			}
			consumed := false
			caps := capFn(func(_, _, _, _ string) bool {
				consumed = true
				return tc.capOK
			})
			_, err := Authorize(req, okSessions{err: tc.sessErr}, policyFn(func(string) bool { return tc.policyOK }), caps)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err=%v want %v", err, tc.wantErr)
			}
			if consumed != tc.wantConsume {
				t.Fatalf("capability consumed=%v want %v — a failed earlier leg must burn nothing", consumed, tc.wantConsume)
			}
		})
	}
}
