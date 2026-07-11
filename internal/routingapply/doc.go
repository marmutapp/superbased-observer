// Package routingapply owns the §R10 Channel A apply mechanics: turning
// the §R10.3 evidence-backed sub-agent recommendations into each AI
// tool's NATIVE configuration. It is the single owner of the write
// mechanics — planning idempotent frontmatter edits, backup-before-write
// (<file>.bak-observer-<stamp>), per-file revert, and the paste-able
// advisory snippets for tools observer doesn't write directly.
//
// Two thin frontends call through it: the CLI (`observer routing
// apply`, cmd/observer/routing_apply.go) and the dashboard endpoints
// (internal/intelligence/dashboard/routingapply.go). Neither
// reimplements the mechanics; both consume Plan/Write/Revert and the
// §R10.2 support-matrix vocabulary (ToolMode).
//
// The package touches agent files on disk and nothing else: no SQL, no
// HTTP, no fsnotify. Evidence loading stays with the callers (the
// modelvalue store seam) — recommendations are passed in.
package routingapply
