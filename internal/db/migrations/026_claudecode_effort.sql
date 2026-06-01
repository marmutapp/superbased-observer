-- 026_claudecode_effort.sql — v1.6.22 effort-capture sidecar for
-- claude-code.
--
-- Background: Claude Code's JSONL transcript does NOT serialize the
-- user's effort dropdown selection (Max / Extra High / High / Medium /
-- Low). Effort is a request-side `thinking.budget_tokens` parameter
-- the response stream never echoes. The only per-turn capture surface
-- is the Claude Code hook payload: PreToolUse / PostToolUse / Stop /
-- SubagentStop receive `effort.level` in stdin JSON (verified against
-- code.claude.com/docs/en/hooks).
--
-- This table is the sidecar that bridges the hook-fire moment and the
-- (later) JSONL-ingest moment. The hook upserts (session_id,
-- tool_use_id) → effort_level. The store enricher / JSONL adapter
-- consults this table by joining on actions.source_event_id (which
-- the claude-code adapter sets to the Anthropic toolu_xxx ID for
-- tool_use rows). Race-safe in either direction: hook may fire before
-- or after the JSONL parser sees the assistant message.
--
-- Schema notes:
--   - session_id + tool_use_id form the natural key. PRIMARY KEY
--     enforces last-write-wins on the rare retry path.
--   - tool_use_id is the Anthropic `toolu_xxxxx` block ID for
--     PreToolUse/PostToolUse. For Stop / SubagentStop (no tool_use
--     context) the hook writes a synthetic key
--     "__stop__:<event_uuid>" so session-level effort signals don't
--     collide with per-tool rows.
--   - effort_level is one of: minimal, low, medium, high, xhigh, max.
--     We accept whatever Anthropic emits — no enum check.
--   - event_name records which hook fired (diagnostic only).
--   - received_at is RFC3339 UTC; used for TTL cleanup down the road.

CREATE TABLE IF NOT EXISTS claudecode_effort (
    session_id   TEXT NOT NULL,
    tool_use_id  TEXT NOT NULL,
    effort_level TEXT NOT NULL,
    event_name   TEXT NOT NULL,
    received_at  TEXT NOT NULL,
    PRIMARY KEY (session_id, tool_use_id)
);

CREATE INDEX IF NOT EXISTS idx_claudecode_effort_session
    ON claudecode_effort (session_id, received_at);
