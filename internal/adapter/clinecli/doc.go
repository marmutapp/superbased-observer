// Package clinecli implements the Adapter interface for the
// npm-distributed Cline CLI (`cline` package, Cline Bot Inc.,
// Apache-2.0). Distinct from internal/adapter/cline, which targets
// the VS Code Cline extension (`saoudrizwan.claude-dev`) — same
// brand, different product, completely different on-disk layout.
//
// Cline CLI stores all session data under `~/.cline/data/` (every OS,
// NOT %LOCALAPPDATA% on Windows — confirmed against a live Windows
// install). Layout:
//
//	~/.cline/data/
//	├── db/
//	│   ├── sessions.db         ← canonical SQLite session-meta
//	│   ├── sessions.db-wal     ← WAL sidecar (always present, WAL mode)
//	│   ├── sessions.db-shm     ← SHM sidecar
//	│   ├── cron.db             ← scheduler
//	│   └── teams.db            ← teams runtime state
//	├── sessions/
//	│   └── <id>/
//	│       ├── <id>.json           ← session metadata (cost, checkpoint history)
//	│       └── <id>.messages.json  ← Anthropic-shaped conversation
//	└── logs/
//	    └── hooks.jsonl         ← optional — only written when hook
//	                              commands are registered OR subagents run
//
// Capture strategy is **SQLite-backfill primary + messages.json
// content-block walker** (same hybrid model as the hermes adapter,
// minus the always-on hook plugin Hermes ships):
//
//   - SQLite backfill (primary): watch `db/sessions.db` (+ WAL / SHM
//     sidecars as poll triggers). Walk rows past the last-seen
//     `updated_at` watermark. For each new/updated session, read its
//     paired `<id>.messages.json` and emit ToolEvent per `tool_use`
//     content block + TokenEvent per assistant message's `metrics`.
//   - Hook log (secondary): tail `logs/hooks.jsonl` when present.
//     OPT-IN by Cline design — only written when the operator
//     registers a hook command file under
//     `<workspace>/.clinerules/hooks/` or `<CLINE_DIR>/hooks/`, OR
//     when a subagent is spawned (subagent-audit fires regardless).
//     Cannot be the primary capture path. Phase 3 of the plan.
//   - Hook command registration: deferred to v2 (analogous to the
//     Hermes plugin's `observer init --hermes` flow; Cline CLI's hook
//     subprocess API is documented but writing a Node hook script
//     into a user's `<CLINE_DIR>/hooks/` directory is opt-in and we
//     prefer the SQLite path covers 100% of v1).
//
// Token capture is Tier 2 (approximate) on both paths today:
//   - SQLite path: session-level aggregates from
//     `metadata_json.usage` (`inputTokens`, `outputTokens`,
//     `cacheReadTokens`, `cacheWriteTokens`).
//   - messages.json path: per-message `metrics` block carrying the
//     same fields — finer-grain than the session aggregate.
//   - Per-API-call Tier 1 (accurate) via proxy routing requires
//     writing `baseUrl` overrides into `~/.cline/data/settings/providers.json`,
//     which carries OAuth tokens. Deferred to v2.
//
// First-class sub-agent model — `sessions.parent_session_id` /
// `parent_agent_id` / `agent_id` / `is_subagent` columns surface
// directly onto ActionMetadata + the existing dashboard's parent-
// child grouping. The 18 `team_*` tools all map to `mcp_call` for v1
// (see normalize.go); dedicated team-comm action types may come in v2.
//
// NEVER reads (security): `~/.cline/data/settings/providers.json`
// (WorkOS OAuth + refresh tokens + per-provider API keys),
// `~/.cline/data/cache/user_input_history.jsonl` (cross-session
// prompts), `~/.cline/data/secrets.json` (root-level secrets blob
// surfaced during Phase 0 fixture capture, post-dating the plan
// Appendix B).
//
// See:
//   - docs/plans/cline-cli-adapter-plan-2026-06-06.md — the research
//     note + per-commit build order.
//   - testdata/clinecli/ — schema dumps + redacted live-session
//     samples + synthesized hook JSONL.
package clinecli
