package main

import "testing"

// droidInject mirrors the promptInjection descriptor newDroidCmd hands to the
// shared continueFromArgs seam, so the argv-shape assertions below exercise
// the exact contract the launcher declares. NO tool binary is spawned.
var droidInject = promptInjection{
	Kind:        injectTrailingPositional,
	Subcommands: droidSubcommands,
}

// TestDroidInjectShape pins the grounded seed contract: droid takes the
// initial prompt as a bare TRAILING positional on its default (TUI) command
// — `droid [options] [command] [prompt...]`, worked example
// `droid "review app.tsx"` (droid --help, v0.181.0).
func TestDroidInjectShape(t *testing.T) {
	const prompt = "SEEDED HANDOVER"

	t.Run("bare seed is the sole positional", func(t *testing.T) {
		got, err := injectPrompt(nil, droidInject, prompt)
		if err != nil {
			t.Fatalf("injectPrompt: %v", err)
		}
		if !equalArgs(got, []string{prompt}) {
			t.Fatalf("got %v, want %v", got, []string{prompt})
		}
	})

	t.Run("forwarded flags are preserved before the seed", func(t *testing.T) {
		got, err := injectPrompt([]string{"--auto=high"}, droidInject, prompt)
		if err != nil {
			t.Fatalf("injectPrompt: %v", err)
		}
		if !equalArgs(got, []string{"--auto=high", prompt}) {
			t.Fatalf("got %v, want [--auto=high <prompt>]", got)
		}
	})

	t.Run("a forwarded bare prompt collides", func(t *testing.T) {
		if _, err := injectPrompt([]string{"do the thing"}, droidInject, prompt); err == nil {
			t.Fatal("a forwarded positional prompt must collide with the seed")
		}
	})

	t.Run("separated value-flag reads as a competing positional", func(t *testing.T) {
		// Grammar-light conflict check: forward value-flags as --flag=value
		// (documented in docs/session-handoff.md).
		if _, err := injectPrompt([]string{"--auto", "high"}, droidInject, prompt); err == nil {
			t.Fatal("separated --auto value must trip the conservative collision check")
		}
	})
}

// TestDroidSubcommandsNotMisreadAsPrompt pins that a forwarded droid verb is
// not mistaken for a competing positional prompt by the generic check — the
// seeded path rejects the non-interactive verbs explicitly instead (see
// newDroidCmd's argsLeadWithSubcommand guard), which is what makes the error
// message accurate rather than "you also forwarded a prompt".
func TestDroidSubcommandsNotMisreadAsPrompt(t *testing.T) {
	for _, sub := range []string{"exec", "daemon", "search", "find", "update", "mcp", "plugin", "computer"} {
		if forwardedPromptConflict([]string{sub}, droidSubcommands) {
			t.Errorf("subcommand %q was misread as a forwarded prompt", sub)
		}
	}
	// A subcommand FOLLOWED by a bare prompt is still a collision.
	if !forwardedPromptConflict([]string{"exec", "do it"}, droidSubcommands) {
		t.Error("subcommand + bare prompt must still collide")
	}
}

// TestDroidHeadlessSubcommandsAreRejectedForSeeding pins the leading-verb
// guard the seeded path uses: `droid exec` runs non-interactively, so it must
// be caught before the handoff render rather than silently seeded. Since the
// FINDING-2 fix the guard is GROUNDED in droid's own flag grammar
// (droidValueFlags / droidBoolFlags off `droid --help` v0.181.0), so a SPLIT
// flag value no longer parks itself in the operand slot and hides the verb.
func TestDroidHeadlessSubcommandsAreRejectedForSeeding(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"exec", "analyze"}, true},
		{[]string{"--auto=high", "exec"}, true},
		{[]string{"mcp", "list"}, true},
		{[]string{"--auto=high"}, false},
		{[]string{"--", "exec"}, false},
		// FINDING-2 regression: a SPLIT value used to be read as the operand,
		// so the guard returned false and the handoff launched through the
		// headless `exec`. Every one of these is a grounded droid value-flag.
		{[]string{"--auto", "high", "exec"}, true},
		{[]string{"--append-system-prompt", "x=y", "exec"}, true},
		{[]string{"--settings", "/tmp/s.json", "mcp"}, true},
		{[]string{"--cwd", "/repo", "update"}, true},
		// …and the value itself must not be mistaken for the verb.
		{[]string{"--auto", "high"}, false},
		{[]string{"--append-system-prompt", "exec"}, false},
		// Grounded SWITCHES keep the scan aligned, so an ordinary multi-word
		// bare prompt whose Nth word happens to equal a verb is NOT rejected.
		{[]string{"--use-spec", "fix", "the", "mcp", "config"}, false},
		// An OPTIONAL-value option (`-r, --resume [sessionId]`) is in neither
		// grounded set, so the scan goes conservative: a headless verb behind
		// it is refused with a clear error rather than silently launched.
		{[]string{"-r", "exec"}, true},
	}
	for _, tc := range cases {
		if got := droidHeadlessScan.leads(tc.args); got != tc.want {
			t.Errorf("droidHeadlessScan.leads(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// TestDroidFlagGrammarIsDisjoint pins that no droid flag is claimed as BOTH a
// value-taking flag and a switch — the two tables are read off one `--help`
// block, and an overlap would mean one of them is a misreading.
func TestDroidFlagGrammarIsDisjoint(t *testing.T) {
	for f := range droidValueFlags {
		if droidBoolFlags[f] {
			t.Errorf("droid flag %q is in BOTH droidValueFlags and droidBoolFlags", f)
		}
	}
	// The OPTIONAL-value options must stay in NEITHER set (see leadingVerbScan).
	for _, f := range []string{"-r", "--resume", "-w", "--worktree"} {
		if droidValueFlags[f] || droidBoolFlags[f] {
			t.Errorf("optional-value flag %q must stay ungrounded so the scan goes conservative", f)
		}
	}
}

// TestDroidAttachPassthrough pins the wrapper-flag forwarding to the
// daemon-spawned inner launcher.
func TestDroidAttachPassthrough(t *testing.T) {
	if got := droidAttachPassthrough("/opt/droid"); !equalArgs(got, []string{"--droid-path", "/opt/droid"}) {
		t.Fatalf("droidAttachPassthrough = %v", got)
	}
	if got := droidAttachPassthrough(""); len(got) != 0 {
		t.Fatalf("expected empty passthrough, got %v", got)
	}
}
