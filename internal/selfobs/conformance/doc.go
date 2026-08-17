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
// This club ships ONE Wired conformer — the synthetic reference — as the
// executable proof of the emission contract; the real routing / advisor /
// admission / eval / insight-agent components are registered Wired:false
// (tracked-not-failed, retrofit DEFERRED until the Plane-B cmd/observer sink
// construction lands). P1-10 is therefore PARTIAL.
//
// The package is pure-ish: it imports only internal/selfobs/emit,
// internal/selfobs/run, and internal/provenance (plus context). It never
// imports internal/obs, database/sql, or fsnotify (pinned by imports_test.go).
package conformance
