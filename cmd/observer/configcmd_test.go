package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
)

func runConfigCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	// Never poke a real daemon from tests (WSL2 forwards localhost).
	orig := pokeReload
	pokeReload = func() bool { return false }
	t.Cleanup(func() { pokeReload = orig })
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"config"}, args...))
	err := root.Execute()
	return out.String(), err
}

// TestConfigSet_Global pins the global front door: dotted keys land
// in config.toml through the shared write owner, round-trip through
// config.Load, and invalid values are refused pre-write.
func TestConfigSet_Global(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	out, err := runConfigCmd(t, "set", "compression.conversation.target_ratio", "0.7", "--config", cfgPath)
	if err != nil {
		t.Fatalf("set: %v\n%s", err, out)
	}
	reloaded, err := config.Load(config.LoadOptions{GlobalPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Compression.Conversation.TargetRatio != 0.7 {
		t.Errorf("persisted ratio: got %v", reloaded.Compression.Conversation.TargetRatio)
	}

	if _, err := runConfigCmd(t, "set", "proxy.port", "99999999", "--config", cfgPath); err == nil {
		t.Error("invariant-violating value must be refused (Validate before write)")
	}
	if _, err := runConfigCmd(t, "set", "no.such.key", "1", "--config", cfgPath); err == nil {
		t.Error("unknown key must error")
	}
}

// TestConfigSet_Project pins the --project front door: keys land in
// the repo-local override file; daemon-level keys are refused.
func TestConfigSet_Project(t *testing.T) {
	repo := t.TempDir()
	out, err := runConfigCmd(t, "set", "--project", repo, "compression.conversation.enabled", "false")
	if err != nil {
		t.Fatalf("project set: %v\n%s", err, out)
	}
	if !strings.Contains(out, "NEW sessions automatically") {
		t.Errorf("project set should explain the no-restart pickup:\n%s", out)
	}
	raw, err := os.ReadFile(filepath.Join(repo, ".observer", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	base := config.Default().Compression
	base.Conversation.Enabled = true
	got, err := config.ProjectCompression(base, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Conversation.Enabled {
		t.Error("project off-switch did not land")
	}

	if _, err := runConfigCmd(t, "set", "--project", repo, "observer.db_path", "/evil"); err == nil {
		t.Error("daemon-level key must be refused in a project file")
	}
}
