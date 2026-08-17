-- 081_org_policy_resource.sql — Plane-A P0-5 unified policy resource v1,
-- agent-side scoped persistence (docs/plans/plane-a-p0-5-unified-policy-resource-v1-plan.md
-- §6.2, §6.9, §6.10; docs/plane-a/unified-policy-resource.md §6-§7).
--
-- org_enrolment_generation is the durable CROSS-PROCESS fence (plan §6.9):
-- one row per enrolment identity (org_key), a monotonically increasing
-- generation, and a tombstone bit set on unenrolment. The row is NEVER
-- deleted — unlike org_enrolment (which IS deleted on unenrol), this table
-- is exactly what lets a separate `observer unenroll` (or re-enrol)
-- invocation be observed by a running daemon without waiting for its next
-- poll: the daemon's live orgLayer carries (OrgKey, Generation) and is
-- cleared the moment it disagrees with this row (§6.9/§6.10).
--
-- org_policy_resource_state is the per-(org_key, family) durable replay
-- floor + last-verified-envelope identity, scoped by generation.
-- floor_version is the monotonic anti-replay fact; last_version/body_hash/
-- msg_digest describe the last envelope this process durably verified and
-- cached. msg_digest is the FULL PolicyResourceSigningMessage digest (not
-- BodyHash alone) — the equal-floor replay rule (plan §6.3/§6.5) compares
-- against it, so a same-version republish that only changes capabilities or
-- selectors is caught too, not just a changed Body.
--
-- org_key = hex(SHA256(normalizedOrgURL + "\x00" + OrgID)) (plan §6.2) — an
-- enrolment identity, not merely the server URL, since two organisations at
-- one control-plane URL must never share replay floors, cached envelopes,
-- or ETags.
--
-- Both tables are NODE-LOCAL control-plane state — see
-- tests/invariant/privacy_test.go's forbiddenCacheTables. Neither is ever
-- read by internal/store/orgpush.go; internal/store/policyresource.go is
-- their one owner.

CREATE TABLE IF NOT EXISTS org_enrolment_generation (
    org_key    TEXT PRIMARY KEY,
    generation INTEGER NOT NULL,
    tombstoned INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS org_policy_resource_state (
    org_key       TEXT NOT NULL,
    family        TEXT NOT NULL,
    generation    INTEGER NOT NULL,
    floor_version INTEGER NOT NULL DEFAULT 0,
    last_version  INTEGER NOT NULL DEFAULT 0,
    body_hash     TEXT NOT NULL DEFAULT '',
    msg_digest    TEXT NOT NULL DEFAULT '',
    updated_at    TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (org_key, family)
);
