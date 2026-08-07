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
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
)

// limitsRecordingLM is a LaunchManager (via the embedded fake) that ALSO
// implements the optional terminalLimitsSetter, recording the live-apply call
// so the verb test can assert the persisted values reached the manager.
type limitsRecordingLM struct {
	*fakeLaunchManager
	called  bool
	gotMax  int
	gotIdle time.Duration
}

func (m *limitsRecordingLM) SetTerminalLimits(maxConcurrent int, idleTimeout time.Duration) {
	m.called = true
	m.gotMax = maxConcurrent
	m.gotIdle = idleTimeout
}

// newLimitsServer builds a manage-capable server (writable config + real DB +
// ready controller) with the supplied LaunchManager wired, returning the server,
// its handler, and the config path so tests can assert persistence. A nil lm
// leaves the launcher seam absent (restart-required path).
func newLimitsServer(t *testing.T, lm LaunchManager) (*Server, http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "observer.db")
	database, err := openTestDB(context.Background(), db.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	cfgPath := filepath.Join(dir, "config.toml")
	body := "[observer]\ndb_path = \"" + filepath.ToSlash(dbPath) + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	rc, _ := newReadyRemoteController(t)
	s, err := New(Options{DB: database, ConfigPath: cfgPath, Remote: rc, LaunchManager: lm})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, s.Handler(), cfgPath
}

// postLimits performs POST /api/terminal/limits with the confirm cookie/header
// and the given raw JSON body.
func postLimits(t *testing.T, h http.Handler, ck *http.Cookie, token, rawBody string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/terminal/limits", strings.NewReader(rawBody))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(ck)
	req.Header.Set(remoteConfirmHeader, token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestTerminalLimitsRequiresConfirmToken pins the §10 CSRF hardening negatives
// for the new verb: wrong method → 405, non-JSON → 415, missing/mismatched
// confirm token → 403.
func TestTerminalLimitsRequiresConfirmToken(t *testing.T) {
	_, h, _ := newLimitsServer(t, &limitsRecordingLM{fakeLaunchManager: &fakeLaunchManager{}})
	ck, token := getConfirm(t, h)
	const path = "/api/terminal/limits"

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"max_concurrent":5}`))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "text/plain")
	req.AddCookie(ck)
	req.Header.Set(remoteConfirmHeader, token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain = %d, want 415", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"max_concurrent":5}`))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("no-token = %d, want 403", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"max_concurrent":5}`))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(ck)
	req.Header.Set(remoteConfirmHeader, token+"x")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("mismatched-token = %d, want 403", rec.Code)
	}
}

// TestTerminalLimitsStrictDecode pins the strict pointer decode: both fields
// absent → 400 (no silent zero rewrite), and a wrong-typed field → 400.
func TestTerminalLimitsStrictDecode(t *testing.T) {
	_, h, _ := newLimitsServer(t, &limitsRecordingLM{fakeLaunchManager: &fakeLaunchManager{}})
	ck, token := getConfirm(t, h)

	for _, tc := range []struct {
		name string
		body string
	}{
		{"both absent", `{}`},
		{"empty-ish other key", `{"unrelated":1}`},
		{"max wrong type", `{"max_concurrent":"nope"}`},
		{"idle wrong type", `{"idle_timeout":123}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := postLimits(t, h, ck, token, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s = %d, want 400", tc.name, rec.Code)
			}
		})
	}
}

// TestTerminalLimitsPersistsAndLiveApplies is the happy path: the values persist
// to the config file AND the live-apply setter is called with the persisted
// values, and the response reports restart_required:false.
func TestTerminalLimitsPersistsAndLiveApplies(t *testing.T) {
	lm := &limitsRecordingLM{fakeLaunchManager: &fakeLaunchManager{}}
	_, h, cfgPath := newLimitsServer(t, lm)
	ck, token := getConfirm(t, h)

	rec := postLimits(t, h, ck, token, `{"max_concurrent":12,"idle_timeout":"30m"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK              bool   `json:"ok"`
		RestartRequired bool   `json:"restart_required"`
		MaxConcurrent   int    `json:"max_concurrent"`
		IdleTimeout     string `json:"idle_timeout"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.OK || resp.RestartRequired || resp.MaxConcurrent != 12 || resp.IdleTimeout != "30m" {
		t.Fatalf("resp = %+v, want ok=true restart=false max=12 idle=30m", resp)
	}

	// Persisted to disk.
	cfg, err := loadConfigForDashboard(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Terminal.MaxConcurrent != 12 || cfg.Terminal.IdleTimeout != "30m" {
		t.Fatalf("persisted max=%d idle=%q, want 12/30m", cfg.Terminal.MaxConcurrent, cfg.Terminal.IdleTimeout)
	}

	// Live-applied with the persisted values.
	if !lm.called || lm.gotMax != 12 || lm.gotIdle != 30*time.Minute {
		t.Fatalf("live-apply called=%v max=%d idle=%v, want true/12/30m", lm.called, lm.gotMax, lm.gotIdle)
	}

	// A partial write leaves the untouched key at its persisted value.
	rec = postLimits(t, h, ck, token, `{"idle_timeout":"0"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("partial POST = %d, body=%s", rec.Code, rec.Body.String())
	}
	cfg, _ = loadConfigForDashboard(cfgPath)
	if cfg.Terminal.MaxConcurrent != 12 || cfg.Terminal.IdleTimeout != "0" {
		t.Fatalf("after partial: max=%d idle=%q, want 12/0", cfg.Terminal.MaxConcurrent, cfg.Terminal.IdleTimeout)
	}
	if lm.gotIdle != 0 {
		t.Fatalf("partial live-apply idle=%v, want 0 (reaping disabled)", lm.gotIdle)
	}
}

// TestTerminalLimitsRestartRequiredWithoutSetter pins the adapter-absent path: a
// LaunchManager that does NOT implement terminalLimitsSetter still persists the
// write, but the response reports restart_required:true.
func TestTerminalLimitsRestartRequiredWithoutSetter(t *testing.T) {
	// A bare fakeLaunchManager satisfies LaunchManager but NOT terminalLimitsSetter.
	_, h, cfgPath := newLimitsServer(t, &fakeLaunchManager{})
	ck, token := getConfirm(t, h)

	rec := postLimits(t, h, ck, token, `{"max_concurrent":7}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		RestartRequired bool `json:"restart_required"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.RestartRequired {
		t.Fatalf("restart_required = false, want true (no live-apply setter)")
	}
	cfg, _ := loadConfigForDashboard(cfgPath)
	if cfg.Terminal.MaxConcurrent != 7 {
		t.Fatalf("persisted max=%d, want 7", cfg.Terminal.MaxConcurrent)
	}
}

// TestTerminalLimitsValidationRejectionDoesNotPersist pins fail-closed: a
// negative max or an unparseable duration is rejected by config.Validate and
// NOTHING is written (the pre-write config file is unchanged) and the setter is
// never called.
func TestTerminalLimitsValidationRejectionDoesNotPersist(t *testing.T) {
	lm := &limitsRecordingLM{fakeLaunchManager: &fakeLaunchManager{}}
	_, h, cfgPath := newLimitsServer(t, lm)
	ck, token := getConfirm(t, h)

	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	for _, tc := range []struct {
		name string
		body string
	}{
		{"negative max", `{"max_concurrent":-1}`},
		{"bad duration", `{"idle_timeout":"notaduration"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := postLimits(t, h, ck, token, tc.body)
			if rec.Code == http.StatusOK {
				t.Fatalf("%s accepted (code=%d), want rejection", tc.name, rec.Code)
			}
			after, _ := os.ReadFile(cfgPath)
			if string(after) != string(before) {
				t.Fatalf("%s mutated the config file despite validation failure", tc.name)
			}
			if lm.called {
				t.Fatalf("%s live-applied despite validation failure", tc.name)
			}
		})
	}
}
