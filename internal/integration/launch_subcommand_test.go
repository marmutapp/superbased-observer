package integration_test

import (
	"testing"

	"github.com/marmutapp/superbased-observer/internal/integration"
)

// TestToolForLaunchSubcommand pins the reverse verb -> canonical tool-key
// lookup against a sample of the registry's actual Handoff.Launch.Subcommand
// values (verified by reading internal/integration/integration.go — do not
// guess spellings here). Includes an unknown verb and the empty verb, plus
// an "identity-ish" case (codex's verb equals its tool key).
func TestToolForLaunchSubcommand(t *testing.T) {
	cases := []struct {
		verb    string
		wantKey string
		wantOK  bool
	}{
		{"claude", "claude-code", true},
		{"gemini", "gemini-cli", true},
		{"codex", "codex", true},       // verb == key
		{"opencode", "opencode", true}, // verb == key
		{"kilo", "kilo-code-cli", true},
		{"qwen", "qwen-code", true},
		{"kiro", "kiro-cli", true},
		{"nonexistent-verb", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := integration.ToolForLaunchSubcommand(tc.verb)
		if ok != tc.wantOK || got != tc.wantKey {
			t.Errorf("ToolForLaunchSubcommand(%q) = (%q, %v), want (%q, %v)", tc.verb, got, ok, tc.wantKey, tc.wantOK)
		}
	}
}

// TestLaunchSubcommandsAreUnique pins that every non-empty
// Handoff.Launch.Subcommand across the registry appears on exactly ONE row
// — the well-definedness precondition for ToolForLaunchSubcommand. If this
// test fails, two adapters have declared the same `observer <verb>`
// launcher, which is a registry bug (report it, don't relax this test).
func TestLaunchSubcommandsAreUnique(t *testing.T) {
	seenBy := map[string]string{} // subcommand -> owning tool key
	for _, c := range integration.Capabilities() {
		launch := c.Handoff.Launch
		if launch == nil || launch.Subcommand == "" {
			continue
		}
		if owner, dup := seenBy[launch.Subcommand]; dup {
			t.Errorf("launcher verb %q is declared by both %q and %q — ToolForLaunchSubcommand cannot be well-defined until this is resolved",
				launch.Subcommand, owner, c.Tool)
			continue
		}
		seenBy[launch.Subcommand] = c.Tool
	}
	if len(seenBy) == 0 {
		t.Fatal("no launchable rows found in the registry — test is vacuous, something is broken upstream")
	}
}
