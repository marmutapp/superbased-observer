package termscan

import (
	"strconv"
	"strings"
)

// HintKind classifies a parsed hint. The prompt-mark kinds mirror the FinalTerm
// / iTerm2 (OSC 133) and VS Code (OSC 633) shell-integration vocabulary; they
// are the substrate for command/turn boundaries (F3) and status (F4).
type HintKind string

const (
	// HintPromptStart — OSC 133;A / 633;A: a fresh prompt is being drawn (the
	// shell is idle, waiting for the user). A strong "waiting" hint for F4.
	HintPromptStart HintKind = "prompt_start"
	// HintCommandStart — OSC 133;B / 633;B: end of prompt, start of the typed
	// command region.
	HintCommandStart HintKind = "command_start"
	// HintCommandExecuted — OSC 133;C / 633;C: the command began executing
	// (output follows). A "working" hint for F4.
	HintCommandExecuted HintKind = "command_executed"
	// HintCommandFinished — OSC 133;D[;code] / 633;D[;code]: the command
	// finished; ExitCode is set when the mark carried one.
	HintCommandFinished HintKind = "command_finished"
	// HintBell — a BEL (0x07) outside a string sequence: an attention signal
	// (a prompt/notification), a weak "waiting" hint.
	HintBell HintKind = "bell"
	// HintTitle — OSC 0/1/2: a window/icon title change. Title carries the
	// bounded text for IN-MEMORY status use only; callers never persist it.
	HintTitle HintKind = "title"
)

// Hint is one parsed, untrusted hint. It never authorizes input (§2.1b).
type Hint struct {
	Kind HintKind
	// ExitCode is the parsed exit code for HintCommandFinished, or nil.
	ExitCode *int
	// Title is the bounded title text for HintTitle, "" otherwise. For
	// in-memory status hints ONLY — never persist it (untrusted content).
	Title string
}

// Bounds on a single string sequence. Deliberately small (§2.1b — NOT xterm's
// 10 MB default). A sequence exceeding maxOSCBytes is discarded (overflow
// recovery) so a hostile child cannot force unbounded buffering; a title is
// additionally truncated to maxTitleBytes.
const (
	maxOSCBytes   = 1024
	maxTitleBytes = 256
)

type state uint8

const (
	stText      state = iota // normal text
	stEsc                    // saw ESC (0x1b)
	stCSI                    // ESC [ … final-byte
	stOSC                    // ESC ] … (BEL | ST)
	stOSCEsc                 // inside OSC, saw ESC (maybe ST = ESC \)
	stString                 // DCS/APC/PM/SOS … ST
	stStringEsc              // inside a string, saw ESC (maybe ST)
)

// Scanner is an incremental, bounded VT parser that emits hints. It is NOT
// safe for concurrent use — one Scanner per session, fed from that session's
// single output-drain goroutine. Zero value is not usable; call New.
type Scanner struct {
	onHint func(Hint)
	st     state
	// osc accumulates the current OSC payload (bounded); overflowed marks a
	// sequence past the cap so it is discarded on terminate.
	osc        []byte
	overflowed bool
}

// New builds a Scanner that calls onHint for every parsed hint. onHint must be
// cheap/non-blocking (it runs on the output-drain path); a nil onHint makes
// Write a no-op.
func New(onHint func(Hint)) *Scanner {
	return &Scanner{onHint: onHint, osc: make([]byte, 0, 64)}
}

// Write feeds terminal output bytes. It never errors and never blocks on the
// bytes themselves — untrusted input is parsed defensively, and any malformed
// or oversized sequence is recovered from without affecting the byte stream the
// PTY bridge forwards to the client (this is a passive tap).
func (s *Scanner) Write(p []byte) {
	if s.onHint == nil {
		return
	}
	for _, b := range p {
		s.step(b)
	}
}

func (s *Scanner) step(b byte) {
	switch s.st {
	case stText:
		s.stepText(b)
	case stEsc:
		s.stepEsc(b)
	case stCSI:
		// Consume until a CSI final byte (0x40–0x7e); we don't need CSI content.
		if b >= 0x40 && b <= 0x7e {
			s.st = stText
		}
	case stOSC:
		s.stepOSC(b)
	case stOSCEsc:
		if b == '\\' { // ST
			s.terminateOSC()
		} else {
			// Not an ST — abandon the OSC and reprocess this byte as text.
			s.resetOSC()
			s.st = stText
			s.step(b)
		}
	case stString:
		if b == 0x1b {
			s.st = stStringEsc
		}
	case stStringEsc:
		// ST ends the string; anything else stays inside it.
		s.st = stString
		if b == '\\' {
			s.st = stText
		}
	}
}

func (s *Scanner) stepText(b byte) {
	switch b {
	case 0x1b: // ESC
		s.st = stEsc
	case 0x07: // BEL outside a string
		s.emit(Hint{Kind: HintBell})
	}
}

func (s *Scanner) stepEsc(b byte) {
	switch b {
	case '[':
		s.st = stCSI
	case ']':
		s.resetOSC()
		s.st = stOSC
	case 'P', '_', '^', 'X': // DCS / APC / PM / SOS — consume to ST, ignore
		s.st = stString
	default:
		// Two-char escape (or garbage) — ignore and return to text.
		s.st = stText
	}
}

func (s *Scanner) stepOSC(b byte) {
	switch b {
	case 0x07: // BEL terminator
		s.terminateOSC()
	case 0x1b: // maybe ST (ESC \)
		s.st = stOSCEsc
	default:
		if len(s.osc) >= maxOSCBytes {
			s.overflowed = true // stop appending; keep consuming until terminator
			return
		}
		s.osc = append(s.osc, b)
	}
}

func (s *Scanner) resetOSC() {
	s.osc = s.osc[:0]
	s.overflowed = false
}

// terminateOSC parses the accumulated OSC payload into a hint (unless it
// overflowed) and returns to text.
func (s *Scanner) terminateOSC() {
	if !s.overflowed {
		s.parseOSC(string(s.osc))
	}
	s.resetOSC()
	s.st = stText
}

// parseOSC decodes an OSC payload of the form "<ps>;<pt>". Only allow-listed
// codes produce hints: 0/1/2 (title) and 133/633 (shell-integration prompt
// marks). Everything else (OSC 8 hyperlinks, OSC 52 clipboard, images, …) is
// ignored — the plan mandates NOT acting on those.
func (s *Scanner) parseOSC(payload string) {
	ps, rest, _ := strings.Cut(payload, ";")
	switch ps {
	case "0", "1", "2":
		title := rest
		if len(title) > maxTitleBytes {
			title = title[:maxTitleBytes]
		}
		s.emit(Hint{Kind: HintTitle, Title: title})
	case "133", "633":
		s.parsePromptMark(rest)
	}
}

// parsePromptMark decodes the sub-command of an OSC 133/633 prompt mark. The
// first field is the mark letter (A/B/C/D); for D an optional numeric exit code
// may follow. VS Code's OSC 633;E (command line) and other sub-commands carry
// command TEXT and are deliberately ignored — we take boundaries only, never
// content.
func (s *Scanner) parsePromptMark(rest string) {
	mark, tail, _ := strings.Cut(rest, ";")
	switch mark {
	case "A":
		s.emit(Hint{Kind: HintPromptStart})
	case "B":
		s.emit(Hint{Kind: HintCommandStart})
	case "C":
		s.emit(Hint{Kind: HintCommandExecuted})
	case "D":
		h := Hint{Kind: HintCommandFinished}
		if code, ok := parseExitCode(tail); ok {
			h.ExitCode = &code
		}
		s.emit(h)
	}
}

// parseExitCode extracts a leading integer exit code from a D-mark tail
// ("<code>" or "<code>;<extra>"). Returns ok=false when absent/non-numeric.
func parseExitCode(tail string) (int, bool) {
	if tail == "" {
		return 0, false
	}
	field, _, _ := strings.Cut(tail, ";")
	n, err := strconv.Atoi(strings.TrimSpace(field))
	if err != nil {
		return 0, false
	}
	return n, true
}

func (s *Scanner) emit(h Hint) { s.onHint(h) }
