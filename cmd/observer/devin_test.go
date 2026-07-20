package main

import "testing"

// devinInject mirrors the promptInjection descriptor newDevinCmd hands to the
// shared continueFromArgs seam, so the argv-shape assertions below exercise
// the exact contract the launcher declares.
var devinInject = promptInjection{
	Kind:          injectTrailingPositionalAfterDashDash,
	ConflictFlags: []string{"-p", "--print", "--prompt-file"},
	Subcommands:   devinSubcommands,
}

func TestDevinInjectShape(t *testing.T) {
	const prompt = "SEEDED HANDOVER"
	t.Run("bare seed lands after a `--` separator", func(t *testing.T) {
		got, err := injectPrompt(nil, devinInject, prompt)
		if err != nil {
			t.Fatalf("injectPrompt: %v", err)
		}
		want := []string{"--", prompt}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("forwarded value-flag is preserved before the `--` seed", func(t *testing.T) {
		got, err := injectPrompt([]string{"--model=opus"}, devinInject, prompt)
		if err != nil {
			t.Fatalf("injectPrompt: %v", err)
		}
		want := []string{"--model=opus", "--", prompt}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	})

	t.Run("headless -p collides with the seeded handover", func(t *testing.T) {
		if _, err := injectPrompt([]string{"-p", "headless"}, devinInject, prompt); err == nil {
			t.Fatal("forwarded -p must collide with the seed")
		}
	})

	t.Run("separated --prompt-file value reads as a competing positional", func(t *testing.T) {
		// The grammar-light conflict check flags the separated value as a
		// bare positional (documented guidance: forward value-flags as
		// --flag=value so their value is not mistaken for a prompt).
		if _, err := injectPrompt([]string{"--prompt-file", "/tmp/p.md"}, devinInject, prompt); err == nil {
			t.Fatal("separated --prompt-file value must trip the conservative collision check")
		}
	})

	t.Run("value-flag in --flag=value form is tolerated (no false collision)", func(t *testing.T) {
		got, err := injectPrompt([]string{"--prompt-file=/tmp/p.md"}, devinInject, prompt)
		if err != nil {
			t.Fatalf("--flag=value form must not trip the collision check: %v", err)
		}
		if got[len(got)-1] != prompt || got[len(got)-2] != "--" {
			t.Fatalf("seed must still land after `--`, got %v", got)
		}
	})

	t.Run("a forwarded bare prompt collides", func(t *testing.T) {
		if _, err := injectPrompt([]string{"do the thing"}, devinInject, prompt); err == nil {
			t.Fatal("a forwarded positional prompt must collide with the seed")
		}
	})
}

func TestDevinSubcommandsNotMisreadAsPrompt(t *testing.T) {
	// A forwarded devin subcommand token (e.g. `devin list`) must not be
	// mistaken for a competing positional prompt.
	for _, sub := range []string{"list", "ls", "cloud", "acp", "auth", "version"} {
		if forwardedPromptConflict([]string{sub}, devinSubcommands) {
			t.Errorf("subcommand %q was misread as a forwarded prompt", sub)
		}
	}
	// A subcommand FOLLOWED by a bare prompt is still a collision.
	if !forwardedPromptConflict([]string{"list", "do it"}, devinSubcommands) {
		t.Error("subcommand + trailing prompt must be flagged as a conflict")
	}
}
