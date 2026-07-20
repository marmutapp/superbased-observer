package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/remoteauth"
	"github.com/marmutapp/superbased-observer/internal/remotecfg"
)

// revokeRecordingManager records RevokeAllRemoteWriters calls so the
// revoke-kills-open-writers invariant is observable in a handler test.
type revokeRecordingManager struct {
	*fakeLaunchManager
	mu      sync.Mutex
	revokes []string
}

func (m *revokeRecordingManager) RevokeAllRemoteWriters(reason string) int {
	m.mu.Lock()
	m.revokes = append(m.revokes, reason)
	m.mu.Unlock()
	return 1
}

func (m *revokeRecordingManager) revokeReasons() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.revokes...)
}

// newStandingController builds a *remoteController wired with a known standing
// secret hash + enabled gate + a captured audit sink + a tight rate limiter,
// for direct VerifyStandingTerminalControl unit coverage.
func newStandingController(t *testing.T, enabled bool, rate int) (*remoteController, string, *[]RemoteAuditRecord, *sync.Mutex) {
	t.Helper()
	raw, enc, err := remoteauth.GenerateStandingSecret()
	if err != nil {
		t.Fatalf("GenerateStandingSecret: %v", err)
	}
	hash, err := remoteauth.HashSecret(raw)
	if err != nil {
		t.Fatalf("HashSecret: %v", err)
	}
	pRaw, _, _ := remoteauth.GenerateSecret()
	pHash, _ := remoteauth.HashSecret(pRaw)
	var mu sync.Mutex
	var audit []RemoteAuditRecord
	rc := NewRemoteController(RemoteOptions{
		HashedSecret:               pHash,
		AllowedHosts:               []string{testRemoteHost},
		RateLimitPerMin:            rate,
		StandingTerminalSecretHash: hash,
		StandingTerminalEnabled:    enabled,
		Audit: func(r RemoteAuditRecord) {
			mu.Lock()
			audit = append(audit, r)
			mu.Unlock()
		},
	})
	return rc.(*remoteController), enc, &audit, &mu
}

// TestStandingVerifyHappyAndBadSecret pins the core credential leg: the correct
// standing secret verifies; a different one is refused; both are audited with
// the device fingerprint (never the secret).
func TestStandingVerifyHappyAndBadSecret(t *testing.T) {
	mc, enc, audit, mu := newStandingController(t, true, 60)

	if !mc.VerifyStandingTerminalControl(enc, "device-abc", "handle-1") {
		t.Fatal("correct standing secret was refused")
	}
	_, other, _ := remoteauth.GenerateStandingSecret()
	if mc.VerifyStandingTerminalControl(other, "device-abc", "handle-1") {
		t.Fatal("a DIFFERENT standing secret verified")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*audit) < 2 {
		t.Fatalf("want >=2 audit rows, got %d", len(*audit))
	}
	for _, r := range *audit {
		if r.Kind != "terminal_control_standing_acquire" {
			t.Errorf("audit kind = %q", r.Kind)
		}
		if strings.Contains(r.Detail, enc) || strings.Contains(r.SessionID, enc) {
			t.Error("audit row leaked the standing secret")
		}
		if r.SessionID == "device-abc" {
			t.Error("audit recorded the raw device id, not a fingerprint")
		}
	}
	if (*audit)[0].Decision != "ok" || (*audit)[1].Decision != "deny" {
		t.Errorf("decisions = %q,%q want ok,deny", (*audit)[0].Decision, (*audit)[1].Decision)
	}
}

// TestStandingVerifyDisabledDenies: with the master gate off, even the correct
// secret is refused (defense in depth over the config toggle).
func TestStandingVerifyDisabledDenies(t *testing.T) {
	mc, enc, _, _ := newStandingController(t, false, 60)
	if mc.VerifyStandingTerminalControl(enc, "device-abc", "handle-1") {
		t.Fatal("standing secret verified while standing access is DISABLED")
	}
}

// TestStandingVerifyEmptyHashDenies: no provisioned secret ⇒ deny.
func TestStandingVerifyEmptyHashDenies(t *testing.T) {
	mc, _, _, _ := newStandingController(t, true, 60)
	mc.ReloadStandingTerminalSecret("", true) // clear the hash but keep enabled
	if mc.VerifyStandingTerminalControl("standing.whatever", "device-abc", "handle-1") {
		t.Fatal("verify succeeded with no standing hash provisioned")
	}
}

// TestStandingVerifyRateLimited: after the per-device budget is spent, even the
// CORRECT secret is throttled (brute-force protection), audited as rate_limited.
func TestStandingVerifyRateLimited(t *testing.T) {
	mc, enc, audit, mu := newStandingController(t, true, 2) // burst 2
	if !mc.VerifyStandingTerminalControl(enc, "dev", "h") {
		t.Fatal("attempt 1 refused")
	}
	if !mc.VerifyStandingTerminalControl(enc, "dev", "h") {
		t.Fatal("attempt 2 refused")
	}
	if mc.VerifyStandingTerminalControl(enc, "dev", "h") {
		t.Fatal("attempt 3 (over budget) should be rate-limited even with the correct secret")
	}
	mu.Lock()
	defer mu.Unlock()
	last := (*audit)[len(*audit)-1]
	if last.Decision != "deny" || last.Detail != "rate_limited" {
		t.Errorf("last audit = %q/%q, want deny/rate_limited", last.Decision, last.Detail)
	}
}

// TestStandingReloadInvalidatesLiveSecret: a revoke-style reload ("" + false)
// makes the previously-valid secret stop verifying immediately (the Phase-4
// revoke invariant at the credential leg).
// TestStandingReloadBumpsGeneration pins the finding-1 TOCTOU primitive: EVERY
// ReloadStandingTerminalSecret (mint, rotate, revoke, disable) advances the
// standing generation, so the cmd adapter's install-time recheck can fence an
// in-flight verify that raced the transition.
func TestStandingReloadBumpsGeneration(t *testing.T) {
	mc, _, _, _ := newStandingController(t, true, 60)
	g0 := mc.StandingTerminalGeneration()
	mc.ReloadStandingTerminalSecret("new-hash", true) // rotate-style
	g1 := mc.StandingTerminalGeneration()
	mc.ReloadStandingTerminalSecret("", false) // revoke-style
	g2 := mc.StandingTerminalGeneration()
	if g1 != g0+1 || g2 != g1+1 {
		t.Fatalf("generation must bump on every reload: g0=%d g1=%d g2=%d", g0, g1, g2)
	}
}

// TestStandingLimiterSharedAcrossDevices pins the finding-4 fix: the standing
// rate limit rides ONE GLOBAL bucket, so rotating the device session (logout +
// re-pair mints a fresh session id) cannot mint a fresh budget. Device A spends
// the burst; device B (a "fresh" identity) is throttled immediately.
func TestStandingLimiterSharedAcrossDevices(t *testing.T) {
	mc, enc, audit, mu := newStandingController(t, true, 2) // burst 2
	if !mc.VerifyStandingTerminalControl(enc, "device-A", "h") {
		t.Fatal("device A attempt 1 refused")
	}
	if !mc.VerifyStandingTerminalControl(enc, "device-A", "h") {
		t.Fatal("device A attempt 2 refused")
	}
	if mc.VerifyStandingTerminalControl(enc, "device-B", "h") {
		t.Fatal("a 'fresh' device identity bypassed the standing rate limit — the bucket must be global, not per-session")
	}
	mu.Lock()
	defer mu.Unlock()
	last := (*audit)[len(*audit)-1]
	if last.Decision != "deny" || last.Detail != "rate_limited" {
		t.Errorf("device B audit = %q/%q, want deny/rate_limited", last.Decision, last.Detail)
	}
}

// TestStandingLimiterZeroRateStillThrottles pins the finding-5 fix:
// rate_limit_per_min=0 means "unlimited" for the pairing endpoint but the
// standing verifier clamps it to the 6/min default — each attempt costs a
// 19 MiB argon2 compute, so it must never be unlimited.
func TestStandingLimiterZeroRateStillThrottles(t *testing.T) {
	mc, enc, _, _ := newStandingController(t, true, 0) // 0 = pairing-unlimited
	allowed := 0
	for i := 0; i < 20; i++ {
		if mc.VerifyStandingTerminalControl(enc, "dev", "h") {
			allowed++
		}
	}
	if allowed == 20 {
		t.Fatal("rate_limit_per_min=0 left the standing verifier UNLIMITED — it must clamp to the default")
	}
	if allowed != 6 {
		t.Errorf("allowed = %d, want the clamped default burst of 6", allowed)
	}
}

func TestStandingReloadInvalidatesLiveSecret(t *testing.T) {
	mc, enc, _, _ := newStandingController(t, true, 60)
	if !mc.VerifyStandingTerminalControl(enc, "dev", "h") {
		t.Fatal("secret should verify before revoke")
	}
	mc.ReloadStandingTerminalSecret("", false)
	if mc.VerifyStandingTerminalControl(enc, "dev", "h") {
		t.Fatal("secret still verified after a revoke-style reload")
	}
}

// --- handler-level coverage (mint / status / revoke) ---

// newStandingManageServer builds a manage server with a recording LaunchManager
// and arms remote + allow_terminal so the standing mint precondition holds.
func newStandingManageServer(t *testing.T) (*Server, http.Handler, *revokeRecordingManager, *http.Cookie, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "observer.db")
	database, err := db.Open(context.Background(), db.Options{Path: dbPath})
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
	lm := &revokeRecordingManager{fakeLaunchManager: &fakeLaunchManager{}}
	s, err := New(Options{DB: database, ConfigPath: cfgPath, Remote: rc, LaunchManager: lm})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := s.Handler()
	ck, token := getConfirm(t, h)
	// Arm remote WITH allow_terminal so the standing mint precondition holds.
	if rec := postConfirm(t, h, "/api/remote/enable", `{"host":"box.ts.net","allow_terminal":true}`, ck, token); rec.Code != http.StatusOK {
		t.Fatalf("enable = %d: %s", rec.Code, rec.Body.String())
	}
	return s, h, lm, ck, token
}

// TestStandingMintReturnsSecretOnceThenRevokeKillsWriters is the end-to-end
// management flow: mint returns the secret once + hot-loads it onto the live
// controller (so it verifies), status reflects enabled/present without leaking
// it, and revoke both disables verification AND kills every live remote writer.
func TestStandingMintReturnsSecretOnceThenRevokeKillsWriters(t *testing.T) {
	s, h, lm, ck, token := newStandingManageServer(t)
	mc := s.opts.Remote.(*remoteController)

	// Mint.
	rec := postConfirm(t, h, "/api/remote/standing-terminal/mint", `{}`, ck, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("mint = %d: %s", rec.Code, rec.Body.String())
	}
	var mintResp struct {
		OK      bool   `json:"ok"`
		Secret  string `json:"secret"`
		Rotated bool   `json:"rotated"`
		Warning string `json:"warning"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &mintResp); err != nil {
		t.Fatalf("decode mint: %v", err)
	}
	if !mintResp.OK || mintResp.Secret == "" || mintResp.Rotated {
		t.Fatalf("mint response bad: %+v", mintResp)
	}
	if !remoteauth.IsStandingSecret(mintResp.Secret) {
		t.Fatalf("minted secret %q is not a standing secret", mintResp.Secret)
	}
	if !strings.Contains(mintResp.Warning, "EVERY live terminal") {
		t.Error("mint response missing the standing-access security warning")
	}
	// The hot-reload means the live controller verifies the minted secret.
	if !mc.VerifyStandingTerminalControl(mintResp.Secret, "device-x", "handle-x") {
		t.Fatal("live controller did not verify the freshly minted standing secret")
	}

	// Status GET reflects enabled/present and NEVER leaks the secret.
	g := httptest.NewRequest(http.MethodGet, "/api/remote/standing-terminal", nil)
	g.Host = "127.0.0.1:8080"
	grec := httptest.NewRecorder()
	h.ServeHTTP(grec, g)
	if grec.Code != http.StatusOK {
		t.Fatalf("status = %d", grec.Code)
	}
	if strings.Contains(grec.Body.String(), mintResp.Secret) {
		t.Fatal("status GET leaked the standing secret")
	}
	var st struct {
		Enabled       bool `json:"enabled"`
		SecretPresent bool `json:"secret_present"`
	}
	_ = json.Unmarshal(grec.Body.Bytes(), &st)
	if !st.Enabled || !st.SecretPresent {
		t.Fatalf("status did not reflect the minted secret: %+v", st)
	}

	// Revoke: verification stops AND every live remote writer is killed.
	rrec := postConfirm(t, h, "/api/remote/standing-terminal/revoke", `{}`, ck, token)
	if rrec.Code != http.StatusOK {
		t.Fatalf("revoke = %d: %s", rrec.Code, rrec.Body.String())
	}
	if mc.VerifyStandingTerminalControl(mintResp.Secret, "device-x", "handle-x") {
		t.Fatal("standing secret still verified after revoke")
	}
	killed := false
	for _, reason := range lm.revokeReasons() {
		if strings.Contains(reason, "standing terminal access revoked") {
			killed = true
		}
	}
	if !killed {
		t.Fatalf("revoke did not kill live remote writers; reasons=%v", lm.revokeReasons())
	}
	// The secret file is gone (true revocation).
	cfg, _ := loadConfigForDashboard(s.opts.ConfigPath)
	if _, err := os.Stat(remotecfg.StandingTerminalSecretPath(cfg)); !os.IsNotExist(err) {
		t.Error("standing secret file still present after revoke")
	}
}

// TestStandingMintRotateKillsOldWriters: minting again while already enabled is
// a ROTATE — it must kill writers acquired via the OLD secret.
func TestStandingMintRotateKillsOldWriters(t *testing.T) {
	_, h, lm, ck, token := newStandingManageServer(t)
	if rec := postConfirm(t, h, "/api/remote/standing-terminal/mint", `{}`, ck, token); rec.Code != http.StatusOK {
		t.Fatalf("first mint = %d: %s", rec.Code, rec.Body.String())
	}
	rec := postConfirm(t, h, "/api/remote/standing-terminal/mint", `{}`, ck, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("second mint (rotate) = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Rotated bool `json:"rotated"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.Rotated {
		t.Error("second mint should report rotated=true")
	}
	killed := false
	for _, reason := range lm.revokeReasons() {
		if strings.Contains(reason, "rotated") {
			killed = true
		}
	}
	if !killed {
		t.Fatalf("rotate did not kill old-secret writers; reasons=%v", lm.revokeReasons())
	}
}

// TestStandingSecretSurvivesRestart: the hash-at-rest file lets a rebuilt
// controller (a daemon restart) verify the SAME secret — persistence without a
// DB table (mirrors the pairing-secret file discipline).
func TestStandingSecretSurvivesRestart(t *testing.T) {
	s, h, _, ck, token := newStandingManageServer(t)
	rec := postConfirm(t, h, "/api/remote/standing-terminal/mint", `{}`, ck, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("mint = %d: %s", rec.Code, rec.Body.String())
	}
	var mintResp struct {
		Secret string `json:"secret"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &mintResp)

	// Simulate a restart: re-read the persisted hash file + rebuild a controller.
	cfg, _ := loadConfigForDashboard(s.opts.ConfigPath)
	hashBytes, err := os.ReadFile(remotecfg.StandingTerminalSecretPath(cfg))
	if err != nil {
		t.Fatalf("standing secret file not persisted: %v", err)
	}
	pRaw, _, _ := remoteauth.GenerateSecret()
	pHash, _ := remoteauth.HashSecret(pRaw)
	rebuilt := NewRemoteController(RemoteOptions{
		HashedSecret:               pHash,
		AllowedHosts:               []string{testRemoteHost},
		RateLimitPerMin:            60,
		StandingTerminalSecretHash: strings.TrimSpace(string(hashBytes)),
		StandingTerminalEnabled:    cfg.Remote.AllowStandingTerminalControl,
	}).(*remoteController)
	if !rebuilt.VerifyStandingTerminalControl(mintResp.Secret, "device-x", "handle-x") {
		t.Fatal("rebuilt controller did not verify the persisted standing secret after restart")
	}
}

// mintStandingSecret mints via the handler and returns the one-time secret.
func mintStandingSecret(t *testing.T, h http.Handler, ck *http.Cookie, token string) string {
	t.Helper()
	rec := postConfirm(t, h, "/api/remote/standing-terminal/mint", `{}`, ck, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("mint = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Secret == "" {
		t.Fatalf("mint returned no secret (err=%v): %s", err, rec.Body.String())
	}
	return resp.Secret
}

// TestStandingRevokePersistFailureStillKillsLiveAccess pins the finding-2 fix:
// when the durable half of a revoke fails (here: the secret file has been
// replaced by a non-empty directory so the unlink errors), the handler must
// STILL have hot-disabled the live verifier and killed every remote writer
// BEFORE returning the error — a persist failure degrades to access-OFF, never
// fail-open behind an HTTP 500.
func TestStandingRevokePersistFailureStillKillsLiveAccess(t *testing.T) {
	s, h, lm, ck, token := newStandingManageServer(t)
	mc := s.opts.Remote.(*remoteController)
	secret := mintStandingSecret(t, h, ck, token)
	if !mc.VerifyStandingTerminalControl(secret, "dev", "h") {
		t.Fatal("minted secret should verify before revoke")
	}

	// F2(a): the hash UNLINK is now BEST-EFFORT — the config write
	// (allow_standing_terminal_control=false) is the durable gate. Replace the
	// hash file with a NON-EMPTY directory so os.Remove fails; the revoke must
	// still SUCCEED (200), kill live access, and durably persist enabled=false so a
	// restart cannot resurrect the orphan hash.
	cfg, _ := loadConfigForDashboard(s.opts.ConfigPath)
	secretPath := remotecfg.StandingTerminalSecretPath(cfg)
	if err := os.Remove(secretPath); err != nil {
		t.Fatalf("remove secret file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(secretPath, "blocker"), 0o755); err != nil {
		t.Fatalf("mkdir blocker: %v", err)
	}

	rec := postConfirm(t, h, "/api/remote/standing-terminal/revoke", `{}`, ck, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke with a failing (best-effort) unlink = %d, want 200 — durably off via the config gate", rec.Code)
	}
	// The SECURITY half must have happened: verifier dead + writers killed.
	if mc.VerifyStandingTerminalControl(secret, "dev", "h") {
		t.Fatal("standing secret STILL verifies after revoke — fail-open")
	}
	// Durable gate: the persisted config has standing disabled, so a restart that
	// reloads the orphan hash still refuses (StandingTerminalEnabled=false).
	reloaded, _ := loadConfigForDashboard(s.opts.ConfigPath)
	if reloaded.Remote.AllowStandingTerminalControl {
		t.Fatal("config still enables standing access after revoke — a restart would resurrect the orphan hash")
	}
	killed := false
	for _, reason := range lm.revokeReasons() {
		if strings.Contains(reason, "standing terminal access revoked") {
			killed = true
		}
	}
	if !killed {
		t.Fatalf("failed-persist revoke did not kill live remote writers; reasons=%v", lm.revokeReasons())
	}
}

// TestAllowTerminalFlipGatesStandingVerifier pins the finding-3 fix: flipping
// [remote].allow_terminal OFF hot-disables the standing verifier immediately
// (a paired device cannot reopen a socket and reacquire with the reusable
// secret until restart), and flipping it back ON restores standing access from
// the persisted config + hash file.
func TestAllowTerminalFlipGatesStandingVerifier(t *testing.T) {
	s, h, _, ck, token := newStandingManageServer(t)
	mc := s.opts.Remote.(*remoteController)
	secret := mintStandingSecret(t, h, ck, token)
	if !mc.VerifyStandingTerminalControl(secret, "dev", "h") {
		t.Fatal("minted secret should verify")
	}
	if rec := postConfirm(t, h, "/api/remote/allow-terminal", `{"allow_terminal":false}`, ck, token); rec.Code != http.StatusOK {
		t.Fatalf("allow-terminal false = %d: %s", rec.Code, rec.Body.String())
	}
	if mc.VerifyStandingTerminalControl(secret, "dev", "h") {
		t.Fatal("standing secret still verifies after allow_terminal→false — reacquire window open until restart")
	}
	if rec := postConfirm(t, h, "/api/remote/allow-terminal", `{"allow_terminal":true}`, ck, token); rec.Code != http.StatusOK {
		t.Fatalf("allow-terminal true = %d: %s", rec.Code, rec.Body.String())
	}
	if !mc.VerifyStandingTerminalControl(secret, "dev", "h") {
		t.Fatal("standing access did not restore after allow_terminal→true (config still enables it)")
	}
}

// TestRemoteDisableGatesStandingVerifier pins the other finding-3 leg: turning
// remote access OFF hot-disables the standing verifier immediately.
func TestRemoteDisableGatesStandingVerifier(t *testing.T) {
	s, h, _, ck, token := newStandingManageServer(t)
	mc := s.opts.Remote.(*remoteController)
	secret := mintStandingSecret(t, h, ck, token)
	if !mc.VerifyStandingTerminalControl(secret, "dev", "h") {
		t.Fatal("minted secret should verify")
	}
	if rec := postConfirm(t, h, "/api/remote/disable", `{}`, ck, token); rec.Code != http.StatusOK {
		t.Fatalf("disable = %d: %s", rec.Code, rec.Body.String())
	}
	if mc.VerifyStandingTerminalControl(secret, "dev", "h") {
		t.Fatal("standing secret still verifies after remote disable")
	}
}

// TestTerminalPolicyPutSerializedWithManageMutex pins the finding-6 fix: the
// terminal-policy PUT's whole-config read-modify-write participates in
// remoteManageMu, so it can never interleave with (and clobber) a concurrent
// standing/remote manage verb's config write. Proven by lock exclusion: with
// the mutex held, the PUT blocks; on release it completes.
func TestTerminalPolicyPutSerializedWithManageMutex(t *testing.T) {
	_, h := newManageServer(t)
	ck, token := getConfirm(t, h)

	remoteManageMu.Lock()
	done := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPut, "/api/terminal/policy", strings.NewReader(`{"allow_fresh_agent":false,"allowed_tools":[],"allowed_project_roots":[]}`))
		req.Host = "127.0.0.1:8080"
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(ck)
		req.Header.Set(remoteConfirmHeader, token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		done <- rec.Code
	}()
	select {
	case code := <-done:
		remoteManageMu.Unlock()
		t.Fatalf("terminal-policy PUT completed (%d) while remoteManageMu was held — the config RMW must serialize with the manage verbs", code)
	case <-time.After(200 * time.Millisecond):
		// Blocked, as required.
	}
	remoteManageMu.Unlock()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("terminal-policy PUT = %d after unlock: want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("terminal-policy PUT never completed after the mutex was released")
	}
}

// TestAllowTerminalFlipUpdatesLiveGate pins the finding-3 residual at the
// controller boundary: the LIVE AllowTerminal() gate (which the production
// single-use acquire path now reads) flips with the allow-terminal handler,
// with no restart. The end-to-end single-use refusal is covered in cmd/observer.
func TestAllowTerminalFlipUpdatesLiveGate(t *testing.T) {
	s, h, _, ck, token := newStandingManageServer(t)
	mc := s.opts.Remote.(*remoteController)
	if !mc.AllowTerminal() {
		t.Fatal("allow_terminal should start ON (armed with allow_terminal=true)")
	}
	rec := postConfirm(t, h, "/api/remote/allow-terminal", `{"allow_terminal":false}`, ck, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("allow-terminal false = %d: %s", rec.Code, rec.Body.String())
	}
	// allow_terminal hot-reloads onto the live gate + standing verifier: the
	// disable takes effect immediately, so the handler must NOT ask for a restart
	// (a stale restart_required=true would fire a misleading "restart to take
	// effect" banner for a change that is already live).
	var flip struct {
		RestartRequired bool `json:"restart_required"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &flip); err != nil {
		t.Fatalf("decode allow-terminal response: %v", err)
	}
	if flip.RestartRequired {
		t.Error("allow_terminal hot-reloads the live gate → restart_required must be false")
	}
	if mc.AllowTerminal() {
		t.Fatal("live AllowTerminal() still true after allow_terminal->false")
	}
	if rec := postConfirm(t, h, "/api/remote/allow-terminal", `{"allow_terminal":true}`, ck, token); rec.Code != http.StatusOK {
		t.Fatalf("allow-terminal true = %d: %s", rec.Code, rec.Body.String())
	}
	if !mc.AllowTerminal() {
		t.Fatal("live AllowTerminal() did not restore after allow_terminal->true")
	}
}

// TestRemoteDisableGatesLiveAllowTerminal pins that remote-disable ALSO flips the
// live allow_terminal gate off (so the single-use path refuses too), not only
// the standing verifier.
func TestRemoteDisableGatesLiveAllowTerminal(t *testing.T) {
	s, h, _, ck, token := newStandingManageServer(t)
	mc := s.opts.Remote.(*remoteController)
	if !mc.AllowTerminal() {
		t.Fatal("allow_terminal should start ON")
	}
	if rec := postConfirm(t, h, "/api/remote/disable", `{}`, ck, token); rec.Code != http.StatusOK {
		t.Fatalf("disable = %d: %s", rec.Code, rec.Body.String())
	}
	if mc.AllowTerminal() {
		t.Fatal("live AllowTerminal() still true after remote disable")
	}
}

// TestRemoteDisablePersistFailureStillKillsLiveAccess pins the finding-2(b) fix:
// when remote-disable's durable half fails (the pairing-secret path is a
// non-empty directory so config persist / unlink cannot complete cleanly), the
// handler must STILL have hot-disabled the live allow_terminal gate + standing
// verifier and killed writers BEFORE returning — never fail-open.
func TestRemoteDisablePersistFailureStillKillsLiveAccess(t *testing.T) {
	s, h, lm, ck, token := newStandingManageServer(t)
	mc := s.opts.Remote.(*remoteController)
	secret := mintStandingSecret(t, h, ck, token)
	if !mc.VerifyStandingTerminalControl(secret, "dev", "h") || !mc.AllowTerminal() {
		t.Fatal("preconditions: standing secret + allow_terminal should be live")
	}
	// Sabotage the durable WRITE (not the read): make the config's parent dir
	// read-only so WriteToml's temp-create/rename fails, while the existing config
	// file stays READABLE (loadConfigForManage must still succeed so the handler
	// reaches the kill-first code). Restored in cleanup for TempDir teardown.
	dir := filepath.Dir(s.opts.ConfigPath)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod dir read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	rec := postConfirm(t, h, "/api/remote/disable", `{}`, ck, token)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("disable with an unwritable config = %d, want 500", rec.Code)
	}
	// The SECURITY half must have happened FIRST regardless: verifier + gate dead,
	// writers killed.
	if mc.VerifyStandingTerminalControl(secret, "dev", "h") {
		t.Fatal("standing secret still verifies after a failed-persist disable — fail-open")
	}
	if mc.AllowTerminal() {
		t.Fatal("live allow_terminal still ON after a failed-persist disable — single-use path still open")
	}
	killed := false
	for _, reason := range lm.revokeReasons() {
		if strings.Contains(reason, "remote disabled") {
			killed = true
		}
	}
	if !killed {
		t.Fatalf("failed-persist disable did not kill live remote writers; reasons=%v", lm.revokeReasons())
	}
}
