package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
