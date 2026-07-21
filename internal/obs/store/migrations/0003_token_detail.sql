-- 0003_token_detail.sql — capture cache + reasoning (thinking) token detail on
-- obs_spans (plan docs/plans/obs-token-detail-capture-plan-2026-06-28.md).
-- All three are nullable INTEGER, authoritative-on-merge like the existing
-- token columns: a non-null incoming value wins, NULL leaves the stored value.
-- Node-local (obs_*), no agent migration, no wire/privacy change.
ALTER TABLE obs_spans ADD COLUMN cache_read_tokens  INTEGER;
ALTER TABLE obs_spans ADD COLUMN cache_write_tokens INTEGER;
ALTER TABLE obs_spans ADD COLUMN reasoning_tokens   INTEGER;
