// Package processobs is the generic, OS-independent core of the
// Process Observability feature (docs/process-observability.md).
//
// It turns a stream of minimal OS process events (fork / exec / exit,
// and later network / file / privilege events) into attributed,
// scrubbed ProcessRun envelopes ready for the store, joining each run
// back to the AI session — and, when a run_command action seeded the
// spawn, down to the originating action/turn (§9.2.4, resolved by a
// later deferred correlation pass).
//
// The package is PURE LOGIC (spec §6.1 / CLAUDE.md module discipline):
// it performs no I/O — no database/sql, no net/http, no fsnotify, no
// direct /proc reads. Every infrastructure dependency is injected by
// the caller through a small interface or function value:
//
//   - Backend   — the OS event source (linuxebpf / etw / endpointsec /
//     poll). Implementations live in their own OS-specific packages and
//     own all privileged/event-source code.
//   - Enricher  — fills OS-specific envelope fields (e.g. the Linux
//     backend reads /proc). Optional; nil means "events arrive already
//     enriched", which is how the fake backend and tests run.
//   - SeedLookup — resolves a pid to its session (wraps the existing
//     internal/pidbridge at the boundary). The bridged pid is the AI-tool
//     ROOT process; descendants are reached by tree inheritance here, not
//     by bridge lookup.
//   - Sink      — persists finished runs (the store implements it and
//     translates processobs domain types into its own SQL row types at
//     the boundary, exactly as the cachetrack engine does — domain types
//     never leak into store SQL or vice versa).
//
// Privacy (spec §12): this package never stores file contents, command
// output, network payloads, or full environment. Argv is capped + scrubbed
// + hashed; paths and env are reduced to posture/hashes via injected
// redaction. The store tables it feeds (process_runs / process_events) are
// NODE-LOCAL and pinned out of the org-push wire path by
// tests/invariant/privacy_test.go.
//
// PID reuse (spec §9.3): nothing in this package joins on pid alone. Every
// run and event is keyed by ProcessKey = sha256(boot_id:pid:start_time).
// An event whose start time could not be read is counted for health but
// never persisted as a fresh attributed run.
package processobs
