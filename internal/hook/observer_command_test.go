package hook

import "testing"

// TestIsObserverClaudeCodeHookCommand pins the STRICT ownership
// predicate the doctor probe reports on. The harm it exists to prevent
// is a FALSE POSITIVE: warning a user about double-wiring that names
// hook entries observer never wrote.
//
// Contrast isObserverClaudeEntry (TestIsObserverClaudeEntry elsewhere),
// which is deliberately loose because the REGISTRAR must recognise its
// own stale entries in order to refresh them.
func TestIsObserverClaudeCodeHookCommand(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want bool
	}{
		// --- ours: must match ---
		{
			"native linux",
			"/home/u/superbased-observer/bin/observer hook claude-code stop",
			true,
		},
		{
			"native with --config suffix",
			"/home/u/bin/observer hook claude-code pre-tool --config '/home/u/.observer/config.toml'",
			true,
		},
		{
			"windows .exe, forward slashes",
			"D:/programsx/superbased-observer/bin/observer.exe hook claude-code stop",
			true,
		},
		{
			"quoted path with spaces",
			`"C:/Program Files/observer/observer.exe" hook claude-code stop`,
			true,
		},
		{
			"single-quoted path",
			"'/opt/my tools/observer' hook claude-code stop",
			true,
		},
		{
			"wsl.exe bridge with MSYS prefix",
			"MSYS_NO_PATHCONV=1 wsl.exe -d Ubuntu-20.04 -- /home/u/bin/observer hook claude-code stop",
			true,
		},
		{
			"wsl.exe bridge without the MSYS prefix (pre-v1.6.22 shape)",
			"wsl.exe -d Ubuntu -- /home/u/bin/observer hook claude-code session-start",
			true,
		},
		{
			"npm-bundled binary path",
			"/proj/node_modules/@superbased/observer-linux-x64/bin/observer hook claude-code stop",
			true,
		},
		{
			"suffixed observer binary (versioned / A-B build)",
			"/tmp/observer-A hook claude-code stop",
			true,
		},
		{
			"superbased-named binary",
			"/usr/local/bin/superbased hook claude-code stop",
			true,
		},
		{
			"bare name resolved from PATH",
			"observer hook claude-code stop",
			true,
		},

		// --- NOT ours: must not match (codex L6) ---
		{
			"third-party binary that happens to take the same subcommand",
			"/opt/acme hook claude-code audit",
			false,
		},
		{
			"third-party binary with observer in the middle of its name",
			"/opt/acme-observer-wrapper hook claude-code audit",
			false,
		},
		{
			"our binary, a DIFFERENT tool",
			"/home/u/bin/observer hook cursor pre-tool",
			false,
		},
		{
			"our binary, not the hook subcommand",
			"/home/u/bin/observer statusline",
			false,
		},
		{
			"a user's own linter",
			"my-own-linter",
			false,
		},
		{
			"the tokens appear only inside an argument",
			"/opt/acme --note 'hook claude-code stop'",
			false,
		},
		{
			"wsl bridge with no -- separator",
			"wsl.exe -d Ubuntu /home/u/bin/observer hook claude-code stop",
			false,
		},
		{
			"empty",
			"",
			false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsObserverClaudeCodeHookCommand(c.cmd); got != c.want {
				t.Errorf("IsObserverClaudeCodeHookCommand(%q) = %v, want %v", c.cmd, got, c.want)
			}
		})
	}
}
