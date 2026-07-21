// Package aggregate is the pure-logic core of the G25 opt-in aggregate rail
// (docs/plans/g25-optin-aggregate-rail-design-2026-07-11.md).
//
// It owns the WIRE TYPES of the monthly cost aggregate — the only shape that
// may ever leave a node once the rail is enabled — plus the pure functions
// that build them: the model_family normalizer (a closed, versioned
// vocabulary; unknown model strings collapse to "other" so no long-tail model
// id can reach the wire), the provenance-split roll-up (twin _acc / _est
// counters so a proxy-accurate cut never blends estimated volume), and the
// local rare-cell coarsening that shrinks the per-node fingerprint before it
// is serialized.
//
// PURE BOUNDARY (pinned by imports_test.go, mirroring
// internal/intelligence/modelvalue): this package imports NO database/sql,
// net/http, internal/store, internal/orgclient, or internal/proxy. The SQL
// read that feeds Build lives OUTSIDE the package (internal/aggregatesource),
// and the network egress lives in a SEPARATE package (internal/aggregateclient,
// Phase 3). The types here carry ONLY the allow-listed fields in §3 of the
// design — an invariant test (tests/invariant/aggregate_test.go) fails if any
// field is added, so nothing content-bearing can leak in silently.
//
// The rail is off by default and no code in this package performs any I/O:
// Build is a total, deterministic transform from injected DTOs to the wire
// envelope.
package aggregate
