-- Agent-side cache of the org-distributed announcement document (rail
-- R3 of docs/plans/dashboard-announcements-banner-plan-2026-07-31.md
-- §4; paired with org-server migration 022_org_announcements.sql).
--
-- Single-row cache: the newest VERIFIED announcement document plus the
-- TOFU-pinned server signing key, mirroring org_routing_policies
-- (migration 043) exactly — same shape, same pin, same monotonic
-- version rule.
--
-- NODE-LOCAL: this table never enters the org push. It is RECEIVED
-- state — pushing it back would tell the server which of its own
-- announcements a node holds, i.e. a read receipt, and the plan (§6)
-- records acknowledgment wires as a deliberate non-goal because they
-- are telemetry. Pinned in tests/invariant/privacy_test.go.
--
-- body is the plan §1 announcement JSON (one object or an array of
-- them); an EMPTY body is the retraction — a signed, version-bumped
-- document that means "show nothing". The dashboard reads this row,
-- decodes it, and merges it with the compiled-in release rail; it is
-- never interpreted as anything but banner text. The node operator can
-- silence the whole rail with [dashboard].org_announcements = false,
-- and the org admin has no remote toggle for that (same posture as
-- [org_client.share].full_content).
CREATE TABLE IF NOT EXISTS org_announcements (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    version       INTEGER NOT NULL,
    body          TEXT NOT NULL,
    body_hash     TEXT NOT NULL,
    signature     TEXT NOT NULL,
    server_pubkey TEXT NOT NULL,
    received_at   TEXT NOT NULL
);
