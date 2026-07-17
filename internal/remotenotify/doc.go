// Package remotenotify builds and dispatches opt-in outbound notifications for
// remote-orchestration events — the #1 remote demand ("tell me when my agent
// finished / got blocked") from the remote-dashboard-access plan (§7 Phase 0).
//
// It is a PURE-LOGIC package (CLAUDE.md module rule #1): it builds a
// transport-agnostic delivery Request and hands it to an injected Sender. It
// imports NO net/http, database/sql, or fsnotify — the HTTP-backed sender lives
// in the cmd wiring. This keeps the "no network calls in observer/watcher"
// invariant intact: the notifier is invoked ONLY from the dashboard/lifecycle
// layer (the termsession session-exit seam), NEVER from internal/watcher or
// internal/proxy. An import-boundary invariant test pins that
// (tests/invariant/remotenotify_boundary_test.go), and the outbound call is the
// same posture as the existing opt-in orgclient push.
//
// The rail is opt-in and default-off ([remote.notify].enabled = false); when
// disabled the Notifier is a no-op and Observer opens no inbound port.
package remotenotify
