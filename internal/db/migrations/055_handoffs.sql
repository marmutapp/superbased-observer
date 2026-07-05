-- 055_handoffs.sql — session-handoff (continue-anywhere) linkage records
-- (docs/plans/session-handoff-plan-2026-07-03.md §5).
--
-- One row per handoff: source session + resolved fork point + carry mode
-- + target tool + estimate snapshot + delivery. The rendered HandoverDoc
-- itself is NEVER stored — it is delivered and forgotten (re-derivable
-- from the source transcript), keeping the "no contents in the DB" Don't
-- intact. fork_anchor_hash (sha256 of the last included message) exists
-- so a later re-run can detect source-transcript drift instead of
-- silently producing a different cut.
--
-- NODE-LOCAL — which tool a user moved a session to, when, and at what
-- estimated cost is personal workflow telemetry. Pinned in
-- tests/invariant/privacy_test.go (forbidden-table sentinel) and excluded
-- from internal/store/orgpush.go by construction (explicit allow-list).
-- Same posture as the cachetrack / limit_snapshots tables.

CREATE TABLE IF NOT EXISTS handoffs (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    source_session_id   TEXT    NOT NULL,
    source_tool         TEXT    NOT NULL,
    target_tool         TEXT    NOT NULL,
    carry_mode          TEXT    NOT NULL,  -- metadata|distilled|distilled_tail|full
    fork_kind           TEXT    NOT NULL,  -- last|message_index|timestamp
    fork_message_index  INTEGER,           -- resolved, post-snap; NULL for last
    fork_message_time   TEXT,              -- timestamp of the last included message
    fork_anchor_hash    TEXT,              -- sha256 of the last included message content
    requested_index     INTEGER,           -- pre-snap request (drift/UX audit)
    doc_token_estimate  INTEGER,
    estimate_json       TEXT,              -- MigrationEstimate snapshot (numbers/enums only)
    delivery            TEXT    NOT NULL,  -- inject_file|inject_mcp|inject_hook|inject_prompt
    delivery_ref        TEXT,              -- e.g. the written HANDOFF-*.md path
    target_session_id   TEXT,              -- best-effort backfill (P3 linker)
    created_at          TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_handoffs_source ON handoffs(source_session_id);
CREATE INDEX IF NOT EXISTS idx_handoffs_target ON handoffs(target_tool, created_at);
