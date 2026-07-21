-- Scheduled report digests (gap-register G13) send-once de-dup marker.
-- SERVER-LOCAL bookkeeping only — never on the agent wire (nothing about it
-- enters the push envelope). The digest scheduler records the Period.Key of the
-- most-recently-sent digest so a server restart (or an extra tick within the
-- same period) never re-sends. Keyed by `kind` so future digest variants get
-- independent markers; today there is one row, kind='org'.
CREATE TABLE IF NOT EXISTS digest_state (
    kind        TEXT PRIMARY KEY,
    last_period TEXT NOT NULL,
    sent_at     TEXT NOT NULL
);
