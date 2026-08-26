package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
)

// TestCollectIntegritySignals asserts the boundary seam assembles coarse labels
// from the diag + proxyroute detectors: a cross-OS sibling observer.db becomes
// an "origin/os" label, and a claude route repointed off observer becomes the
// "claude-code" drifted-tool label. Only labels are produced (no paths).
func TestCollectIntegritySignals(t *testing.T) {
	nativeHome := t.TempDir()
	foreignHome := t.TempDir()

	// Daemon's own DB in the native home.
	daemonDir := filepath.Join(nativeHome, ".observer")
	if err := os.MkdirAll(daemonDir, 0o755); err != nil {
		t.Fatal(err)
	}
	daemonDB := filepath.Join(daemonDir, "observer.db")
	if err := os.WriteFile(daemonDB, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A cross-OS sibling observer.db in a foreign home.
	fdir := filepath.Join(foreignHome, ".observer")
	if err := os.MkdirAll(fdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fdir, "observer.db"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A drifted claude route in the native home.
	cdir := filepath.Join(nativeHome, ".claude")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cdir, "settings.json"),
		[]byte(`{"env":{"ANTHROPIC_BASE_URL":"https://api.anthropic.com"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	homes := []crossmount.HomeRoot{
		{Path: nativeHome, OS: "linux", Origin: "native"},
		{Path: foreignHome, OS: "windows", Origin: "wsl-mnt:marmu"},
	}
	siblings, drifted := collectIntegritySignalsFrom(daemonDB, nativeHome, homes)

	if len(siblings) != 1 || siblings[0] != "wsl-mnt:marmu/windows" {
		t.Errorf("siblings = %v, want [wsl-mnt:marmu/windows]", siblings)
	}
	if len(drifted) != 1 || drifted[0] != "claude-code" {
		t.Errorf("drifted = %v, want [claude-code]", drifted)
	}
}

// TestCollectIntegritySignals_Clean: default-path daemon + observer-pointing
// route yields no evidence.
func TestCollectIntegritySignals_Clean(t *testing.T) {
	nativeHome := t.TempDir()
	daemonDir := filepath.Join(nativeHome, ".observer")
	if err := os.MkdirAll(daemonDir, 0o755); err != nil {
		t.Fatal(err)
	}
	daemonDB := filepath.Join(daemonDir, "observer.db")
	if err := os.WriteFile(daemonDB, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cdir := filepath.Join(nativeHome, ".claude")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cdir, "settings.json"),
		[]byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8820"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	homes := []crossmount.HomeRoot{{Path: nativeHome, OS: "linux", Origin: "native"}}
	siblings, drifted := collectIntegritySignalsFrom(daemonDB, nativeHome, homes)
	if len(siblings) != 0 || len(drifted) != 0 {
		t.Errorf("clean host produced evidence: siblings=%v drifted=%v", siblings, drifted)
	}
}
