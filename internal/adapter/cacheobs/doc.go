// Package cacheobs is the shared Tier-2 cache-observation builder
// used by every transcript/SQLite adapter that emits
// models.CacheTurnObservation rows for the internal/cachetrack
// engine (spec §14.3).
//
// Before this package existed, each Tier-2 producer (claudecode,
// opencode, kilo-code CLI, cline-cli) hand-rolled an identical
// per-session accumulator (pending-block delta, a cumulative
// block cap, compaction-reset bookkeeping) plus the identical
// "cachetrack:"+messageID idempotency-key convention and the
// identical three-line §15.3 implicit-cache overlay. cacheobs
// factors the adapter-independent parts of that logic — the
// PART-SHAPE canonicalization (which JSON fields belong in the
// chain, which are volatile and excluded) stays in each adapter's
// own cachetrack.go, because that part is genuinely tool-specific.
//
// Module boundary (CLAUDE.md "Module Boundaries & Anti-Spaghetti
// Discipline" + spec §24.1): cacheobs is pure logic. It may import
// internal/models (the wire-shape data type) and internal/cachetrack
// (also pure logic — the §15.3 provider-shape table), but never
// database/sql, net/http, or fsnotify. I/O and per-tool parsing
// stay in the adapter package; cacheobs only assembles the
// resulting observation.
package cacheobs
