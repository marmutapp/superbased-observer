-- obs subsystem schema v1 — node-local span/trace layer.
--
-- Separability (plan §2.2/§3.1, decision D3): these tables are OWNED by the
-- obs subsystem and applied by internal/obs/store's own applier ONLY when
-- [observability] enabled — NOT by the host migrator (internal/db). A node
-- with the subsystem disabled never creates them (true zero schema cost).
--
-- Privacy (plan §10): every table here is NODE-LOCAL — pinned by
-- tests/invariant/privacy_test.go's forbiddenCacheTables AST sentinel so the
-- obs_* names can never appear in internal/store/orgpush.go. No SQL foreign
-- keys into existing tables; the link to api_turns is a soft request_id value.

CREATE TABLE IF NOT EXISTS obs_traces (
    trace_id     TEXT PRIMARY KEY,
    session_id   TEXT,
    thread_id    TEXT,
    tenant       TEXT,
    user         TEXT,
    source       TEXT NOT NULL,
    root_span_id TEXT,
    project_root TEXT,            -- gated like existing content (raw only under ContentGate)
    status       TEXT NOT NULL DEFAULT 'unset',
    started_at   TEXT NOT NULL,
    ended_at     TEXT
);
CREATE INDEX IF NOT EXISTS idx_obs_traces_session ON obs_traces(session_id);
CREATE INDEX IF NOT EXISTS idx_obs_traces_started ON obs_traces(started_at);

CREATE TABLE IF NOT EXISTS obs_spans (
    span_id              TEXT PRIMARY KEY,
    trace_id             TEXT NOT NULL,          -- FK WITHIN obs_* only (soft)
    parent_span_id       TEXT,                   -- nullable ⇒ tree root
    kind                 TEXT NOT NULL,
    name                 TEXT,
    status               TEXT NOT NULL DEFAULT 'unset',
    started_at           TEXT NOT NULL,
    ended_at             TEXT,
    model                TEXT,
    provider             TEXT,
    input_tokens         INTEGER,                -- nullable, authoritative-on-merge
    output_tokens        INTEGER,
    total_tokens         INTEGER,
    cost_usd             REAL,
    request_id           TEXT,                   -- soft join value to api_turns (NOT an FK)
    provider_response_id TEXT,
    tool_call_id         TEXT,
    source               TEXT
);
CREATE INDEX IF NOT EXISTS idx_obs_spans_trace  ON obs_spans(trace_id);
CREATE INDEX IF NOT EXISTS idx_obs_spans_parent ON obs_spans(parent_span_id);
CREATE INDEX IF NOT EXISTS idx_obs_spans_request ON obs_spans(request_id);

CREATE TABLE IF NOT EXISTS obs_span_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    span_id    TEXT NOT NULL,                    -- FK WITHIN obs_* only
    time       TEXT NOT NULL,
    name       TEXT NOT NULL,
    attributes TEXT,                             -- metadata-only JSON blob
    UNIQUE(span_id, time, name)
);
CREATE INDEX IF NOT EXISTS idx_obs_span_events_span ON obs_span_events(span_id);

CREATE TABLE IF NOT EXISTS obs_span_links (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    span_id       TEXT NOT NULL,                 -- FK WITHIN obs_* only
    linked_trace  TEXT NOT NULL,
    linked_span   TEXT,
    attributes    TEXT,
    UNIQUE(span_id, linked_trace, linked_span)
);
CREATE INDEX IF NOT EXISTS idx_obs_span_links_span ON obs_span_links(span_id);

CREATE TABLE IF NOT EXISTS obs_span_content (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    span_id      TEXT NOT NULL,                  -- FK WITHIN obs_* only
    trace_id     TEXT,
    request_id   TEXT,
    kind         TEXT NOT NULL,
    content      TEXT,                           -- RAW body; only when ContentGate allows
    content_hash TEXT NOT NULL,                  -- content-free signal, always present
    time         TEXT NOT NULL,
    UNIQUE(content_hash, kind, span_id)
);
CREATE INDEX IF NOT EXISTS idx_obs_span_content_span ON obs_span_content(span_id);
