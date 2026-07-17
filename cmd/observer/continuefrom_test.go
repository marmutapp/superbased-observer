package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/handoff"
	"github.com/marmutapp/superbased-observer/internal/integration"
)

func TestLaunchDir(t *testing.T) {
	dir := t.TempDir()
	// An existing directory is a valid launch cwd.
	if got := launchDir(dir); got != dir {
		t.Errorf("launchDir(existing dir) = %q, want %q", got, dir)
	}
	// Empty / missing / non-directory all fall back to "" (inherit the
	// caller's cwd) so a bad or unreachable project root never breaks the
	// launch — it just doesn't chdir.
	if got := launchDir(""); got != "" {
		t.Errorf("launchDir(empty) = %q, want empty", got)
	}
	if got := launchDir(filepath.Join(dir, "does-not-exist")); got != "" {
		t.Errorf("launchDir(missing) = %q, want empty", got)
	}
	file := filepath.Join(dir, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := launchDir(file); got != "" {
		t.Errorf("launchDir(file) = %q, want empty (not a directory)", got)
	}
}

func TestBuildInjectedPrompt(t *testing.T) {
	const marker = "<!-- superbased-handoff abcd1234 -->"
	doc := marker + "\n# Session handoff\n\nbody body body"

	t.Run("inlines the full doc when it fits", func(t *testing.T) {
		got := buildInjectedPrompt(doc, "/proj/HANDOFF-abcd1234.md", 100*1024)
		if got != doc {
			t.Errorf("small doc must be inlined verbatim, got %q", got)
		}
	})

	t.Run("degrades to a pointer prompt past the bound, keeping the marker", func(t *testing.T) {
		got := buildInjectedPrompt(doc, "/proj/HANDOFF-abcd1234.md", 8)
		if got == doc {
			t.Fatal("over-bound doc must degrade to a pointer, not inline")
		}
		if !strings.HasPrefix(got, marker) {
			t.Errorf("pointer prompt must keep the handoff marker for the linker, got %q", got)
		}
		if !strings.Contains(got, "/proj/HANDOFF-abcd1234.md") {
			t.Errorf("pointer prompt must name the on-disk doc path, got %q", got)
		}
		if strings.Contains(got, "body body body") {
			t.Error("pointer prompt must NOT inline the full body")
		}
	})

	t.Run("maxBytes<=0 disables the bound", func(t *testing.T) {
		if got := buildInjectedPrompt(doc, "/p.md", 0); got != doc {
			t.Errorf("maxBytes<=0 must inline, got %q", got)
		}
	})

	t.Run("pointer without a marker line omits it cleanly", func(t *testing.T) {
		plain := "no marker here, just a long body that exceeds the bound"
		got := buildInjectedPrompt(plain, "/p.md", 4)
		if strings.Contains(got, "<!--") {
			t.Errorf("must not fabricate a marker, got %q", got)
		}
		if !strings.Contains(got, "/p.md") {
			t.Errorf("pointer must still name the path, got %q", got)
		}
	})
}

func TestHandoffMarkerLine(t *testing.T) {
	const marker = "<!-- superbased-handoff deadbeef -->"
	if got := handoffMarkerLine(marker + "\nrest"); got != marker {
		t.Errorf("marker line = %q, want %q", got, marker)
	}
	if got := handoffMarkerLine("# no marker\nrest"); got != "" {
		t.Errorf("non-marker doc must return empty, got %q", got)
	}
	if got := handoffMarkerLine(marker + "\r\nrest"); got != marker {
		t.Errorf("CRLF marker line must strip the CR, got %q", got)
	}
}

func TestForwardedPromptConflict(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		subcommands map[string]bool
		want        bool
	}{
		{"empty args, no conflict", nil, nil, false},
		{"only flags, no conflict", []string{"--print", "--model=opus"}, nil, false},
		{"separated flag-value is conservatively flagged (use --flag=value)", []string{"--model", "opus"}, nil, true},
		{"bare positional is a conflict", []string{"do the thing"}, nil, true},
		{"kv-shaped bare token is not a prompt", []string{"-c", "openai_base_url=x"}, nil, false},
		{"codex exec subcommand alone is not a prompt", []string{"exec"}, codexSubcommands, false},
		{"codex exec + prompt is a conflict", []string{"exec", "do it"}, codexSubcommands, true},
		{"codex resume + id is a conflict", []string{"resume", "sess-123"}, codexSubcommands, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := forwardedPromptConflict(tc.args, tc.subcommands); got != tc.want {
				t.Errorf("forwardedPromptConflict(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestForkFromFlags(t *testing.T) {
	t.Run("zero value is last-message fork", func(t *testing.T) {
		fp, err := forkFromFlags(0, "")
		if err != nil || fp.Kind != handoff.ForkLast {
			t.Errorf("got (%+v, %v), want last-message fork", fp, err)
		}
	})
	t.Run("message index", func(t *testing.T) {
		fp, err := forkFromFlags(5, "")
		if err != nil || fp.Kind != handoff.ForkMessageIndex || fp.MessageIndex != 5 {
			t.Errorf("got (%+v, %v)", fp, err)
		}
	})
	t.Run("rfc3339 time", func(t *testing.T) {
		fp, err := forkFromFlags(0, "2026-07-03T10:00:00Z")
		if err != nil || fp.Kind != handoff.ForkTime || fp.Time.IsZero() {
			t.Errorf("got (%+v, %v)", fp, err)
		}
	})
	t.Run("mutually exclusive", func(t *testing.T) {
		if _, err := forkFromFlags(5, "2026-07-03T10:00:00Z"); err == nil {
			t.Error("both flags set must error")
		}
	})
	t.Run("bad time errors", func(t *testing.T) {
		if _, err := forkFromFlags(0, "not-a-time"); err == nil {
			t.Error("malformed time must error")
		}
	})
}

func TestContinueFromLauncher(t *testing.T) {
	wired := map[string]string{
		"claude-code":     "observer claude",
		"codex":           "observer codex",
		"gemini-cli":      "observer gemini",
		"pi":              "observer pi",
		"opencode":        "observer opencode",
		"copilot-cli":     "observer copilot-cli",
		"cline-cli":       "observer cline-cli",
		"kilo-code-cli":   "observer kilo",
		"cursor":          "observer cursor",
		"openclaw":        "observer openclaw",
		"hermes":          "observer hermes",
		"antigravity-cli": "observer antigravity-cli",
		"qwen-code":       "observer qwen",
		"kiro-cli":        "observer kiro",
		"kimi-code":       "observer kimi",
		"grok":            "observer grok",
		"qoder":           "observer qoder",
		"goose":           "observer goose",
		"devin":           "observer devin",
	}
	for tool, want := range wired {
		if got := continueFromLauncher(tool); got != want {
			t.Errorf("continueFromLauncher(%q) = %q, want %q", tool, got, want)
		}
	}
	// Non-launcher tools return "": "cline" (without the -cli suffix) is the
	// IDE-extension adapter, not a CLI (its CLI sibling is "cline-cli").
	for _, tool := range []string{"cline", "antigravity"} {
		if got := continueFromLauncher(tool); got != "" {
			t.Errorf("non-launcher tool %q launcher = %q, want empty", tool, got)
		}
	}
}

// TestLaunchCapabilityMatchesWiredLauncher pins the declared launch
// capability (integration.HandoffCapability.Launch) against the actually
// wired launcher (continueFromLauncher), bidirectionally, so the dashboard
// embedded-terminal feature can never spawn a subcommand that isn't wired
// nor omit one that is. A registry Launch row without a launcher — or a
// launcher without a Launch row — fails here. The Subcommand must equal the
// launcher's verb (`observer <sub>`), which is what termsession spawns.
func TestLaunchCapabilityMatchesWiredLauncher(t *testing.T) {
	for _, c := range integration.Capabilities() {
		launcher := continueFromLauncher(c.Tool)
		if c.Handoff.Launchable() {
			if launcher == "" {
				t.Errorf("adapter %q declares Launch but has no wired continueFromLauncher", c.Tool)
				continue
			}
			if want := "observer " + c.Handoff.Launch.Subcommand; launcher != want {
				t.Errorf("adapter %q: Launch.Subcommand %q implies launcher %q, but continueFromLauncher = %q",
					c.Tool, c.Handoff.Launch.Subcommand, want, launcher)
			}
		} else if launcher != "" {
			t.Errorf("adapter %q has a wired continueFromLauncher %q but declares no Launch capability — drift",
				c.Tool, launcher)
		}
	}
}

func TestInjectPrompt(t *testing.T) {
	const prompt = "SEEDED HANDOVER"
	cases := []struct {
		name    string
		args    []string
		spec    promptInjection
		want    []string
		wantErr bool
	}{
		{
			name: "leading positional prepends",
			args: []string{"--print"},
			spec: promptInjection{Kind: injectLeadingPositional},
			want: []string{prompt, "--print"},
		},
		{
			name:    "leading positional conflicts with a forwarded prompt",
			args:    []string{"do the thing"},
			spec:    promptInjection{Kind: injectLeadingPositional},
			wantErr: true,
		},
		{
			name: "trailing positional appends",
			args: []string{"--model=opus"},
			spec: promptInjection{Kind: injectTrailingPositional},
			want: []string{"--model=opus", prompt},
		},
		{
			name: "trailing positional honors subcommands",
			args: []string{"exec"},
			spec: promptInjection{Kind: injectTrailingPositional, Subcommands: codexSubcommands},
			want: []string{"exec", prompt},
		},
		{
			name:    "trailing positional conflicts with subcommand + prompt",
			args:    []string{"exec", "do it"},
			spec:    promptInjection{Kind: injectTrailingPositional, Subcommands: codexSubcommands},
			wantErr: true,
		},
		{
			name: "trailing positional after dash-dash inserts the separator then the prompt",
			args: []string{"--model=opus"},
			spec: promptInjection{Kind: injectTrailingPositionalAfterDashDash},
			want: []string{"--model=opus", "--", prompt},
		},
		{
			name: "trailing positional after dash-dash with no forwarded args",
			args: nil,
			spec: promptInjection{Kind: injectTrailingPositionalAfterDashDash},
			want: []string{"--", prompt},
		},
		{
			name:    "trailing positional after dash-dash conflicts with a forwarded positional prompt",
			args:    []string{"do the thing"},
			spec:    promptInjection{Kind: injectTrailingPositionalAfterDashDash, Subcommands: devinSubcommands},
			wantErr: true,
		},
		{
			name: "trailing positional after dash-dash honors subcommands",
			args: []string{"list"},
			spec: promptInjection{Kind: injectTrailingPositionalAfterDashDash, Subcommands: devinSubcommands},
			want: []string{"list", "--", prompt},
		},
		{
			name: "flag value appends the flag once",
			args: []string{"--model=gemini-3"},
			spec: promptInjection{Kind: injectFlagValue, Flag: "-i", ConflictFlags: []string{"-i", "--prompt-interactive", "-p", "--prompt"}},
			want: []string{"--model=gemini-3", "-i", prompt},
		},
		{
			name:    "flag value conflicts when the seed flag is already forwarded",
			args:    []string{"-i", "existing"},
			spec:    promptInjection{Kind: injectFlagValue, Flag: "-i", ConflictFlags: []string{"-i", "--prompt-interactive", "-p", "--prompt"}},
			wantErr: true,
		},
		{
			name:    "flag value conflicts when the headless prompt flag is forwarded",
			args:    []string{"-p", "headless"},
			spec:    promptInjection{Kind: injectFlagValue, Flag: "-i", ConflictFlags: []string{"-i", "--prompt-interactive", "-p", "--prompt"}},
			wantErr: true,
		},
		{
			name:    "flag value conflicts with a bare positional prompt when positional is a prompt",
			args:    []string{"an initial query"},
			spec:    promptInjection{Kind: injectFlagValue, Flag: "-i", ConflictFlags: []string{"-i"}, BarePositionalIsPrompt: true},
			wantErr: true,
		},
		{
			name: "flag value tolerates a bare positional path when positional is not a prompt",
			args: []string{"./my-project"},
			spec: promptInjection{Kind: injectFlagValue, Flag: "--prompt", ConflictFlags: []string{"--prompt"}},
			want: []string{"./my-project", "--prompt", prompt},
		},
		{
			name: "flag value appends -i for the copilot/cline interactive seed",
			args: nil,
			spec: promptInjection{Kind: injectFlagValue, Flag: "-i", ConflictFlags: []string{"-i", "--interactive", "-p", "--prompt"}},
			want: []string{"-i", prompt},
		},
		{
			name:    "flag value without a flag errors",
			args:    nil,
			spec:    promptInjection{Kind: injectFlagValue},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := injectPrompt(tc.args, tc.spec, prompt)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("injectPrompt(%v) = %v, want error", tc.args, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("injectPrompt(%v) unexpected error: %v", tc.args, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("injectPrompt(%v) = %v, want %v", tc.args, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("injectPrompt(%v)[%d] = %q, want %q", tc.args, i, got[i], tc.want[i])
				}
			}
			// The prompt must appear exactly once (never duplicated).
			n := 0
			for _, a := range got {
				if a == prompt {
					n++
				}
			}
			if n != 1 {
				t.Errorf("prompt appeared %d times, want exactly 1: %v", n, got)
			}
		})
	}
}
