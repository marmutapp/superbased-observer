package policy

import (
	"strings"
	"testing"
)

// TestPSDialect_Parsing pins PowerShell-dialect lexing: alias
// resolution, separators, the call operator, backtick escapes and
// stop-parsing.
func TestPSDialect_Parsing(t *testing.T) {
	t.Parallel()

	t.Run("alias resolution", func(t *testing.T) {
		cmds := ParseCommand(`rm -Recurse -Force C:\Users\u\proj`, DialectPowerShell)
		if len(cmds) != 1 || cmds[0].Base != "remove-item" {
			t.Fatalf("got %s", bases(cmds))
		}
	})

	t.Run("canonical name passes through", func(t *testing.T) {
		cmds := ParseCommand(`Remove-Item -r C:\x`, DialectPowerShell)
		if len(cmds) != 1 || cmds[0].Base != "remove-item" {
			t.Fatalf("got %s", bases(cmds))
		}
	})

	t.Run("backslashes are literal", func(t *testing.T) {
		cmds := ParseCommand(`Get-Content C:\Users\u\.ssh\id_rsa`, DialectPowerShell)
		if len(cmds) != 1 || cmds[0].Argv[1] != `C:\Users\u\.ssh\id_rsa` {
			t.Fatalf("argv = %v (PS must not strip backslashes)", cmds[0].Argv)
		}
	})

	t.Run("semicolon separator", func(t *testing.T) {
		cmds := ParseCommand("Get-Content x.txt; rm -r y", DialectPowerShell)
		if len(cmds) != 2 || cmds[0].Base != "get-content" || cmds[1].Base != "remove-item" {
			t.Fatalf("got %s", bases(cmds))
		}
	})

	t.Run("call operator", func(t *testing.T) {
		cmds := ParseCommand(`& "C:\tools\cleaner.exe" -all`, DialectPowerShell)
		if len(cmds) != 1 || cmds[0].Base != "cleaner" {
			t.Fatalf("got %s", bases(cmds))
		}
	})

	t.Run("backtick escapes a space", func(t *testing.T) {
		cmds := ParseCommand("Get-Content File` Name.txt", DialectPowerShell)
		if len(cmds) != 1 || cmds[0].Argv[1] != "File Name.txt" {
			t.Fatalf("argv = %v", cmds[0].Argv)
		}
	})

	t.Run("stop-parsing token", func(t *testing.T) {
		cmds := ParseCommand("icacls --% C:\\x /grant Everyone:F", DialectPowerShell)
		if len(cmds) != 1 || cmds[0].Base != "icacls" {
			t.Fatalf("got %s", bases(cmds))
		}
		if !strings.Contains(strings.Join(cmds[0].Argv, " "), "/grant") {
			t.Errorf("literal words after --%% lost: %v", cmds[0].Argv)
		}
	})

	t.Run("pipeline", func(t *testing.T) {
		cmds := ParseCommand("Get-ChildItem | Remove-Item -Recurse", DialectPowerShell)
		if len(cmds) != 2 || !cmds[1].Pipeline {
			t.Fatalf("got %+v", cmds)
		}
	})
}

// TestPSParamMatching pins the prefix-disambiguation model the
// Remove-Item rules depend on.
func TestPSParamMatching(t *testing.T) {
	t.Parallel()
	cases := []struct {
		token, full string
		minLen      int
		want        bool
	}{
		{"-r", "recurse", 1, true},
		{"-rec", "recurse", 1, true},
		{"-recurse", "recurse", 1, true},
		{"-Recurse", "recurse", 1, true}, // case-insensitive
		{"-recursex", "recurse", 1, false},
		{"-f", "force", 2, false}, // ambiguous with -Filter: minLen 2
		{"-fo", "force", 2, true},
		{"-force", "force", 2, true},
		{"-x", "recurse", 1, false},
		{"r", "recurse", 1, false}, // no dash
		{"-c", "command", 1, true},
		{"-com", "command", 1, true},
		{"-confirm", "command", 1, false},
		{"-e", "encodedcommand", 1, true},
		{"-enc", "encodedcommand", 1, true},
		{"-ex", "encodedcommand", 1, false}, // prefix of executionpolicy, not ours
	}
	for _, tc := range cases {
		if got := psParamMatches(tc.token, tc.full, tc.minLen); got != tc.want {
			t.Errorf("psParamMatches(%q, %q, %d) = %v, want %v", tc.token, tc.full, tc.minLen, got, tc.want)
		}
	}
}

// TestDecodeEncodedPS pins the base64/UTF-16LE round trip and the
// reject paths.
func TestDecodeEncodedPS(t *testing.T) {
	t.Parallel()
	const want = `Remove-Item -Recurse C:\`
	enc := encodePS(t, want)
	got, ok := decodeEncodedPS(enc)
	if !ok || got != want {
		t.Fatalf("round trip = %q/%v", got, ok)
	}
	if _, ok := decodeEncodedPS("not-base64!!!"); ok {
		t.Error("invalid base64 must not decode")
	}
	if _, ok := decodeEncodedPS("YQ=="); ok { // 1 byte: odd UTF-16 length
		t.Error("odd-length payload must not decode")
	}
}

// TestCmdDialect_Parsing pins cmd.exe lexing: slash flags (spaced and
// run-together), '&' separation, caret escapes and the no-semicolon
// rule.
func TestCmdDialect_Parsing(t *testing.T) {
	t.Parallel()

	t.Run("del with flags", func(t *testing.T) {
		cmds := ParseCommand(`del /s /q C:\foo\*`, DialectCmd)
		if len(cmds) != 1 || cmds[0].Base != "del" {
			t.Fatalf("got %s", bases(cmds))
		}
		if !cmdHasFlag(&cmds[0], "s") || !cmdHasFlag(&cmds[0], "q") || cmdHasFlag(&cmds[0], "f") {
			t.Error("slash flags wrong")
		}
		if pos := cmds[0].Positionals(); len(pos) != 1 || pos[0] != `C:\foo\*` {
			t.Errorf("positionals = %v", pos)
		}
	})

	t.Run("run-together flags", func(t *testing.T) {
		cmds := ParseCommand(`del /s/q C:\foo`, DialectCmd)
		if !cmdHasFlag(&cmds[0], "s") || !cmdHasFlag(&cmds[0], "q") {
			t.Error("run-together /s/q not recognized")
		}
	})

	t.Run("ampersand separates", func(t *testing.T) {
		cmds := ParseCommand(`rd /s /q C:\x & echo done`, DialectCmd)
		if len(cmds) != 2 || cmds[0].Base != "rd" || cmds[1].Base != "echo" {
			t.Fatalf("got %s", bases(cmds))
		}
	})

	t.Run("semicolon is not a separator", func(t *testing.T) {
		cmds := ParseCommand("echo a;b", DialectCmd)
		if len(cmds) != 1 {
			t.Fatalf("cmd dialect must not split on ';': %s", bases(cmds))
		}
	})

	t.Run("caret escape", func(t *testing.T) {
		cmds := ParseCommand("echo a^&b", DialectCmd)
		if len(cmds) != 1 || cmds[0].Argv[1] != "a&b" {
			t.Fatalf("got %+v", cmds)
		}
	})

	t.Run("no posix wrappers in cmd", func(t *testing.T) {
		// "timeout" is a real cmd.exe utility, not a wrapper.
		cmds := ParseCommand("timeout /t 5", DialectCmd)
		if len(cmds) != 1 || cmds[0].Base != "timeout" {
			t.Fatalf("got %s", bases(cmds))
		}
	})
}
