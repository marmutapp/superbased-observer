// Package copilotanalytics is the server-side poller for GitHub Copilot's org
// analytics surfaces (native-console instance #3, Rail C). It is the Copilot
// sibling of internal/orgserver/ccanalytics + internal/orgserver/codexanalytics,
// but Copilot's Rail C splits into THREE unrelated surfaces resolved once at
// construction into a surfaceSpec strategy (so the poll loop never branches on
// surface identity — CLAUDE.md anti-spaghetti rule #3):
//
//   - SurfaceEngagement — the new usage-metrics report API
//     GET /orgs|enterprises/{owner}/copilot/metrics/reports/<report>. A TWO-STEP
//     fetch: the JSON envelope returns signed download links whose bodies are
//     NDJSON (one object per line). ENGAGEMENT ONLY — no token or cost field.
//     The legacy /copilot/metrics endpoint closed down 2026-04-02.
//   - SurfaceSeats — GET /orgs/{owner}/copilot/billing[/seats]. Seat COUNTS
//     (no dollars); multiply by the per-seat price ($19 Business / $39
//     Enterprise) at the sibling cost read. Identity = GitHub login.
//   - SurfaceBilling — GET /organizations/{owner}/settings/billing/premium_request/usage.
//     Enhanced-billing $ line items (netAmount USD), the post-June-2026
//     AI-Credits / premium-request metered spend, summed per day.
//
// Everything normalizes to DailyMetric and lands in copilot_analytics_daily
// (server migration 010) keyed (day, user_key, surface, metric), carrying a
// per-metric unit so the cross-vendor unit trap is respected (count | seats |
// usd | tokens — NEVER summed with another vendor's cents/credits/dollars).
// Admin-keyed, server-side only — never on the agent wire.
//
// THE MERGE INVERSION (instance §5.2): unlike CC/Codex, Copilot has no per-token
// agent spend to dedup against — its cost is a flat seat subscription + an
// account-level metered billing line. So Copilot cost does NOT enter
// rollup/cost.go::spendCTE; CostSummary (cost.go) is a SIBLING read that converts
// seats×price + billing-usd to a USD subscription/usage figure, reported
// alongside but never summed with per-turn spend.
//
// IDENTITY (instance §5.3): Copilot's identity is the GitHub login, NOT an email
// (GitHub emails are frequently private). ResolveOrgUserID takes an admin-supplied
// login→email map (Users-API or a hand map) and does the org_members email join.
//
// Schema confidence: built against the Phase-0 findings' documented shapes
// (docs/plans/native-console-copilot-findings-2026-06-16.md +
// copilot-analytics-samples.json). The report-envelope (download_links/report_day),
// seats, and enhanced-billing field names are doc-confirmed; the per-row NDJSON
// engagement keys + the exact enhanced-billing product/sku strings carry live-key
// residuals flagged inline. Lock them against a real payload via the capture
// script before trusting the cost figures.
package copilotanalytics
