-- obs subsystem schema v5 — input-admission audit (admission spec §7).
--
-- Separability/privacy (obs plan §2.2/§10, decision D3): like every obs_*
-- table these are NODE-LOCAL and applied only by the obs applier when
-- [observability] is enabled. They are pinned by
-- tests/invariant/privacy_test.go's forbiddenCacheTables sentinel so the names
-- can never reach internal/store/orgpush.go — no org wire, no server pair.
--
-- Privacy: the raw request text is NEVER stored — only message_hash. The one
-- content-bearing column, reason_excerpt (which may quote the request), is
-- written only under the injected ContentGate (ShareOptions.shipsRawContent()),
-- exactly like obs_span_content; content_hash-style provenance is always the
-- message_hash. trace_id/request_id are SOFT join values to obs_spans/api_turns
-- (no FK), so a verdict renders as enrichment on the trajectory view.

CREATE TABLE IF NOT EXISTS obs_admission_events (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    ts             TEXT NOT NULL,
    prev_hash      TEXT NOT NULL DEFAULT '',   -- chain_prev: row_hash of the prior row
    row_hash       TEXT NOT NULL,              -- SHA-256(prev || 0x1e || canonical bytes)
    mode           TEXT NOT NULL,              -- observe | enforce (off never records)
    decision       TEXT NOT NULL,              -- allow | flag | ask | deny
    severity       TEXT NOT NULL,              -- info | warn | high | critical
    criterion_id   TEXT,                       -- the criterion that fired ('' if none)
    policy_hash    TEXT NOT NULL,              -- soft join to obs_admission_policy_versions
    judge_used     INTEGER NOT NULL DEFAULT 0,
    judge_hosting  TEXT,                       -- local | provider | aggregator | private | off
    degraded       TEXT,                       -- '' | timeout-failopen | cache | prefiltered
    latency_ms     INTEGER NOT NULL DEFAULT 0,
    trace_id       TEXT,                       -- soft join to obs_spans (NOT an FK)
    session_id     TEXT,
    tenant         TEXT,
    user           TEXT,
    request_id     TEXT,                       -- soft join to api_turns (NOT an FK)
    message_hash   TEXT NOT NULL,              -- raw request is NEVER stored, only its hash
    reason_excerpt TEXT                        -- may quote request → written only under ContentGate
);
CREATE INDEX IF NOT EXISTS idx_obs_admission_events_ts       ON obs_admission_events(ts);
CREATE INDEX IF NOT EXISTS idx_obs_admission_events_trace    ON obs_admission_events(trace_id);
CREATE INDEX IF NOT EXISTS idx_obs_admission_events_request  ON obs_admission_events(request_id);
CREATE INDEX IF NOT EXISTS idx_obs_admission_events_decision ON obs_admission_events(decision);

-- Content-addressed policy snapshots (which version admitted/blocked a
-- request). body is the ADMIN's authored policy (config, not end-user content),
-- so it is stored plainly; user requests never land here.
CREATE TABLE IF NOT EXISTS obs_admission_policy_versions (
    policy_hash    TEXT PRIMARY KEY,
    created_at     TEXT NOT NULL,
    mode           TEXT NOT NULL,
    scope          TEXT NOT NULL,
    criteria_count INTEGER NOT NULL DEFAULT 0,
    body           TEXT NOT NULL DEFAULT ''
);
