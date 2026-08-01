package crossmount

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAutoDetectSuppressed is the table for the one owner of the sandbox rule
// (incident 2026-07-31 + its containment follow-up). Production shape = no
// local home override: nothing is ever suppressed, including a bare Windows
// home override (`observer init --windows-claude-home=...`).
func TestAutoDetectSuppressed(t *testing.T) {
	sandbox := t.TempDir()
	inside := filepath.Join(sandbox, "mnt", "c", "Users", "tester")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	notYetCreated := filepath.Join(sandbox, "will-be-mkdired", "later")
	outside := t.TempDir()

	cases := []struct {
		name          string
		local, forgn  string
		wantSuppessed bool
	}{
		{"production: no overrides at all", "", "", false},
		{"production: bare foreign override", "", "/mnt/c/Users/operator", false},
		{"sandbox: no foreign override", sandbox, "", true},
		{"sandbox: contained foreign override", sandbox, inside, false},
		{"sandbox: foreign override == the sandbox itself", sandbox, sandbox, false},
		// A registrar mkdir's its target on first install, so a contained
		// path that does not exist yet must still be honoured.
		{"sandbox: contained but not yet created", sandbox, notYetCreated, false},
		{"sandbox: independent root", sandbox, outside, true},
		{"sandbox: real windows home", sandbox, "/mnt/c/Users/operator", true},
		{"sandbox: dot-dot escape", sandbox, filepath.Join(sandbox, "..", filepath.Base(outside)), true},
		{"sandbox: prefix-lookalike sibling", sandbox, sandbox + "-evil", true},
		{"sandbox: relative path resolving elsewhere", sandbox, ".", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AutoDetectSuppressed(c.local, c.forgn); got != c.wantSuppessed {
				t.Errorf("AutoDetectSuppressed(%q, %q) = %v, want %v", c.local, c.forgn, got, c.wantSuppessed)
			}
		})
	}
}

// TestPathUnder pins the containment primitive itself, including the symlink
// hop that a purely lexical check would miss.
func TestPathUnder(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	child := filepath.Join(base, "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	// A symlink INSIDE base pointing OUT of it must not count as contained.
	escape := filepath.Join(base, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if !PathUnder(base, base) {
		t.Error("base must contain itself")
	}
	if !PathUnder(base, child) {
		t.Error("a real child must be contained")
	}
	if PathUnder(base, outside) {
		t.Error("an independent root must not be contained")
	}
	if PathUnder(base, escape) {
		t.Error("a symlink out of base must not be contained (lexical-only check would pass here)")
	}
	if PathUnder(base, filepath.Join(escape, "deeper")) {
		t.Error("a path THROUGH an escaping symlink must not be contained")
	}
}
