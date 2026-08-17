-- 083_org_enrolment_grant_renewal.sql — admin-controlled Plane B Phase 1b,
-- GRANT RENEWAL (docs/plans/admin-controlled-plane-b-phase-1b-mini-spec-2026-08-15.md
-- §4.4).
--
-- Renewal is a LOCAL write. Nothing about it travels: no wire field, no new
-- endpoint, and therefore NO SERVER MIGRATION PAIR. That is deliberate and it
-- is the same "exception that proves the rule" shape as server migration 011
-- (an index over data the agent already ships) and server 056. The
-- paired-migration rule was applied here, not forgotten.
--
-- WHY A SECOND COLUMN RATHER THAN MUTATING expires_at:
-- renewal must not destroy the EVIDENCE property. Amendment A1 established
-- that the grant's signature is evidence / non-repudiation — an auditor must
-- be able to verify what the organization actually asked for. Renewing in
-- place would leave a row whose expires_at no longer matches the signed
-- message, i.e. a signature that no longer verifies against its own row.
--
--   signed_expires_at — the SIGNED value, never modified after the grant is
--                       written. It is what bounds renewal: the derived TTL
--                       is (signed_expires_at - granted_at), so a renewal can
--                       never extend the grant beyond the window the
--                       organization actually signed for.
--   expires_at        — becomes the WORKING clock, moved forward (only ever
--                       forward) by store.RenewEnrolmentGrant.
--   last_renewed_at   — when that last happened, for `observer org grant show`.
--
-- The backfill only rescues rows that already exist. Rows written AFTER this
-- migration get signed_expires_at from store.WriteEnrolmentGrant's own column
-- list (review M1): without that change, every node enrolled after 1b ships
-- would have an empty signed_expires_at, a NEGATIVE derived TTL, and renewal
-- would silently never fire.
--
-- NODE-LOCAL control-plane state. org_enrolment_grant is already in
-- tests/invariant/privacy_test.go's forbiddenCacheTables (added by 082), and
-- the sentinel forbids the TABLE NAME, so it already covers these columns —
-- no privacy-test edit is needed or wanted.
--
-- Additive with defaults, and LoadEnrolmentGrant selects explicit columns, so
-- a Phase-1a binary on an 083 database works unchanged. No down-migration is
-- needed or provided.

ALTER TABLE org_enrolment_grant ADD COLUMN signed_expires_at TEXT NOT NULL DEFAULT '';
ALTER TABLE org_enrolment_grant ADD COLUMN last_renewed_at   TEXT NOT NULL DEFAULT '';

UPDATE org_enrolment_grant SET signed_expires_at = expires_at WHERE signed_expires_at = '';
