// Package termfeed is a normalized, bounded, in-process event feed
// (docs/plans/terminal-product-exploitation-plan-2026-07-12.md §3 F4, Phase 0
// item S0e).
//
// Agent status fusion (F4) needs a LIVE event stream, not a database poll:
// "events already land in the store" is not a live feed, and polling the large
// DB per status tick is neither real-time nor cheap. This package is that feed
// — a single publisher-to-many-subscribers fan-out over a normalized Event, with
// a bounded replay ring (a late subscriber gets recent history) and bounded
// per-subscriber queues (a slow consumer is degraded with a visible gap, never
// back-pressuring the publisher).
//
// The never-back-pressure rule mirrors the PTY spine's "keep draining"
// discipline (§2.1/F2): the producers here are the hook / transcript ingestion
// boundaries on hot paths, so Publish is non-blocking and drops the OLDEST
// queued event for a full subscriber (incrementing that subscriber's Lost
// counter so the consumer sees a gap and can re-sync) rather than stalling the
// producer.
//
// This package is PURE (CLAUDE.md §1): sync + time only — no database/sql,
// net/http, or fsnotify (pinned by imports_test.go), and NO dependency on the
// terminal packages (F4 requirement: the feed is a generic primitive the
// application service wires the ingestion boundaries into; it never imports
// termsession / termrun / termoob). The Event vocabulary (Kind strings) is
// defined by the producers that publish, not here.
package termfeed
