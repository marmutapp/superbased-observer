// Package rollup computes the aggregation queries that power the org
// dashboard: overview, per-team, per-project, per-developer, budget-status,
// and audit-log rollups over the server's pushed data tables (sessions,
// actions, api_turns, token_usage) and identity tables (org_members,
// org_teams, org_team_members, org_project_team).
//
// Each rollup function takes a context, the server *sql.DB, a Window, and any
// scope filter, and returns a strongly-typed result. The result types
// (defined in types.go) are the single source of truth for the dashboard wire
// shape: the OpenAPI spec references them via x-go-type and golden_test.go
// pins their JSON encoding so an accidental field change fails the build.
//
// Results are wrapped by a small read-through TTL cache (cache.go); the
// handlers in internal/orgserver/dashboard call the cached variants. The
// cache is keyed by (rollup-type, scope, window) and is purely an in-memory
// accelerator — it holds no authority and may be dropped at any time.
//
// PRIVACY: every query selects only content-free columns. No prompt text, raw
// tool I/O, reasoning, or error message is ever read here — consistent with
// the structural-privacy posture of the pushed-row schema (migration 001).
//
// COST SOURCE: spend mirrors the agent's local dashboard so the org rollup
// agrees with each developer's local view for the same window (spec §1.6 drift
// metric, ±2%). That means a proxy-deduplicated UNION of the two pre-computed
// cost columns:
//
//	SUM(api_turns.cost_usd)                       -- proxy-observed turns
//	+ SUM(token_usage.estimated_cost_usd)         -- adapter-derived rows,
//	    WHERE source_event_id NOT IN (api_turns.request_id within window)
//
// i.e. an api_turns turn is authoritative and the matching token_usage row is
// excluded, exactly as internal/intelligence/dashboard does. Costs are taken
// as the agent pre-recorded them (the server holds no pricing engine); the
// residual where a row's recorded cost is zero is the documented dedup slack.
// See costSelectCTE in cost.go for the shared SQL fragment.
package rollup
