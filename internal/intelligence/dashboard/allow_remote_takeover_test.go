package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// TestAllowRemoteTakeoverManageVerb pins the owner-local config write, strict
// decode, persisted GET read-back, and live controller hot-swap for the
// default-on authenticated-remote takeover policy.
func TestAllowRemoteTakeoverManageVerb(t *testing.T) {
	s, h := newManageServer(t)
	ck, token := getConfirm(t, h)

	if rec := postConfirm(t, h, "/api/remote/allow-remote-takeover", `{"allow_remote_terminal_takeover":false}`, ck, token); rec.Code != http.StatusBadRequest {
		t.Fatalf("takeover toggle with remote off = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	cfg, err := loadConfigForDashboard(s.opts.ConfigPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Remote.Enabled = true
	if err := config.WriteToml(s.opts.ConfigPath, cfg); err != nil {
		t.Fatalf("arm test config without a listener: %v", err)
	}
	if rec := postConfirm(t, h, "/api/remote/allow-remote-takeover", `{}`, ck, token); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty takeover toggle = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	// Omitted config key inherits the default true and the GET used by Settings
	// must expose it before the first toggle.
	statusReq := httptest.NewRequest(http.MethodGet, "/api/remote/standing-terminal", nil)
	statusRec := httptest.NewRecorder()
	h.ServeHTTP(statusRec, statusReq)
	var initial struct {
		Allow bool `json:"allow_remote_terminal_takeover"`
	}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &initial); err != nil || !initial.Allow {
		t.Fatalf("initial status = %s err=%v, want default true", statusRec.Body.String(), err)
	}

	rec := postConfirm(t, h, "/api/remote/allow-remote-takeover", `{"allow_remote_terminal_takeover":false}`, ck, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable takeover = %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		OK      bool `json:"ok"`
		Restart bool `json:"restart_required"`
		Allow   bool `json:"allow_remote_terminal_takeover"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !response.OK || response.Restart || response.Allow {
		t.Fatalf("disable response = %+v", response)
	}
	cfg, err = loadConfigForDashboard(s.opts.ConfigPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if cfg.Remote.AllowRemoteTerminalTakeover {
		t.Fatal("allow_remote_terminal_takeover was not persisted false")
	}
	rc := s.opts.Remote.(*remoteController)
	if rc.AllowRemoteTerminalTakeover() {
		t.Fatal("live takeover policy was not hot-swapped false")
	}

	statusRec = httptest.NewRecorder()
	h.ServeHTTP(statusRec, httptest.NewRequest(http.MethodGet, "/api/remote/standing-terminal", nil))
	if !strings.Contains(statusRec.Body.String(), `"allow_remote_terminal_takeover":false`) {
		t.Fatalf("status did not read back persisted false: %s", statusRec.Body.String())
	}
	if rec := postConfirm(t, h, "/api/remote/allow-remote-takeover", `{"allow_remote_terminal_takeover":true}`, ck, token); rec.Code != http.StatusOK {
		t.Fatalf("re-enable takeover = %d: %s", rec.Code, rec.Body.String())
	}
	if !rc.AllowRemoteTerminalTakeover() {
		t.Fatal("live takeover policy was not hot-swapped true")
	}
}
