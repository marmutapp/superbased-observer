package termscan

import (
	"strings"
	"testing"
)

// collect feeds input through a fresh Scanner and returns every emitted hint.
func collect(input string) []Hint {
	var got []Hint
	s := New(func(h Hint) { got = append(got, h) })
	s.Write([]byte(input))
	return got
}

func TestParsePromptMarksAndTitle(t *testing.T) {
	esc := "\x1b"
	code7 := 7
	tests := []struct {
		name  string
		input string
		want  []Hint
	}{
		{
			name:  "OSC 133 A/B/C/D BEL-terminated",
			input: esc + "]133;A\x07" + esc + "]133;B\x07" + esc + "]133;C\x07" + esc + "]133;D;0\x07",
			want: []Hint{
				{Kind: HintPromptStart},
				{Kind: HintCommandStart},
				{Kind: HintCommandExecuted},
				{Kind: HintCommandFinished, ExitCode: intp(0)},
			},
		},
		{
			name:  "OSC 133 D with nonzero exit, ST-terminated",
			input: esc + "]133;D;7" + esc + "\\",
			want:  []Hint{{Kind: HintCommandFinished, ExitCode: &code7}},
		},
		{
			name:  "OSC 133 D without exit code",
			input: esc + "]133;D\x07",
			want:  []Hint{{Kind: HintCommandFinished}},
		},
		{
			name:  "OSC 633 (VS Code) marks recognised",
			input: esc + "]633;A\x07" + esc + "]633;C\x07",
			want:  []Hint{{Kind: HintPromptStart}, {Kind: HintCommandExecuted}},
		},
		{
			name:  "OSC 633;E command line is IGNORED (content, not boundary)",
			input: esc + "]633;E;rm -rf /\x07",
			want:  nil,
		},
		{
			name:  "OSC 2 title",
			input: esc + "]2;my-title\x07",
			want:  []Hint{{Kind: HintTitle, Title: "my-title"}},
		},
		{
			name:  "OSC 0 title with embedded text",
			input: esc + "]0;claude — thinking\x07",
			want:  []Hint{{Kind: HintTitle, Title: "claude — thinking"}},
		},
		{
			name:  "bare BEL is a bell hint",
			input: "output\x07more",
			want:  []Hint{{Kind: HintBell}},
		},
		{
			name:  "OSC 8 hyperlink is ignored (not allow-listed)",
			input: esc + "]8;;https://evil.example\x07link\x07",
			want:  []Hint{{Kind: HintBell}}, // the trailing BEL after 'link'
		},
		{
			name:  "OSC 52 clipboard is ignored",
			input: esc + "]52;c;ZXZpbA==\x07",
			want:  nil,
		},
		{
			name:  "plain text yields nothing",
			input: "hello world\nno escapes here",
			want:  nil,
		},
		{
			name:  "CSI sequences are consumed, not emitted",
			input: esc + "[1;31mred" + esc + "[0m",
			want:  nil,
		},
		{
			name:  "DCS string consumed to ST",
			input: esc + "Psome dcs payload" + esc + "\\" + esc + "]2;t\x07",
			want:  []Hint{{Kind: HintTitle, Title: "t"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := collect(tc.input)
			assertHints(t, got, tc.want)
		})
	}
}

func TestByteSplitInvariance(t *testing.T) {
	// The same stream fed one byte at a time must yield identical hints — the
	// parser is incremental and must not depend on chunk boundaries.
	esc := "\x1b"
	stream := esc + "]133;A\x07text" + esc + "]133;C\x07" + esc + "]2;title\x07" + esc + "]133;D;3\x07"
	whole := collect(stream)

	var perByte []Hint
	s := New(func(h Hint) { perByte = append(perByte, h) })
	for i := 0; i < len(stream); i++ {
		s.Write([]byte{stream[i]})
	}
	assertHints(t, perByte, whole)
	if len(whole) != 4 {
		t.Fatalf("expected 4 hints, got %d", len(whole))
	}
}

func TestOversizedOSCDiscardedAndRecovers(t *testing.T) {
	esc := "\x1b"
	// A giant OSC 2 title (past maxOSCBytes) must be discarded, and the parser
	// must recover to emit the following well-formed mark.
	big := esc + "]2;" + strings.Repeat("A", maxOSCBytes+5000) + "\x07"
	follow := esc + "]133;A\x07"
	got := collect(big + follow)
	// The oversized title is dropped; only the prompt-start survives.
	if len(got) != 1 || got[0].Kind != HintPromptStart {
		t.Fatalf("expected recovery to a single prompt_start, got %+v", got)
	}
}

func TestNilCallbackIsNoop(t *testing.T) {
	s := New(nil)
	s.Write([]byte("\x1b]133;A\x07")) // must not panic
}

func TestUnterminatedOSCDoesNotEmit(t *testing.T) {
	// An OSC with no terminator never fires (no boundary observed yet).
	got := collect("\x1b]133;A")
	if len(got) != 0 {
		t.Fatalf("unterminated OSC should not emit, got %+v", got)
	}
}

// FuzzScanner asserts the parser never panics and stays bounded on arbitrary
// (attacker-controlled) input — the §2.1b hardening requirement.
func FuzzScanner(f *testing.F) {
	f.Add([]byte("\x1b]133;D;0\x07"))
	f.Add([]byte("\x1b]2;title\x07\x1b[31m\x07"))
	f.Add([]byte("\x1bP\x1b\\\x1b]999;x\x07"))
	f.Add([]byte("\x1b]0;" + strings.Repeat("x", 5000) + "\x07"))
	f.Fuzz(func(t *testing.T, data []byte) {
		s := New(func(Hint) {})
		s.Write(data)
		// The OSC buffer must never exceed the cap regardless of input.
		if len(s.osc) > maxOSCBytes {
			t.Fatalf("OSC buffer exceeded cap: %d > %d", len(s.osc), maxOSCBytes)
		}
	})
}

func intp(i int) *int { return &i }

func assertHints(t *testing.T, got, want []Hint) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("hint count = %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Kind != want[i].Kind || got[i].Title != want[i].Title {
			t.Errorf("hint[%d] = %+v, want %+v", i, got[i], want[i])
		}
		switch {
		case got[i].ExitCode == nil && want[i].ExitCode == nil:
		case got[i].ExitCode != nil && want[i].ExitCode != nil:
			if *got[i].ExitCode != *want[i].ExitCode {
				t.Errorf("hint[%d] exit = %d, want %d", i, *got[i].ExitCode, *want[i].ExitCode)
			}
		default:
			t.Errorf("hint[%d] exit-code presence mismatch: got %v want %v", i, got[i].ExitCode, want[i].ExitCode)
		}
	}
}
