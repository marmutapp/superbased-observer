package policy

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"unicode/utf16"
)

// findCmd returns the first parsed command with the given base, plus
// whether one exists.
func findCmd(cmds []Command, base string) (*Command, bool) {
	for i := range cmds {
		if cmds[i].Base == base {
			return &cmds[i], true
		}
	}
	return nil, false
}

// bases lists parsed bases in order (test diagnostics).
func bases(cmds []Command) string {
	var b []string
	for _, c := range cmds {
		b = append(b, c.Base)
	}
	return strings.Join(b, ",")
}

// TestParseCommand_Basics pins tokenization fundamentals: quoting,
// separators, pipelines, redirects, env assignments.
func TestParseCommand_Basics(t *testing.T) {
	t.Parallel()

	t.Run("simple", func(t *testing.T) {
		cmds := ParseCommand("rm -rf /tmp/x", DialectPosix)
		if len(cmds) != 1 || cmds[0].Base != "rm" {
			t.Fatalf("got %s", bases(cmds))
		}
		if got := cmds[0].Argv; len(got) != 3 || got[1] != "-rf" || got[2] != "/tmp/x" {
			t.Errorf("argv = %v", got)
		}
	})

	t.Run("quoting", func(t *testing.T) {
		cmds := ParseCommand(`echo "a b" 'c d' e\ f`, DialectPosix)
		if len(cmds) != 1 {
			t.Fatalf("got %s", bases(cmds))
		}
		want := []string{"echo", "a b", "c d", "e f"}
		for i, w := range want {
			if cmds[0].Argv[i] != w {
				t.Errorf("argv[%d] = %q, want %q", i, cmds[0].Argv[i], w)
			}
		}
	})

	t.Run("separators and pipeline", func(t *testing.T) {
		cmds := ParseCommand("a; b && c | d", DialectPosix)
		if len(cmds) != 4 {
			t.Fatalf("want 4 units, got %s", bases(cmds))
		}
		if cmds[0].Pipeline || cmds[1].Pipeline || !cmds[2].Pipeline || !cmds[3].Pipeline {
			t.Errorf("pipeline flags wrong: %+v", cmds)
		}
	})

	t.Run("redirects", func(t *testing.T) {
		cmds := ParseCommand("echo x > /tmp/out 2>/dev/null", DialectPosix)
		if len(cmds) != 1 {
			t.Fatalf("got %s", bases(cmds))
		}
		c := cmds[0]
		if len(c.Argv) != 2 || c.Argv[1] != "x" {
			t.Errorf("argv = %v (redirect targets must not be argv)", c.Argv)
		}
		if len(c.RedirectTargets) != 2 || c.RedirectTargets[0] != "/tmp/out" || c.RedirectTargets[1] != "/dev/null" {
			t.Errorf("redirects = %v", c.RedirectTargets)
		}
	})

	t.Run("fd duplication has no target", func(t *testing.T) {
		cmds := ParseCommand("cmd1 >&2 arg", DialectPosix)
		if len(cmds) != 1 || len(cmds[0].RedirectTargets) != 0 {
			t.Fatalf("redirects = %v", cmds[0].RedirectTargets)
		}
		if len(cmds[0].Argv) != 2 || cmds[0].Argv[1] != "arg" {
			t.Errorf("argv = %v", cmds[0].Argv)
		}
	})

	t.Run("env assignments stripped", func(t *testing.T) {
		cmds := ParseCommand("FOO=1 BAR=baz make all", DialectPosix)
		if len(cmds) != 1 || cmds[0].Base != "make" {
			t.Fatalf("got %s", bases(cmds))
		}
	})

	t.Run("subshell parens separate", func(t *testing.T) {
		cmds := ParseCommand("(cd /x && rm -rf y)", DialectPosix)
		if _, ok := findCmd(cmds, "rm"); !ok {
			t.Fatalf("rm not found in %s", bases(cmds))
		}
	})

	t.Run("empty", func(t *testing.T) {
		if got := ParseCommand("   ", DialectPosix); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})
}

// TestParseCommand_Wrappers pins wrapper stripping: sudo recording,
// flag/operand consumption, chains, xargs retention in Wrappers.
func TestParseCommand_Wrappers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in     string
		wantBase     string
		wantSudo     bool
		wantWrappers []string
	}{
		{"sudo", "sudo rm -rf /", "rm", true, []string{"sudo"}},
		{"sudo with user flag", "sudo -u root rm -rf /", "rm", true, []string{"sudo"}},
		{"chain", "env FOO=1 timeout 5 nice -n 10 rm x", "rm", false, []string{"env", "timeout", "nice"}},
		{"xargs", "xargs rm -rf", "rm", false, []string{"xargs"}},
		{"nohup", "nohup ./server", "server", false, []string{"nohup"}},
		{"exec", "exec rm -rf /tmp/x", "rm", false, []string{"exec"}},
		{"bare wrapper keeps original", "sudo", "sudo", false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmds := ParseCommand(tc.in, DialectPosix)
			if len(cmds) == 0 {
				t.Fatal("no commands")
			}
			c := cmds[0]
			if c.Base != tc.wantBase || c.Sudo != tc.wantSudo {
				t.Errorf("base=%q sudo=%v, want %q/%v", c.Base, c.Sudo, tc.wantBase, tc.wantSudo)
			}
			if fmt.Sprint(c.Wrappers) != fmt.Sprint(tc.wantWrappers) {
				t.Errorf("wrappers = %v, want %v", c.Wrappers, tc.wantWrappers)
			}
		})
	}
}

// TestParseCommand_Unwrap pins recursive -c unwrapping, the depth
// cap, eval, command substitution and interpreter payload capture.
func TestParseCommand_Unwrap(t *testing.T) {
	t.Parallel()

	t.Run("bash -c", func(t *testing.T) {
		cmds := ParseCommand(`bash -c "rm -rf /"`, DialectPosix)
		rm, ok := findCmd(cmds, "rm")
		if !ok {
			t.Fatalf("rm not unwrapped: %s", bases(cmds))
		}
		if rm.Depth != 1 {
			t.Errorf("depth = %d, want 1", rm.Depth)
		}
	})

	t.Run("combined flags -lc", func(t *testing.T) {
		cmds := ParseCommand(`bash -lc "rm -rf /tmp/x"`, DialectPosix)
		if _, ok := findCmd(cmds, "rm"); !ok {
			t.Fatalf("rm not unwrapped: %s", bases(cmds))
		}
	})

	t.Run("five levels still unwrap", func(t *testing.T) {
		in := "rm -rf /"
		for i := 0; i < 5; i++ {
			in = fmt.Sprintf("bash -c %q", in)
		}
		cmds := ParseCommand(in, DialectPosix)
		rm, ok := findCmd(cmds, "rm")
		if !ok {
			t.Fatalf("rm not unwrapped at depth 5: %s", bases(cmds))
		}
		if rm.Depth != 5 {
			t.Errorf("depth = %d, want 5", rm.Depth)
		}
	})

	t.Run("six levels go opaque", func(t *testing.T) {
		in := "rm -rf /"
		for i := 0; i < 6; i++ {
			in = fmt.Sprintf("bash -c %q", in)
		}
		cmds := ParseCommand(in, DialectPosix)
		if _, ok := findCmd(cmds, "rm"); ok {
			t.Fatalf("depth cap not enforced: %s", bases(cmds))
		}
		deepest := cmds[len(cmds)-1]
		if deepest.PayloadKind != "shell" || !strings.Contains(deepest.Payload, "rm") {
			t.Errorf("opaque payload missing: kind=%q payload=%q", deepest.PayloadKind, deepest.Payload)
		}
	})

	t.Run("eval", func(t *testing.T) {
		cmds := ParseCommand(`eval "rm -rf /tmp/x"`, DialectPosix)
		if _, ok := findCmd(cmds, "rm"); !ok {
			t.Fatalf("eval not re-parsed: %s", bases(cmds))
		}
	})

	t.Run("command substitution", func(t *testing.T) {
		cmds := ParseCommand("echo $(rm -rf /) done", DialectPosix)
		if _, ok := findCmd(cmds, "rm"); !ok {
			t.Fatalf("substitution not extracted: %s", bases(cmds))
		}
	})

	t.Run("backticks", func(t *testing.T) {
		cmds := ParseCommand("echo `rm -rf /tmp/y`", DialectPosix)
		if _, ok := findCmd(cmds, "rm"); !ok {
			t.Fatalf("backtick substitution not extracted: %s", bases(cmds))
		}
	})

	t.Run("python one-liner stays opaque", func(t *testing.T) {
		cmds := ParseCommand(`python3 -c "import os; os.system('rm -rf /')"`, DialectPosix)
		py, ok := findCmd(cmds, "python3")
		if !ok {
			t.Fatalf("python3 missing: %s", bases(cmds))
		}
		if py.PayloadKind != "python" || !strings.Contains(py.Payload, "os.system") {
			t.Errorf("payload = %q/%q", py.PayloadKind, py.Payload)
		}
		// The payload is NOT parsed as shell (gap F1: exposed, not
		// pretended-understood).
		if rm, ok := findCmd(cmds, "rm"); ok && rm.Depth > 0 {
			t.Errorf("python payload must not unwrap as shell")
		}
	})

	t.Run("script file is not a payload", func(t *testing.T) {
		cmds := ParseCommand("bash deploy.sh", DialectPosix)
		if len(cmds) != 1 || cmds[0].Payload != "" {
			t.Errorf("script operand misread as payload: %+v", cmds)
		}
	})
}

// TestParseCommand_DialectSwitch pins mid-parse dialect switches:
// powershell -Command, -EncodedCommand, cmd /c.
func TestParseCommand_DialectSwitch(t *testing.T) {
	t.Parallel()

	t.Run("powershell -Command", func(t *testing.T) {
		cmds := ParseCommand(`powershell -NoProfile -Command "Remove-Item -Recurse C:\Users\u\proj"`, DialectPosix)
		ri, ok := findCmd(cmds, "remove-item")
		if !ok {
			t.Fatalf("remove-item missing: %s", bases(cmds))
		}
		if ri.Dialect != DialectPowerShell {
			t.Errorf("dialect = %q", ri.Dialect)
		}
	})

	t.Run("powershell -EncodedCommand", func(t *testing.T) {
		enc := encodePS(t, `Remove-Item -Recurse -Force C:\`)
		cmds := ParseCommand("powershell -e "+enc, DialectPosix)
		if _, ok := findCmd(cmds, "remove-item"); !ok {
			t.Fatalf("encoded payload not decoded: %s", bases(cmds))
		}
	})

	t.Run("cmd /c", func(t *testing.T) {
		cmds := ParseCommand(`cmd /c del /s /q C:\foo`, DialectPosix)
		del, ok := findCmd(cmds, "del")
		if !ok {
			t.Fatalf("del missing: %s", bases(cmds))
		}
		if del.Dialect != DialectCmd || !cmdHasFlag(del, "s") {
			t.Errorf("dialect=%q /s=%v", del.Dialect, cmdHasFlag(del, "s"))
		}
	})
}

// encodePS builds a -EncodedCommand value (base64 over UTF-16LE).
func encodePS(t *testing.T, s string) string {
	t.Helper()
	u16 := utf16.Encode([]rune(s))
	raw := make([]byte, 2*len(u16))
	for i, u := range u16 {
		raw[2*i] = byte(u)
		raw[2*i+1] = byte(u >> 8)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// TestParseCommand_Heredoc pins heredoc capture: body attached to the
// owning unit, surrounding line preserved, following units intact.
func TestParseCommand_Heredoc(t *testing.T) {
	t.Parallel()

	t.Run("basic body", func(t *testing.T) {
		cmds := ParseCommand("psql <<EOF\nDROP TABLE users;\nEOF", DialectPosix)
		psql, ok := findCmd(cmds, "psql")
		if !ok {
			t.Fatalf("psql missing: %s", bases(cmds))
		}
		if !strings.Contains(psql.Heredoc, "DROP TABLE users") {
			t.Errorf("heredoc = %q", psql.Heredoc)
		}
	})

	t.Run("line after delimiter survives", func(t *testing.T) {
		cmds := ParseCommand("cat <<EOF > out.txt\nhello\nEOF\necho done", DialectPosix)
		cat, ok := findCmd(cmds, "cat")
		if !ok {
			t.Fatalf("cat missing: %s", bases(cmds))
		}
		if !strings.Contains(cat.Heredoc, "hello") {
			t.Errorf("heredoc = %q", cat.Heredoc)
		}
		if len(cat.RedirectTargets) != 1 || cat.RedirectTargets[0] != "out.txt" {
			t.Errorf("redirects = %v (text between heredoc intro and newline must survive)", cat.RedirectTargets)
		}
		if _, ok := findCmd(cmds, "echo"); !ok {
			t.Errorf("unit after heredoc body lost: %s", bases(cmds))
		}
	})

	t.Run("quoted delimiter and dash form", func(t *testing.T) {
		cmds := ParseCommand("mysql <<-'SQL'\nTRUNCATE TABLE logs;\n\tSQL", DialectPosix)
		my, ok := findCmd(cmds, "mysql")
		if !ok || !strings.Contains(my.Heredoc, "TRUNCATE") {
			t.Fatalf("heredoc = %+v", cmds)
		}
	})
}

// TestCommandAccessors pins the flag/positional helpers rules build
// on.
func TestCommandAccessors(t *testing.T) {
	t.Parallel()

	c := ParseCommand("git push --force-with-lease origin main", DialectPosix)[0]
	if c.HasLongFlag("--force") {
		t.Error("--force must not prefix-match --force-with-lease")
	}
	if !c.HasLongFlag("--force-with-lease") {
		t.Error("--force-with-lease not found")
	}

	c = ParseCommand("git push --force-with-lease=main origin", DialectPosix)[0]
	if !c.HasLongFlag("--force-with-lease") {
		t.Error("embedded-value long flag not matched")
	}

	c = ParseCommand("rm -rf x", DialectPosix)[0]
	if !c.HasShortFlag('r') || !c.HasShortFlag('f') || c.HasShortFlag('R') {
		t.Error("short flag cluster parsing wrong (must be case-sensitive)")
	}

	c = ParseCommand("git -C /elsewhere push origin main", DialectPosix)[0]
	if c.Sub() != "push" {
		t.Errorf("Sub() = %q, want push (value flag -C must consume /elsewhere)", c.Sub())
	}
	pos := c.Positionals()
	if len(pos) != 3 || pos[1] != "origin" || pos[2] != "main" {
		t.Errorf("positionals = %v", pos)
	}

	c = ParseCommand("git checkout -- file.txt", DialectPosix)[0]
	pos = c.Positionals()
	if len(pos) != 2 || pos[1] != "file.txt" {
		t.Errorf("positionals after -- = %v", pos)
	}

	c = ParseCommand(`psql --command="DROP TABLE x" db`, DialectPosix)[0]
	if v, ok := c.FlagValue("-c", "--command"); !ok || !strings.Contains(v, "DROP") {
		t.Errorf("FlagValue = %q/%v", v, ok)
	}
	c = ParseCommand("mysql -e 'DELETE FROM t'", DialectPosix)[0]
	if v, ok := c.FlagValue("-e", "--execute"); !ok || !strings.Contains(v, "DELETE") {
		t.Errorf("FlagValue separate form = %q/%v", v, ok)
	}
}

// TestParseArgv pins the pre-split argv entry point.
func TestParseArgv(t *testing.T) {
	t.Parallel()
	cmds := ParseArgv([]string{"sudo", "rm", "-rf", "/"}, DialectPosix)
	if len(cmds) == 0 || cmds[0].Base != "rm" || !cmds[0].Sudo {
		t.Fatalf("got %+v", cmds)
	}
	if got := ParseArgv(nil, DialectPosix); got != nil {
		t.Errorf("nil argv: %v", got)
	}
	// argv with a -c payload still unwraps.
	cmds = ParseArgv([]string{"bash", "-c", "rm -rf /"}, DialectPosix)
	if _, ok := findCmd(cmds, "rm"); !ok {
		t.Fatalf("argv -c payload not unwrapped: %s", bases(cmds))
	}
}

// TestCanonicalBase pins base canonicalization across path prefixes,
// extensions and dialects.
func TestCanonicalBase(t *testing.T) {
	t.Parallel()
	cases := []struct {
		word    string
		dialect Dialect
		want    string
	}{
		{"/usr/bin/rm", DialectPosix, "rm"},
		{`C:\Windows\System32\cmd.exe`, DialectPosix, "cmd"},
		{"RM", DialectPosix, "rm"},
		{"rm", DialectPowerShell, "remove-item"},
		{"rm", DialectPosix, "rm"}, // aliases only in PS dialect
		{"del.exe", DialectCmd, "del"},
		{"format.com", DialectCmd, "format"},
	}
	for _, tc := range cases {
		if got := canonicalBase(tc.word, tc.dialect); got != tc.want {
			t.Errorf("canonicalBase(%q, %s) = %q, want %q", tc.word, tc.dialect, got, tc.want)
		}
	}
}
