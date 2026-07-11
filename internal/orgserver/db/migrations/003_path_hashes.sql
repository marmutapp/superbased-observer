-- 003_path_hashes.sql — v1.8.0 server-side privacy posture (Issues 1 + 2 of
-- the 2026-06-02 teams test findings).
--
-- The Teams push seam at the agent now ships sha256-hex hashes for the
-- content-bearing path columns by default. A node operator can opt into
-- shipping the raw values via [org_client.share].full_content = true; the
-- org admin cannot force this on. To accept both metadata-only and
-- full-content pushes without losing dedup or rollup signal, the server
-- gains four denormalized hash columns:
--
--   actions.target_hash, actions.source_file_hash
--   sessions.project_root_hash, sessions.git_remote_hash
--   api_turns.project_root_hash
--   token_usage.project_root_hash, token_usage.source_file_hash
--
-- Backward compatibility with v1.7.x agents: those pushes still carry raw
-- `target`/`source_file`/`project_root`/`git_remote`. The ingest layer
-- computes the corresponding hash on the fly when only the raw value is
-- present, so a v1.7.x agent pushing into a v1.8.x server lands rows with
-- both columns populated and downstream rollup queries that key on the
-- hash work uniformly.
--
-- Forward compatibility with v1.8.x metadata-only agents: those pushes
-- carry only the hash columns and leave the raw fields empty/NULL. The
-- ingest layer falls back to the hash for the (source_file,
-- source_event_id, user_id) primary-key dedup (computing
-- COALESCE(NULLIF(source_file,''), source_file_hash)), so the PK keeps
-- working without a table rebuild.

ALTER TABLE actions     ADD COLUMN target_hash       TEXT;
ALTER TABLE actions     ADD COLUMN source_file_hash  TEXT;
ALTER TABLE sessions    ADD COLUMN project_root_hash TEXT;
ALTER TABLE sessions    ADD COLUMN git_remote_hash   TEXT;
ALTER TABLE api_turns   ADD COLUMN project_root_hash TEXT;
ALTER TABLE token_usage ADD COLUMN project_root_hash TEXT;
ALTER TABLE token_usage ADD COLUMN source_file_hash  TEXT;

CREATE INDEX IF NOT EXISTS idx_actions_target_hash       ON actions(target_hash);
CREATE INDEX IF NOT EXISTS idx_actions_source_file_hash  ON actions(source_file_hash);
CREATE INDEX IF NOT EXISTS idx_sessions_proj_root_hash   ON sessions(project_root_hash);
CREATE INDEX IF NOT EXISTS idx_apiturns_proj_root_hash   ON api_turns(project_root_hash);
CREATE INDEX IF NOT EXISTS idx_tokenusage_src_file_hash  ON token_usage(source_file_hash);
