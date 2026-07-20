package dashboard

import (
	"testing"

	"github.com/marmutapp/superbased-observer/internal/remoteauth"
)

// TestReloadSecretSwapsLivePairingSecret pins the hot-reload: after a dashboard
// rotate/enable, the RUNNING controller must verify pairing against the NEW
// pairing secret (so a freshly-minted QR pairs without a daemon restart), and
// the OLD secret must no longer verify.
func TestReloadSecretSwapsLivePairingSecret(t *testing.T) {
	rc, encA := newReadyRemoteController(t) // built with secret A
	mc := rc.(*remoteController)

	rawA, err := remoteauth.DecodeSecret(encA)
	if err != nil {
		t.Fatalf("decode A: %v", err)
	}
	if !remoteauth.VerifySecret(mc.currentSecret(), rawA) {
		t.Fatal("secret A must verify before reload")
	}

	// Mint an independent secret B and hot-reload onto the live controller.
	rawB, _, err := remoteauth.GenerateSecret()
	if err != nil {
		t.Fatalf("generate B: %v", err)
	}
	hashB, err := remoteauth.HashSecret(rawB)
	if err != nil {
		t.Fatalf("hash B: %v", err)
	}
	rc.ReloadSecret(hashB)

	if remoteauth.VerifySecret(mc.currentSecret(), rawA) {
		t.Error("secret A must NOT verify after reload — the running controller kept the old hash")
	}
	if !remoteauth.VerifySecret(mc.currentSecret(), rawB) {
		t.Error("secret B must verify after reload — the new QR could not pair without a restart")
	}
	if !rc.Ready() {
		t.Error("controller must stay Ready after reloading a non-empty secret")
	}

	// Reloading an empty hash leaves the controller not-Ready (disable path).
	rc.ReloadSecret("")
	if rc.Ready() {
		t.Error("controller must be not-Ready after an empty reload")
	}
}
