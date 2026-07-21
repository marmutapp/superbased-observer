package remotecfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/remoteauth"
)

// loadPersisted reloads the persisted global config file.
func loadPersisted(t *testing.T, cfgPath string) (config.Config, error) {
	t.Helper()
	return config.Load(config.LoadOptions{GlobalPath: cfgPath})
}

// standingBase arms a base config (remote enabled + allow_terminal) suitable
// for StandingTerminalEnable, via the real Enable transaction.
func standingBase(t *testing.T) (cfgPath string, secretPath string, firstSecret string) {
	t.Helper()
	cfg, cfgPath := baseCfg(t)
	if _, err := Enable(cfg, cfgPath, EnableOptions{Host: "box.ts.net", AllowTerminal: true}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	// Reload the ARMED config Enable persisted (trusted_hosts, backend addr,
	// allow_terminal) rather than hand-mirroring it.
	cfg, err := loadPersisted(t, cfgPath)
	if err != nil {
		t.Fatalf("reload armed config: %v", err)
	}
	info, err := StandingTerminalEnable(cfg, cfgPath)
	if err != nil {
		t.Fatalf("StandingTerminalEnable (first): %v", err)
	}
	if info.Rotated {
		t.Fatal("first enable must not report rotated")
	}
	return cfgPath, info.SecretPath, info.EncodedSecret
}

// readHash reads the standing hash-at-rest.
func readHash(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read standing hash: %v", err)
	}
	return strings.TrimSpace(string(b))
}

// TestStandingRotateFailureRestoresPriorHash pins the finding-7 fix: when the
// config persist of a ROTATE fails, the PRIOR (operator-known) hash is restored
// — the process must never be left with a live-but-unknown fresh secret.
func TestStandingRotateFailureRestoresPriorHash(t *testing.T) {
	cfgPath, secretPath, firstSecret := standingBase(t)
	priorHash := readHash(t, secretPath)

	// Rebuild a cfg equivalent to the persisted state (standing already on).
	dir := filepath.Dir(cfgPath)
	cfg, err := loadPersisted(t, cfgPath)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	if !cfg.Remote.AllowStandingTerminalControl {
		t.Fatal("persisted config should have standing enabled after the first mint")
	}

	// Force the rotate's config persist to fail: point cfgPath under a PATH
	// COMPONENT that is a regular file, so WriteToml's MkdirAll errors.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	badCfgPath := filepath.Join(blocker, "config.toml")

	if _, err := StandingTerminalEnable(cfg, badCfgPath); err == nil {
		t.Fatal("rotate with an unwritable config path must fail")
	}

	// The prior hash must be back in place, and the ORIGINAL secret must still
	// verify against it — no live-but-unknown secret.
	afterHash := readHash(t, secretPath)
	if afterHash != priorHash {
		t.Fatalf("failed rotate did not restore the prior hash:\n prior=%s\n after=%s", priorHash, afterHash)
	}
	raw, err := remoteauth.DecodeStandingSecret(firstSecret)
	if err != nil {
		t.Fatalf("DecodeStandingSecret: %v", err)
	}
	if !remoteauth.VerifySecret(afterHash, raw) {
		t.Fatal("original standing secret no longer verifies after a failed rotate")
	}
}

// TestStandingFirstEnableFailureRemovesFile pins the companion path: a FAILED
// first enable leaves no orphan hash file (no half-enabled state).
func TestStandingFirstEnableFailureRemovesFile(t *testing.T) {
	cfg, cfgPath := baseCfg(t)
	if _, err := Enable(cfg, cfgPath, EnableOptions{Host: "box.ts.net", AllowTerminal: true}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	cfg, err := loadPersisted(t, cfgPath)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	dir := filepath.Dir(cfgPath)
	blocker := filepath.Join(dir, "blocker2")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	if _, err := StandingTerminalEnable(cfg, filepath.Join(blocker, "config.toml")); err == nil {
		t.Fatal("first enable with an unwritable config path must fail")
	}
	if _, err := os.Stat(StandingTerminalSecretPath(cfg)); !os.IsNotExist(err) {
		t.Fatalf("failed first enable left the standing hash file behind (stat err=%v)", err)
	}
}
