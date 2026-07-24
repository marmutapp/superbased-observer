package diag

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
)

// writeWindowsClaudeRoute writes a Windows-side .claude/settings.json with
// the given ANTHROPIC_BASE_URL under a fresh home dir, returning the home.
func writeWindowsClaudeRoute(t *testing.T, url string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"env":{"ANTHROPIC_BASE_URL":"` + url + `"}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// writeWindowsClaudeDirOnly creates a Windows-side .claude/ with no route.
func writeWindowsClaudeDirOnly(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	return home
}

func okProbe(code int) windowsCurlProbe {
	return func(context.Context, string) (int, error) { return code, nil }
}

func failProbe() windowsCurlProbe {
	return func(context.Context, string) (int, error) { return 0, errors.New("curl.exe unavailable") }
}

// ownsAll / ownsNone are the two deterministic ownership seams the tests inject
// so ambiguous / unverified cross-OS homes are exercised without a cmd.exe
// interop shell.
func ownsAll(string) bool  { return true }
func ownsNone(string) bool { return false }

func TestWindowsProxyRoutes_SkipsWhenNotWSL(t *testing.T) {
	home := writeWindowsClaudeRoute(t, "http://localhost:8820")
	homes := []crossmount.HomeRoot{{Path: home, OS: crossmount.OSWindows}}
	_, ok := windowsProxyRoutesCheck(context.Background(), config.Config{}, false, homes, ownsAll, okProbe(200))
	if ok {
		t.Error("expected no check row when not WSL")
	}
}

func TestWindowsProxyRoutes_SkipsWhenNoWindowsHome(t *testing.T) {
	// A Linux home with a .claude must not be treated as a Windows target.
	home := writeWindowsClaudeRoute(t, "http://localhost:8820")
	homes := []crossmount.HomeRoot{{Path: home, OS: crossmount.OSLinux}}
	_, ok := windowsProxyRoutesCheck(context.Background(), config.Config{}, true, homes, ownsAll, okProbe(200))
	if ok {
		t.Error("expected no check row when no windows home detected")
	}
}

func TestWindowsProxyRoutes_RoutedAndReachable(t *testing.T) {
	home := writeWindowsClaudeRoute(t, "http://localhost:8820")
	homes := []crossmount.HomeRoot{{Path: home, OS: crossmount.OSWindows}}
	cfg := config.Config{}
	cfg.Proxy.Port = 8820
	c, ok := windowsProxyRoutesCheck(context.Background(), cfg, true, homes, ownsAll, okProbe(200))
	if !ok {
		t.Fatal("expected a check row")
	}
	if c.Status != StatusOK {
		t.Errorf("status = %v want OK", c.Status)
	}
	joined := strings.Join(c.Details, "\n")
	if !strings.Contains(joined, "routed in") || !strings.Contains(joined, "reachable from Windows") {
		t.Errorf("details missing routed/reachable lines:\n%s", joined)
	}
}

func TestWindowsProxyRoutes_RoutedButUnverifiable(t *testing.T) {
	home := writeWindowsClaudeRoute(t, "http://localhost:8820")
	homes := []crossmount.HomeRoot{{Path: home, OS: crossmount.OSWindows}}
	cfg := config.Config{}
	cfg.Proxy.Port = 8820
	c, ok := windowsProxyRoutesCheck(context.Background(), cfg, true, homes, ownsAll, failProbe())
	if !ok {
		t.Fatal("expected a check row")
	}
	if c.Status != StatusOK {
		t.Errorf("status = %v want OK (unverifiable must not fail)", c.Status)
	}
	if c.Status == StatusFail {
		t.Error("must never be StatusFail")
	}
	joined := strings.Join(c.Details, "\n")
	if !strings.Contains(joined, "reachability unverifiable") || !strings.Contains(joined, ".wslconfig") {
		t.Errorf("expected honest unverifiable note:\n%s", joined)
	}
}

func TestWindowsProxyRoutes_PresentButNotRouted(t *testing.T) {
	home := writeWindowsClaudeDirOnly(t)
	homes := []crossmount.HomeRoot{{Path: home, OS: crossmount.OSWindows}}
	// probe should not even be consulted (no route written); use a probe
	// that would fail the test if called with a success code expectation.
	c, ok := windowsProxyRoutesCheck(context.Background(), config.Config{}, true, homes, ownsAll, okProbe(200))
	if !ok {
		t.Fatal("expected a check row")
	}
	if c.Status != StatusWarn {
		t.Errorf("status = %v want Warn (dir present, not routed)", c.Status)
	}
	joined := strings.Join(c.Details, "\n")
	if !strings.Contains(joined, "NOT routed") {
		t.Errorf("expected not-routed detail:\n%s", joined)
	}
	// No reachability line when nothing is routed.
	if strings.Contains(joined, "reachable from Windows") {
		t.Errorf("unexpected reachability probe on an unrouted target:\n%s", joined)
	}
}

func TestWindowsProxyRoutes_AmbiguousHomesWarn(t *testing.T) {
	// Two Windows homes carrying .claude → the doctor WARNs, listing both
	// candidates and naming the WindowsClaudeHome override (F3).
	homeA := writeWindowsClaudeRoute(t, "http://localhost:8820")
	homeB := writeWindowsClaudeRoute(t, "http://localhost:8820")
	homes := []crossmount.HomeRoot{
		{Path: homeA, OS: crossmount.OSWindows},
		{Path: homeB, OS: crossmount.OSWindows},
	}
	cfg := config.Config{}
	cfg.Proxy.Port = 8820
	c, ok := windowsProxyRoutesCheck(context.Background(), cfg, true, homes, ownsNone, okProbe(200))
	if !ok {
		t.Fatal("expected a check row")
	}
	if c.Status != StatusWarn {
		t.Errorf("status = %v want Warn (ambiguous homes)", c.Status)
	}
	if c.Status == StatusFail {
		t.Error("must never be StatusFail")
	}
	joined := strings.Join(c.Details, "\n")
	if !strings.Contains(joined, "multiple Windows-side homes") || !strings.Contains(joined, "observer init --windows-claude-home") {
		t.Errorf("expected ambiguity WARN naming the real disambiguation flag:\n%s", joined)
	}
	if !strings.Contains(joined, filepath.Join(homeA, ".claude")) ||
		!strings.Contains(joined, filepath.Join(homeB, ".claude")) {
		t.Errorf("ambiguity WARN should list both candidate config homes:\n%s", joined)
	}
}

// TestWindowsProxyRoutes_OneOwnedAmongManyNoOwnershipWarn pins the healthy
// multi-user case: several Windows homes carry .claude but exactly ONE is
// owned by the current Windows user — resolveWindowsHome / the hook registrar
// auto-pick that owned home, so the doctor must NOT emit the ownership
// disambiguation WARN (a routed-state note for each detected home is fine).
func TestWindowsProxyRoutes_OneOwnedAmongManyNoOwnershipWarn(t *testing.T) {
	homeOwned := writeWindowsClaudeRoute(t, "http://localhost:8820")
	homeOther := writeWindowsClaudeRoute(t, "http://localhost:8820")
	homes := []crossmount.HomeRoot{
		{Path: homeOwned, OS: crossmount.OSWindows},
		{Path: homeOther, OS: crossmount.OSWindows},
	}
	owned := func(p string) bool { return p == homeOwned }
	cfg := config.Config{}
	cfg.Proxy.Port = 8820
	c, ok := windowsProxyRoutesCheck(context.Background(), cfg, true, homes, owned, okProbe(200))
	if !ok {
		t.Fatal("expected a check row")
	}
	joined := strings.Join(c.Details, "\n")
	if strings.Contains(joined, "will NOT auto-pick") || strings.Contains(joined, "could not verify") {
		t.Errorf("healthy one-owned-home setup must not emit an ownership WARN:\n%s", joined)
	}
}

// TestWindowsProxyRoutes_SingleUnverifiedWarn pins F2c: a SINGLE Windows-side
// .claude home whose ownership can't be verified is WARNed precisely (naming
// the exact home + the --windows-claude-home flag), not silently dropped.
func TestWindowsProxyRoutes_SingleUnverifiedWarn(t *testing.T) {
	home := writeWindowsClaudeDirOnly(t)
	homes := []crossmount.HomeRoot{{Path: home, OS: crossmount.OSWindows}}
	cfg := config.Config{}
	cfg.Proxy.Port = 8820
	c, ok := windowsProxyRoutesCheck(context.Background(), cfg, true, homes, ownsNone, okProbe(200))
	if !ok {
		t.Fatal("expected a check row")
	}
	if c.Status != StatusWarn {
		t.Errorf("status = %v want Warn (unverified ownership)", c.Status)
	}
	joined := strings.Join(c.Details, "\n")
	if !strings.Contains(joined, "could not verify it belongs to the current Windows user") {
		t.Errorf("expected a single-candidate ownership WARN:\n%s", joined)
	}
	if !strings.Contains(joined, filepath.Join(home, ".claude")) ||
		!strings.Contains(joined, "observer init --windows-claude-home") {
		t.Errorf("ownership WARN should name the home + recovery flag:\n%s", joined)
	}
}

// TestWindowsProxyRoutes_CursorAmbiguousWarn pins F2c's cursor coverage: cursor
// is hooks-only (no route), but ambiguous/unverified Windows-side .cursor homes
// still get a WARN naming --windows-cursor-home, so the operator has a recovery
// path.
func TestWindowsProxyRoutes_CursorAmbiguousWarn(t *testing.T) {
	homeA, homeB := t.TempDir(), t.TempDir()
	for _, h := range []string{homeA, homeB} {
		if err := os.MkdirAll(filepath.Join(h, ".cursor"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	homes := []crossmount.HomeRoot{
		{Path: homeA, OS: crossmount.OSWindows},
		{Path: homeB, OS: crossmount.OSWindows},
	}
	cfg := config.Config{}
	cfg.Proxy.Port = 8820
	c, ok := windowsProxyRoutesCheck(context.Background(), cfg, true, homes, ownsNone, okProbe(200))
	if !ok {
		t.Fatal("expected a check row for ambiguous cursor homes")
	}
	if c.Status != StatusWarn {
		t.Errorf("status = %v want Warn (ambiguous cursor homes)", c.Status)
	}
	joined := strings.Join(c.Details, "\n")
	if !strings.Contains(joined, "cursor-windows") || !strings.Contains(joined, "observer init --windows-cursor-home") {
		t.Errorf("expected a cursor-windows WARN naming --windows-cursor-home:\n%s", joined)
	}
}

func TestWindowsProxyRoutes_ThirdPartyURLNotConsideredRouted(t *testing.T) {
	// A non-loopback base URL is the operator's own choice — not an observer
	// route — so it reads as "present but not routed", never clobber-worthy.
	home := writeWindowsClaudeRoute(t, "https://api.anthropic.com")
	homes := []crossmount.HomeRoot{{Path: home, OS: crossmount.OSWindows}}
	c, ok := windowsProxyRoutesCheck(context.Background(), config.Config{}, true, homes, ownsAll, okProbe(200))
	if !ok {
		t.Fatal("expected a check row")
	}
	if c.Status != StatusWarn {
		t.Errorf("status = %v want Warn", c.Status)
	}
}
