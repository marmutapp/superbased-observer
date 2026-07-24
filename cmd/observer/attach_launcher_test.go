package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/integration"
)

// writeMinimalConfig writes a minimal valid observer config (just the required
// observer.db_path) into a temp dir and returns its path, so launcherAttach's
// internal config.Load succeeds deterministically.
func writeMinimalConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	body := "[observer]\ndb_path = " + filepathQuote(filepath.Join(dir, "observer.db")) + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath
}

// filepathQuote TOML-quotes a path with forward slashes so a Windows path in the
// db_path value never introduces a stray backslash escape.
func filepathQuote(p string) string {
	return "\"" + filepath.ToSlash(p) + "\""
}

// TestLauncherAttachFallsThroughWhenUngrounded pins that a tool with no grounded
// Attach capability resolves to handled=false / err=nil so the launcher takes
// its normal bare path. (In the non-TTY test env decideAttach row 3 also forces
// bare, so this doubles as a non-attach smoke test — the key assertion is that
// the gate never tries to attach and never errors.)
func TestLauncherAttachFallsThroughWhenUngrounded(t *testing.T) {
	if c, _ := integration.For("aider"); c.Attach != nil {
		t.Skip("fixture assumes aider is not attach-grounded")
	}
	var stderr bytes.Buffer
	outcome, err := launcherAttach(context.Background(), launcherAttachSpec{
		tool:       "aider",
		configPath: writeMinimalConfig(t),
		stderr:     &stderr,
	})
	if err != nil {
		t.Fatalf("ungrounded launcherAttach err = %v, want nil", err)
	}
	if outcome.handled {
		t.Fatal("ungrounded tool must fall through to the bare path (handled=false)")
	}
}

// TestLauncherAttachFallsThroughWhenConfigLoadFails pins the fail-OPEN contract:
// a config that cannot be parsed yields {handled:false}, nil so the launcher
// still takes its bare path rather than being refused at the attach gate.
func TestLauncherAttachFallsThroughWhenConfigLoadFails(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "broken.toml")
	if err := os.WriteFile(bad, []byte("this is = = not valid toml ["), 0o600); err != nil {
		t.Fatalf("write bad config: %v", err)
	}
	var stderr bytes.Buffer
	outcome, err := launcherAttach(context.Background(), launcherAttachSpec{
		tool:       "claude-code",
		configPath: bad,
		stderr:     &stderr,
	})
	if err != nil {
		t.Fatalf("config-load-failure launcherAttach err = %v, want nil (fail open)", err)
	}
	if outcome.handled {
		t.Fatal("a config load failure must fail OPEN to the bare path (handled=false)")
	}
}

// TestLauncherAttachFallsThroughWhenNotTTY pins that even a fully grounded,
// attach-eligible tool resolves to bare (handled=false) when stdin/stdout are
// not terminals — the near-certain script/pipe signature decideAttach row 3
// filters SILENTLY. The test process's stdio is not a TTY, so this is the
// natural env; the assertion is that the gate neither attaches nor prints a
// notice on this scripted path.
func TestLauncherAttachFallsThroughWhenNotTTY(t *testing.T) {
	if c, _ := integration.For("claude-code"); c.Attach == nil {
		t.Skip("fixture assumes claude-code is attach-grounded")
	}
	var stderr bytes.Buffer
	outcome, err := launcherAttach(context.Background(), launcherAttachSpec{
		tool:       "claude-code",
		configPath: writeMinimalConfig(t),
		stderr:     &stderr,
	})
	if err != nil {
		t.Fatalf("not-a-TTY launcherAttach err = %v, want nil", err)
	}
	if outcome.handled {
		t.Fatal("a non-TTY launch must fall through to the bare path (handled=false)")
	}
	if stderr.Len() != 0 {
		t.Errorf("a non-TTY scripted launch must be SILENT, got stderr %q", stderr.String())
	}
}

// TestRegisterAttachFlagsGroundedOnly pins that --attach/--no-attach are
// registered ONLY for a tool whose registry row grounds an Attach capability
// (capability dispatch, never a tool-name branch): present for a grounded tool
// (opencode, one of the fan-out rows), absent for an ungrounded one (aider).
func TestRegisterAttachFlagsGroundedOnly(t *testing.T) {
	grounded := &cobra.Command{Use: "opencode"}
	registerAttachFlags(grounded, "opencode")
	if grounded.Flags().Lookup("attach") == nil || grounded.Flags().Lookup("no-attach") == nil {
		t.Error("a grounded tool must register --attach and --no-attach")
	}

	ungrounded := &cobra.Command{Use: "aider"}
	registerAttachFlags(ungrounded, "aider")
	if ungrounded.Flags().Lookup("attach") != nil || ungrounded.Flags().Lookup("no-attach") != nil {
		t.Error("an ungrounded tool must NOT register --attach/--no-attach")
	}
}

// TestContinueFamilyEngaged pins the shared handoff-fork incompatible predicate:
// any member of the family (continue-from OR carry OR from-message OR from-time)
// engages it; the all-zero case does not.
func TestContinueFamilyEngaged(t *testing.T) {
	cases := []struct {
		name         string
		continueFrom string
		carry        string
		fromMessage  int
		fromTime     string
		want         bool
	}{
		{"none", "", "", 0, "", false},
		{"continue-from", "sess-1", "", 0, "", true},
		{"carry", "", "distilled", 0, "", true},
		{"from-message", "", "", 4, "", true},
		{"from-time", "", "", 0, "2026-07-24T00:00:00Z", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := continueFamilyEngaged(tc.continueFrom, tc.carry, tc.fromMessage, tc.fromTime); got != tc.want {
				t.Fatalf("continueFamilyEngaged = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestArgsContainHeadlessFlag pins the generic headless-flag scanner incl. the
// `--` boundary: a matching flag BEFORE a bare `--` is the real flag (true); the
// same token AFTER a bare `--` is a positional prompt (false); `flag=value`
// combined forms match.
func TestArgsContainHeadlessFlag(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"bare -p", []string{"-p", "hi"}, true},
		{"long --prompt", []string{"--prompt", "hi"}, true},
		{"combined --prompt=hi", []string{"--prompt=hi"}, true},
		{"none", []string{"--model", "x"}, false},
		{"after bare -- is positional", []string{"--", "-p", "hi"}, false},
		{"before bare -- still counts", []string{"-p", "--", "text"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := argsContainHeadlessFlag(tc.args, "-p", "--prompt"); got != tc.want {
				t.Fatalf("argsContainHeadlessFlag(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestArgsLeadWithSubcommand pins the leading-subcommand scanner incl. the `--`
// boundary: a headless verb leading the args (after any flags) matches; the same
// verb AFTER a bare `--` is positional (false); a non-headless leading verb does
// not match.
func TestArgsLeadWithSubcommand(t *testing.T) {
	subs := map[string]bool{"run": true}
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"leading run", []string{"run", "prompt"}, true},
		{"run after a leading flag", []string{"--flag", "run"}, true},
		{"non-headless verb", []string{"chat"}, false},
		{"run after bare -- is positional", []string{"--", "run"}, false},
		{"empty", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := argsLeadWithSubcommand(tc.args, subs); got != tc.want {
				t.Fatalf("argsLeadWithSubcommand(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
