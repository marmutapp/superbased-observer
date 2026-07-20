-- 067_process_network_bodies.sql — local process-network request/response bodies
--
-- Extends process_events (migration 044) with an opt-in, node-local body
-- capture table for network diagnostics. process_events remains the one-row
-- metadata/event substrate; this table carries capped/scrubbed request and
-- response excerpts only when a capture source can see plaintext (currently
-- the Observer proxy path). Non-proxied TLS egress stays metadata-only and
-- records an unavailable reason in process_events.details_json.
--
-- Privacy posture: NODE-LOCAL. This table MUST NOT appear in
-- internal/store/orgpush.go::SelectUnpushedSince and is pinned by
-- tests/invariant/privacy_test.go. It may contain provider request/response
-- excerpts, so it is never part of any org push without a separate explicit
-- privacy design.

CREATE TABLE IF NOT EXISTS process_network_bodies (
    id                         INTEGER PRIMARY KEY AUTOINCREMENT,
    process_event_id           INTEGER NOT NULL REFERENCES process_events(id) ON DELETE CASCADE,
    capture_source             TEXT NOT NULL,
    api_turn_id                INTEGER REFERENCES api_turns(id),
    request_id                 TEXT,
    method                     TEXT,
    url                        TEXT,
    host                       TEXT,
    status_code                INTEGER,
    duration_ms                INTEGER,
    request_headers_json       TEXT,
    response_headers_json      TEXT,
    request_body               TEXT,
    request_body_sha256        TEXT,
    request_body_bytes         INTEGER,
    request_body_truncated     INTEGER NOT NULL DEFAULT 0,
    response_body              TEXT,
    response_body_sha256       TEXT,
    response_body_bytes        INTEGER,
    response_body_truncated    INTEGER NOT NULL DEFAULT 0,
    response_content_type      TEXT,
    body_unavailable_reason    TEXT,
    created_at                 TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_process_network_bodies_event
    ON process_network_bodies(process_event_id);

CREATE INDEX IF NOT EXISTS idx_process_network_bodies_request
    ON process_network_bodies(request_id);

CREATE INDEX IF NOT EXISTS idx_process_network_bodies_api_turn
    ON process_network_bodies(api_turn_id);
