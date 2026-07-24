package proxyroute

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
)

// forceWSL pins the isWSL seam to want for the duration of a test (restored on
// cleanup), so the F4 gate is deterministic no matter whether the test host is
// a real WSL guest or a native Linux box.
func forceWSL(t *testing.T, want bool) {
	t.Helper()
	prev := isWSL
	isWSL = func() bool { return want }
	t.Cleanup(func() { isWSL = prev })
}

// forceHomes pins the allHomes seam to a fixed layout (restored on cleanup) so
// the F3 ambiguity branch can be exercised without a real /mnt/c mount.
func forceHomes(t *testing.T, homes []crossmount.HomeRoot) {
	t.Helper()
	prev := allHomes
	allHomes = func() []crossmount.HomeRoot { return homes }
	t.Cleanup(func() { allHomes = prev })
}

// forceWindowsUser pins the windowsUserName seam to name (restored on cleanup)
// so the R1 ownership check is deterministic and never shells out to cmd.exe.
// "" simulates interop-off / unknown-user (ownership unverifiable → refuse).
func forceWindowsUser(t *testing.T, name string) {
	t.Helper()
	prev := windowsUserName
	windowsUserName = func() string { return name }
	t.Cleanup(func() { windowsUserName = prev })
}

// newWindowsRegistrar builds a Registrar whose cross-OS writers target a
// temp Windows-home override, so the tests never touch a real /mnt/c home
// or depend on the ambient crossmount detection of the host they run on. The
// isWSL gate is forced ON so the override-driven write path runs deterministically.
func newWindowsRegistrar(t *testing.T, port int) (*Registrar, string) {
	t.Helper()
	forceWSL(t, true)
	winHome := t.TempDir()
	r, err := NewRegistrar(RegisterOptions{
		ProxyPort:         port,
		HomeDir:           t.TempDir(), // distinct native home, unused by the windows writers
		WindowsClaudeHome: winHome,
		WindowsCodexHome:  winHome,
	})
	if err != nil {
		t.Fatalf("NewRegistrar: %v", err)
	}
	return r, winHome
}

func TestWindowsBaseURL(t *testing.T) {
	if got := windowsBaseURL(8820); got != "http://localhost:8820" {
		t.Errorf("windowsBaseURL = %q", got)
	}
	// The value IsObserverBaseURL must recognize a localhost URL so a
	// re-run is idempotent rather than treated as a third-party proxy.
	if !IsObserverBaseURL(windowsBaseURL(8820)) {
		t.Error("IsObserverBaseURL should accept the localhost windows base URL")
	}
	if !IsObserverBaseURL(windowsBaseURL(8820) + "/v1") {
		t.Error("IsObserverBaseURL should accept the localhost /v1 windows base URL")
	}
}

func TestResolveWindowsHome_OverrideFirst(t *testing.T) {
	override := t.TempDir()
	dir, ambiguous := resolveWindowsHome(override, ".claude")
	want := filepath.Join(override, ".claude")
	if dir != want || ambiguous != nil {
		t.Errorf("override-first: got (%q, %v) want (%q, nil)", dir, ambiguous, want)
	}
	// Authoritative even when the dir does not exist yet.
	if _, err := os.Stat(dir); err == nil {
		t.Errorf("override dir should not have been created: %q", dir)
	}
}

func TestResolveWindowsHome_NotFound(t *testing.T) {
	// No Windows homes at all → deterministic (empty, nil): "not detected".
	forceHomes(t, nil)
	if dir, ambiguous := resolveWindowsHome("", ".claude"); dir != "" || ambiguous != nil {
		t.Errorf("expected (empty, nil), got (%q, %v)", dir, ambiguous)
	}
}

// TestResolveWindowsHome_SingleHomeHappy: exactly one Windows home carrying the
// config, OWNED by the current Windows user (base matches %USERNAME%), resolves
// to that home with no ambiguity (R1).
func TestResolveWindowsHome_SingleHomeHappy(t *testing.T) {
	winHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(winHome, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	forceHomes(t, []crossmount.HomeRoot{
		{OS: crossmount.OSWindows, Path: winHome},
		{OS: "linux", Path: t.TempDir()}, // a native home that must be ignored
	})
	// The home's leaf is the "username" — force interop to report it so
	// ownership verifies and the single home resolves.
	forceWindowsUser(t, filepath.Base(winHome))
	dir, ambiguous := resolveWindowsHome("", ".claude")
	if want := filepath.Join(winHome, ".claude"); dir != want || ambiguous != nil {
		t.Errorf("single-home: got (%q, %v) want (%q, nil)", dir, ambiguous, want)
	}
}

// TestResolveWindowsHome_SingleHomeUnownedRefused: a SINGLE Windows home whose
// leaf does NOT match the current Windows user is REFUSED, not auto-picked —
// the core R1 security fix (a WSL daemon must not rewrite another user's
// config just because theirs is the only one mounted).
func TestResolveWindowsHome_SingleHomeUnownedRefused(t *testing.T) {
	winHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(winHome, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	forceHomes(t, []crossmount.HomeRoot{{OS: crossmount.OSWindows, Path: winHome}})
	forceWindowsUser(t, "someone-else") // does not match filepath.Base(winHome)
	dir, refuse := resolveWindowsHome("", ".claude")
	if dir != "" {
		t.Errorf("unowned single home should be refused, got dir %q", dir)
	}
	if len(refuse) != 1 || refuse[0] != filepath.Join(winHome, ".claude") {
		t.Errorf("refuse should list the single unowned candidate, got %v", refuse)
	}
}

// TestResolveWindowsHome_UnknownUserRefused: when interop can't name the
// current user ("" — interop off), ownership is unverifiable and even a single
// candidate is refused (R1). An explicit override still cuts through.
func TestResolveWindowsHome_UnknownUserRefused(t *testing.T) {
	winHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(winHome, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	forceHomes(t, []crossmount.HomeRoot{{OS: crossmount.OSWindows, Path: winHome}})
	forceWindowsUser(t, "")
	if dir, refuse := resolveWindowsHome("", ".claude"); dir != "" || len(refuse) != 1 {
		t.Errorf("unknown user: got (%q, %v) want refusal with 1 candidate", dir, refuse)
	}
	// Override wins unconditionally even when the user is unknown.
	override := t.TempDir()
	if d, amb := resolveWindowsHome(override, ".claude"); d != filepath.Join(override, ".claude") || amb != nil {
		t.Errorf("override should resolve even with unknown user: got (%q, %v)", d, amb)
	}
}

// TestResolveWindowsHome_MultiHomeAmbiguous: two Windows homes carrying the same
// config refuse to auto-pick — dir empty, both candidates returned (F3).
func TestResolveWindowsHome_MultiHomeAmbiguous(t *testing.T) {
	homeA, homeB := t.TempDir(), t.TempDir()
	for _, h := range []string{homeA, homeB} {
		if err := os.MkdirAll(filepath.Join(h, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	forceHomes(t, []crossmount.HomeRoot{
		{OS: crossmount.OSWindows, Path: homeA},
		{OS: crossmount.OSWindows, Path: homeB},
	})
	forceWindowsUser(t, "") // ownership unverifiable → refuse listing both
	dir, ambiguous := resolveWindowsHome("", ".claude")
	if dir != "" {
		t.Errorf("ambiguous: expected empty dir, got %q", dir)
	}
	if len(ambiguous) != 2 {
		t.Fatalf("ambiguous: expected 2 candidates, got %v", ambiguous)
	}
	// An explicit override cuts through the ambiguity.
	override := t.TempDir()
	if d, amb := resolveWindowsHome(override, ".claude"); d != filepath.Join(override, ".claude") || amb != nil {
		t.Errorf("override should disambiguate: got (%q, %v)", d, amb)
	}
}

// TestRegisterClaudeCodeWindows_AmbiguousRefused: the register writer refuses an
// ambiguous multi-home layout with an error naming the override option, and
// writes nothing (F3).
func TestRegisterClaudeCodeWindows_AmbiguousRefused(t *testing.T) {
	forceWSL(t, true)
	homeA, homeB := t.TempDir(), t.TempDir()
	for _, h := range []string{homeA, homeB} {
		if err := os.MkdirAll(filepath.Join(h, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	forceHomes(t, []crossmount.HomeRoot{
		{OS: crossmount.OSWindows, Path: homeA},
		{OS: crossmount.OSWindows, Path: homeB},
	})
	forceWindowsUser(t, "") // ownership unverifiable → refuse
	r, err := NewRegistrar(RegisterOptions{ProxyPort: 8820, HomeDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	res := r.RegisterClaudeCodeWindows()
	if res.Error == nil {
		t.Fatal("expected an error refusing to auto-pick an ambiguous layout")
	}
	if !strings.Contains(res.Error.Error(), "WindowsClaudeHome") {
		t.Errorf("error should name the WindowsClaudeHome override: %v", res.Error)
	}
	if res.Added {
		t.Error("nothing should have been written on an ambiguity refusal")
	}
	// No settings.json written into either home.
	for _, h := range []string{homeA, homeB} {
		if _, err := os.Stat(filepath.Join(h, ".claude", "settings.json")); err == nil {
			t.Errorf("settings.json must not have been written into %q", h)
		}
	}
}

// TestRegisterClaudeCodeWindows_SingleUnownedMessage pins F2b: when exactly ONE
// Windows-side .claude/ home is detected but ownership can't be verified, the
// refusal names the SPECIFIC home found and the real --windows-claude-home flag
// — it must NOT falsely imply several candidates.
func TestRegisterClaudeCodeWindows_SingleUnownedMessage(t *testing.T) {
	forceWSL(t, true)
	winHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(winHome, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	forceHomes(t, []crossmount.HomeRoot{{OS: crossmount.OSWindows, Path: winHome}})
	forceWindowsUser(t, "someone-else") // single home, not owned → refuse
	r, err := NewRegistrar(RegisterOptions{ProxyPort: 8820, HomeDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	res := r.RegisterClaudeCodeWindows()
	if res.Error == nil {
		t.Fatal("expected a refusal error for a single unowned home")
	}
	msg := res.Error.Error()
	if !strings.Contains(msg, "found "+filepath.Join(winHome, ".claude")) {
		t.Errorf("message should name the exact single home found: %v", msg)
	}
	if !strings.Contains(msg, "could not verify it belongs to the current Windows user") {
		t.Errorf("message should be single-candidate honest: %v", msg)
	}
	if !strings.Contains(msg, "--windows-claude-home") {
		t.Errorf("message should name the real disambiguation flag: %v", msg)
	}
	if strings.Contains(msg, "multiple") {
		t.Errorf("single-candidate message must not imply plural candidates: %v", msg)
	}
}

// TestRegisterWindows_NotWSLSkips: off WSL the register writers benignly skip
// (ConfigMissing, no error, no write) — F4.
func TestRegisterWindows_NotWSLSkips(t *testing.T) {
	forceWSL(t, false)
	winHome := t.TempDir()
	r, err := NewRegistrar(RegisterOptions{
		ProxyPort:         8820,
		HomeDir:           t.TempDir(),
		WindowsClaudeHome: winHome,
		WindowsCodexHome:  winHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range []RegistrationResult{r.RegisterClaudeCodeWindows(), r.RegisterCodexWindows()} {
		if res.Error != nil {
			t.Errorf("%s: expected benign skip off WSL, got error %v", res.Tool, res.Error)
		}
		if !res.ConfigMissing {
			t.Errorf("%s: expected ConfigMissing skip off WSL: %+v", res.Tool, res)
		}
		if res.Added {
			t.Errorf("%s: nothing should be written off WSL", res.Tool)
		}
	}
	if _, err := os.Stat(filepath.Join(winHome, ".claude", "settings.json")); err == nil {
		t.Error("settings.json must not be written off WSL")
	}
	// WindowsRouteTargets is empty off WSL too.
	if tgts := r.WindowsRouteTargets(); len(tgts) != 0 {
		t.Errorf("WindowsRouteTargets off WSL = %v, want empty", tgts)
	}
}

// TestWindowsRouteTargets_ExcludesAmbiguous: a tool whose config is present in
// two Windows homes is omitted from the batch-init target set (F3), while an
// unambiguous tool remains.
func TestWindowsRouteTargets_ExcludesAmbiguous(t *testing.T) {
	forceWSL(t, true)
	homeA, homeB, codexHome := t.TempDir(), t.TempDir(), t.TempDir()
	for _, h := range []string{homeA, homeB} {
		if err := os.MkdirAll(filepath.Join(h, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(codexHome, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	forceHomes(t, []crossmount.HomeRoot{
		{OS: crossmount.OSWindows, Path: homeA},     // .claude → ambiguous with homeB
		{OS: crossmount.OSWindows, Path: homeB},     // .claude
		{OS: crossmount.OSWindows, Path: codexHome}, // .codex → unambiguous
	})
	// Own the codex home (its leaf == %USERNAME%) but neither claude home:
	// codex resolves (single owned) while claude stays ambiguous/refused.
	forceWindowsUser(t, filepath.Base(codexHome))
	r, err := NewRegistrar(RegisterOptions{ProxyPort: 8820, HomeDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	got := r.WindowsRouteTargets()
	if len(got) != 1 || got[0] != "codex-windows" {
		t.Errorf("WindowsRouteTargets = %v, want [codex-windows] (ambiguous claude excluded)", got)
	}
}

func TestRegisterClaudeCodeWindows_WritesLocalhostRoute(t *testing.T) {
	r, winHome := newWindowsRegistrar(t, 8820)
	res := r.RegisterClaudeCodeWindows()
	if res.Error != nil {
		t.Fatalf("RegisterClaudeCodeWindows: %v", res.Error)
	}
	if !res.Added || res.AlreadySet {
		t.Errorf("expected Added=true AlreadySet=false: %+v", res)
	}
	if res.Tool != "claude-code-windows" {
		t.Errorf("Tool = %q", res.Tool)
	}
	if res.BaseURL != "http://localhost:8820" {
		t.Errorf("BaseURL = %q", res.BaseURL)
	}

	settings := readClaudeSettings(t, winHome)
	env, _ := settings["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != "http://localhost:8820" {
		t.Errorf("ANTHROPIC_BASE_URL = %v", env["ANTHROPIC_BASE_URL"])
	}

	// Re-run is idempotent (AlreadySet, no error).
	res2 := r.RegisterClaudeCodeWindows()
	if res2.Error != nil || !res2.AlreadySet {
		t.Errorf("re-run expected AlreadySet=true err=nil: %+v", res2)
	}
}

func TestRegisterCodexWindows_WritesLocalhostV1Route(t *testing.T) {
	r, winHome := newWindowsRegistrar(t, 8820)
	res := r.RegisterCodexWindows()
	if res.Error != nil {
		t.Fatalf("RegisterCodexWindows: %v", res.Error)
	}
	if !res.Added || res.AlreadySet {
		t.Errorf("expected Added=true AlreadySet=false: %+v", res)
	}
	if res.Tool != "codex-windows" {
		t.Errorf("Tool = %q", res.Tool)
	}
	if res.BaseURL != "http://localhost:8820/v1" {
		t.Errorf("BaseURL = %q", res.BaseURL)
	}

	body, err := os.ReadFile(filepath.Join(winHome, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := toml.Unmarshal(body, &root); err != nil {
		t.Fatalf("parse config.toml: %v\n%s", err, body)
	}
	if mp, _ := root["model_provider"].(string); mp != ProviderName {
		t.Errorf("model_provider = %q want %q", mp, ProviderName)
	}
	providers, _ := root["model_providers"].(map[string]any)
	ours, _ := providers[ProviderName].(map[string]any)
	if ours["base_url"] != "http://localhost:8820/v1" {
		t.Errorf("base_url = %v", ours["base_url"])
	}

	res2 := r.RegisterCodexWindows()
	if res2.Error != nil || !res2.AlreadySet {
		t.Errorf("re-run expected AlreadySet=true err=nil: %+v", res2)
	}
}

func TestWindowsRouteTargets_OverridePresent(t *testing.T) {
	r, _ := newWindowsRegistrar(t, 8820)
	got := r.WindowsRouteTargets()
	want := []string{"claude-code-windows", "codex-windows"}
	if len(got) != len(want) {
		t.Fatalf("WindowsRouteTargets = %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("WindowsRouteTargets[%d] = %q want %q", i, got[i], want[i])
		}
	}
}

func TestWindowsRouteTargets_NoneWhenUnset(t *testing.T) {
	// No overrides + a native-only Registrar. On a host with no /mnt/c the
	// result is empty; on a WSL host with a real Windows .claude it may be
	// non-empty. Assert only that any element is a known virtual target so
	// the test is stable everywhere. Force the user unknown so the R1
	// ownership check can never resolve a real host home into the result.
	forceWindowsUser(t, "")
	r, err := NewRegistrar(RegisterOptions{ProxyPort: 8820, HomeDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, tgt := range r.WindowsRouteTargets() {
		if tgt != "claude-code-windows" && tgt != "codex-windows" {
			t.Errorf("unexpected target %q", tgt)
		}
	}
}

// TestWindowsRouteCandidates_UnresolvedListed: a tool whose Windows-side config
// is present but unresolvable (ambiguous claude, unverifiable ownership) is
// reported by WindowsRouteCandidates with its USER-home candidates, while a
// cleanly-owned tool (codex) is omitted (it resolves, so no picker needed).
func TestWindowsRouteCandidates_UnresolvedListed(t *testing.T) {
	forceWSL(t, true)
	homeA, homeB, codexHome := t.TempDir(), t.TempDir(), t.TempDir()
	for _, h := range []string{homeA, homeB} {
		if err := os.MkdirAll(filepath.Join(h, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(codexHome, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	forceHomes(t, []crossmount.HomeRoot{
		{OS: crossmount.OSWindows, Path: homeA},
		{OS: crossmount.OSWindows, Path: homeB},
		{OS: crossmount.OSWindows, Path: codexHome},
	})
	forceWindowsUser(t, filepath.Base(codexHome)) // owns codex, neither claude
	r, err := NewRegistrar(RegisterOptions{ProxyPort: 8820, HomeDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cands := r.WindowsRouteCandidates()
	if _, ok := cands["codex-windows"]; ok {
		t.Errorf("codex resolved cleanly (owned) — must NOT appear in candidates: %v", cands)
	}
	got := cands["claude-code-windows"]
	if len(got) != 2 {
		t.Fatalf("claude-code-windows candidates = %v, want 2 USER homes", got)
	}
	// Candidates are USER homes (feedable as the override), not the .claude dir.
	want := map[string]bool{homeA: true, homeB: true}
	for _, h := range got {
		if !want[h] {
			t.Errorf("unexpected candidate %q (want one of %v)", h, want)
		}
	}
}

// TestWindowsRouteCandidates_NoneWhenResolvedOrEmpty: when every detected tool
// resolves cleanly (owned single home) OR nothing is detected, the map is nil;
// and it is nil off WSL regardless.
func TestWindowsRouteCandidates_NoneWhenResolvedOrEmpty(t *testing.T) {
	// Owned single claude home → resolves → no candidates.
	winHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(winHome, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	forceWSL(t, true)
	forceHomes(t, []crossmount.HomeRoot{{OS: crossmount.OSWindows, Path: winHome}})
	forceWindowsUser(t, filepath.Base(winHome))
	r, err := NewRegistrar(RegisterOptions{ProxyPort: 8820, HomeDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if cands := r.WindowsRouteCandidates(); cands != nil {
		t.Errorf("resolved single owned home should yield nil candidates, got %v", cands)
	}

	// Off WSL → nil regardless of detected homes.
	forceWSL(t, false)
	if cands := r.WindowsRouteCandidates(); cands != nil {
		t.Errorf("off WSL candidates should be nil, got %v", cands)
	}
}
