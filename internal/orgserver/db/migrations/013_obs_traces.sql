-- Org-tier observability T2 — trace + span STRUCTURE (obs-org-tier plan §5.1).
--
-- obs_traces / obs_spans / obs_span_events receive the trace skeleton the
-- agent pushes under [org_client.share] obs_traces — topology, kind, name,
-- model, tokens, cost, latency, status, and the content-free request_id (the
-- soft join key for the proxy-exact wedge: obs_spans LEFT JOIN api_turns ON
-- request_id). HASHES ONLY — no prompt/response/tool bodies live here (those
-- are T3 / obs_content under a separate opt-in). project_root rides only when
-- the node shares full content; project_hash always.
--
-- Multi-tenant safety: the PRIMARY KEY is composite with org_id, because a
-- member's trace_id/span_id is unique only within their own node (plan §7,
-- §15-Q5). Server-only; the rows ride via the obs provider seam.

CREATE TABLE IF NOT EXISTS obs_traces (
    org_id            TEXT NOT NULL,
    trace_id          TEXT NOT NULL,
    user_email        TEXT NOT NULL DEFAULT '',
    session_id        TEXT,
    thread_id         TEXT,
    source            TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL DEFAULT '',
    started_at        TEXT NOT NULL DEFAULT '',
    ended_at          TEXT,
    project_hash      TEXT NOT NULL DEFAULT '',
    project_root      TEXT,                      -- only under full_content/admin_managed
    root_span_id      TEXT,
    span_count        INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER NOT NULL DEFAULT 0,
    cost_usd          REAL NOT NULL DEFAULT 0,
    pushed_at         TEXT NOT NULL,
    pushed_by_user_id TEXT NOT NULL,
    PRIMARY KEY (org_id, trace_id)
);
CREATE INDEX IF NOT EXISTS idx_obs_traces_org_started ON obs_traces(org_id, started_at);
CREATE INDEX IF NOT EXISTS idx_obs_traces_org_user ON obs_traces(org_id, user_email);

CREATE TABLE IF NOT EXISTS obs_spans (
    org_id            TEXT NOT NULL,
    trace_id          TEXT NOT NULL,
    span_id           TEXT NOT NULL,
    user_email        TEXT NOT NULL DEFAULT '',
    parent_span_id    TEXT,
    kind              TEXT NOT NULL DEFAULT '',
    name              TEXT,                      -- operation label, not a body (plan §8 decision 2)
    started_at        TEXT NOT NULL DEFAULT '',
    ended_at          TEXT,
    duration_ms       INTEGER NOT NULL DEFAULT 0,
    status            TEXT NOT NULL DEFAULT '',
    model             TEXT,
    provider          TEXT,
    input_tokens      INTEGER NOT NULL DEFAULT 0,
    output_tokens     INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens  INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER NOT NULL DEFAULT 0,
    cost_usd          REAL NOT NULL DEFAULT 0,
    cost_source       TEXT,
    request_id        TEXT,                      -- content-free soft join key → api_turns
    tool_call_id      TEXT,
    pushed_at         TEXT NOT NULL,
    pushed_by_user_id TEXT NOT NULL,
    PRIMARY KEY (org_id, trace_id, span_id)
);
CREATE INDEX IF NOT EXISTS idx_obs_spans_org_trace ON obs_spans(org_id, trace_id);
CREATE INDEX IF NOT EXISTS idx_obs_spans_org_request ON obs_spans(org_id, request_id);

CREATE TABLE IF NOT EXISTS obs_span_events (
    org_id            TEXT NOT NULL,
    trace_id          TEXT NOT NULL,
    span_id           TEXT NOT NULL,
    user_email        TEXT NOT NULL DEFAULT '',
    time              TEXT NOT NULL DEFAULT '',
    name              TEXT NOT NULL DEFAULT '',
    pushed_at         TEXT NOT NULL,
    pushed_by_user_id TEXT NOT NULL,
    UNIQUE (org_id, span_id, time, name)
);
CREATE INDEX IF NOT EXISTS idx_obs_span_events_org_span ON obs_span_events(org_id, span_id);
