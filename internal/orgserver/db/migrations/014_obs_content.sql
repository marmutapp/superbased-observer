-- Org-tier observability T3 — raw span CONTENT bodies (obs-org-tier plan §5.1).
--
-- obs_content receives prompt/response/tool_io bodies the agent pushes under
-- [org_client.share] obs_content — but the raw `content` rides ONLY when the
-- node also shares full content (full_content/admin_managed); the agent strips
-- it otherwise and the server stores NULL, so it cannot tell "stripped" from
-- "never had one" (no posture leak — same as otel_content, server 007). The
-- content_hash always rides. Mirrors the otel_content store; read by the
-- AUDITED org span-content viewer (a view_span_content audit row precedes any
-- disclosure). Composite org_id key for multi-tenant safety.
CREATE TABLE IF NOT EXISTS obs_content (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id            TEXT NOT NULL,
    content_hash      TEXT NOT NULL,
    kind              TEXT,                      -- prompt/response/tool_io
    span_id           TEXT NOT NULL,
    user_email        TEXT NOT NULL DEFAULT '',
    trace_id          TEXT,
    content           TEXT,                      -- NULL unless the node shares full content
    timestamp         TEXT NOT NULL DEFAULT '',
    pushed_at         TEXT NOT NULL,
    pushed_by_user_id TEXT NOT NULL,
    UNIQUE (org_id, content_hash, kind, span_id)
);
CREATE INDEX IF NOT EXISTS idx_obs_content_org_span ON obs_content(org_id, span_id);
