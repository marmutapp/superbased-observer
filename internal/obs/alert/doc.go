// Package alert is the obs subsystem's pure node-side alert-evaluation logic
// (general-observability gap-audit item #9, §2.6). It is the node analogue of
// the org-server evaluator internal/orgserver/obsalert: it evaluates the SAME
// alert-rule dialect — error-rate / cost / p95-latency thresholds over a
// content-free metric snapshot — but for the node's OWN local obs_* data,
// so a node with org sharing turned off still gets threshold alerting.
//
// Purity (CLAUDE.md rule #1, obs plan §11): this package takes rules + metric
// snapshots in and returns fired-alert values out. It performs NO I/O — no
// database/sql, net/http, or fsnotify, and no internal/config. The store read
// (the metric snapshot), the webhook POST, the scheduler loop, and the
// dedup/cooldown persistence all live at the boundary: internal/obs/store
// computes the snapshot + records fired events (node-local obs-owned tables,
// NEVER on the org wire), and cmd/observer runs the evaluator loop + webhook
// client. Pinned by imports_test.go.
//
// There is no node-dashboard (web/) surface: obs surfaces are Plane-A/web2-only
// per the plane separation (docs/deployment-models.md). The honest node
// surfaces are the CLI (`observer obs alerts`) and the configured webhook.
package alert
