// Package ccanalytics polls a coding assistant's own org-analytics API (the
// Claude Code Analytics API) and lands per-(day, user, metric) values into the
// server-side cc_analytics_daily table (native-console Workstream C / Phase 5).
//
// It is server-side and admin-keyed: the agent never touches this API (it is
// org-scoped and key-bearing; agents stay content-free and key-free). The key
// is supplied via the CC_ANALYTICS_API_KEY env var or a secret file, selected
// by api_kind ("enterprise" | "admin"). Default disabled.
//
// SCHEMA CAVEAT: parseAnalyticsResponse maps a PROVISIONAL response shape — the
// exact Anthropic Analytics API field names + the user-identity model (how an
// analytics actor maps to org_members.user_id) need validation against a live
// Teams/Enterprise key before this is wired into the shared spend definition
// (rollup/cost.go::spendCTE) for cost/token dedup. The CC-EXCLUSIVE metrics
// (lines accepted, accept rate, DAU) carry no duplication risk and can surface
// directly. The mapping point is deliberately isolated so only one function
// changes when the live schema is confirmed.
package ccanalytics
