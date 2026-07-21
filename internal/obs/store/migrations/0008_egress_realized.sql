-- obs subsystem schema v8 — Plane-A egress REALIZED-outcome annotation (G22
-- WAVE 2 / design §7 "the outcome the proxy actually realized"). Slot 0008
-- verified: obs migrations end at 0007_egress.sql.
--
-- Wave 1 recorded `applied`/`fail_closed` as INTENT at decision time (the proxy
-- had no callback yet). Wave 2 adds a proxy→obs realized-outcome callback
-- (routed through the cmd/observer/obs_wire.go seam — internal/proxy never
-- imports internal/obs) that UPDATES `applied`/`fail_closed` in place AFTER the
-- forward, plus these two annotation columns:
--
--   realized_at      — RFC3339Nano stamp of when the proxy reported the outcome
--   realized_outcome — a closed label: applied | fail_closed | fallback_open |
--                      upstream_error | splice_failed | breaker_open (etc.)
--
-- Chain note: `applied`, `fail_closed`, `realized_at`, and `realized_outcome`
-- are MUTABLE realized annotations and are DELIBERATELY EXCLUDED from the
-- SHA-256 hash-chain preimage (see internal/obs/store/egress.go
-- canonicalBytes). The chain remains tamper-evident over the immutable DECISION
-- (rule, policy, action, target, verdict, message_hash, must_use_target,
-- switch_held); the realized outcome is a linked, once-updated status that a
-- post-hoc in-place UPDATE must not invalidate. This is the standard
-- audit-decision-immutable / outcome-mutable split.
--
-- Privacy: both columns are content-free operator/runtime labels. Same
-- NODE-LOCAL posture as the rest of obs_egress_decisions — never on any org
-- wire (pinned by tests/invariant/privacy_test.go).

ALTER TABLE obs_egress_decisions ADD COLUMN realized_at      TEXT;
ALTER TABLE obs_egress_decisions ADD COLUMN realized_outcome TEXT;
