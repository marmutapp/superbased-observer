package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
)

func runProfileCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	// Never poke a real daemon from tests — WSL2 forwards localhost,
	// so a live WSL daemon is reachable from Windows-host test runs.
	orig := pokeReload
	pokeReload = func() bool { return false }
	t.Cleanup(func() { pokeReload = orig })
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"profile"}, args...))
	err := root.Execute()
	return out.String(), err
}

// TestProfileList renders every profile with the baked default
// assignments and the hot-reload footer.
func TestProfileList(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	out, err := runProfileCmd(t, "list", "--config", cfgPath)
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	for _, want := range []string{"claude-code", "codex-safe", "codex-variant", "default", "anthropic", "openai", "new sessions"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
}

// TestProfileShow prints resolved TOML: profile-pinned keys win,
// master-owned enablement is honest (default master = disabled).
func TestProfileShow(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	out, err := runProfileCmd(t, "show", "claude-code", "--config", cfgPath)
	if err != nil {
		t.Fatalf("show: %v\n%s", err, out)
	}
	if !strings.Contains(out, `mode = "cache_aware"`) {
		t.Errorf("show missing claude-code's cache_aware mode:\n%s", out)
	}
	if !strings.Contains(out, "enabled = false") {
		t.Errorf("show must reflect master-owned enablement (off by default):\n%s", out)
	}

	if out, err := runProfileCmd(t, "show", "no-such", "--config", cfgPath); err == nil {
		t.Errorf("show of unknown profile must error; got:\n%s", out)
	}
}

// TestProfileAssign writes [profiles] through the shared write owner
// and round-trips through config.Load; unknown profiles and unknown
// traffic classes stay loud.
func TestProfileAssign(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	out, err := runProfileCmd(t, "assign", "anthropic", "codex-variant", "--config", cfgPath)
	if err != nil {
		t.Fatalf("assign: %v\n%s", err, out)
	}
	if !strings.Contains(out, "assigned anthropic → codex-variant") {
		t.Errorf("assign confirmation missing:\n%s", out)
	}

	reloaded, err := config.Load(config.LoadOptions{GlobalPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Profiles.ByProvider["anthropic"]; got != "codex-variant" {
		t.Errorf("persisted assignment: got %q want codex-variant", got)
	}
	// The openai baked default must survive the targeted write.
	if got := config.ResolveProfileName(reloaded.Profiles, "openai", ""); got != "codex-safe" {
		t.Errorf("openai assignment disturbed: got %q want codex-safe", got)
	}

	// default target sets [profiles].default.
	if _, err := runProfileCmd(t, "assign", "default", "codex-safe", "--config", cfgPath); err != nil {
		t.Fatalf("assign default: %v", err)
	}
	reloaded, err = config.Load(config.LoadOptions{GlobalPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Profiles.Default != "codex-safe" {
		t.Errorf("table default: got %q want codex-safe", reloaded.Profiles.Default)
	}

	// A second write creates the .bak of the first (shared write-owner
	// semantics — same as dashboard saves).
	if _, err := os.Stat(cfgPath + ".bak"); err != nil {
		t.Errorf("expected .bak after second save: %v", err)
	}

	if _, err := runProfileCmd(t, "assign", "anthropic", "no-such", "--config", cfgPath); err == nil {
		t.Error("assigning an unknown profile must error")
	}
	if _, err := runProfileCmd(t, "assign", "gemini", "codex-safe", "--config", cfgPath); err == nil {
		t.Error("unknown traffic class must error (R1 vocabulary: anthropic, openai, default)")
	}

	// R2: tool:<name> writes [profiles.by_tool] and wins resolution
	// over the provider tier; list renders it with the same prefix.
	if _, err := runProfileCmd(t, "assign", "tool:cline", "codex-variant", "--config", cfgPath); err != nil {
		t.Fatalf("assign tool:cline: %v", err)
	}
	reloaded, err = config.Load(config.LoadOptions{GlobalPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Profiles.ByTool["cline"]; got != "codex-variant" {
		t.Errorf("by_tool persisted: got %q want codex-variant", got)
	}
	if got := config.ResolveProfileName(reloaded.Profiles, "anthropic", "cline"); got != "codex-variant" {
		t.Errorf("tool tier must win resolution: got %q want codex-variant", got)
	}
	out, err = runProfileCmd(t, "list", "--config", cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "tool:cline") {
		t.Errorf("list must render tool assignments:\n%s", out)
	}
	if _, err := runProfileCmd(t, "assign", "tool:", "codex-safe", "--config", cfgPath); err == nil {
		t.Error("empty tool name must error")
	}
}

// TestProfileCRUD covers the P3.4 CLI lifecycle: create from a
// built-in, set a key, list shows it as user-sourced, assign it,
// show resolves it, delete refusals + cleanup.
func TestProfileCRUD(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.toml")

	if out, err := runProfileCmd(t, "create", "my-tuning", "--from", "codex-safe", "--config", cfgPath); err != nil {
		t.Fatalf("create: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(tmp, "profiles", "my-tuning.toml")); err != nil {
		t.Fatalf("profile file not created: %v", err)
	}
	if _, err := runProfileCmd(t, "create", "claude-code", "--config", cfgPath); err == nil {
		t.Error("creating a built-in name must error")
	}

	if out, err := runProfileCmd(t, "set", "my-tuning", "compression.conversation.target_ratio", "0.9", "--config", cfgPath); err != nil {
		t.Fatalf("set: %v\n%s", err, out)
	}
	if _, err := runProfileCmd(t, "set", "codex-safe", "compression.conversation.target_ratio", "0.9", "--config", cfgPath); err == nil {
		t.Error("setting a key on a built-in must error")
	}

	out, err := runProfileCmd(t, "list", "--config", cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "my-tuning") || !strings.Contains(out, "user") {
		t.Errorf("list must show the user profile with its source:\n%s", out)
	}

	if out, err := runProfileCmd(t, "assign", "openai", "my-tuning", "--config", cfgPath); err != nil {
		t.Fatalf("assign custom: %v\n%s", err, out)
	}
	reloaded, err := config.Load(config.LoadOptions{GlobalPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Profiles.ByProvider["openai"] != "my-tuning" {
		t.Errorf("custom assignment persisted: got %q", reloaded.Profiles.ByProvider["openai"])
	}

	out, err = runProfileCmd(t, "show", "my-tuning", "--config", cfgPath)
	if err != nil {
		t.Fatalf("show custom: %v\n%s", err, out)
	}
	if !strings.Contains(out, "target_ratio = 0.9") {
		t.Errorf("show must resolve the edited key:\n%s", out)
	}

	if _, err := runProfileCmd(t, "delete", "codex-safe", "--config", cfgPath); err == nil {
		t.Error("deleting a built-in must error")
	}
	if _, err := runProfileCmd(t, "delete", "my-tuning", "--config", cfgPath); err != nil {
		t.Fatal(err)
	}
	if _, err := runProfileCmd(t, "show", "my-tuning", "--config", cfgPath); err == nil {
		t.Error("deleted profile must not resolve")
	}
}
