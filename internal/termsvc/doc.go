// Package termsvc is the terminal application service (terminal-product-
// exploitation plan §2 principle #7). It is the ONE place that ties together
// the PTY/viewer lifecycle (internal/termsession), the durable run identity +
// correlation model (internal/termrun), the persistence seam (a RunRecorder,
// satisfied by a cmd adapter over internal/store), the trusted out-of-band
// launcher control channel (internal/termoob, drained by cmd), and the
// in-process event feed (internal/termfeed) that agent-status detection (F4)
// consumes.
//
// termsession stays responsible ONLY for process/PTY/viewer lifecycle and must
// never import store/handoff/integration. termsvc is the orchestration layer
// wired from cmd: it validates the fresh-launch authorization (the operator's
// [terminal.launch] opt-in — allow_fresh_agent + allowed_tools +
// allowed_project_roots), mints a terminal_run identity at spawn, propagates a
// correlation nonce out of band, and records the run + its scored correlations.
//
// It is not a "pure" package — it performs filesystem canonicalization for
// project-root authorization — but every persistence and PTY dependency is
// injected as an interface so the service is unit-testable with fakes.
package termsvc
