package statusline

import "testing"

// TestSanitizeControls pins sanitizeControls's documented policy (F1,
// sanitize.go): CR/LF, every other C0 control (including ESC and TAB),
// DEL, and C1 (decoded rune) collapse — one space per run of one or more
// consecutive control runes, then leading/trailing whitespace trimmed.
func TestSanitizeControls(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty string is a no-op", "", ""},
		{"already-clean text is unchanged", "claude-opus-4-5", "claude-opus-4-5"},
		{"embedded newline collapses to a space", "safe\nERROR", "safe ERROR"},
		{"CRLF collapses to a single space (one run)", "safe\r\nERROR", "safe ERROR"},
		{"lone ESC collapses to a space", "safe\x1bERROR", "safe ERROR"},
		{
			name: "ANSI SGR sequence: only the leading ESC is a control rune",
			in:   "safe\x1b[31mRED",
			want: "safe [31mRED",
		},
		{"tab collapses to a space, never preserved literally", "safe\tEND", "safe END"},
		{"DEL (0x7F) collapses to a space", "safe\x7fEND", "safe END"},
		{"C1 control (U+0085 NEL) collapses to a space", "safeEND", "safe END"},
		{"leading and trailing control runs are trimmed away entirely", "\n\nsafe\n\n", "safe"},
		{"a run of several consecutive controls collapses to ONE space", "safe\n\n\nEND", "safe END"},
		{"an all-control string sanitizes to the empty string", "\x00\x01\x02", ""},
		{"C0 range boundary (0x1F) is a control", "safe\x1fEND", "safe END"},
		{"C1 range boundary (0x9F) is a control", "safeEND", "safe END"},
		{"first printable rune after 0x9F (U+00A0 NBSP) is NOT stripped", "safe END", "safe END"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeControls(tt.in); got != tt.want {
				t.Errorf("sanitizeControls(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestIsControlRune spot-checks the exact boundaries of every range
// isControlRune classifies, since sanitizeControls's whole behavior rests
// on this predicate being precisely scoped (neither over- nor
// under-inclusive).
func TestIsControlRune(t *testing.T) {
	controls := []rune{0x00, 0x09, 0x0A, 0x0D, 0x1B, 0x1F, 0x7F, 0x80, 0x85, 0x9F}
	for _, r := range controls {
		if !isControlRune(r) {
			t.Errorf("isControlRune(%U) = false, want true", r)
		}
	}
	notControls := []rune{0x20, 'a', 'Z', '0', 0xA0, 0x100, '·', '…'}
	for _, r := range notControls {
		if isControlRune(r) {
			t.Errorf("isControlRune(%U) = true, want false", r)
		}
	}
}
