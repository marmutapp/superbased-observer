-- 056_handoffs_hook_arm.sql — arm-and-deliver columns for the
-- session-handoff inject_hook lane (plan §10 inject_hook, §12 hook_ttl).
--
-- The rendered HandoverDoc still lives ONLY on disk (delivery_ref points at
-- the HANDOFF-*.md file); these columns carry NO conversation content —
-- just the arming window, a one-shot delivery timestamp, and the project
-- path used to match an armed handoff to the next target session:
--
--   hook_expires_at   RFC3339 — armed until; NULL for non-hook rows.
--   hook_delivered_at RFC3339 — set when a SessionStart claims the row
--                     (one-shot). NULL = still armed.
--   project_root      the source session's project root (a PATH, allowed
--                     by the §5 "hashes/counts/enums/paths — no content"
--                     invariant) — the claim query matches on it so an
--                     armed handoff fires only for the same project.
--
-- NODE-LOCAL: handoffs is pinned in tests/invariant/privacy_test.go and
-- excluded from internal/store/orgpush.go by construction.

ALTER TABLE handoffs ADD COLUMN hook_expires_at TEXT;
ALTER TABLE handoffs ADD COLUMN hook_delivered_at TEXT;
ALTER TABLE handoffs ADD COLUMN project_root TEXT;

CREATE INDEX IF NOT EXISTS idx_handoffs_hook_arm
    ON handoffs(target_tool, delivery, hook_delivered_at, hook_expires_at);
