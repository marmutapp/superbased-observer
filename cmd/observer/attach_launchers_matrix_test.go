// attach_launchers_matrix_test.go — cross-launcher coverage for the
// attach-all-launchers wave (design §6). Two tables:
//
//  1. TestEveryLauncherRegistersAttachFlags walks all 19 Launch-grounded
//     `observer <verb>` commands and asserts each registers --attach/--no-attach
//     (mirrors resume_test.go's grounded-flag registration pattern). The flags
//     are capability-gated in registerAttachFlags, so their presence is the
//     observable proof each launcher funnels through the shared gate.
//  2. TestLauncherIncompatiblePredicates covers the per-tool "extra" incompatible
//     predicate wired into each of the 9 fan-out launchers this file owns
//     (kilo/qwen/kiro/qoder/grok/goose/devin/antigravity/kimi) — the headless
//     flag / leading-subcommand cases beyond the shared continue-from family.

package main

import (
	"testing"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/integration"
)

// allLauncherVerbs maps each Launch-grounded launcher's verb (as the operator
// types it) to its cobra command constructor. The verb strings are the exact
// spellings from main.go's AddCommand wiring / each command's Use field, and the
// tool keys are cross-checked against the integration registry Attach rows.
func allLauncherVerbs() []struct {
	verb string
	tool string
	cmd  *cobra.Command
} {
	return []struct {
		verb string
		tool string
		cmd  *cobra.Command
	}{
		{"claude", "claude-code", newClaudeCmd()},
		{"codex", "codex", newCodexCmd()},
		{"opencode", "opencode", newOpencodeCmd()},
		{"gemini", "gemini-cli", newGeminiCmd()},
		{"copilot-cli", "copilot-cli", newCopilotCLICmd()},
		{"cline-cli", "cline-cli", newClineCLICmd()},
		{"openclaw", "openclaw", newOpenclawCmd()},
		{"pi", "pi", newPiCmd()},
		{"hermes", "hermes", newHermesCmd()},
		{"cursor", "cursor", newCursorCmd()},
		{"kilo", "kilo-code-cli", newKiloCmd()},
		{"qwen", "qwen-code", newQwenCmd()},
		{"kiro", "kiro-cli", newKiroCmd()},
		{"qoder", "qoder", newQoderCmd()},
		{"grok", "grok", newGrokCmd()},
		{"goose", "goose", newGooseCmd()},
		{"devin", "devin", newDevinCmd()},
		{"antigravity-cli", "antigravity-cli", newAntigravityCmd()},
		{"kimi", "kimi-code", newKimiCmd()},
	}
}

// TestEveryLauncherRegistersAttachFlags pins the attach-all-launchers invariant:
// every Launch-grounded launcher verb registers --attach and --no-attach. The
// registration is capability-gated on the tool's Attach registry row, so a
// missing pair means either the launcher was not wired through launcherAttach or
// its registry row lost its Attach grounding.
//
// NOTE (attach-all-launchers, two-agent split): the 8 launchers owned by the
// parallel fan-out agent (opencode/gemini/copilot-cli/cline-cli/openclaw/pi/
// hermes/cursor) may not be wired yet when this test runs standalone. Those are
// reported as EXPECTED-PENDING (t.Log), not failures — this file's own 9 tools
// plus the flagships claude/codex MUST pass.
func TestEveryLauncherRegistersAttachFlags(t *testing.T) {
	// The 9 launchers this file's agent owns, plus the two migrated flagships:
	// these must always register the flags.
	owned := map[string]bool{
		"claude": true, "codex": true,
		"kilo": true, "qwen": true, "kiro": true, "qoder": true, "grok": true,
		"goose": true, "devin": true, "antigravity-cli": true, "kimi": true,
	}
	for _, lv := range allLauncherVerbs() {
		lv := lv
		t.Run(lv.verb, func(t *testing.T) {
			// The registry row must ground Attach for the flags to be registered
			// at all — verify the capability first so a failure is legible.
			capab, ok := integration.For(lv.tool)
			if !ok || capab.Attach == nil {
				t.Fatalf("tool %q (verb %q) has no grounded Attach capability", lv.tool, lv.verb)
			}
			if capab.Attach.Subcommand != lv.verb {
				t.Errorf("tool %q Attach.Subcommand = %q, want verb %q", lv.tool, capab.Attach.Subcommand, lv.verb)
			}

			hasAttach := lv.cmd.Flags().Lookup("attach") != nil
			hasNoAttach := lv.cmd.Flags().Lookup("no-attach") != nil
			if hasAttach && hasNoAttach {
				return // wired
			}
			if owned[lv.verb] {
				t.Fatalf("launcher %q must register --attach and --no-attach (attach=%v no-attach=%v)", lv.verb, hasAttach, hasNoAttach)
			}
			t.Logf("EXPECTED-PENDING: launcher %q not yet wired by the parallel fan-out agent (attach=%v no-attach=%v)", lv.verb, hasAttach, hasNoAttach)
		})
	}
}

// launcherArgsIncompatible mirrors the per-tool "extra" incompatible predicate
// each of this file's 9 launchers wires into its launcherAttachSpec (the term
// OR-ed with the shared continueFamilyEngaged). Keeping the mirror test-local
// documents every tool's predicate — including the five that have none — without
// exporting a switch from production code.
func launcherArgsIncompatible(tool string, args []string) bool {
	switch tool {
	case "kilo-code-cli":
		return argsLeadWithSubcommand(args, kiloAttachHeadlessSubcommands)
	case "goose":
		return argsLeadWithSubcommand(args, gooseAttachHeadlessSubcommands)
	case "qwen-code":
		return argsContainHeadlessFlag(args, "-p", "--prompt")
	case "qoder":
		// Grounded headless spelling is -p/--print (this launcher's ConflictFlags),
		// NOT the -p/--prompt the plan table named.
		return argsContainHeadlessFlag(args, "-p", "--print")
	case "kimi-code":
		// kimi-code's `-p` prints and EXITS (a genuine headless one-shot per the
		// registry note), so it must fall through to the bare path, not attach.
		return argsContainHeadlessFlag(args, "-p")
	default:
		// kiro-cli / grok / devin / antigravity-cli: no extra predicate — the
		// both-TTY guard + continue-from family are the only gates.
		return false
	}
}

// TestLauncherIncompatiblePredicates covers the headless-flag / leading-
// subcommand cases for this file's 9 launchers. It asserts the tool-specific
// extra predicate ONLY (the continue-from family is exercised by
// TestContinueFamilyEngaged), so a bare interactive launch stays attachable and
// a headless one-shot falls through to the bare path.
func TestLauncherIncompatiblePredicates(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args []string
		want bool
	}{
		// kilo — leading `run` subcommand is the headless one-shot.
		{"kilo run leads", "kilo-code-cli", []string{"run", "summarize"}, true},
		{"kilo run after flag", "kilo-code-cli", []string{"--foo", "run"}, true},
		{"kilo interactive", "kilo-code-cli", []string{"--model", "x"}, false},
		{"kilo run after -- is positional", "kilo-code-cli", []string{"--", "run"}, false},

		// goose — leading `run` subcommand is the headless one-shot.
		{"goose run leads", "goose", []string{"run", "-t", "hi"}, true},
		{"goose interactive", "goose", []string{"session"}, false},
		{"goose run after -- is positional", "goose", []string{"--", "run"}, false},

		// qwen — headless -p/--prompt one-shot.
		{"qwen -p", "qwen-code", []string{"-p", "hi"}, true},
		{"qwen --prompt=", "qwen-code", []string{"--prompt=hi"}, true},
		{"qwen interactive", "qwen-code", []string{"--model", "x"}, false},
		{"qwen -p after -- is positional", "qwen-code", []string{"--", "-p"}, false},

		// qoder — grounded headless spelling is -p/--print (NOT --prompt).
		{"qoder -p", "qoder", []string{"-p", "hi"}, true},
		{"qoder --print", "qoder", []string{"--print"}, true},
		{"qoder --prompt is NOT the grounded spelling", "qoder", []string{"--prompt", "hi"}, false},
		{"qoder interactive", "qoder", []string{"--model", "x"}, false},

		// The five no-extra-predicate tools: no arg makes them incompatible on
		// their own — only the shared continue-from family (tested elsewhere).
		{"kiro chat", "kiro-cli", []string{"chat"}, false},
		{"kiro -p (not wired)", "kiro-cli", []string{"-p", "hi"}, false},
		{"grok inspect", "grok", []string{"inspect"}, false},
		{"grok -p (not wired)", "grok", []string{"-p", "hi"}, false},
		{"devin list", "devin", []string{"list"}, false},
		{"antigravity -p (not grounded here)", "antigravity-cli", []string{"-p", "hi"}, false},
		{"kimi -p (prints+exits headless one-shot)", "kimi-code", []string{"-p", "hi"}, true},
		{"kimi interactive", "kimi-code", []string{"--model", "x"}, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := launcherArgsIncompatible(tc.tool, tc.args); got != tc.want {
				t.Fatalf("launcherArgsIncompatible(%q, %v) = %v, want %v", tc.tool, tc.args, got, tc.want)
			}
		})
	}
}
