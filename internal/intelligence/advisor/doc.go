// Package advisor is the suggestions engine: prescriptive, evidence-backed,
// dollar- or minute-quantified recommendations computed locally from data the
// observer already stores (spec §15.7; plan
// docs/plans/suggestions-engine-implementation-plan-2026-06-10.md).
//
// Design (per the plan + CLAUDE.md module boundaries):
//
//   - Detectors are pure decisions over a loaded Facts bundle — the only SQL
//     in the package lives in the Facts loaders (load.go). Detectors never
//     touch *sql.DB and are unit-tested with literal Facts fixtures.
//   - The registry is an ordered, table-driven slice; adding a detector is
//     one registry entry + one Detect func + table tests.
//   - Pricing always goes through the injected PriceFn (backed by
//     cost.Engine.ComputeBreakdown) so fast-tier multipliers and
//     long-context dispatch are never re-derived here (Phase-0 calibration
//     T2: flat-rate math under-prices fast sessions ~1.75×).
//   - Suggestions are computed on read (P3: never stale); only user state
//     (dismiss/snooze, Phase 2) persists, node-local.
//
// Threshold defaults come from the Phase-0 calibration against the live
// corpus (docs/plans/advisor-calibration-2026-06-10.md §8).
package advisor
