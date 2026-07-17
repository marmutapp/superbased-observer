-- 001_initial.sql
--
-- Teams & Org Visibility M1: the org server's initial schema. This DB lives
-- at /var/lib/observer-org/server.db and is owned exclusively by the
-- observer-org binary; the agent never opens it.
--
-- The schema has three groups of tables:
--
--   1. Identity (org singleton + SCIM-provisioned users/teams/memberships +
--      project→team mapping). Mirrors the agent's org_meta tables (migration
--      028) but reframed for the server: there is exactly one org per
--      deployment, users carry the agent's bound Ed25519 public key, and
--      projects are keyed by git root (no local projects table to FK to).
--
--   2. Auth (one-time enrolment tokens, argon2id-hashed; bearer revocation
--      list keyed by jti). Bearers themselves are stateless signed envelopes
--      and are NOT stored — validation re-checks signature + expiry + the
--      revocation list + that the subject user still exists and is active,
--      so deleting/deactivating a user implicitly revokes their bearer.
--
--   3. Data (sessions/actions/api_turns/token_usage as pushed by agents,
--      each with pushed_at + pushed_by_user_id and a per-user dedup key).
--      M1 does NOT ingest — the push endpoint terminates at a 202 ACK — so
--      these tables exist but stay empty until M2 wires the ingest handler.
--      They carry NO content columns (no prompt text, no raw tool I/O, no
--      reasoning, no free-form error messages); privacy is structural, per
--      the orgcontract row types this schema is shaped to receive.

-- ---------------------------------------------------------------------------
-- 1. Identity
-- ---------------------------------------------------------------------------

-- org is a singleton: one organisation per server deployment. org_id is
-- generated once on first boot (crypto/rand) and never changes, so it is a
-- stable bearer audience even if external_url is later reconfigured.
CREATE TABLE IF NOT EXISTS org (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    org_id        TEXT NOT NULL,
    org_name      TEXT NOT NULL,
    external_url  TEXT NOT NULL,
    created_at    TEXT NOT NULL
);

-- org_members is the SCIM User resource store. user_id is the server-issued
-- SCIM id; external_id is the IdP's id; agent_public_key (base64url Ed25519)
-- is bound at enrolment and used in M2 to verify a bearer is presented by
-- the agent that enrolled it.
CREATE TABLE IF NOT EXISTS org_members (
    user_id          TEXT PRIMARY KEY,
    external_id      TEXT,
    user_name        TEXT NOT NULL UNIQUE,
    email            TEXT NOT NULL,
    display_name     TEXT,
    active           INTEGER NOT NULL DEFAULT 1,
    agent_public_key TEXT,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_org_members_email ON org_members(email);

-- org_teams is the SCIM Group resource store (a team == a SCIM group).
CREATE TABLE IF NOT EXISTS org_teams (
    team_id      TEXT PRIMARY KEY,
    external_id  TEXT,
    display_name TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);

-- org_team_members carries the per-membership role (member|lead). SCIM Group
-- membership PATCH/PUT operations maintain this table; the role defaults to
-- 'member' and is promoted to 'lead' out-of-band (team-lead designation is a
-- dashboard action in M3, not a standard SCIM attribute).
CREATE TABLE IF NOT EXISTS org_team_members (
    team_id TEXT NOT NULL REFERENCES org_teams(team_id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES org_members(user_id) ON DELETE CASCADE,
    role    TEXT NOT NULL DEFAULT 'member',
    PRIMARY KEY (team_id, user_id)
);

-- org_project_team maps a project (by git root) to a team, either manually
-- or by a git-remote rule. Projects are identified by their root path as
-- pushed; there is no separate projects table on the server.
CREATE TABLE IF NOT EXISTS org_project_team (
    project_root TEXT NOT NULL,
    team_id      TEXT NOT NULL REFERENCES org_teams(team_id) ON DELETE CASCADE,
    source       TEXT NOT NULL DEFAULT 'manual',  -- 'manual' | 'git-remote-rule'
    created_at   TEXT NOT NULL,
    PRIMARY KEY (project_root, team_id)
);

-- ---------------------------------------------------------------------------
-- 2. Auth
-- ---------------------------------------------------------------------------

-- enrolment_tokens stores one-time tokens, argon2id-hashed (never the
-- cleartext). A token is minted by an admin for a specific user, has a TTL,
-- and is burned (used_at set) on a successful enrol. The cleartext is
-- returned to the admin exactly once at mint time.
CREATE TABLE IF NOT EXISTS enrolment_tokens (
    id          TEXT PRIMARY KEY,
    token_hash  TEXT NOT NULL,
    user_id     TEXT NOT NULL REFERENCES org_members(user_id) ON DELETE CASCADE,
    created_at  TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    used_at     TEXT
);

CREATE INDEX IF NOT EXISTS idx_enrolment_tokens_user ON enrolment_tokens(user_id);

-- revoked_bearers is the jti revocation list. An admin Revoke action (M3)
-- inserts the bearer's jti here; bearer validation rejects any jti present.
CREATE TABLE IF NOT EXISTS revoked_bearers (
    jti        TEXT PRIMARY KEY,
    revoked_at TEXT NOT NULL
);

-- ---------------------------------------------------------------------------
-- 3. Data (pushed rows — empty until M2 wires ingest)
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS sessions (
    id                TEXT NOT NULL,
    user_id           TEXT NOT NULL,
    org_id            TEXT,
    user_email        TEXT,
    project_root      TEXT,
    git_remote        TEXT,
    tool              TEXT,
    model             TEXT,
    git_branch        TEXT,
    started_at        TEXT,
    ended_at          TEXT,
    total_actions     INTEGER,
    pushed_at         TEXT NOT NULL,
    pushed_by_user_id TEXT NOT NULL,
    PRIMARY KEY (id, user_id)
);

CREATE TABLE IF NOT EXISTS actions (
    source_file       TEXT NOT NULL,
    source_event_id   TEXT NOT NULL,
    user_id           TEXT NOT NULL,
    session_id        TEXT,
    org_id            TEXT,
    user_email        TEXT,
    timestamp         TEXT,
    tool              TEXT,
    action_type       TEXT,
    target            TEXT,
    turn_index        INTEGER,
    success           INTEGER,
    duration_ms       INTEGER,
    is_sidechain      INTEGER,
    pushed_at         TEXT NOT NULL,
    pushed_by_user_id TEXT NOT NULL,
    PRIMARY KEY (source_file, source_event_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_actions_session ON actions(session_id);

-- api_turns has no (source_file, source_event_id) natural key in the
-- orgcontract row type, so it uses a surrogate id plus a best-effort dedup
-- index on (user_id, session_id, request_id, timestamp). M2 finalises the
-- ingest dedup semantics; the index is a guard, not the contract.
CREATE TABLE IF NOT EXISTS api_turns (
    id                       INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id                  TEXT NOT NULL,
    org_id                   TEXT,
    user_email               TEXT,
    session_id               TEXT,
    project_root             TEXT,
    timestamp                TEXT,
    provider                 TEXT,
    model                    TEXT,
    request_id               TEXT,
    input_tokens             INTEGER,
    output_tokens            INTEGER,
    cache_read_tokens        INTEGER,
    cache_creation_tokens    INTEGER,
    cache_creation_1h_tokens INTEGER,
    web_search_requests      INTEGER,
    cost_usd                 REAL,
    message_count            INTEGER,
    tool_use_count           INTEGER,
    system_prompt_hash       TEXT,
    message_prefix_hash      TEXT,
    time_to_first_token_ms   INTEGER,
    total_response_ms        INTEGER,
    stop_reason              TEXT,
    http_status              INTEGER,
    error_class              TEXT,
    pushed_at                TEXT NOT NULL,
    pushed_by_user_id        TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_api_turns_dedup
    ON api_turns(user_id, session_id, request_id, timestamp);

CREATE TABLE IF NOT EXISTS token_usage (
    source_file              TEXT NOT NULL,
    source_event_id          TEXT NOT NULL,
    user_id                  TEXT NOT NULL,
    org_id                   TEXT,
    user_email               TEXT,
    session_id               TEXT,
    project_root             TEXT,
    timestamp                TEXT,
    tool                     TEXT,
    model                    TEXT,
    input_tokens             INTEGER,
    output_tokens            INTEGER,
    cache_read_tokens        INTEGER,
    cache_creation_tokens    INTEGER,
    cache_creation_1h_tokens INTEGER,
    reasoning_tokens         INTEGER,
    web_search_requests      INTEGER,
    estimated_cost_usd       REAL,
    source                   TEXT,
    reliability              TEXT,
    pushed_at                TEXT NOT NULL,
    pushed_by_user_id        TEXT NOT NULL,
    PRIMARY KEY (source_file, source_event_id, user_id)
);
