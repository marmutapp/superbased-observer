-- 028_org_meta.sql
--
-- Teams & Org Visibility M0 (foundations): org-mode metadata tables.
-- Records which org this agent is enrolled in, which users belong to
-- it, which teams those users belong to, which projects (by git root)
-- map to which teams, and a local log of what the agent has pushed.
--
-- All tables are EMPTY on a solo-local install and are never read by
-- the existing dashboard query paths. They are populated only once the
-- agent enrols (M2). The org_id / user_email attribution columns on
-- actions/sessions/api_turns/token_usage live in migration 029 and are
-- nullable + default NULL, so a never-enrolled agent's behaviour and
-- on-disk shape are byte-identical to pre-028.
--
-- ON CONFLICT / FK semantics: org_team_members and org_project_team
-- cascade-delete with their parents so a SCIM deprovision or project
-- removal cleans up membership rows. Foreign keys are a no-op on
-- solo-local installs (the parent tables are empty). org_enrolment is
-- a singleton (CHECK id = 1) — at most one org per agent.

CREATE TABLE IF NOT EXISTS org_enrolment (
    id              INTEGER PRIMARY KEY CHECK (id = 1),
    org_id          TEXT NOT NULL,
    org_name        TEXT NOT NULL,
    org_server_url  TEXT NOT NULL,
    user_id         TEXT NOT NULL,        -- SCIM user id
    user_email      TEXT NOT NULL,
    enrolled_at     TEXT NOT NULL,
    bearer_key_id   TEXT NOT NULL         -- handle into OS keychain
);

CREATE TABLE IF NOT EXISTS org_members (
    org_id          TEXT NOT NULL,
    user_id         TEXT NOT NULL,        -- SCIM user id
    user_email      TEXT NOT NULL,
    display_name    TEXT,
    active          INTEGER NOT NULL DEFAULT 1,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    PRIMARY KEY (org_id, user_id)
);

CREATE TABLE IF NOT EXISTS org_teams (
    org_id          TEXT NOT NULL,
    team_id         TEXT NOT NULL,        -- SCIM group id
    team_name       TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    PRIMARY KEY (org_id, team_id)
);

CREATE TABLE IF NOT EXISTS org_team_members (
    org_id          TEXT NOT NULL,
    team_id         TEXT NOT NULL,
    user_id         TEXT NOT NULL,
    role            TEXT NOT NULL DEFAULT 'member',  -- 'lead' | 'member'
    PRIMARY KEY (org_id, team_id, user_id),
    FOREIGN KEY (org_id, team_id) REFERENCES org_teams(org_id, team_id) ON DELETE CASCADE,
    FOREIGN KEY (org_id, user_id) REFERENCES org_members(org_id, user_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS org_project_team (
    project_id      INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    org_id          TEXT NOT NULL,
    team_id         TEXT NOT NULL,
    source          TEXT NOT NULL DEFAULT 'manual', -- 'manual' | 'git-remote-rule'
    created_at      TEXT NOT NULL,
    PRIMARY KEY (project_id, org_id, team_id)
);

CREATE TABLE IF NOT EXISTS org_push_log (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    pushed_at       TEXT NOT NULL,
    row_count       INTEGER NOT NULL,
    byte_count      INTEGER NOT NULL,
    status          TEXT NOT NULL,        -- 'ok' | 'retry' | 'failed'
    error           TEXT
);

CREATE INDEX IF NOT EXISTS idx_org_push_log_pushed_at ON org_push_log(pushed_at DESC);
