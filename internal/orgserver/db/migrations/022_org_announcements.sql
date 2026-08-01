-- Org announcements — rail R3 of
-- docs/plans/dashboard-announcements-banner-plan-2026-07-31.md §4,
-- paired with agent-side migration 076_org_announcements.sql.
--
-- org_announcements is the versioned, signed announcement registry:
-- the org admin publishes; enrolled agents fetch the latest on the
-- push-loop cycle they already run and cache it node-locally. Shape
-- and mechanism deliberately mirror org_routing_policies (006_routing.sql:33)
-- — same MAX(version)+1 sequencing, same Ed25519 signature over the
-- body bytes, and the SAME signing key (routing_policy_keys, id=1), so
-- an agent that has TOFU-pinned the org key on one rail sees the same
-- key on this one.
--
-- body is the plan §1 announcement JSON (one object or an array of
-- them), already validated through internal/announce.Validate BEFORE
-- signing. An EMPTY body is the retraction: a real, signed, version-
-- bumped document that instructs nodes to show nothing. Retraction is
-- a publish rather than a delete because the agent's monotonic-version
-- short-circuit (fetch nothing when cached version >= served version)
-- is what keeps the poll cheap — a deleted row would be invisible to it.
--
-- This document can only ever put dismissible text in a banner. It
-- carries no toggle, no command, and no code; the node's
-- [dashboard].org_announcements switch silences it locally and no
-- server-side override for that exists (same posture as
-- [org_client.share].full_content).
CREATE TABLE IF NOT EXISTS org_announcements (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    version     INTEGER NOT NULL UNIQUE,
    body        TEXT NOT NULL,
    body_hash   TEXT NOT NULL,
    signature   TEXT NOT NULL,
    created_by  TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

-- org_announcement_audit is the change log: who published (or
-- retracted) which version when. Written in the SAME transaction as the
-- publish, exactly like routing_policy_audit — a published version with
-- no audit row must be impossible, not merely unlikely.
CREATE TABLE IF NOT EXISTS org_announcement_audit (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    version    INTEGER NOT NULL,
    action     TEXT NOT NULL,
    actor      TEXT NOT NULL,
    at         TEXT NOT NULL
);
