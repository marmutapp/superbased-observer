package remotecfg

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// baseCfg returns a valid default config whose data dir is a fresh temp dir.
func baseCfg(t *testing.T) (config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Observer.DBPath = filepath.Join(dir, "observer.db")
	cfgPath := filepath.Join(dir, "config.toml")
	return cfg, cfgPath
}

func TestEnableProducesSecretAndConfig(t *testing.T) {
	cfg, cfgPath := baseCfg(t)
	info, err := Enable(cfg, cfgPath, EnableOptions{Host: "box.tail-scale.ts.net", AllowTerminal: true})
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if info.Host != "box.tail-scale.ts.net" || info.BackendAddr == "" || info.EncodedSecret == "" {
		t.Fatalf("PairingInfo incomplete: %+v", info)
	}
	if info.PairingURL != "https://box.tail-scale.ts.net/#pair="+info.EncodedSecret {
		t.Errorf("PairingURL = %q", info.PairingURL)
	}
	// Secret file written 0600 (POSIX).
	st, err := os.Stat(info.SecretPath)
	if err != nil {
		t.Fatalf("stat secret: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Errorf("secret file perm = %o, want 600", perm)
		}
	}
	// Config persisted + reloads with the armed [remote] block.
	loaded, err := config.Load(config.LoadOptions{GlobalPath: cfgPath})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	r := loaded.Remote
	if !r.Enabled || r.Mode != "tailscale" || !r.RequireTLS || !r.AllowTerminal {
		t.Errorf("armed [remote] = %+v", r)
	}
	if r.TailscaleBackendAddr == "" {
		t.Error("TailscaleBackendAddr not persisted")
	}
	if r.RateLimitPerMin != 6 {
		t.Errorf("RateLimitPerMin = %d, want default 6", r.RateLimitPerMin)
	}
	found := false
	for _, h := range r.TrustedHosts {
		if h == "box.tail-scale.ts.net" {
			found = true
		}
	}
	if !found {
		t.Errorf("trusted host not added: %v", r.TrustedHosts)
	}
}

func TestEnableRequiresHost(t *testing.T) {
	cfg, cfgPath := baseCfg(t)
	if _, err := Enable(cfg, cfgPath, EnableOptions{Host: "   "}); err == nil {
		t.Fatal("Enable accepted an empty host")
	}
}

func TestEnableRollsBackSecretOnPersistFailure(t *testing.T) {
	cfg, _ := baseCfg(t)
	// cfgPath points at an existing DIRECTORY → WriteToml fails; the secret must
	// be rolled back (removed) so a half-armed state is never left on disk.
	badCfgPath := t.TempDir()
	if _, err := Enable(cfg, badCfgPath, EnableOptions{Host: "box.ts.net"}); err == nil {
		t.Fatal("Enable succeeded writing config to a directory path")
	}
	if _, err := os.Stat(SecretPath(cfg)); !os.IsNotExist(err) {
		t.Errorf("secret file survived a persist failure (want rolled back): stat err=%v", err)
	}
}

func TestDisableRemovesSecret(t *testing.T) {
	cfg, cfgPath := baseCfg(t)
	if _, err := Enable(cfg, cfgPath, EnableOptions{Host: "box.ts.net"}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	removed, err := Disable(cfg, cfgPath)
	if err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if !removed {
		t.Error("Disable reported no secret removed")
	}
	if _, err := os.Stat(SecretPath(cfg)); !os.IsNotExist(err) {
		t.Error("secret file survived Disable")
	}
	loaded, _ := config.Load(config.LoadOptions{GlobalPath: cfgPath})
	if loaded.Remote.Enabled || loaded.Remote.Mode != "off" {
		t.Errorf("Disable left [remote] armed: %+v", loaded.Remote)
	}
	// Idempotent: a second Disable reports removed=false, not an error.
	if removed2, err := Disable(cfg, cfgPath); err != nil || removed2 {
		t.Errorf("second Disable: removed=%v err=%v", removed2, err)
	}
}

func TestRotateRequiresEnabled(t *testing.T) {
	cfg, cfgPath := baseCfg(t)
	if _, err := Rotate(cfg, cfgPath); err == nil {
		t.Fatal("Rotate succeeded while remote access disabled")
	}
}

func TestRotateMintsFreshSecret(t *testing.T) {
	cfg, cfgPath := baseCfg(t)
	info, err := Enable(cfg, cfgPath, EnableOptions{Host: "box.ts.net"})
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	firstHash, _ := os.ReadFile(info.SecretPath)
	// Rotate reads the enabled state from the reloaded config.
	loaded, _ := config.Load(config.LoadOptions{GlobalPath: cfgPath})
	rinfo, err := Rotate(loaded, cfgPath)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if rinfo.EncodedSecret == "" || rinfo.EncodedSecret == info.EncodedSecret {
		t.Error("Rotate did not mint a distinct secret")
	}
	if rinfo.Host != "box.ts.net" || rinfo.PairingURL == "" {
		t.Errorf("Rotate PairingInfo = %+v", rinfo)
	}
	secondHash, _ := os.ReadFile(info.SecretPath)
	if string(firstHash) == string(secondHash) {
		t.Error("Rotate did not rewrite the secret hash file")
	}
}

func TestBackendPortOnly(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:54321": ":54321",
		"[::1]:8080":      ":8080",
		"nonsense":        "nonsense",
	}
	for in, want := range cases {
		if got := BackendPortOnly(in); got != want {
			t.Errorf("BackendPortOnly(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAllowTerminalPreservedAcrossTransactions pins the plan §4.δ invariant that
// remotecfg NEVER infers allow_terminal from another remote setting and
// PRESERVES it across enable / rotate / disable. A Phase-4 regression that
// coupled it to another field would flip terminal control on silently.
func TestAllowTerminalPreservedAcrossTransactions(t *testing.T) {
	// Zero-value + default config both produce allow_terminal=false.
	if config.Default().Remote.AllowTerminal {
		t.Fatal("default config has allow_terminal=true — must default false")
	}
	var zero config.Config
	if zero.Remote.AllowTerminal {
		t.Fatal("zero-value config has allow_terminal=true — must default false")
	}

	// Enable WITHOUT terminal keeps it false; enable/rotate never turns it on.
	cfg, cfgPath := baseCfg(t)
	if _, err := Enable(cfg, cfgPath, EnableOptions{Host: "box.ts.net", AllowTerminal: false}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	loaded, _ := config.Load(config.LoadOptions{GlobalPath: cfgPath})
	if loaded.Remote.AllowTerminal {
		t.Fatal("enable(AllowTerminal=false) produced allow_terminal=true")
	}
	// rotate preserves the false value (never infers it on).
	rotated := loaded
	if _, err := Rotate(rotated, cfgPath); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if rotated.Remote.AllowTerminal {
		t.Fatal("rotate flipped allow_terminal to true")
	}

	// Now enable WITH terminal → true, and disable must LEAVE the field
	// untouched (it flips enabled/mode, never allow_terminal).
	cfg2, cfgPath2 := baseCfg(t)
	if _, err := Enable(cfg2, cfgPath2, EnableOptions{Host: "box.ts.net", AllowTerminal: true}); err != nil {
		t.Fatalf("Enable(terminal): %v", err)
	}
	loaded2, _ := config.Load(config.LoadOptions{GlobalPath: cfgPath2})
	if !loaded2.Remote.AllowTerminal {
		t.Fatal("enable(AllowTerminal=true) did not persist allow_terminal=true")
	}
	if _, err := Disable(loaded2, cfgPath2); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	disabled, _ := config.Load(config.LoadOptions{GlobalPath: cfgPath2})
	if !disabled.Remote.AllowTerminal {
		t.Fatal("disable() cleared allow_terminal — it must leave the field untouched")
	}
}
