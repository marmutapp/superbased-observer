-- Org-tier observability T6 — input-admission verdicts + policy snapshots
-- (Plane-A admission org tier, gap-audit 2026-07-10 §2.1 / #1a). Server-only
-- pair for node obs migration 0005 tables (obs_admission_events /
-- obs_admission_policy_versions); the rows arrive under
-- [org_client.share.obs].admission and are composed via the obs provider seam
-- (orgpush.go never names these tables — the privacy sentinel stays green).
--
-- obs_admission_events receives the per-verdict metadata a node records when it
-- judges an end-user request against its admission policy. The raw request text
-- is NEVER stored on the node — only message_hash — so the verdict metadata is
-- content-free and always ships. The three content-bearing columns (tenant,
-- end_user, reason_excerpt) arrive ONLY when the node shares full content and
-- are stored NULL otherwise, so the server cannot tell "stripped" from "never
-- had one" (no posture leak — the same posture as obs_content / otel_content).
-- The org admin cannot force any of this on remotely.
--
-- Verdicts are immutable (INSERT OR IGNORE): the node hash-chains them and
-- row_hash is the per-node dedup key. UNIQUE (org_id, user_email, row_hash)
-- keeps each pushing node's chain distinct (user_email = the re-pinned pusher;
-- a re-pushed window dedups on row_hash).
CREATE TABLE IF NOT EXISTS obs_admission_events (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id            TEXT NOT NULL,
    user_email        TEXT NOT NULL DEFAULT '',   -- the pushing developer/operator (re-pinned)
    row_hash          TEXT NOT NULL,              -- node tamper-evidence chain head; server dedup key
    prev_hash         TEXT NOT NULL DEFAULT '',   -- node tamper-evidence chain link
    ts                TEXT NOT NULL,              -- verdict instant (RFC3339)
    mode              TEXT NOT NULL DEFAULT '',   -- observe | enforce
    decision          TEXT NOT NULL DEFAULT '',   -- allow | flag | ask | deny (node vocabulary, verbatim)
    severity          TEXT NOT NULL DEFAULT '',   -- info | warn | high | critical
    criterion_id      TEXT NOT NULL DEFAULT '',
    policy_hash       TEXT NOT NULL DEFAULT '',   -- soft join → obs_admission_policy_versions
    judge_used        INTEGER NOT NULL DEFAULT 0,
    judge_hosting     TEXT NOT NULL DEFAULT '',   -- local | provider | aggregator | private | off
    degraded          TEXT NOT NULL DEFAULT '',   -- '' | timeout-failopen | cache | prefiltered
    latency_ms        INTEGER NOT NULL DEFAULT 0,
    trace_id          TEXT NOT NULL DEFAULT '',   -- content-free soft join → obs_spans
    session_id        TEXT NOT NULL DEFAULT '',
    request_id        TEXT NOT NULL DEFAULT '',   -- content-free soft join → api_turns
    message_hash      TEXT NOT NULL DEFAULT '',   -- hash of the raw request (raw text never stored)
    tenant            TEXT,                       -- NULL unless full-content sharing (gated)
    end_user          TEXT,                       -- NULL unless full-content sharing (gated; PII, app-shared)
    reason_excerpt    TEXT,                       -- NULL unless full-content sharing (gated; verdict prose)
    pushed_at         TEXT NOT NULL,
    pushed_by_user_id TEXT NOT NULL,
    UNIQUE (org_id, user_email, row_hash)
);
CREATE INDEX IF NOT EXISTS idx_obs_admission_events_org_ts ON obs_admission_events(org_id, ts);
CREATE INDEX IF NOT EXISTS idx_obs_admission_events_org_decision ON obs_admission_events(org_id, decision);
CREATE INDEX IF NOT EXISTS idx_obs_admission_events_org_request ON obs_admission_events(org_id, request_id);

-- obs_admission_policy_versions receives the content-addressed admission policy
-- snapshots the verdicts reference. body is the ADMIN's authored policy
-- (config, not end-user content), so it ALWAYS ships (never gated) — user
-- requests never land in this table. Content-addressed: a re-push of the same
-- policy_hash UPSERTs (idempotent), so the natural key is
-- (org_id, user_email, policy_hash).
CREATE TABLE IF NOT EXISTS obs_admission_policy_versions (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id            TEXT NOT NULL,
    user_email        TEXT NOT NULL DEFAULT '',   -- the pushing developer/operator (re-pinned)
    policy_hash       TEXT NOT NULL,
    created_at        TEXT NOT NULL DEFAULT '',
    mode              TEXT NOT NULL DEFAULT '',
    scope             TEXT NOT NULL DEFAULT '',
    criteria_count    INTEGER NOT NULL DEFAULT 0,
    body              TEXT NOT NULL DEFAULT '',
    pushed_at         TEXT NOT NULL,
    pushed_by_user_id TEXT NOT NULL,
    UNIQUE (org_id, user_email, policy_hash)
);
CREATE INDEX IF NOT EXISTS idx_obs_admission_policy_versions_org ON obs_admission_policy_versions(org_id);
