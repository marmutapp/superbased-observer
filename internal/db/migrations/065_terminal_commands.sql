-- 065_terminal_commands.sql — command/turn boundary coordinates for a
-- terminal_run (docs/plans/terminal-product-exploitation-plan-2026-07-12.md
-- §7 / F3).
--
-- Each row is ONE observed command/turn boundary on a terminal run: when it
-- started/ended, its exit code, the buffer/recording coordinates later features
-- (F6 cost decorations, F9 replay, F10 command blocks) anchor to, and a
-- PROVENANCE/trust column marking whether the boundary came from the TRUSTED
-- out-of-band launcher channel (`oob`, internal/termoob — unforgeable) or from
-- an UNTRUSTED hint parsed off the PTY byte stream (`hint`, internal/termscan —
-- the child can forge it, §2.1b).
--
-- CONTENT DISCIPLINE (CLAUDE.md "don't store command outputs"): metadata /
-- coordinates only. NO command text and NO output is ever stored. An optional
-- `cmd_hash` is a domain-separated hash for correlation only, never the command
-- itself. The buffer coordinates are populated only once the F2 mirror lands;
-- until then they stay NULL (F2 is out of scope for this phase).
--
-- IDENTITY: rows carry the durable `run_id` FK (never a raw PTY handle). A cost
-- row correlates via run_id → terminal_run_session → session id + turn_seq,
-- never a session_handle directly (§7 / codex P2 #17).
--
-- NODE-LOCAL — this table MUST NOT leave the machine: pinned in
-- tests/invariant/privacy_test.go (forbidden-table sentinel + an end-to-end
-- SelectUnpushedSince assertion) and excluded from internal/store/orgpush.go by
-- construction (orgpush names an explicit table allow-list; it is not in it).
-- No paired orgserver migration exists, by design — same posture as
-- terminal_run / remote_audit.

CREATE TABLE IF NOT EXISTS terminal_commands (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id        TEXT NOT NULL,               -- FK → terminal_run.run_id
    turn_seq      INTEGER NOT NULL,            -- monotonic boundary index within a run
    started_at    TEXT,                        -- RFC3339 UTC; NULL if unknown
    ended_at      TEXT,                        -- RFC3339 UTC; NULL while running
    exit_code     INTEGER,                     -- NULL if unknown/running
    buffer_epoch  INTEGER,                     -- F2 mirror coordinates (NULL until F2)
    buffer_start  INTEGER,
    buffer_end    INTEGER,
    marker_offset INTEGER,
    trust         TEXT NOT NULL,               -- oob (trusted) | hint (untrusted)
    cmd_hash      TEXT,                         -- domain-separated hash; NEVER command text
    UNIQUE(run_id, turn_seq)
);

CREATE INDEX IF NOT EXISTS idx_terminal_commands_run ON terminal_commands(run_id);
