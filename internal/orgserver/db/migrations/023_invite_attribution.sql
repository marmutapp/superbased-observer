-- Invite attribution — Arc 2 of
-- docs/plans/tier3-local-contract-and-teams-invite-plan-2026-07-31.md
-- (the Teams bottom-up invite loop).
--
-- SERVER-ONLY, with NO paired agent migration — the same shape as 008 /
-- 009 / 011. Nothing here changes the agent↔server wire: a delegated
-- invite is a mint the server performs on a caller's behalf, and the
-- column below only records WHO asked.
--
-- minted_by is the org_members.user_id of the actor that requested the
-- mint, or NULL for the historical/unattributed path (every token minted
-- before this migration, and the `observer-org invite` CLI, which runs as
-- the server operator against the DB directly and has no session identity).
-- It is deliberately NOT a foreign key: an inviter may be deprovisioned
-- later (org_members rows are cascade-deleted), and losing the attribution
-- on a still-outstanding token would be worse than a dangling id.
--
-- Two jobs, one column:
--   1. the per-member monthly mint CAP counts rows by (minted_by, created_at)
--      — the cap must survive an audit_log prune, so it is enforced against
--      the token table itself rather than the audit trail;
--   2. invite→enrolment CONVERSION is then a pure read of this table
--      (used_at IS NOT NULL grouped by minted_by) — server-side only, in the
--      org's own DB, nothing new on any wire.
--
-- Carries no content: a user id and the timestamps already present.
ALTER TABLE enrolment_tokens ADD COLUMN minted_by TEXT;

CREATE INDEX IF NOT EXISTS idx_enrolment_tokens_minted_by
    ON enrolment_tokens(minted_by, created_at);
