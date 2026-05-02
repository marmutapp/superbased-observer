// Package cost is the pricing / cost-estimation engine (spec §15.4, §24).
//
// It maps (model, token counts) → USD using a pricing table sourced from
// baked-in defaults and overridden by config.toml. The engine is the single
// source of truth for the `observer cost` CLI, the `get_cost_summary` MCP
// tool, and the dashboard's cost-breakdown panel.
//
// Reliability follows the spec §24 matrix: api_turns rows are "accurate";
// token_usage rows carry their own reliability tag (claude-code JSONL is
// "unreliable", codex is "approximate", etc.). Summaries surface the
// reliability mix so callers can decide whether numbers are trustworthy.
package cost
