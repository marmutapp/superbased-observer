-- Native-console integration (docs/native-console-integration-template.md,
-- Phase 2b body ingest). Stores CONTENT bodies a coding assistant emits on its
-- native OTel stream — user prompts, tool input/output, raw API bodies — but
-- only when the admin enabled the OTEL_LOG_* flags at the source. Content is
-- scrubbed for secrets before insert (same as actions.raw_tool_input).
--
-- Posture: this is a PUSHED-WITH-GATE table (native-console template §2.4). The
-- node stores content locally exactly like actions.raw_tool_input; the org-push
-- seam (internal/store/orgpush.go) ships content_hash always and the raw
-- content only under the content-sharing gate (full_content / admin_managed).
-- It is therefore NOT in the privacy-sentinel forbidden-table set — that set is
-- for tables that must NEVER push (cache_*, router_decisions, …). The push
-- wiring + a content sentinel land with the Layer-B commit.
--
-- request_id is the turn join key (may be empty for a session-level prompt);
-- tool_use_id is set for tool_input/tool_output kinds. The UNIQUE key makes
-- re-delivered telemetry idempotent (OTLP is at-least-once).
CREATE TABLE IF NOT EXISTS otel_content (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id   TEXT,
    session_id   TEXT,
    tool_use_id  TEXT,
    kind         TEXT NOT NULL,
    content      TEXT,
    content_hash TEXT NOT NULL,
    timestamp    TEXT NOT NULL,
    source       TEXT,
    UNIQUE(content_hash, kind, request_id, tool_use_id)
);

CREATE INDEX IF NOT EXISTS idx_otel_content_request_id
    ON otel_content(request_id) WHERE request_id IS NOT NULL;
