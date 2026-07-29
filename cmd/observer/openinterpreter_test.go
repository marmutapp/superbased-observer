package main

import "testing"

// openInterpreterInject mirrors the promptInjection descriptor
// newOpenInterpreterCmd hands to the shared continueFromArgs seam. NO tool
// binary is spawned.
var openInterpreterInject = promptInjection{
	Kind:        injectTrailingPositional,
	Subcommands: openInterpreterSubcommands,
}

// TestOpenInterpreterInjectShape pins the grounded seed contract read off THIS
// fork's own help (`interpreter --help`): `Usage: interpreter [OPTIONS]
// [PROMPT]` with `[PROMPT]  Optional user prompt to start the session` — the
// codex trailing-positional shape.
func TestOpenInterpreterInjectShape(t *testing.T) {
	const prompt = "SEEDED HANDOVER"

	t.Run("bare seed is the sole positional", func(t *testing.T) {
		got, err := injectPrompt(nil, openInterpreterInject, prompt)
		if err != nil {
			t.Fatalf("injectPrompt: %v", err)
		}
		if !equalArgs(got, []string{prompt}) {
			t.Fatalf("got %v, want %v", got, []string{prompt})
		}
	})

	t.Run("forwarded flags are preserved before the seed", func(t *testing.T) {
		got, err := injectPrompt([]string{"--model=gpt-5"}, openInterpreterInject, prompt)
		if err != nil {
			t.Fatalf("injectPrompt: %v", err)
		}
		if !equalArgs(got, []string{"--model=gpt-5", prompt}) {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("a forwarded exec subcommand still seeds (codex contract)", func(t *testing.T) {
		// `exec [PROMPT]` takes the same trailing positional, so a forwarded
		// verb is not a two-prompt collision — it is a different run mode the
		// operator asked for explicitly (mirrors `observer codex -- exec`).
		got, err := injectPrompt([]string{"exec"}, openInterpreterInject, prompt)
		if err != nil {
			t.Fatalf("injectPrompt: %v", err)
		}
		if !equalArgs(got, []string{"exec", prompt}) {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("a forwarded bare prompt collides", func(t *testing.T) {
		if _, err := injectPrompt([]string{"do the thing"}, openInterpreterInject, prompt); err == nil {
			t.Fatal("a forwarded positional prompt must collide with the seed")
		}
	})

	t.Run("subcommand + bare prompt collides", func(t *testing.T) {
		if _, err := injectPrompt([]string{"exec", "do it"}, openInterpreterInject, prompt); err == nil {
			t.Fatal("subcommand + forwarded prompt must collide")
		}
	})
}

// TestOpenInterpreterSubcommandsCoverTheForkExtras pins that the subcommand
// map is this fork's OWN command list, not codex's: the rebadged build ships
// resume/fork/archive/plugin/features on top of codex's verbs, and a missing
// entry would make a forwarded verb read as a competing prompt.
func TestOpenInterpreterSubcommandsCoverTheForkExtras(t *testing.T) {
	for _, sub := range []string{"resume", "fork", "archive", "unarchive", "delete", "plugin", "features", "review", "doctor", "update"} {
		if !openInterpreterSubcommands[sub] {
			t.Errorf("subcommand %q missing from openInterpreterSubcommands", sub)
		}
		if forwardedPromptConflict([]string{sub}, openInterpreterSubcommands) {
			t.Errorf("subcommand %q was misread as a forwarded prompt", sub)
		}
	}
	// Every codex verb must still be covered — the fork is a superset.
	for sub := range codexSubcommands {
		if !openInterpreterSubcommands[sub] {
			t.Errorf("codex verb %q missing from openInterpreterSubcommands", sub)
		}
	}
}

// TestOpenInterpreterHeadlessSubcommands pins the attach/seed incompatibility
// set: `exec`/`e` ("Run Codex non-interactively") and `review` ("Run a code
// review non-interactively"). `resume` is interactive and must NOT be in it.
// Since the FINDING-2 fix the scan is GROUNDED in the fork's clap flag
// grammar, so a SPLIT flag value no longer hides a following headless verb.
func TestOpenInterpreterHeadlessSubcommands(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"exec", "do it"}, true},
		{[]string{"e"}, true},
		{[]string{"review"}, true},
		{[]string{"resume", "uuid"}, false},
		{[]string{"-m", "gpt-5"}, false},
		{[]string{"--", "exec"}, false},
		// FINDING-2 regression: split values used to park in the operand slot.
		{[]string{"-m", "gpt-5", "exec"}, true},
		{[]string{"--config", "model=o3", "review"}, true},
		{[]string{"-s", "workspace-write", "exec"}, true},
		{[]string{"--add-dir", "/repo", "e"}, true},
		// The value itself is not the verb.
		{[]string{"--model", "exec"}, false},
		// A grounded switch keeps the scan aligned.
		{[]string{"--search", "explain", "the", "exec", "path"}, false},
		// `-i, --image <FILE>...` is VARIADIC → ungrounded on purpose, so the
		// scan goes conservative and refuses rather than silently launching.
		{[]string{"-i", "a.png", "b.png", "exec"}, true},
	}
	for _, tc := range cases {
		if got := openInterpreterHeadlessScan.leads(tc.args); got != tc.want {
			t.Errorf("openInterpreterHeadlessScan.leads(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// TestOpenInterpreterFlagGrammarIsDisjoint pins that no flag is claimed as
// both value-taking and a switch, and that the variadic `-i/--image` stays
// ungrounded so the conservative branch owns it.
func TestOpenInterpreterFlagGrammarIsDisjoint(t *testing.T) {
	for f := range openInterpreterValueFlags {
		if openInterpreterBoolFlags[f] {
			t.Errorf("flag %q is in BOTH openInterpreterValueFlags and openInterpreterBoolFlags", f)
		}
	}
	for _, f := range []string{"-i", "--image"} {
		if openInterpreterValueFlags[f] || openInterpreterBoolFlags[f] {
			t.Errorf("variadic flag %q must stay ungrounded so the scan goes conservative", f)
		}
	}
}

// TestOpenInterpreterAttachPassthrough pins the wrapper-flag forwarding.
func TestOpenInterpreterAttachPassthrough(t *testing.T) {
	if got := openInterpreterAttachPassthrough("/opt/interpreter"); !equalArgs(got, []string{"--interpreter-path", "/opt/interpreter"}) {
		t.Fatalf("openInterpreterAttachPassthrough = %v", got)
	}
	if got := openInterpreterAttachPassthrough(""); len(got) != 0 {
		t.Fatalf("expected empty passthrough, got %v", got)
	}
}
