// Package govern is the PURE node-governance resolver
// (docs/plans/admin-controlled-plane-b-spec-2026-08-15.md §3.7): it
// intersects an org-delivered node.governance PolicySpec with the node's own
// enrolment GRANT and returns the Effective posture the node actually
// applies, recording every directive it dropped and why.
//
// The one sentence this package exists to make true: the node never applies
// the delivered body. It applies delivered ∩ granted, and a partial
// application can never masquerade as convergence — when anything is
// dropped the caller reports accepted_inert / not_preauthorized instead of
// effective.
//
// Resolve is table-driven (CLAUDE.md #5): an ordered rule set walked
// top-down, first match wins, one test row each. It is a pure function of
// its arguments, including the clock — grant expiry is decided by the `now`
// the caller passes, never by time.Now inside.
//
// Purity (CLAUDE.md #1): no database/sql, no net/http, no fsnotify, no
// internal/store, no internal/config — pinned by imports_test.go. The grant
// row is read by internal/store/orggrant.go and handed here as a plain
// struct; the delivered spec is compiled by internal/policyfam/nodegov and
// handed here the same way.
package govern
