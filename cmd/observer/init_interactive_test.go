package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/proxyroute"
)

// interactiveHome builds a sandbox home with a .claude dir (so
// claude-code detects) and returns it. Every registry in the
// interactive flow honours HomeDir, so the test can never touch the
// developer's real tool configs.
func interactiveHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	return home
}

// TestRunInteractiveInit_OneConsentPerWrite pins the wizard-parity
// consent semantics: hooks accepted → written; MCP declined (its
// default — never pre-selected) → NOT written; route accepted →
// written. Each answer maps to exactly one write.
func TestRunInteractiveInit_OneConsentPerWrite(t *testing.T) {
	home := interactiveHome(t)
	var out strings.Builder
	// hooks: enter (default yes) · mcp: enter (default NO) · route: y.
	// claude-code sorts first, so the three scripted answers are its.
	// The trailing "n" padding absorbs any environment-dependent extra
	// tools (crossmount detects *-windows variants from REAL foreign
	// homes regardless of the sandbox HomeDir) — every extra prompt
	// gets a decline, so the test can never write a real tool config.
	in := strings.NewReader("\n\ny\n" + strings.Repeat("n\n", 12))

	err := runInteractiveInit(&out, in, interactiveInitOptions{
		BinaryPath: filepath.Join(home, "observer"),
		ProxyPort:  18820,
		HomeDir:    home,
	})
	if err != nil {
		t.Fatalf("runInteractiveInit: %v\noutput:\n%s", err, out.String())
	}
	text := out.String()
	if !strings.Contains(text, "detected: claude-code") {
		t.Fatalf("detection line missing:\n%s", text)
	}
	if !strings.Contains(text, "~1,800 tokens") && !strings.Contains(text, "1,800 tokens") {
		t.Errorf("MCP honesty note missing:\n%s", text)
	}

	settings, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("settings.json not written: %v\noutput:\n%s", err, out.String())
	}
	if !strings.Contains(string(settings), "hooks") {
		t.Errorf("hooks consent given but no hook entries in settings.json:\n%s", settings)
	}
	if !strings.Contains(string(settings), "ANTHROPIC_BASE_URL") {
		t.Errorf("route consent given but no ANTHROPIC_BASE_URL in settings.json:\n%s", settings)
	}
	if !strings.Contains(string(settings), "18820") {
		t.Errorf("route did not carry the configured proxy port:\n%s", settings)
	}
	// MCP declined: no mcpServers entry may exist anywhere it would land.
	if raw, err := os.ReadFile(filepath.Join(home, ".claude.json")); err == nil &&
		strings.Contains(string(raw), "superbased") {
		t.Errorf("MCP was declined but an entry landed in .claude.json:\n%s", raw)
	}
}

// TestRunInteractiveInit_DeclineWritesNothing pins the other half: a
// run answering no to everything leaves the tool config untouched and
// prints the claude routing hint (the declined-route fallback).
func TestRunInteractiveInit_DeclineWritesNothing(t *testing.T) {
	home := interactiveHome(t)
	var out strings.Builder
	in := strings.NewReader(strings.Repeat("n\n", 15))

	err := runInteractiveInit(&out, in, interactiveInitOptions{
		BinaryPath: filepath.Join(home, "observer"),
		HomeDir:    home,
	})
	if err != nil {
		t.Fatalf("runInteractiveInit: %v\noutput:\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
		raw, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
		t.Errorf("all consents declined but settings.json exists:\n%s", raw)
	}
	// At least claude-code's three integrations decline; crossmount-
	// detected extra tools may add more skips on some machines.
	if got := strings.Count(out.String(), "skipped."); got < 3 {
		t.Errorf("skipped lines = %d, want >= 3 (hooks, mcp, route):\n%s", got, out.String())
	}
	if !strings.Contains(out.String(), "ANTHROPIC_BASE_URL") {
		t.Errorf("declined claude route should print the manual routing hint:\n%s", out.String())
	}
}

// TestRunInteractiveInit_EOFAborts pins the closed-stdin contract:
// the flow stops with an error and performs no further writes.
func TestRunInteractiveInit_EOFAborts(t *testing.T) {
	home := interactiveHome(t)
	var out strings.Builder
	in := strings.NewReader("") // immediate EOF at the first prompt

	err := runInteractiveInit(&out, in, interactiveInitOptions{
		BinaryPath: filepath.Join(home, "observer"),
		HomeDir:    home,
	})
	if err == nil {
		t.Fatalf("want error on closed stdin, got nil\noutput:\n%s", out.String())
	}
	if _, statErr := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(statErr) {
		t.Error("EOF abort still wrote settings.json")
	}
}

// NOTE: the "no tools detected" branch is not unit-testable in a
// sandbox — crossmount detection of *-windows variants reads REAL
// foreign-OS homes regardless of HomeDir, so a dev box always detects
// something. The branch is two lines; covered by inspection.

// TestRunWindowsRouteDisambiguation_PicksHome pins the R4 numbered picker:
// given two unresolved Windows homes it prints them numbered, and choosing "1"
// completes without error (the chosen home feeds a one-off override registrar;
// the proxyroute writer's override behaviour is covered by
// TestRegisterClaudeCodeWindows_WritesLocalhostRoute under forced WSL).
// Candidate homes are temp dirs so any write on a real WSL host stays in temp.
func TestRunWindowsRouteDisambiguation_PicksHome(t *testing.T) {
	homeA, homeB := t.TempDir(), t.TempDir()
	cands := map[string][]string{"claude-code-windows": {homeA, homeB}}
	var out strings.Builder
	p := &initPrompter{in: bufio.NewReader(strings.NewReader("1\n")), out: &out}
	if err := runWindowsRouteDisambiguation(&out, p, cands, 18820, interactiveInitOptions{HomeDir: t.TempDir()}); err != nil {
		t.Fatalf("runWindowsRouteDisambiguation: %v\n%s", err, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "claude-code-windows") {
		t.Errorf("picker should name the tool label:\n%s", s)
	}
	if !strings.Contains(s, "1) "+homeA) || !strings.Contains(s, "2) "+homeB) {
		t.Errorf("picker should list both homes numbered:\n%s", s)
	}
}

// TestRunWindowsRouteDisambiguation_WiresHooksIntoPickedHome pins F1.d + F1's
// separate-consent contract: after picking home 1 and CONSENTING to the hook
// prompt ("1" then "y"), the picked Windows home drives BOTH the proxy route
// and the hook registrar (claude-code only), so both land in the SAME home
// within one run. WSL is forced on and an explicit distro is supplied so the
// cross-OS writers engage on any host; the picked home is a temp dir.
func TestRunWindowsRouteDisambiguation_WiresHooksIntoPickedHome(t *testing.T) {
	defer proxyroute.SetWSLForTest(true)()
	homeA, homeB := t.TempDir(), t.TempDir()
	cands := map[string][]string{"claude-code-windows": {homeA, homeB}}
	var out strings.Builder
	// pick 1, then consent to the separate hook prompt.
	p := &initPrompter{in: bufio.NewReader(strings.NewReader("1\ny\n")), out: &out}
	err := runWindowsRouteDisambiguation(&out, p, cands, 18820, interactiveInitOptions{
		BinaryPath: "/bin/observer",
		HomeDir:    t.TempDir(),
		WSLDistro:  "Ubuntu",
	})
	if err != nil {
		t.Fatalf("runWindowsRouteDisambiguation: %v\n%s", err, out.String())
	}
	// The route AND the hooks must have landed in the PICKED home (homeA), not homeB.
	settings := filepath.Join(homeA, ".claude", "settings.json")
	body, rerr := os.ReadFile(settings)
	if rerr != nil {
		t.Fatalf("picked home settings.json missing (route+hooks should have written it): %v\n%s", rerr, out.String())
	}
	if !strings.Contains(string(body), "hook claude-code") {
		t.Errorf("picked home settings.json missing observer hooks:\n%s", body)
	}
	if !strings.Contains(string(body), "ANTHROPIC_BASE_URL") {
		t.Errorf("picked home settings.json missing the proxy route:\n%s", body)
	}
	if _, serr := os.Stat(filepath.Join(homeB, ".claude", "settings.json")); serr == nil {
		t.Errorf("unpicked home homeB must not have been written")
	}
}

// TestRunWindowsRouteDisambiguation_HookDeclinedWritesRouteOnly pins F1's
// separate consent: picking a home and DECLINING the hook prompt ("1" then
// "n") writes the proxy route but leaves hooks unwritten.
func TestRunWindowsRouteDisambiguation_HookDeclinedWritesRouteOnly(t *testing.T) {
	defer proxyroute.SetWSLForTest(true)()
	homeA := t.TempDir()
	cands := map[string][]string{"claude-code-windows": {homeA, t.TempDir()}}
	var out strings.Builder
	p := &initPrompter{in: bufio.NewReader(strings.NewReader("1\nn\n")), out: &out}
	err := runWindowsRouteDisambiguation(&out, p, cands, 18820, interactiveInitOptions{
		BinaryPath: "/bin/observer",
		HomeDir:    t.TempDir(),
		WSLDistro:  "Ubuntu",
	})
	if err != nil {
		t.Fatalf("runWindowsRouteDisambiguation: %v\n%s", err, out.String())
	}
	body, rerr := os.ReadFile(filepath.Join(homeA, ".claude", "settings.json"))
	if rerr != nil {
		t.Fatalf("route should still have written settings.json: %v\n%s", rerr, out.String())
	}
	if !strings.Contains(string(body), "ANTHROPIC_BASE_URL") {
		t.Errorf("route consent given but no ANTHROPIC_BASE_URL:\n%s", body)
	}
	if strings.Contains(string(body), "hook claude-code") {
		t.Errorf("hook prompt declined but hooks were written:\n%s", body)
	}
}

// TestRunWindowsRouteDisambiguation_RouteRefusedSkipsHook pins F1's other half:
// when the route write REFUSES (an existing third-party ANTHROPIC_BASE_URL in
// the picked home), no hook prompt is offered and no hooks are written — the
// picker consumes only the "1" (no answer for a hook prompt is needed).
func TestRunWindowsRouteDisambiguation_RouteRefusedSkipsHook(t *testing.T) {
	defer proxyroute.SetWSLForTest(true)()
	homeA := t.TempDir()
	// Seed a conflicting third-party route the observer writer must refuse to
	// clobber (no --force in this path).
	claudeDir := filepath.Join(homeA, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"),
		[]byte(`{"env":{"ANTHROPIC_BASE_URL":"https://third-party.example/api"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cands := map[string][]string{"claude-code-windows": {homeA, t.TempDir()}}
	var out strings.Builder
	// Only "1" — if a hook prompt were (wrongly) offered, EOF would error.
	p := &initPrompter{in: bufio.NewReader(strings.NewReader("1\n")), out: &out}
	err := runWindowsRouteDisambiguation(&out, p, cands, 18820, interactiveInitOptions{
		BinaryPath: "/bin/observer",
		HomeDir:    t.TempDir(),
		WSLDistro:  "Ubuntu",
	})
	if err != nil {
		t.Fatalf("runWindowsRouteDisambiguation should not error on a refused route: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "route ✗") {
		t.Errorf("route refusal should be surfaced honestly:\n%s", out.String())
	}
	// The seeded third-party settings.json must be untouched — no hooks added.
	body, rerr := os.ReadFile(filepath.Join(claudeDir, "settings.json"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if strings.Contains(string(body), "hook claude-code") {
		t.Errorf("route refused but hooks were written into the picked home:\n%s", body)
	}
}

// TestRunWindowsRouteDisambiguation_Skip pins the skip branch: answering "n"
// prints "skipped." and writes nothing.
func TestRunWindowsRouteDisambiguation_Skip(t *testing.T) {
	cands := map[string][]string{"claude-code-windows": {t.TempDir(), t.TempDir()}}
	var out strings.Builder
	p := &initPrompter{in: bufio.NewReader(strings.NewReader("n\n")), out: &out}
	if err := runWindowsRouteDisambiguation(&out, p, cands, 18820, interactiveInitOptions{HomeDir: t.TempDir()}); err != nil {
		t.Fatalf("runWindowsRouteDisambiguation: %v", err)
	}
	if !strings.Contains(out.String(), "skipped.") {
		t.Errorf("declining should print 'skipped.':\n%s", out.String())
	}
}

// TestRunWindowsRouteDisambiguation_EOFAborts pins the closed-stdin contract:
// an immediate EOF at the picker returns an error, matching the rest of the
// interactive flow.
func TestRunWindowsRouteDisambiguation_EOFAborts(t *testing.T) {
	cands := map[string][]string{"claude-code-windows": {t.TempDir(), t.TempDir()}}
	var out strings.Builder
	p := &initPrompter{in: bufio.NewReader(strings.NewReader("")), out: &out}
	if err := runWindowsRouteDisambiguation(&out, p, cands, 18820, interactiveInitOptions{HomeDir: t.TempDir()}); err == nil {
		t.Fatalf("want error on closed stdin, got nil\n%s", out.String())
	}
}

// TestRunWindowsRouteDisambiguation_NoCandidatesIsNoOp: an empty/nil map is a
// clean no-op (nothing printed, no error) — the common case where every
// detected tool resolved cleanly.
func TestRunWindowsRouteDisambiguation_NoCandidatesIsNoOp(t *testing.T) {
	var out strings.Builder
	p := &initPrompter{in: bufio.NewReader(strings.NewReader("")), out: &out}
	if err := runWindowsRouteDisambiguation(&out, p, nil, 18820, interactiveInitOptions{HomeDir: t.TempDir()}); err != nil {
		t.Fatalf("nil candidates should be a no-op, got: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("no candidates should print nothing, got %q", out.String())
	}
}
