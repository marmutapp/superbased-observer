-- Migration 005: Add summary_md column for AI-generated session summaries.
-- See spec §25 (Strand B — AI-compressed session summaries).
ALTER TABLE sessions ADD COLUMN summary_md TEXT;

INSERT OR REPLACE INTO schema_meta (key, value) VALUES ('version', '5');
