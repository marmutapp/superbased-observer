// Package cachetrack implements the provider-cache state model
// described in
// docs/plans/cache-tracking-implementation-spec-2026-06-08.md.
//
// The package is pure logic: it accepts canonical block bytes and
// observed usage envelopes; it returns chain hashes, entry
// mutations, and per-turn attribution events. It performs no I/O
// (no DB, no HTTP, no filesystem); all infrastructure is supplied
// by callers (the proxy at runtime, the store at watcher-ingest
// time) through explicit function arguments and small interfaces.
//
// Two callers feed the engine:
//
//   - The proxy (Tier 1, byte-exact) calls into a single shared
//     seam from buildTurn / buildStreamTurn after upstream usage is
//     known.
//   - The watcher (Tier 2, transcript reconstruction) calls into
//     the same seam via store.Ingest after parsing JSONL records.
//
// Tier provenance is recorded as a column on persisted rows
// (data, not control flow): logic in this package must NEVER
// branch on tier strings — see spec §24.3 (capability-based
// branching). Capability flags carried on inputs are the right
// place to gate Tier-2-blind behavior.
//
// See spec §3 for the architecture diagram and §24 for the
// module-boundary discipline this package answers to.
package cachetrack
