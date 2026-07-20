// Package obs is the root of the generalized-observability subsystem: a
// self-contained, separable module that captures arbitrary custom-app /
// agent activity as a canonical span graph (see internal/obs/span), persists
// it in node-local obs_* tables it owns (internal/obs/store), and — in later
// phases — ingests OTLP traces (internal/obs/ingest), surfaces a trajectory
// UI (internal/obs/httpapi), and runs a minimal eval plane (internal/obs/eval).
//
// Separability contract (docs/plans/generalized-observability-custom-app-plan-2026-06-27.md
// §2): NO package outside internal/obs/ imports internal/obs/... — the sole
// exception is the single cmd/observer wiring file. The subsystem depends on
// the host only through the three narrow interfaces defined in interfaces.go
// (TurnSink, ProxyEnricher, ContentGate), which obs declares and the host
// implements at the wiring point. Its tables are obs_-prefixed, node-local
// (never on the org-push wire), and carry no SQL foreign keys into existing
// tables. The whole subsystem is gated by [observability] enabled (default
// false) and the //go:build !no_obs constraint on its wiring; removing the
// wiring call + this tree leaves the rest of Observer compiling and green.
package obs
