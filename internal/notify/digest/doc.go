// Package digest is the PURE composition layer for scheduled report digests
// (gap-register G13). Given already-assembled, content-free spend data (period
// totals, per-model / per-project / per-developer breakdowns, top movers vs the
// prior period, and an alert-count summary), it renders a digest email through
// the shared internal/notify/email composer.
//
// It is deliberately I/O-free (module discipline #1): NO database/sql, net/http,
// or fsnotify. The store access that produces Data is injected by the caller
// (the node daemon and the org server each assemble Data from their own
// rollups). Composition is separated from delivery so the same Data can back a
// dry-run print and a real send.
//
// Honesty rule: digest copy frames every number as OBSERVED spend. It never
// cites compression dollar-savings (a retracted claim).
package digest
