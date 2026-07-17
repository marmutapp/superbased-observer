-- 036_cache_tracking.sql — cache tracking system tables (Phase P0).
--
-- Backs the cachetrack arc (docs/plans/cache-tracking-implementation-spec-2026-06-08.md
-- §4). Three tables:
--
--   cache_segments — points of interest on the per-turn prefix-hash
--                    chain (breakpoints, level boundaries, divergence
--                    points). Append-only. NOT every block of every
--                    turn — only what the dashboard + freshness join
--                    need. Expected volume: ≤ ~6 rows/turn.
--   cache_entries  — the modelled live provider cache: what we believe
--                    the provider holds right now per
--                    (model, cache_scope, prefix_hash). State machine
--                    in §6 walks live↔expired↔invalidated↔unverified.
--   cache_events   — per-turn reconciliation verdict + cause attribution
--                    (§7 decision tree). One row per turn that touches
--                    the cache state (hit, write, expiry, etc.).
--
-- Privacy posture (CLAUDE.md "no contents in DB" Don't + cachetrack
-- spec §16 invariants):
-- - Hashes (SHA-256), counts, enum strings, and refs only — NEVER
--   request/response bodies, prompt text, or tool output.
-- - All three tables are NODE-LOCAL in P0–P2: explicitly EXCLUDED
--   from internal/store/orgpush.go::SelectUnpushedSince. C11 lands
--   the privacy sentinel that fails loudly on regression.
-- - cache_scope (cache_entries) is a hash of the proxy-observed
--   auth identity (R7), never the raw API key or JWT.
--
-- Tier semantics (R-engine = single store-owned engine, two feed paths;
-- see §24.4):
-- - tier='proxy'       — byte-exact observation from the Anthropic
--                        proxy path. Sees tools + system + markers.
-- - tier='transcript'  — claudecode JSONL reconstruction. Cannot see
--                        tools/system bytes; messages-level chain only.
-- - tier='counts'      — adapters with only token counts + cadence
--                        (Tier 3; P2 rollout).
-- The (api_turn_id, token_usage_id) anchor pair is mutually exclusive
-- per row: tier='proxy' rows set api_turn_id, tier='transcript' /
-- tier='counts' rows set token_usage_id. The dedup gate in §4 design
-- notes / §9 prevents both being written for the same logical turn.
--
-- Idempotency: cache_events rows are filtered by
-- (session_id, model, timestamp, kind) at the engine layer (the engine
-- is the single writer of these tables via the §24.2 store seam); a
-- UNIQUE constraint here would over-constrain (the same turn can
-- legitimately emit multiple events when a write resets multiple
-- entries). cache_entries has a UNIQUE on (model, cache_scope,
-- prefix_hash) — the natural identity. cache_segments has no UNIQUE;
-- C5 store layer dedupes via the per-turn batch shape.

CREATE TABLE IF NOT EXISTS cache_segments (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id      TEXT    NOT NULL,
    api_turn_id     INTEGER REFERENCES api_turns(id),   -- NULL on Tier 2/3
    token_usage_id  INTEGER,                             -- Tier-2/3 anchor; NULL on Tier 1
    tier            TEXT    NOT NULL,                    -- 'proxy' | 'transcript' | 'counts'
    model           TEXT    NOT NULL,
    seq             INTEGER NOT NULL,                    -- block index within request
    level           TEXT    NOT NULL,                    -- 'tools' | 'system' | 'message'
    block_kind      TEXT,                                -- text|tool_use|tool_result|image|thinking|document|attachment
    prefix_hash     TEXT    NOT NULL,                    -- cumulative chain hash blocks[0..seq]
    token_estimate  INTEGER,
    is_breakpoint   INTEGER NOT NULL DEFAULT 0,
    ttl_tier        TEXT,                                -- '5m' | '1h' (anthropic only)
    source_ref      TEXT,                                -- action source_event_id / file target (freshness join seam)
    created_at      TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_cache_segments_session ON cache_segments(session_id);
CREATE INDEX IF NOT EXISTS idx_cache_segments_turn    ON cache_segments(api_turn_id);
CREATE INDEX IF NOT EXISTS idx_cache_segments_hash    ON cache_segments(prefix_hash);

CREATE TABLE IF NOT EXISTS cache_entries (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    model           TEXT    NOT NULL,
    cache_scope     TEXT    NOT NULL DEFAULT 'default',  -- workspace/org key (R7); hash, never raw
    session_id      TEXT,                                -- creating session (informational)
    prefix_hash     TEXT    NOT NULL,
    token_count     INTEGER NOT NULL,                    -- reconciled, not estimated, when available
    ttl_tier        TEXT    NOT NULL DEFAULT '5m',
    tier            TEXT    NOT NULL,                    -- provenance of the belief
    created_at      TEXT    NOT NULL,
    last_refresh_at TEXT    NOT NULL,                    -- read or write that reset TTL
    expires_at      TEXT    NOT NULL,                    -- predicted
    state           TEXT    NOT NULL DEFAULT 'live',     -- live|expired|invalidated|unverified
    UNIQUE(model, cache_scope, prefix_hash)
);
CREATE INDEX IF NOT EXISTS idx_cache_entries_state ON cache_entries(state, expires_at);

CREATE TABLE IF NOT EXISTS cache_events (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id        TEXT    NOT NULL,
    api_turn_id       INTEGER REFERENCES api_turns(id),
    token_usage_id    INTEGER,
    tier              TEXT    NOT NULL,
    timestamp         TEXT    NOT NULL,
    model             TEXT    NOT NULL,
    kind              TEXT    NOT NULL,  -- hit|write|expiry_rewrite|invalidation_rewrite|model_switch_rewrite|compaction_reset|reanchor|mispredict|below_min
    cause             TEXT,               -- §7 vocabulary
    diverged_seq      INTEGER,            -- first differing block vs prior turn (-1 unknown)
    diverged_level    TEXT,               -- tools|system|message|unknown
    tokens_read       INTEGER NOT NULL DEFAULT 0,
    tokens_written    INTEGER NOT NULL DEFAULT 0,
    tokens_written_1h INTEGER NOT NULL DEFAULT 0,
    cost_delta_usd    REAL,               -- write paid minus hypothetical read price
    predicted_kind    TEXT,               -- what the engine expected (reconciliation)
    detail            TEXT                -- compact JSON: bounded diagnostic fields, no content
);
CREATE INDEX IF NOT EXISTS idx_cache_events_session ON cache_events(session_id);
CREATE INDEX IF NOT EXISTS idx_cache_events_kind    ON cache_events(kind);
