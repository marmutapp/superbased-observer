// Package termscan is the UNTRUSTED terminal-hint parser (terminal-product-
// exploitation plan §2.1b / F3, channel (b)). It reads OSC 133/633 shell-
// integration prompt marks, OSC 0/1/2 titles, and BEL from the
// attacker-controlled PTY byte stream and emits structured HINTS — never
// authorizes anything. Everything a child writes to the PTY is forgeable (the
// model can print any escape), so every result carries an implicit trust=hint
// and the byte source stays separate from the trusted out-of-band channel
// (internal/termoob).
//
// It is a pure, incremental state machine: no SQL, no HTTP, no fsnotify, no
// allocation-unbounded buffers. Per §2.1b it uses SMALL per-sequence byte
// limits (NOT xterm's 10 MB OSC default) with malformed-sequence recovery, and
// it is fuzz-tested. It never renders or returns terminal text as HTML, and the
// title it surfaces is bounded and intended only for in-memory status hints —
// callers MUST NOT persist it (terminal_commands is metadata/coordinates only).
package termscan
