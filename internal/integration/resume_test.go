package integration_test

import (
	"testing"

	"github.com/marmutapp/superbased-observer/internal/integration"
)

// TestResumeArgs pins the native-resume composer: every grounded ResumeKind/
// IDMechanism combination maps to the uniform observer-launcher tail
// `--resume <id>`, and every dishonest input errors instead of fabricating a
// command.
func TestResumeArgs(t *testing.T) {
	cases := []struct {
		name    string
		spec    integration.ResumeSpec
		id      string
		want    []string
		wantErr bool
	}{
		// Grounded mechanisms → uniform tail (the launcher translates it).
		{
			"claude flag mechanism",
			integration.ResumeSpec{Kind: integration.ResumeNative, Subcommand: "claude", IDMechanism: "flag:--resume"},
			"sess-abc",
			[]string{"--resume", "sess-abc"},
			false,
		},
		{
			"codex subcommand mechanism",
			integration.ResumeSpec{Kind: integration.ResumeNative, Subcommand: "codex", IDMechanism: "subcommand:resume"},
			"11111111-2222-3333-4444-555555555555",
			[]string{"--resume", "11111111-2222-3333-4444-555555555555"},
			false,
		},
		{
			"positional mechanism",
			integration.ResumeSpec{Kind: integration.ResumeNative, Subcommand: "toolx", IDMechanism: "positional"},
			"id42",
			[]string{"--resume", "id42"},
			false,
		},
		{
			"id is trimmed",
			integration.ResumeSpec{Kind: integration.ResumeNative, Subcommand: "claude", IDMechanism: "flag:--resume"},
			"  sess-trim  ",
			[]string{"--resume", "sess-trim"},
			false,
		},
		// Error cases — never a fabricated command.
		{
			"ResumeNone rejected",
			integration.ResumeSpec{Kind: integration.ResumeNone},
			"sess-abc", nil, true,
		},
		{
			"ResumeFork rejected",
			integration.ResumeSpec{Kind: integration.ResumeFork, Subcommand: "claude"},
			"sess-abc", nil, true,
		},
		{
			"empty subcommand rejected",
			integration.ResumeSpec{Kind: integration.ResumeNative, Subcommand: "", IDMechanism: "flag:--resume"},
			"sess-abc", nil, true,
		},
		{
			"empty id rejected",
			integration.ResumeSpec{Kind: integration.ResumeNative, Subcommand: "claude", IDMechanism: "flag:--resume"},
			"", nil, true,
		},
		{
			"whitespace-only id rejected",
			integration.ResumeSpec{Kind: integration.ResumeNative, Subcommand: "claude", IDMechanism: "flag:--resume"},
			"   ", nil, true,
		},
		{
			"leading-dash id rejected (flag injection)",
			integration.ResumeSpec{Kind: integration.ResumeNative, Subcommand: "claude", IDMechanism: "flag:--resume"},
			"-rf", nil, true,
		},
		{
			"unknown mechanism rejected",
			integration.ResumeSpec{Kind: integration.ResumeNative, Subcommand: "claude", IDMechanism: "flag:--frobnicate"},
			"sess-abc", nil, true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := integration.ResumeArgs(tc.spec, tc.id)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got args %v", got)
				}
				if got != nil {
					t.Fatalf("expected nil args on error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("arg[%d] = %q, want %q (got %v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

// TestResumeArgsGroundedRegistryRows pins that the composer produces a valid
// command for every registry row that DECLARES ResumeNative — the grounded
// tools (claude-code, codex) must compose cleanly, so a mis-grounded row is
// caught here rather than at a dashboard call site.
func TestResumeArgsGroundedRegistryRows(t *testing.T) {
	for _, c := range integration.Capabilities() {
		if c.Resume.Kind != integration.ResumeNative {
			continue
		}
		got, err := integration.ResumeArgs(c.Resume, "some-session-id")
		if err != nil {
			t.Errorf("adapter %q: ResumeNative row does not compose: %v", c.Tool, err)
			continue
		}
		if len(got) != 2 || got[0] != "--resume" || got[1] != "some-session-id" {
			t.Errorf("adapter %q: composed %v, want [--resume some-session-id]", c.Tool, got)
		}
	}
}
