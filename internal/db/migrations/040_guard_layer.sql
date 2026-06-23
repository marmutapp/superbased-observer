-- 040_guard_layer.sql — guard layer (security & control) tables, Phase G-P0.
--
-- Backs the guard arc (docs/plans/guard-layer-implementation-spec-2026-06-10.md
-- §10). Four tables:
--
--   guard_events       — one row per policy verdict worth recording
--                        (decision >= flag, plus guard_error / tamper
--                        events). Append-only, hash-chained (§10.4):
--                        chain_hash = SHA-256(chain_prev || canonical
--                        row bytes) per node, so history is
--                        tamper-EVIDENT (truncation detectable via the
--                        prune checkpoint in schema_meta; silent rewrite
--                        breaks the chain). NOT tamper-proof — an
--                        attacker with DB write access can recompute the
--                        chain; the honest framing lives in docs.
--   guard_pins         — first-sight pin records for MCP servers, hook
--                        configs and compiled native dialects (§9.2,
--                        §13.2). pin_hash is SHA-256 over the pinned
--                        identity material; never raw content.
--   guard_policy_state — append-only version log of every policy source
--                        load whose content hash changed (layer = org |
--                        user | project). Powers the §14.4 policy-change
--                        log; signature is the org bundle's Ed25519
--                        signature (NULL for local layers).
--   guard_approvals    — operator-granted scoped exceptions (§6.3): the
--                        reviewable exception register (§14.4).
--
-- Privacy posture (spec §10.2 — differs from the cache_* tables):
-- - guard_events DO push to the org server (G14 rollups), with the
--   content-bearing columns (reason, target_excerpt, taint_origin)
--   stripped in Go at internal/store/orgpush.go::SelectUnpushedSince
--   unless the node operator set [org_client.share].full_content = true.
--   target_hash always ships. Pinned by tests/invariant/privacy_test.go.
-- - guard_pins / guard_policy_state / guard_approvals are NODE-LOCAL in
--   G-P0/P1: excluded from SelectUnpushedSince entirely (the §14.3
--   pins-ship-hash-only / approvals-register wire surfaces land with the
--   G13/G14 paired-migration arc, which must consciously update the
--   privacy sentinel). Also pinned by tests/invariant/privacy_test.go.
-- - CLAUDE.md "no contents in DB" Don't applies: target_excerpt is a
--   BOUNDED excerpt (the store helper truncates), reason/advice are
--   rule-authored sentences, never file contents or command output.
--
-- Ownership (spec §17.4): every write to these four tables goes through
-- internal/store/guard.go helpers — one owner, each helper owns its own
-- transaction, failure-isolated (a guard-write failure never fails an
-- ingest or delays a hook reply; callers log and continue).

CREATE TABLE IF NOT EXISTS guard_events (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    ts             TEXT    NOT NULL,
    session_id     TEXT,
    action_id      INTEGER REFERENCES actions(id),    -- post-hoc (watcher) anchor; NULL on hook/proxy paths
    api_turn_id    INTEGER REFERENCES api_turns(id),  -- proxy anchor; NULL elsewhere
    tool           TEXT,                              -- client name (reporting only)
    event_kind     TEXT,                              -- policy.EventKind string form
    rule_id        TEXT    NOT NULL,                  -- "R-101" | "guard_error" | ...
    category       TEXT,                              -- destructive|boundary|secrets|exfil|posture|mcp|taint|budget|anomaly
    severity       TEXT,                              -- info|warn|high|critical (policy.Severity.String)
    decision       TEXT,                              -- allow|flag|ask|deny (policy.Decision.String)
    degraded_from  TEXT,                              -- original decision when capability degradation applied (§6.2); NULL otherwise
    enforced       INTEGER NOT NULL DEFAULT 0,        -- 1 when the decision actually blocked/asked (enforce mode)
    source         TEXT,                              -- builtin|user|project|org|llm_judge
    reason         TEXT,                              -- LLM-readable verdict reason (content-bearing: stripped on push)
    target_hash    TEXT,                              -- sha256 hex of the full target (always pushable)
    target_excerpt TEXT,                              -- bounded excerpt of the target (content-bearing: stripped on push)
    taint_origin   TEXT,                              -- taint source description when a taint rule fired (stripped on push)
    chain_prev     TEXT    NOT NULL,                  -- prior row's chain_hash ('' for genesis / checkpoint anchor)
    chain_hash     TEXT    NOT NULL                   -- SHA-256(chain_prev || 0x1e || canonical row bytes), hex
);
CREATE INDEX IF NOT EXISTS idx_guard_events_session ON guard_events(session_id);
CREATE INDEX IF NOT EXISTS idx_guard_events_rule    ON guard_events(rule_id);
CREATE INDEX IF NOT EXISTS idx_guard_events_ts      ON guard_events(ts);

CREATE TABLE IF NOT EXISTS guard_pins (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    kind          TEXT    NOT NULL,                   -- 'mcp_server' | 'hook_config' | 'native_dialect'
    name          TEXT    NOT NULL,                   -- server/config identifier within the kind
    client        TEXT    NOT NULL DEFAULT '',        -- owning AI client ('' when client-agnostic)
    pin_hash      TEXT    NOT NULL,                   -- SHA-256 over the pinned identity material
    first_seen    TEXT    NOT NULL,
    last_verified TEXT    NOT NULL,
    status        TEXT    NOT NULL DEFAULT 'pinned',  -- 'pinned' | 'drifted' | 'approved'
    UNIQUE(kind, name, client)
);

CREATE TABLE IF NOT EXISTS guard_policy_state (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    layer        TEXT    NOT NULL,                    -- 'org' | 'user' | 'project'
    path         TEXT    NOT NULL,                    -- source location (file path / bundle URL)
    version      TEXT    NOT NULL DEFAULT '',         -- bundle/file version string when the source carries one
    content_hash TEXT    NOT NULL,                    -- SHA-256 hex of the policy source bytes
    signature    TEXT,                                -- Ed25519 signature (org bundles only; NULL otherwise)
    loaded_at    TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_guard_policy_state_layer ON guard_policy_state(layer, path, id);

CREATE TABLE IF NOT EXISTS guard_approvals (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                TEXT    NOT NULL,
    rule_id           TEXT    NOT NULL,
    scope             TEXT    NOT NULL,               -- 'once' | 'session' | 'project' | 'global'
    session_id        TEXT    NOT NULL DEFAULT '',    -- set for scope='once'/'session'
    project_root_hash TEXT    NOT NULL DEFAULT '',    -- set for scope='project' (sha256 hex, never the raw path)
    granted_by        TEXT    NOT NULL DEFAULT '',    -- approver identity (local operator / org user)
    expires_at        TEXT    NOT NULL DEFAULT ''     -- RFC3339; '' = does not expire
);
CREATE INDEX IF NOT EXISTS idx_guard_approvals_rule ON guard_approvals(rule_id, scope);
