package main

import (
	"bytes"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/integration"
)

// TestInjectNativeResume pins the shared native-resume argv translation for
// every one of the 15 live-verified launcher verbs (native-resume wave,
// 2026-07-24): plain-flag prepends, the two subcommand-scoped shapes (kiro's
// `chat --resume-id`, goose's `session --resume --session-id`), and the two id
// transforms (goose scope-strip, kimi ensure-prefix). NO tool binary is spawned.
func TestInjectNativeResume(t *testing.T) {
	cases := []struct {
		name string
		verb string
		id   string
		args []string
		want []string
	}{
		// Plain-flag tools: `<flag> <id>` LEADS the argv.
		{"opencode alone", "opencode", "ses_1", nil, []string{"--session", "ses_1"}},
		{"opencode + remainder", "opencode", "ses_1", []string{"--model", "x"}, []string{"--session", "ses_1", "--model", "x"}},
		{"kilo", "kilo", "k1", nil, []string{"--session", "k1"}},
		{"cline-cli --id", "cline-cli", "1782548283719_prf8j", nil, []string{"--id", "1782548283719_prf8j"}},
		{"gemini --resume", "gemini", "11111111-2222-3333-4444-555555555555", nil, []string{"--resume", "11111111-2222-3333-4444-555555555555"}},
		{"copilot-cli --session-id", "copilot-cli", "uuid-c", nil, []string{"--session-id", "uuid-c"}},
		{"pi --session", "pi", "uuid-p", nil, []string{"--session", "uuid-p"}},
		{"qwen --resume", "qwen", "q1", nil, []string{"--resume", "q1"}},
		{"grok --resume", "grok", "g1", nil, []string{"--resume", "g1"}},
		{"devin mnemonic", "devin", "noon-quince", nil, []string{"--resume", "noon-quince"}},
		{"hermes --resume", "hermes", "20260627_132748_325fea", nil, []string{"--resume", "20260627_132748_325fea"}},
		{"qoder --resume", "qoder", "uuid-q", nil, []string{"--resume", "uuid-q"}},
		{"antigravity --conversation", "antigravity-cli", "uuid-a", nil, []string{"--conversation", "uuid-a"}},
		// cursor (live-confirmed 2026-07-25): the chatId IS our stored
		// SessionID — the id must pass through byte-for-byte, no transform.
		// JOINED `--resume=<id>` as ONE argv element: cursor declares the flag
		// with an optional value (`--resume [chatId]`), so the `=` form is the
		// correct spelling rather than relying on the parser to consume the
		// next token. Operator-corrected 2026-07-25.
		{"cursor --resume=", "cursor", "0d0c1289-d163-4743-b9f6-f12ee7c482c0", nil, []string{"--resume=0d0c1289-d163-4743-b9f6-f12ee7c482c0"}},
		{"cursor + remainder", "cursor", "0d0c1289-d163-4743-b9f6-f12ee7c482c0", []string{"--model", "x"}, []string{"--resume=0d0c1289-d163-4743-b9f6-f12ee7c482c0", "--model", "x"}},

		// kimi: `--session` needs the PREFIXED id. Idempotent transform.
		{"kimi prefixed passthrough", "kimi", "session_abc", nil, []string{"--session", "session_abc"}},
		{"kimi bare uuid gets prefixed", "kimi", "abc", nil, []string{"--session", "session_abc"}},

		// goose: `session --resume --session-id <raw>`, scoped id stripped.
		{"goose scope-stripped", "goose", "20260708_1@deadbeef", nil, []string{"session", "--resume", "--session-id", "20260708_1"}},
		{"goose scope-stripped + remainder", "goose", "20260708_1@deadbeef", []string{"-s"}, []string{"session", "--resume", "--session-id", "20260708_1", "-s"}},
		{"goose bare id (no scope)", "goose", "20260708_1", nil, []string{"session", "--resume", "--session-id", "20260708_1"}},

		// kiro: `chat --resume-id <id>`; a user-forwarded leading `chat` is not doubled.
		{"kiro chat subcommand", "kiro", "uuid-k", nil, []string{"chat", "--resume-id", "uuid-k"}},
		{"kiro strips leading chat", "kiro", "uuid-k", []string{"chat", "--foo"}, []string{"chat", "--resume-id", "uuid-k", "--foo"}},
		{"kiro keeps non-chat remainder", "kiro", "uuid-k", []string{"--foo"}, []string{"chat", "--resume-id", "uuid-k", "--foo"}},

		// Fail-open: an unknown verb leaves args untouched.
		{"unknown verb unchanged", "not-a-verb", "x", []string{"a"}, []string{"a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := injectNativeResume(tc.verb, tc.id, tc.args)
			if !equalArgs(got, tc.want) {
				t.Fatalf("injectNativeResume(%q, %q, %v) = %v, want %v", tc.verb, tc.id, tc.args, got, tc.want)
			}
		})
	}
}

// TestResumeTranslationsMatchGroundedRegistry pins the coupling between the
// registry ResumeNative rows and the launcher-side translation table: every
// ResumeNative row (except the bespoke flagships claude/codex, whose translation
// lives in their own injectClaudeResume/injectCodexResume) MUST have a
// resumeTranslations entry keyed by its ResumeSpec.Subcommand, and every
// translation entry must correspond to a grounded ResumeNative row — so a new
// registry row without a launcher translation (or vice-versa) fails loudly.
func TestResumeTranslationsMatchGroundedRegistry(t *testing.T) {
	bespoke := map[string]bool{"claude": true, "codex": true}
	// Every non-flagship ResumeNative row has a translation.
	for _, c := range integration.Capabilities() {
		if c.Resume.Kind != integration.ResumeNative {
			continue
		}
		sub := c.Resume.Subcommand
		if bespoke[sub] {
			continue
		}
		if _, ok := resumeTranslations[sub]; !ok {
			t.Errorf("tool %q: ResumeNative row (verb %q) has no resumeTranslations entry", c.Tool, sub)
		}
	}
	// Every translation entry corresponds to a grounded ResumeNative row.
	grounded := map[string]bool{}
	for _, c := range integration.Capabilities() {
		if c.Resume.Kind == integration.ResumeNative {
			grounded[c.Resume.Subcommand] = true
		}
	}
	for verb := range resumeTranslations {
		if !grounded[verb] {
			t.Errorf("resumeTranslations verb %q has no grounded ResumeNative registry row", verb)
		}
	}
}

// TestEveryLauncherRegistersResumeFlagWhenGrounded pins the flag-gating
// invariant across every launcher verb: `--resume` is registered EXACTLY when
// the tool's registry row grounds ResumeNative (capability dispatch, never a
// tool-name branch). A ResumeNone launchable tool (openclaw) must NOT
// carry the flag — its inner tool has no native-resume argv to translate to.
func TestEveryLauncherRegistersResumeFlagWhenGrounded(t *testing.T) {
	for _, lv := range allLauncherVerbs() {
		lv := lv
		t.Run(lv.verb, func(t *testing.T) {
			capab, _ := integration.For(lv.tool)
			hasFlag := lv.cmd.Flags().Lookup("resume") != nil
			wantFlag := capab.Resume.Kind == integration.ResumeNative
			if hasFlag != wantFlag {
				t.Fatalf("launcher %q (tool %q): --resume registered=%v, want=%v (ResumeKind=%q)",
					lv.verb, lv.tool, hasFlag, wantFlag, capab.Resume.Kind)
			}
		})
	}
}

// TestResumeAttachPassthrough pins that --resume is forwarded to the daemon-
// spawned inner launcher under --attach exactly when a resume was requested
// (never rejected — the mortality backstop).
func TestResumeAttachPassthrough(t *testing.T) {
	if got := resumeAttachPassthrough("sess-1"); !equalArgs(got, []string{"--resume", "sess-1"}) {
		t.Fatalf("resumeAttachPassthrough(sess-1) = %v, want [--resume sess-1]", got)
	}
	if got := resumeAttachPassthrough(""); len(got) != 0 {
		t.Fatalf("resumeAttachPassthrough(\"\") = %v, want empty", got)
	}
}

// TestRejectIncompatibleResumeFlags pins the shared handoff-fork rejection: any
// member of the --continue-from family collides with a native resume; nothing
// else does.
func TestRejectIncompatibleResumeFlags(t *testing.T) {
	cases := []struct {
		name         string
		continueFrom string
		carry        string
		fromMessage  int
		fromTime     string
		wantErr      bool
	}{
		{"clean", "", "", 0, "", false},
		{"continue-from", "s0", "", 0, "", true},
		{"carry", "", "full", 0, "", true},
		{"from-message", "", "", 3, "", true},
		{"from-time", "", "", 0, "2026-01-01T00:00:00Z", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := rejectIncompatibleResumeFlags("toolx", tc.continueFrom, tc.carry, tc.fromMessage, tc.fromTime)
			if tc.wantErr != (err != nil) {
				t.Fatalf("wantErr=%v, got err=%v", tc.wantErr, err)
			}
		})
	}
}

// TestApplyLauncherResume pins the shared bare-resume handler's control flow: a
// no-op when no resume was asked, a loud rejection of the handoff-fork family,
// and the translated argv on success. NO tool binary is spawned (the claim is
// a no-op with an empty DB path).
func TestApplyLauncherResume(t *testing.T) {
	t.Run("no-op when id empty", func(t *testing.T) {
		var stderr bytes.Buffer
		args := []string{"--model", "x"}
		got, release, ok, err := applyLauncherResume(launcherResumeSpec{
			verb: "opencode", label: "opencode", args: args, stderr: &stderr,
		})
		if !ok || err != nil {
			t.Fatalf("no-op: ok=%v err=%v", ok, err)
		}
		if !equalArgs(got, args) {
			t.Fatalf("no-op args = %v, want unchanged %v", got, args)
		}
		release() // must not panic
	})

	t.Run("rejects continue-from", func(t *testing.T) {
		var stderr bytes.Buffer
		_, _, ok, err := applyLauncherResume(launcherResumeSpec{
			verb: "opencode", label: "opencode", id: "s1", continueFrom: "s0", stderr: &stderr,
		})
		if ok || err == nil {
			t.Fatalf("continue-from conflict must fail: ok=%v err=%v", ok, err)
		}
	})

	t.Run("translates on success", func(t *testing.T) {
		var stderr bytes.Buffer
		got, release, ok, err := applyLauncherResume(launcherResumeSpec{
			verb: "goose", label: "goose", id: "20260708_1@deadbeef",
			args: []string{"-s"}, stderr: &stderr,
		})
		if !ok || err != nil {
			t.Fatalf("success path: ok=%v err=%v", ok, err)
		}
		defer release()
		want := []string{"session", "--resume", "--session-id", "20260708_1", "-s"}
		if !equalArgs(got, want) {
			t.Fatalf("translated args = %v, want %v", got, want)
		}
	})
}

// TestStripGooseScope + TestEnsureKimiSessionPrefix pin the two id transforms in
// isolation.
func TestStripGooseScope(t *testing.T) {
	cases := map[string]string{
		"20260708_1@deadbeef": "20260708_1",
		"20260708_1":          "20260708_1", // no scope → unchanged
		"a@b@c":               "a",          // strips at the FIRST '@'
	}
	for in, want := range cases {
		if got := stripGooseScope(in); got != want {
			t.Errorf("stripGooseScope(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnsureKimiSessionPrefix(t *testing.T) {
	cases := map[string]string{
		"session_abc": "session_abc", // already prefixed → unchanged (idempotent)
		"abc":         "session_abc", // bare → prefixed
	}
	for in, want := range cases {
		if got := ensureKimiSessionPrefix(in); got != want {
			t.Errorf("ensureKimiSessionPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}
