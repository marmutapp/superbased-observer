-- 034_org_push_path_hashes.sql — v1.8.0 privacy fix (Issues 1+2 of the
-- 2026-06-02 teams test findings).
--
-- The Teams push seam at internal/store/orgpush.go was shipping
-- `actions.target` (which carries 200-char content excerpts on
-- run_command and assistant prose on task_complete), `actions.source_file`
-- (raw JSONL paths revealing home dir + project structure), and
-- `projects.root_path` / `projects.git_remote` (raw filesystem paths +
-- remote URLs) verbatim to the org server. The privacy invariant test
-- had a blind spot — it asserted `main.go` was present in the wire as
-- "proof of populated payload" — and so CI was green while the leak
-- shipped.
--
-- The v1.8.0 fix replaces those raw fields on the wire with sha256-hex
-- hashes by default; the node operator can flip
-- `[org_client.share].full_content = true` to ship raws (opt-in only,
-- the org admin cannot force it on). To keep `orgpush.SelectUnpushedSince`
-- a pure SQL seam, we denormalize the hashes onto the source tables
-- here:
--
--   projects.root_path_hash    -- always populated, sha256(root_path)
--   projects.git_remote_hash   -- empty when git_remote is NULL/empty,
--                                 sha256(git_remote) otherwise
--   actions.source_file_hash   -- always populated, sha256(source_file)
--   token_usage.source_file_hash -- always populated, sha256(source_file)
--
-- (actions.target_hash already exists since v1.2-era; we just start
--  shipping it.)
--
-- The hash columns DEFAULT to '' so existing rows pass the NOT NULL
-- constraint immediately; a follow-up Go-side backfill at db.Open time
-- populates them from the source values. Subsequent INSERTs at
-- internal/store/store.go stamp the hashes inline alongside the raw
-- values, so a fresh-after-migration row never has an empty hash.

ALTER TABLE projects   ADD COLUMN root_path_hash    TEXT NOT NULL DEFAULT '';
ALTER TABLE projects   ADD COLUMN git_remote_hash   TEXT NOT NULL DEFAULT '';
ALTER TABLE actions    ADD COLUMN source_file_hash  TEXT NOT NULL DEFAULT '';
ALTER TABLE token_usage ADD COLUMN source_file_hash TEXT NOT NULL DEFAULT '';
