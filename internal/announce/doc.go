// Package announce is the pure-data core of the dashboard announcements
// surface (docs/plans/dashboard-announcements-banner-plan-2026-07-31.md).
//
// It owns exactly two things: the Announcement wire/embedded shape (§1 of
// the plan — one shape for every rail) and the merge/expiry/validation
// logic the dashboard endpoint and, later, the org rail run over it.
//
// It deliberately owns NO transport. The three rails the plan approves
// each carry the same shape over a channel that already exists:
//
//   - R1 release-embedded — the releaseAnnouncements slice compiled into
//     this package (announce.go). Zero network by construction.
//   - R2 update-check piggyback — read by the frontend from the SAME
//     click-gated npm registry response web/src/lib/version.ts already
//     fetches. Never reaches this package.
//   - R3 org (Teams) rail — a later work package; it will hand its
//     decoded slice to Merge as one more source. Nothing here knows the
//     org types exist.
//
// The one thing this package must never grow is an outbound call: an
// unsolicited fetch to a SuperBased-controlled host is precisely the
// phone-home class PRIVACY.md, `observer privacy` and the
// measurement-honesty page disprove (plan §6). Discipline mirrors
// internal/predict and internal/routing: NO database/sql, NO net/http,
// NO fsnotify — pinned by imports_test.go.
package announce
