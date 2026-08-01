-- 075_session_tags.sql — session classification: tags, favorites, notes
-- (docs/plans/session-classification-tags-plan-2026-07-31.md §2).
--
-- Three primitives, two tables. session_tags holds the free-form, multi-per-
-- session, user-defined vocabulary (normalized on write: trimmed, lowercased,
-- spaces → '-', charset [a-z0-9._-], ≤ 40 chars, ≤ 16 tags per session).
-- session_annotations holds the orthogonal per-session bookmark (favorite) and
-- the optional ≤ 500-char note explaining WHY a session was starred/tagged.
--
-- ONE OWNER: internal/store/sessiontags.go is the only writer/reader seam
-- (CLAUDE.md module-boundary rule #4). No handler, adapter, or CLI touches
-- these tables directly.
--
-- No FK to sessions, matching house style: a session row can legitimately
-- arrive AFTER the tag (backfill re-parses, cross-OS hook ordering), and a
-- retention sweep that prunes a session must not silently cascade a user's
-- own classification away.
--
-- NODE-LOCAL: never pushed. Tag names and notes are the same privacy class as
-- sessions.git_branch (gated off the org wire 2026-07-02, security review M2)
-- — they encode client names, codenames and ticket ids. Neither table may
-- appear in internal/store/orgpush.go::SelectUnpushedSince under ANY share
-- mode, including admin_managed; both names are pinned in
-- tests/invariant/privacy_test.go's forbiddenCacheTables sentinel and
-- session_tags additionally carries an end-to-end pinned-out-of-push test.
-- No paired orgserver migration exists, by design.

CREATE TABLE IF NOT EXISTS session_tags (
    session_id TEXT NOT NULL,                          -- sessions.id (no FK, see above)
    tag        TEXT NOT NULL,                          -- normalized tag
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (session_id, tag)
);

-- Vocabulary rollups + the tag= list filter both seek by tag.
CREATE INDEX IF NOT EXISTS idx_session_tags_tag ON session_tags(tag);

CREATE TABLE IF NOT EXISTS session_annotations (
    session_id TEXT PRIMARY KEY,                       -- sessions.id (no FK, see above)
    favorite   INTEGER NOT NULL DEFAULT 0,             -- 0/1 bookmark star
    note       TEXT NOT NULL DEFAULT '',               -- ≤ 500 chars, '' = none
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
