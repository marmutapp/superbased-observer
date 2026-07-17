// Package termoob is the pure-logic wire protocol for the TRUSTED out-of-band
// launcher control channel (docs/plans/terminal-product-exploitation-plan-2026-07-12.md
// §2.1b / F3, Phase 0 item S0d).
//
// Everything a child writes to the PTY is attacker-controlled — the model can
// print any byte sequence, so a private OSC on the PTY stream is FORGEABLE
// (§2.1b). Trusted launcher-lifecycle signals therefore ride a dedicated,
// inherited file descriptor the child cannot forge, framed and
// per-session-authenticated by this package. (Untrusted OSC 133/1337/title
// HINTS parsed from the PTY stream are a separate, later concern — the F3
// termscan half — whose result type carries a trust=hint flag and never
// authorizes input. This package is the trusted half only.)
//
// This package is PURE (CLAUDE.md §1): it encodes/decodes frames over an
// injected io.Reader / io.Writer and knows nothing about os.Pipe, ExtraFiles,
// or process spawning. The transport wiring — allocating the inherited FD in
// the launcher-spawn path and draining it on a daemon goroutine — is the F1/F3
// integration that lives in cmd / internal/termsession; it calls NewDecoder /
// NewEncoder with the real pipe ends. No database/sql, net/http, or fsnotify
// (pinned by imports_test.go).
//
// Security posture:
//   - Per-session authentication. The daemon mints a random session secret
//     (NewSessionToken), passes it to the child OUT of the PTY (an inherited
//     env var / the FD handshake), and constructs the Decoder with it. The
//     child's FIRST frame MUST be a Hello whose AuthToken matches (constant-time
//     compare). An unauthenticated or mismatched channel is poisoned — every
//     subsequent Read errors. The FD is already unforgeable by the model; the
//     token is defense in depth against a confused-deputy inheriting the FD.
//   - Small, hard per-frame size limit (MaxFrameBytes, NOT xterm's 10 MB OSC
//     default). An oversized length header poisons the channel.
//   - Fail-closed framing. A length-prefixed binary channel cannot resync
//     mid-stream, so ANY framing/auth error is fatal for the channel (the
//     daemon drops it). Malformed-sequence RECOVERY is a property of the
//     untrusted byte-stream OSC parser (F3), not of this trusted framed channel.
//   - Forward compatibility. An unknown frame type is surfaced as
//     TypeUnknown (still size-bounded) rather than erroring, so F3 can add turn
//     frames without breaking a Phase-0 decoder — but an unknown type NEVER
//     authorizes anything.
package termoob
