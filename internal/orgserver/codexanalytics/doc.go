// Package codexanalytics is the server-side poller for OpenAI Codex's org
// analytics APIs (native-console instance #2, Rail C). It is the Codex sibling
// of internal/orgserver/ccanalytics, but spans TWO vendor surfaces resolved once
// at construction into a surfaceSpec strategy (so the poll loop never branches on
// surface identity — CLAUDE.md anti-spaghetti rule #3):
//
//   - SurfaceChatGPTEnterprise — GET api.chatgpt.com/v1/analytics/codex/...,
//     workspace-scoped, cost in CREDITS, identity = email (where permitted).
//   - SurfaceOpenAIOrg — GET api.openai.com/v1/organization/usage/completions
//   - /costs, org-scoped, cost in DOLLARS, identity = OpenAI user_id (2-step
//     resolve to email).
//
// Everything normalizes to DailyMetric and lands in codex_analytics_daily
// (server migration 009) keyed (day, user_key, surface, metric), carrying a
// per-metric unit so the spendCTE merge (Phase 3, gated) never sums credits with
// dollars with cents. Admin-keyed, server-side only — never on the agent wire.
//
// Schema confidence: built against the Phase-0 findings' documented schemas
// (docs/plans/native-console-codex-findings + ...-rail-c-api-org-path). The
// OpenAI-org page/bucket/results envelope is confirmed from live cookbook
// payloads; the ChatGPT-Enterprise field paths + timestamp format carry live-key
// residuals flagged inline. Lock both against a real payload via the capture
// script before trusting the cost-merge.
package codexanalytics
