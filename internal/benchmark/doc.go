// Package benchmark is the pure-logic core of the Benchmarks Harness
// (docs/plans/benchmarks-harness-plan-2026-07-11.md). It parses and validates
// the declarative run spec (TOML), expands the (task × config × repeat) cell
// matrix, and computes the inferential report — success-rate Wilson intervals,
// paired/block deltas, pre-declared non-inferiority verdicts, and
// cost-per-successful-completion — over plain fact rows fed in by the store
// seam.
//
// Module discipline (CLAUDE.md §1): this package is PURE. It imports no
// database/sql, net/http, fsnotify, or os/exec — I/O is injected at the store
// seam (internal/store/benchmark.go) and the runner (cmd/observer/benchmark.go).
// The purity is pinned by imports_test.go, exactly like internal/predict and
// internal/routing.
//
// The statistics are the harness's OWN (Wilson + paired bootstrap +
// non-inferiority), NOT modelvalue's unexported normal-approximation "parity"
// helpers — that vocabulary over-claimed (plan §3.5). modelvalue is cited as
// precedent for honest verdict discipline, never as a callable engine.
package benchmark
