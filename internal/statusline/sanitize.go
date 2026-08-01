package statusline

import "strings"

// sanitizeControls strips or replaces control characters from text that
// originates OUTSIDE this process — currently only Input.Model's
// DisplayName/ID (segments.go's modelName), the sole externally-sourced
// string this package ever composes into the rendered line (F1: every
// other Input field this package reads — Cost's figures — is numeric and
// goes through formatUSD, never interpolated as raw text; the wordmark
// and every separator/label in this package's output is a Go source
// constant, trusted by construction).
//
// A Claude Code statusLine command's stdout is read VERBATIM and
// re-rendered into a host UI element. An embedded newline or carriage
// return produces a MULTI-LINE status render — the worst failure mode
// for a one-line status surface, since it corrupts the host's own layout
// rather than just this command's output. An embedded ESC (0x1B) or C1
// control byte can alter the host terminal's own rendering state. This
// boundary treats every byte of externally-sourced text as hostile,
// regardless of how implausible a given field looks in practice —
// {"model":{"display_name":"safe\nERROR"}} on stdin must never reach
// stdout un-sanitized.
//
// Scope of what's stripped/replaced (documented, not left implicit):
//   - CR ('\r') and LF ('\n')
//   - every other C0 control, 0x00-0x1F inclusive — this range already
//     subsumes CR, LF, ESC (0x1B), and TAB (0x09): a tab is treated
//     exactly like any other control character under this policy, never
//     preserved as a literal tab in the rendered line.
//   - DEL (0x7F)
//   - the C1 control range, 0x80-0x9F inclusive, once UTF-8-decoded to a
//     single rune (catches a C1 control transmitted as its single-rune
//     form rather than as a 2-byte ESC-prefixed escape sequence — stdin
//     JSON string content is valid UTF-8 text, not raw terminal bytes,
//     so a C1 control here always arrives as one decoded rune).
//
// Policy: a RUN of one or more consecutive control runes collapses to a
// SINGLE space, and the final result is trimmed of leading/trailing
// whitespace. This keeps "safe\nERROR" readable as "safe ERROR" — a
// legible, single-line result — rather than either dropping the
// separator entirely (producing the misleadingly-concatenated
// "safeERROR") or leaving one space per stripped byte. Stripping the
// leading ESC byte of an ANSI escape sequence (e.g. "\x1b[31mRED") also
// neutralizes it as an escape sequence: what remains ("[31mRED") is
// inert literal text to the terminal, since ESC is the byte that gives
// the sequence its meaning.
func sanitizeControls(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	inControlRun := false
	for _, r := range s {
		if isControlRune(r) {
			if !inControlRun {
				b.WriteByte(' ')
				inControlRun = true
			}
			continue
		}
		inControlRun = false
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// isControlRune reports whether r is a control character this package
// treats as unsafe to place in the rendered statusline: C0 (0x00-0x1F,
// which subsumes CR, LF, ESC, and TAB), DEL (0x7F), and the C1 range
// (0x80-0x9F) once decoded to a rune. See sanitizeControls for the full
// rationale and the exact replacement policy.
func isControlRune(r rune) bool {
	switch {
	case r <= 0x1F: // C0 — includes CR 0x0D, LF 0x0A, ESC 0x1B, TAB 0x09
		return true
	case r == 0x7F: // DEL
		return true
	case r >= 0x80 && r <= 0x9F: // C1
		return true
	default:
		return false
	}
}
