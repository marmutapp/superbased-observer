// Package termlease is the pure-logic authorization + policy layer for the
// remote terminal EXECUTE tier (remote-dashboard-access plan §4.α/§4.δ). It
// owns three things and nothing else:
//
//   - WriterGrant — the unforgeable proof that the full §4.δ conjunction held.
//     It carries an unexported sentinel so no caller outside this package can
//     fabricate an authorized grant (the same structural pattern as
//     internal/aggregateclient's Gate). termsession's AcquireWriterRemote
//     accepts ONLY a valid WriterGrant, which makes the conjunction
//     structurally impossible to bypass, not merely documented.
//
//   - Authorize — the SINGLE authorization function that atomically validates
//     the entire §4.δ conjunction (remote-exposed listener AND authenticated
//     device session AND remote.allow_terminal AND applicable [terminal.launch]
//     policy AND a single-use capability bound to (handle+terminal.control+
//     device-session) AND a bound confirm) and, only on success, mints the
//     WriterGrant. Its dependencies are injected as pure interfaces so this
//     package never imports database/sql, net/http, fsnotify, config, or the
//     terminal packages (CLAUDE.md module-boundary #1).
//
//   - The lease-grant / takeover POLICY TABLE (Decide) — the table-driven
//     rule set (CLAUDE.md #5) that decides, given the requester and the current
//     lease holder, whether to grant/refuse and whether to revoke the incumbent.
//     Local acquisition is never refused; authenticated remote acquisition may
//     supersede local or remote writers when the live takeover policy is on and
//     requires an explicit yield when it is off. Every superseded lease is
//     fenced. One input source ever.
//
// The package is pinned pure by imports_test.go.
package termlease
