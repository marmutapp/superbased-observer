package main

import (
	"testing"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/integration"
)

// attachLauncherAFixtures is the set of launchers this fan-out agent wired
// (the "A" half of attach-all-launchers): each row pairs the launcher's cobra
// constructor with its integration-registry key. TestLauncherARegistersAttachFlags
// walks it to pin that every one registers --attach/--no-attach exactly when the
// row grounds an Attach capability (capability dispatch, never a tool-name
// branch — CLAUDE.md #3).
var attachLauncherAFixtures = []struct {
	tool string
	ctor func() *cobra.Command
}{
	{"opencode", newOpencodeCmd},
	{"gemini-cli", newGeminiCmd},
	{"copilot-cli", newCopilotCLICmd},
	{"cline-cli", newClineCLICmd},
	{"openclaw", newOpenclawCmd},
	{"pi", newPiCmd},
	{"hermes", newHermesCmd},
	{"cursor", newCursorCmd},
}

// TestLauncherARegistersAttachFlags pins that each of this agent's 8 launchers
// registers the --attach/--no-attach pair iff its registry row grounds an Attach
// capability. All 8 are grounded today, so the flags must be present; if a row
// were ever un-grounded the flags must vanish (honest-disable, design §3.4).
func TestLauncherARegistersAttachFlags(t *testing.T) {
	for _, f := range attachLauncherAFixtures {
		t.Run(f.tool, func(t *testing.T) {
			capab, ok := integration.For(f.tool)
			if !ok {
				t.Fatalf("registry has no row for %q", f.tool)
			}
			cmd := f.ctor()
			hasAttach := cmd.Flags().Lookup("attach") != nil
			hasNoAttach := cmd.Flags().Lookup("no-attach") != nil
			grounded := capab.Attach != nil
			if grounded && (!hasAttach || !hasNoAttach) {
				t.Fatalf("%s is attach-grounded but --attach/--no-attach missing (attach=%v no-attach=%v)", f.tool, hasAttach, hasNoAttach)
			}
			if !grounded && (hasAttach || hasNoAttach) {
				t.Fatalf("%s is NOT attach-grounded but registered attach flags", f.tool)
			}
		})
	}
}

// TestLauncherAAttachPassthrough pins each launcher's wrapper-flag passthrough
// builder: the binary-path flag is forwarded only when overridden, and the
// multi-flag builders (copilot --model, hermes --upstream/--key-env) forward the
// extra tokens only when set / non-default. These are the tokens the
// daemon-spawned inner launcher must honor so a wrapper-flagged attach is not
// silently downgraded.
func TestLauncherAAttachPassthrough(t *testing.T) {
	cases := []struct {
		name string
		got  []string
		want []string
	}{
		{"opencode empty", opencodeAttachPassthrough(""), nil},
		{"opencode path", opencodeAttachPassthrough("/opt/opencode"), []string{"--opencode-path", "/opt/opencode"}},
		{"gemini empty", geminiAttachPassthrough(""), nil},
		{"gemini path", geminiAttachPassthrough("/opt/gemini"), []string{"--gemini-path", "/opt/gemini"}},
		{"cline empty", clineAttachPassthrough(""), nil},
		{"cline path", clineAttachPassthrough("/opt/cline"), []string{"--cline-path", "/opt/cline"}},
		{"openclaw empty", openclawAttachPassthrough(""), nil},
		{"openclaw path", openclawAttachPassthrough("/opt/openclaw"), []string{"--openclaw-path", "/opt/openclaw"}},
		{"pi empty", piAttachPassthrough(""), nil},
		{"pi path", piAttachPassthrough("/opt/pi"), []string{"--pi-path", "/opt/pi"}},
		{"cursor empty", cursorAttachPassthrough(""), nil},
		{"cursor path", cursorAttachPassthrough("/opt/cursor-agent"), []string{"--cursor-agent-path", "/opt/cursor-agent"}},
		{"copilot none", copilotAttachPassthrough("", ""), nil},
		{"copilot path only", copilotAttachPassthrough("/opt/copilot", ""), []string{"--copilot-path", "/opt/copilot"}},
		{"copilot model only", copilotAttachPassthrough("", "gpt-5"), []string{"--model", "gpt-5"}},
		{"copilot both", copilotAttachPassthrough("/opt/copilot", "gpt-5"), []string{"--copilot-path", "/opt/copilot", "--model", "gpt-5"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !equalArgs(tc.got, tc.want) {
				t.Fatalf("got %v, want %v", tc.got, tc.want)
			}
		})
	}
}

// TestHermesAttachPassthroughDefaults pins hermes' passthrough: --upstream and
// --key-env are forwarded ONLY when the operator overrode them off their
// defaults, so an unchanged default is never re-asserted on the inner argv.
func TestHermesAttachPassthroughDefaults(t *testing.T) {
	// All defaults, no binary override → nothing forwarded.
	if got := hermesAttachPassthrough("", hermesDefaultUpstream, hermesDefaultKeyEnv); len(got) != 0 {
		t.Fatalf("all-default hermes passthrough = %v, want empty", got)
	}
	// Binary path + a non-default upstream + a non-default key env → all three.
	got := hermesAttachPassthrough("/opt/hermes", "groq", "GROQ_API_KEY")
	want := []string{"--hermes-path", "/opt/hermes", "--upstream", "groq", "--key-env", "GROQ_API_KEY"}
	if !equalArgs(got, want) {
		t.Fatalf("overridden hermes passthrough = %v, want %v", got, want)
	}
	// A non-default upstream alone (defaults elsewhere) → only --upstream.
	if got := hermesAttachPassthrough("", "groq", hermesDefaultKeyEnv); !equalArgs(got, []string{"--upstream", "groq"}) {
		t.Fatalf("upstream-only hermes passthrough = %v, want [--upstream groq]", got)
	}
}

// TestOpencodeHeadlessSubcommands pins the leading-verb incompatible set for
// opencode: `run` is the headless one-shot (bare path), an interactive verb is
// not.
func TestOpencodeHeadlessSubcommands(t *testing.T) {
	if !argsLeadWithSubcommand([]string{"run", "do it"}, opencodeHeadlessSubcommands) {
		t.Error("leading `run` must classify opencode incompatible with attach")
	}
	if argsLeadWithSubcommand([]string{"--model", "x"}, opencodeHeadlessSubcommands) {
		t.Error("a plain interactive opencode launch must stay attach-eligible")
	}
}
