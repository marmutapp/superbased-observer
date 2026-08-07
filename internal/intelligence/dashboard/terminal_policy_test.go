package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// putPolicy performs a confirmed PUT /api/terminal/policy and returns the code +
// decoded body. It mints a confirm token via GET first (double-submit, §10).
func putPolicy(t *testing.T, h http.Handler, body string) (int, map[string]any) {
	t.Helper()
	// GET mints the confirm cookie + token.
	greq := httptest.NewRequest(http.MethodGet, "/api/terminal/policy", nil)
	greq.Host = "127.0.0.1:8080"
	grec := httptest.NewRecorder()
	h.ServeHTTP(grec, greq)
	if grec.Code != http.StatusOK {
		t.Fatalf("GET /api/terminal/policy = %d", grec.Code)
	}
	var g struct {
		ConfirmToken string `json:"confirm_token"`
	}
	_ = json.Unmarshal(grec.Body.Bytes(), &g)
	var ck *http.Cookie
	for _, c := range grec.Result().Cookies() {
		if c.Name == remoteConfirmCookie {
			ck = c
		}
	}
	if ck == nil || g.ConfirmToken == "" {
		t.Fatalf("no confirm cookie/token")
	}
	req := httptest.NewRequest(http.MethodPut, "/api/terminal/policy", strings.NewReader(body))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(remoteConfirmHeader, g.ConfirmToken)
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestTerminalPolicyGetDefaults(t *testing.T) {
	_, h := newManageServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/terminal/policy", nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}
	var resp struct {
		ConfirmToken          string   `json:"confirm_token"`
		ConfigWritable        bool     `json:"config_writable"`
		AllowFreshAgent       bool     `json:"allow_fresh_agent"`
		LaunchableTools       []string `json:"launchable_tools"`
		RestartRequiredOnSave bool     `json:"restart_required_on_save"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ConfirmToken == "" {
		t.Error("GET must mint a confirm token")
	}
	if !resp.ConfigWritable {
		t.Error("config_writable should be true (writable config path)")
	}
	if resp.AllowFreshAgent {
		t.Error("default allow_fresh_agent must be false")
	}
	if !resp.RestartRequiredOnSave {
		t.Error("policy save must be flagged restart_required_on_save")
	}
	if len(resp.LaunchableTools) == 0 {
		t.Error("launchable_tools must be sourced from the capability registry")
	}
}

func TestTerminalPolicyPutRoundTrip(t *testing.T) {
	s, h := newManageServer(t)
	tool := launchableTools()[0]
	// A real, existing directory the operator authorizes as a project root.
	root := t.TempDir()
	body := `{"allow_fresh_agent":true,"allowed_tools":["` + tool + `"],"allowed_project_roots":["` + strings.ReplaceAll(root, `\`, `\\`) + `"]}`
	code, out := putPolicy(t, h, body)
	if code != http.StatusOK {
		t.Fatalf("PUT = %d body=%v", code, out)
	}
	if out["saved"] != true {
		t.Errorf("saved != true: %v", out)
	}
	if out["restart_required"] != true {
		t.Errorf("restart_required != true: %v", out)
	}
	// The write landed in config.
	cfg, err := config.Load(config.LoadOptions{GlobalPath: s.opts.ConfigPath})
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if !cfg.Terminal.Launch.AllowFreshAgent {
		t.Error("allow_fresh_agent not persisted")
	}
	if len(cfg.Terminal.Launch.AllowedTools) != 1 || cfg.Terminal.Launch.AllowedTools[0] != tool {
		t.Errorf("allowed_tools = %v", cfg.Terminal.Launch.AllowedTools)
	}
	if len(cfg.Terminal.Launch.AllowedProjectRoots) != 1 {
		t.Errorf("allowed_project_roots = %v", cfg.Terminal.Launch.AllowedProjectRoots)
	}
}

// TestTerminalPolicyAllowShellRoundTrip pins allow_shell as an independent
// opt-in on the same PUT surface: GET defaults to false, a PUT that sets it
// (with no allow_fresh_agent/allowed_tools at all) persists to config and is
// echoed back in the PUT response, and it does not flip allow_fresh_agent.
func TestTerminalPolicyAllowShellRoundTrip(t *testing.T) {
	s, h := newManageServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/terminal/policy", nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var get struct {
		AllowShell bool `json:"allow_shell"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &get); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if get.AllowShell {
		t.Error("default allow_shell must be false")
	}

	code, out := putPolicy(t, h, `{"allow_shell":true}`)
	if code != http.StatusOK {
		t.Fatalf("PUT = %d body=%v", code, out)
	}
	if out["allow_shell"] != true {
		t.Errorf("PUT response allow_shell = %v, want true", out["allow_shell"])
	}

	cfg, err := config.Load(config.LoadOptions{GlobalPath: s.opts.ConfigPath})
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if !cfg.Terminal.Launch.AllowShell {
		t.Error("allow_shell not persisted")
	}
	if cfg.Terminal.Launch.AllowFreshAgent {
		t.Error("allow_shell PUT must not also flip allow_fresh_agent")
	}
}

func TestTerminalPolicyPutRejectsNonLaunchableTool(t *testing.T) {
	_, h := newManageServer(t)
	code, _ := putPolicy(t, h, `{"allow_fresh_agent":true,"allowed_tools":["totally-not-a-tool"]}`)
	if code != http.StatusBadRequest {
		t.Errorf("non-launchable tool = %d, want 400", code)
	}
}

func TestTerminalPolicyPutRejectsBadProjectRoot(t *testing.T) {
	_, h := newManageServer(t)
	// A relative / non-existent root is rejected by termsvc.ValidateProjectRoot.
	code, _ := putPolicy(t, h, `{"allow_fresh_agent":true,"allowed_project_roots":["relative/not/abs"]}`)
	if code != http.StatusBadRequest {
		t.Errorf("bad project root = %d, want 400", code)
	}
}

func TestTerminalPolicyPutRequiresConfirmToken(t *testing.T) {
	_, h := newManageServer(t)
	// No confirm token → 403 before any write.
	req := httptest.NewRequest(http.MethodPut, "/api/terminal/policy", strings.NewReader(`{"allow_fresh_agent":true}`))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("missing confirm token = %d, want 403", rec.Code)
	}
	// Non-JSON content type → 415 (needs a valid token first to reach the CT
	// check; mint one).
	greq := httptest.NewRequest(http.MethodGet, "/api/terminal/policy", nil)
	greq.Host = "127.0.0.1:8080"
	grec := httptest.NewRecorder()
	h.ServeHTTP(grec, greq)
	var g struct {
		ConfirmToken string `json:"confirm_token"`
	}
	_ = json.Unmarshal(grec.Body.Bytes(), &g)
	var ck *http.Cookie
	for _, c := range grec.Result().Cookies() {
		if c.Name == remoteConfirmCookie {
			ck = c
		}
	}
	req2 := httptest.NewRequest(http.MethodPut, "/api/terminal/policy", strings.NewReader("allow_fresh_agent=true"))
	req2.Host = "127.0.0.1:8080"
	req2.Header.Set("Content-Type", "text/plain")
	req2.Header.Set(remoteConfirmHeader, g.ConfirmToken)
	req2.AddCookie(ck)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnsupportedMediaType {
		t.Errorf("non-JSON content type = %d, want 415", rec2.Code)
	}
}
