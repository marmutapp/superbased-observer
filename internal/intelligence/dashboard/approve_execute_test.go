package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/db"
)

// newApproveExecuteServer builds a manage-capable server and returns it, its
// loopback handler, and the concrete controller so the test can seed a device
// session + inspect the capability store.
func newApproveExecuteServer(t *testing.T) (*Server, http.Handler, *remoteController) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "observer.db")
	database, err := db.Open(context.Background(), db.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[observer]\ndb_path = \""+filepath.ToSlash(dbPath)+"\"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	rc, _ := newReadyRemoteController(t)
	s, err := New(Options{DB: database, ConfigPath: cfgPath, Remote: rc})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, s.Handler(), rc.(*remoteController)
}

// TestApproveExecuteMintsLocallyAndIsConsumable exercises the §4.γ/§6 local
// approval: POST /api/remote/approve-execute mints a single-use terminal-control
// capability + bound confirm for a paired device + handle, returns BOTH in the
// response body, and the minted pair is consumable exactly once through the
// controller's ConsumeTerminalControl (keyed by the raw session id → hash).
func TestApproveExecuteMintsLocallyAndIsConsumable(t *testing.T) {
	_, h, mc := newApproveExecuteServer(t)

	// Seed a paired device session; List surfaces its fingerprint + hash id.
	rawSession, err := mc.sessions.Create()
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	sessions := mc.sessions.List()
	if len(sessions) != 1 {
		t.Fatalf("want 1 seeded session, got %d", len(sessions))
	}
	fp := sessions[0].Fingerprint

	ck, token := getConfirm(t, h)
	const handle = "TERM-xyz"
	body, _ := json.Marshal(map[string]string{"device": fp, "handle": handle})
	rec := postConfirm(t, h, "/api/remote/approve-execute", string(body), ck, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve-execute = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK         bool   `json:"ok"`
		Capability string `json:"capability"`
		Confirm    string `json:"confirm"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || resp.Capability == "" || resp.Confirm == "" {
		t.Fatalf("approve-execute must return a capability + confirm, got %+v", resp)
	}

	// A WRONG confirm consumes NOTHING (§4.γ.2) — the capability survives.
	if mc.ConsumeTerminalControl(resp.Capability, "wrong-confirm", rawSession, handle) {
		t.Fatal("a wrong confirm must not consume the capability")
	}
	// The exact (capability, confirm, device, handle) consumes once…
	if !mc.ConsumeTerminalControl(resp.Capability, resp.Confirm, rawSession, handle) {
		t.Fatal("capability+confirm should consume for the exact (device, handle)")
	}
	// …and only once (single-use burn-on-confirmed-hit).
	if mc.ConsumeTerminalControl(resp.Capability, resp.Confirm, rawSession, handle) {
		t.Fatal("capability must be single-use (second consume must fail)")
	}
}

// TestApproveExecuteRemoteRefusedAndAbsentFromReads pins that the approval mint
// is owner-local-only and its secrets never leak through a GET read surface.
func TestApproveExecuteRemoteRefusedAndAbsentFromReads(t *testing.T) {
	s, h, mc := newApproveExecuteServer(t)

	if _, err := mc.sessions.Create(); err != nil {
		t.Fatalf("create session: %v", err)
	}
	fp := mc.sessions.List()[0].Fingerprint
	ck, token := getConfirm(t, h)
	body, _ := json.Marshal(map[string]string{"device": fp, "handle": "TERM-1"})
	rec := postConfirm(t, h, "/api/remote/approve-execute", string(body), ck, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve-execute = %d", rec.Code)
	}
	var resp struct {
		Capability string `json:"capability"`
		Confirm    string `json:"confirm"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	// No GET read surface may echo the capability/confirm.
	for _, path := range []string{"/api/remote/config", "/api/remote/sessions", "/api/remote/audit"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "127.0.0.1:8080"
		gr := httptest.NewRecorder()
		h.ServeHTTP(gr, req)
		if strings.Contains(gr.Body.String(), resp.Capability) || (resp.Confirm != "" && strings.Contains(gr.Body.String(), resp.Confirm)) {
			t.Errorf("GET %s leaked the execute capability/confirm", path)
		}
	}

	// Remote-exposed listener: approve-execute (Local) is refused pre-principal.
	remoteH := s.remoteGuardedHandler(s.opts.Remote)
	req := httptest.NewRequest(http.MethodPost, "/api/remote/approve-execute", strings.NewReader(`{}`))
	req.Host = testRemoteHost
	req.Header.Set("Origin", "https://"+testRemoteHost)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	remoteH.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("approve-execute on the remote listener = %d, want 403 (Local, owner-only)", rr.Code)
	}
}
