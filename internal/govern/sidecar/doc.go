// Package sidecar is the node-local governance sidecar file: the ONE
// artifact through which a resolved, grant-intersected governance posture
// reaches every Observer process (docs/plans/
// admin-controlled-plane-b-phase-1b-mini-spec-2026-08-15.md §1.2).
//
// The constraint it exists to satisfy: whatever carries a pinned value must
// be readable by a process that has only a file path and one millisecond.
// Phase 1a measured 81 non-test config.Load call sites, nine of them in
// cmd/observer/hook.go — short-lived processes with no daemon round trip,
// frequently no store handle, and a latency budget in tens of milliseconds.
// A pinned value honoured only by the daemon would leave the ENFORCEMENT
// point ungoverned while the node reported `effective`, which is the false
// compliance claim Phase 1b exists to remove.
//
// So: one file, ONE writer (the daemon), every reader through config.Load.
//
// # What lives here and what does not (review m1)
//
// This package is stdlib-only (pinned by imports_test.go): the wire struct,
// the encoder/decoder, the expiry rule, and the failure table. It must stay
// that way because it is read on the hook path.
//
// The PATH RESOLVER lives in internal/config beside ResolveGlobalPath,
// because it takes a config.Config — a dependency this package must not
// have. The per-key skip rules likewise live in internal/config, which owns
// the PinnableKeys mirror; this package treats `pinned` as opaque typed
// values and never decides which keys are legal.
//
// # Fail-open is a safety requirement, not a convenience
//
// EVERY failure mode of Read yields "no overlay": absent, unreadable,
// oversize, malformed, unknown field, schema too new, grant expired, state
// not applied. A non-zero PreToolUse hook exit BLOCKS the developer's tool
// call, so a governance file that could make a hook fail would be a
// self-inflicted outage with a fleet-wide blast radius. Read also never
// writes to stderr — some AI clients surface hook stderr on every call.
//
// # One clock, and it is the grant's
//
// There is deliberately no short sidecar TTL. A developer who stops the
// daemon for a day would then silently ungovern every hook and MCP process
// on their machine, and the org would have no signal — governance would flap
// on daemon uptime, which is not a security-relevant variable. The single
// hard clock is GrantExpiresAt, copied verbatim from the resolved grant, so
// offboarding works for short-lived processes even if the daemon is dead,
// removed, or downgraded. WrittenAt is informational and never a gate.
package sidecar
