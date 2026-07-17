package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPreflightRestart pins the "never shut down into a brick" guard: a valid
// config preflights clean, a malformed one is REFUSED (the daemon keeps
// running rather than restarting into a process that won't load).
func TestPreflightRestart(t *testing.T) {
	dir := t.TempDir()

	valid := filepath.Join(dir, "ok.toml")
	if err := os.WriteFile(valid, []byte("[observer]\nlog_level = \"info\"\n"), 0o644); err != nil {
		t.Fatalf("write valid config: %v", err)
	}
	if err := preflightRestart(valid); err != nil {
		t.Fatalf("valid config should preflight clean: %v", err)
	}

	bad := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(bad, []byte("this is not = valid ]["), 0o644); err != nil {
		t.Fatalf("write bad config: %v", err)
	}
	if err := preflightRestart(bad); err == nil {
		t.Fatal("malformed config must be refused by preflightRestart")
	}
}

// TestReexecSupportedIsUnix documents the platform gate: the daemon runs under
// WSL/Linux (unix) where self re-exec works; a native-Windows build refuses.
func TestReexecSupportedIsUnix(t *testing.T) {
	// On the unix build this is true; the !unix build returns false. Either way
	// execSelf must be defined (compile-time) — this just exercises the gate.
	_ = reexecSupported()
}
