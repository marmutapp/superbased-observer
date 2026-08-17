// Package waltest is a NEUTRAL, test-only storage-conformance harness for the
// P0-9 durable-WAL COMMON contract, shared by the observer-edge WAL store
// (internal/edge/wal) and the org-server gateway WAL store
// (internal/orgserver/gateway) so both are driven through byte-identical
// assertions (plan §2.4, Blocker 2/3).
//
// It defines its OWN minimal CommonStore interface — the P0-9-shaped surface
// (Enqueue / Drain / MarkApplied / a COMMON-shaped MarkFailed(ctx, seq, err) /
// Depth / GCApplied) — plus a CommonConfig and a neutral Record, and exposes ONE
// entry point, RunStorageConformance(t, factory). The factory receives a PINNED
// CommonConfig (a small MaxDepth so the capacity arm needs a handful of inserts,
// a small MaxAttempts, a deterministic backoff, and a per-store DB path) and a
// CONTROLLABLE clock the suite advances by hand, so every timing assertion
// (backoff eligibility, quarantine, GC retention) is deterministic with NO
// time.Sleep (plan R2-B3).
//
// The harness asserts ONLY the COMMON contract — enqueue-before-ack +
// MaxDepth/ErrFull, drain eligibility + seq order, attempt/backoff, terminal
// quarantine at MaxAttempts (excluded from drain), bounded GC, GC-never-reaps-
// pending, and payload_ver round-trips. It does NOT reference the edge
// Disposition type or any Close method (the edge failure-disposition extension
// and the reopen/persistence assertions live in the edge-only suite). It imports
// no edge or gateway package; each test package supplies a small adapter closure
// that maps its store (and its native full-capacity error) onto CommonStore and
// ErrFull.
//
// The package is compiled only by tests (it imports "testing"); it holds no SQL,
// HTTP, or fsnotify.
package waltest
