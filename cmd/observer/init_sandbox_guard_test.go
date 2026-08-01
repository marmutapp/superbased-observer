package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// urlRe strips full URLs before path scanning so "http://localhost:8820" is
// not mistaken for a filesystem path.
var urlRe = regexp.MustCompile(`https?://[^\s"',)\]]+`)

// absPathRe matches filesystem-looking absolute paths in registrar output.
var absPathRe = regexp.MustCompile(`/[A-Za-z0-9._+@\-]+(?:/[A-Za-z0-9._+@\-]+)+`)

// TestWireAIClients_SandboxHomeNeverEscapes is the standing guard for the
// 2026-07-31 config-pollution incident (full story:
// crossmount.AutoDetectSuppressed).
//
// WHAT HAPPENED: a test called wireAIClients with HomeDir at a t.TempDir(),
// assuming that sandboxed every write. The "claude-code-windows" hook target
// does not resolve through HomeDir — it walks crossmount for a Windows-side
// .claude — so on this WSL host it resolved the operator's REAL
// /mnt/c/Users/<u>/.claude and wrote 22 test-binary hook entries into it.
//
// WHAT THIS PINS: with a sandbox HomeDir and NO Windows-side overrides, EVERY
// path wireAIClients reports (each registrar's ConfigPath, i.e. exactly the
// files it would write) lies inside the sandbox or the test tempdir. A future
// target that resolves a home some other way — crossmount, an env var, an
// ambient probe — fails here the moment it reports a path outside.
//
// DRY RUN IS LOAD-BEARING: the guard must stay SAFE to run while red. Under
// DryRun every registrar computes its ConfigPath exactly as it would for a
// real write but touches nothing, so a regression is caught by the reported
// path rather than by damage on disk.
func TestWireAIClients_SandboxHomeNeverEscapes(t *testing.T) {
	sandbox := t.TempDir()
	// Give the sandbox every native tool dir so the wire has real work to do
	// and the assertion is not vacuous.
	for _, d := range []string{".claude", ".cursor", ".codex", ".factory", ".commandcode"} {
		if err := os.MkdirAll(filepath.Join(sandbox, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	lines, _, _, _, err := wireAIClients(WireAIClientsOptions{
		ProxyPort: 18820,
		DryRun:    true,
		HomeDir:   sandbox,
		All:       true,
		// Deliberately NO WindowsClaudeHome / WindowsCursorHome /
		// WindowsCodexHome: this is precisely the shape that leaked.
	})
	if err != nil {
		t.Fatalf("wireAIClients: %v", err)
	}
	text := strings.Join(lines, "\n")
	if strings.TrimSpace(text) == "" {
		t.Fatalf("wire produced no output — the guard would be vacuous")
	}

	allowed := allowedSandboxRoots(t, sandbox)
	// The allow-list is the SANDBOX ROOT ITSELF — deliberately not
	// os.TempDir(): a sibling t.TempDir() is exactly the "uncontained
	// override" shape the follow-up round closed, and permitting all of /tmp
	// would let it pass this guard.
	for _, line := range lines {
		for _, p := range absPathRe.FindAllString(urlRe.ReplaceAllString(line, ""), -1) {
			if !underAnyRoot(p, allowed) {
				t.Errorf("wireAIClients with a sandbox HomeDir reported a path OUTSIDE the sandbox: %q\n"+
					"  line: %s\n  allowed roots: %v\n"+
					"  a cross-OS target resolved its home outside the caller's sandbox — see "+
					"crossmount.AutoDetectSuppressed (incident 2026-07-31)", p, line, allowed)
			}
		}
	}

	// Belt and braces: the cross-OS virtual targets must not even be
	// attempted under a pinned home (they are the ones that escape).
	for _, tool := range []string{"claude-code-windows", "cursor-windows", "codex-windows", "cline-windows"} {
		for _, line := range lines {
			if strings.Contains(line, tool) && !strings.Contains(line, "skipped") {
				t.Errorf("cross-OS target %s was wired under a sandbox HomeDir: %s", tool, line)
			}
		}
	}
}

// allowedSandboxRoots is the set of prefixes a sandboxed wire may legitimately
// name: the sandbox home itself (and its symlink-resolved form) plus the exact
// running test binary, which the registrars embed in the hook/MCP commands
// they report. NOT os.TempDir() — see the containment note at the call site.
func allowedSandboxRoots(t *testing.T, sandbox string) []string {
	t.Helper()
	roots := []string{sandbox}
	if bin, err := absoluteBinaryPath(); err == nil {
		roots = append(roots, bin)
	}
	for _, r := range append([]string{}, roots...) {
		if resolved, err := filepath.EvalSymlinks(r); err == nil && resolved != r {
			roots = append(roots, resolved)
		}
	}
	return roots
}

func underAnyRoot(p string, roots []string) bool {
	for _, r := range roots {
		if r != "" && (p == r || strings.HasPrefix(p, strings.TrimSuffix(r, "/")+"/")) {
			return true
		}
	}
	return false
}
