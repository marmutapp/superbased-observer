-- 007_otel_content.sql — server-side native-OTel content store
-- (native-console integration, Phase 2b body-ingest Layer B; paired with
-- agent migration 048).
--
-- Receives PushEnvelope.otel_content rows (orgcontract.OTelContentRow): the
-- content bodies a coding assistant emits on its native OTel stream — prompts,
-- tool input/output — captured on nodes where the admin enabled OTEL_LOG_*.
-- Until this migration an older server ACKed and dropped the key (omitempty
-- additive compat).
--
-- Dedup key: (content_hash, user_id, request_id, tool_use_id). The agent's
-- content_hash (sha256-hex of the scrubbed body) is the natural anchor; the
-- other columns disambiguate the same body across turns/tools.
--
-- Privacy posture (mirrors guard_events / actions.target): content_hash is
-- content-free and ALWAYS present. content is content-bearing — it arrives
-- ONLY when the node shares full content (full_content / admin_managed) and is
-- stored NULL otherwise, so the server cannot tell "stripped" from "never had
-- one" (no posture leak). The org admin cannot force it on remotely.
CREATE TABLE IF NOT EXISTS otel_content (
    content_hash      TEXT NOT NULL,
    user_id           TEXT NOT NULL,
    org_id            TEXT,
    user_email        TEXT,
    request_id        TEXT NOT NULL DEFAULT '',
    session_id        TEXT,
    tool_use_id       TEXT NOT NULL DEFAULT '',
    kind              TEXT,
    content           TEXT,           -- NULL unless full-content sharing
    timestamp         TEXT,           -- RFC3339, event time on the agent
    pushed_at         TEXT NOT NULL,
    pushed_by_user_id TEXT NOT NULL,
    UNIQUE(content_hash, user_id, request_id, tool_use_id)
);

CREATE INDEX IF NOT EXISTS idx_otel_content_user ON otel_content(user_id);
