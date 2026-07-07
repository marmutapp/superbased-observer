-- 057_handoffs_short_id.sql — persist the handoff short-id so the
-- best-effort target-session linker (plan §10) no longer depends on
-- recovering it by parsing the delivered doc's `HANDOFF-<shortid>.md`
-- basename.
--
-- Before this column the short-id was recoverable ONLY from delivery_ref's
-- file name, so a handoff written to a custom `--out` path had no
-- recoverable id and could never be linked. Storing it fixes that: the
-- linker prefers this column and keeps the filename fallback for pre-057
-- rows.
--
-- The short-id is a random opaque token (crypto/rand → hex), not
-- conversation content — same privacy class as a hash. handoffs stays
-- NODE-LOCAL: pinned in tests/invariant/privacy_test.go and excluded from
-- internal/store/orgpush.go by construction.

ALTER TABLE handoffs ADD COLUMN short_id TEXT;
