-- 082_org_enrolment_grant.sql — admin-controlled Plane B, the ENROLMENT GRANT
-- (docs/plans/admin-controlled-plane-b-spec-2026-08-15.md §2.4, Phase 1a).
--
-- The grant is the CONSENT BOUNDARY of the whole feature: a durable,
-- locally-readable, locally-deletable record of the bounded authority this
-- machine handed to an organization at enrolment. A node with no row here
-- ignores the node.governance family entirely, which is what makes "nothing
-- changes for solo users" a structural property rather than a promise.
--
-- One row per enrolment identity (org_key), scoped by the SAME generation
-- fence the P0-5 policy-resource state uses (migration 081), so a re-enrol or
-- unenrol invalidates it through the existing mechanism instead of a second
-- one. `observer unenroll` DELETEs the row (unlike org_enrolment_generation,
-- which is never deleted): revocation must leave nothing behind that could
-- govern the machine.
--
-- authority_json is a canonical JSON array of tokens from the CLOSED
-- vocabulary in internal/govern (dashboard.visibility, settings.pin,
-- capture.raise, feature.lock). A token outside it is ignored and reported,
-- never an enrolment failure — an older agent must still be able to enrol
-- against a newer server.
--
-- key_pin_sha256 is the org policy signing key hash bound at grant time.
-- internal/govern.Resolve compares it against the LIVE TOFU pin on every
-- resolve (adversarial review A2): a grant signed under a key the node no
-- longer pins is a substitution attempt, not authority.
--
-- signature is the base64url Ed25519 signature over the grant's canonical
-- signing message (internal/orgcontract.EnrolmentGrantSigningMessage), stored
-- so the grant is EVIDENCE and not merely a local setting: an operator or an
-- auditor can verify what the organization actually asked for. Phase 1a
-- verifies it at WRITE time against the key pinned earlier in the same
-- enrolment; it is deliberately not re-verified per resolve.
--
-- NOTE the spec's target_group column is deliberately ABSENT (adversarial
-- review A17/D-A): the grant records AUTHORITY, never AUDIENCE. Group
-- targeting is resolved server-side from the subject's authoritative
-- attributes (P0-10), and a grant that also carried a group would
-- permanently reject a correctly-retargeted body after any reassignment.
--
-- NODE-LOCAL control-plane state: pinned out of the org-push wire by
-- tests/invariant/privacy_test.go's forbiddenCacheTables. internal/store/
-- orggrant.go is its ONE owner.

CREATE TABLE IF NOT EXISTS org_enrolment_grant (
    org_key        TEXT PRIMARY KEY,
    generation     INTEGER NOT NULL,
    org_id         TEXT NOT NULL DEFAULT '',
    org_name       TEXT NOT NULL DEFAULT '',
    org_server_url TEXT NOT NULL DEFAULT '',
    key_pin_sha256 TEXT NOT NULL DEFAULT '',
    authority_json TEXT NOT NULL DEFAULT '[]',
    consent_mode   TEXT NOT NULL DEFAULT '',
    consent_actor  TEXT NOT NULL DEFAULT '',
    granted_at     TEXT NOT NULL DEFAULT '',
    expires_at     TEXT NOT NULL DEFAULT '',
    signature      TEXT NOT NULL DEFAULT '',
    receipt_hash   TEXT NOT NULL DEFAULT ''
);
