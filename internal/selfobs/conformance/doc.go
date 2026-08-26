// Package conformance is the executable registry of platform decision
// components that emit self-observability decision runs (P1-2 / P1-10).
//
// Each RegisteredComponents entry declares a decision component by name and
// whether it is Wired — i.e. actually emits attributable run telemetry today.
// A Wired entry carries a Conformer: a real function that builds a
// run.DecisionRun (producer FIXED to system_agent) and drives it through an
// emit.Sink. The enforcement test in tests/invariant invokes every Wired
// Conformer against a capturing fake sink and proves the emitted telemetry is
// attributable (system_agent producer + initiating actor + run id).
//
// All six registered components — routing, advisor, admission, eval,
// insight-agent, and the synthetic reference — are Wired:true: each has a
// real production emit call site (see RegisteredComponents' doc comment in
// registry.go for the current per-component wiring map), and Conformer here
// is an executable SHAPE-proof fixture per component, not a substitute for
// that real call site. A Wired:false entry remains a supported registry
// state (tracked-not-failed, for a component whose retrofit is still
// staged) — there simply are none outstanding as of the insight-agent
// close-out (P2-7).
//
// The package is pure-ish: it imports only internal/selfobs/emit,
// internal/selfobs/run, and internal/provenance (plus context). It never
// imports internal/obs, database/sql, or fsnotify (pinned by imports_test.go).
package conformance
