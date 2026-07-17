package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SessionLineageChild is a compact descriptor of a session spawned from
// (forked or subagent-of) a parent codex session — surfaced in the
// parent's session-detail "spawned sessions" list. Token/cost rollups are
// deliberately NOT included: the per-session cost is a heavy per-turn CTE
// (handleSessionDetail) and building a fresh aggregate here would violate
// the single-owner rule for no user-visible gain in the child rows.
type SessionLineageChild struct {
	ID           string
	ThreadSource string
	StartedAt    string
}

// SessionLineageView is the codex fork/subagent lineage for one session
// (migration 069): its own markers, whether the fork parent is present in
// this database, and the children forked/spawned from it. All fields carry
// zero values for a non-codex or non-lineage session.
type SessionLineageView struct {
	// ForkedFromID / ParentThreadID / ThreadSource mirror the sessions
	// columns of the same name (empty when unset).
	ForkedFromID   string
	ParentThreadID string
	ThreadSource   string
	// ParentInDB reports whether a session row exists for ForkedFromID.
	// Only queried when ForkedFromID is non-empty; false otherwise.
	ParentInDB bool
	// Children are the sessions whose forked_from_id equals this session
	// id, ordered by start time. Nil when there are none.
	Children []SessionLineageChild
}

// LoadSessionLineage resolves the codex fork/subagent lineage for a single
// session (migration 069). It returns sql.ErrNoRows when the session does
// not exist, mirroring LoadSessionShape. Since sessions.id for codex is the
// codex thread uuid, the parent is the row WHERE id = forked_from_id and the
// children are the rows WHERE forked_from_id = sessionID.
func (s *Store) LoadSessionLineage(ctx context.Context, sessionID string) (SessionLineageView, error) {
	var v SessionLineageView
	if sessionID == "" {
		return v, errors.New("store.LoadSessionLineage: sessionID is required")
	}

	var forked, parentThread, source sql.NullString
	if err := s.db.QueryRowContext(
		ctx,
		`SELECT COALESCE(forked_from_id, ''), COALESCE(parent_thread_id, ''),
		        COALESCE(thread_source, '')
		 FROM sessions WHERE id = ?`, sessionID,
	).Scan(&forked, &parentThread, &source); err != nil {
		return v, err // sql.ErrNoRows propagates to the caller unwrapped.
	}
	v.ForkedFromID = forked.String
	v.ParentThreadID = parentThread.String
	v.ThreadSource = source.String

	// Parent presence is only meaningful when this session was forked or
	// spawned from another thread; skip the lookup otherwise.
	if v.ForkedFromID != "" {
		if err := s.db.QueryRowContext(
			ctx,
			`SELECT EXISTS(SELECT 1 FROM sessions WHERE id = ?)`, v.ForkedFromID,
		).Scan(&v.ParentInDB); err != nil {
			return v, fmt.Errorf("store.LoadSessionLineage: parent presence: %w", err)
		}
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, COALESCE(thread_source, ''), started_at
		 FROM sessions WHERE forked_from_id = ?
		 ORDER BY started_at`, sessionID)
	if err != nil {
		return v, fmt.Errorf("store.LoadSessionLineage: children: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c SessionLineageChild
		if err := rows.Scan(&c.ID, &c.ThreadSource, &c.StartedAt); err != nil {
			return v, fmt.Errorf("store.LoadSessionLineage: scan child: %w", err)
		}
		v.Children = append(v.Children, c)
	}
	if err := rows.Err(); err != nil {
		return v, fmt.Errorf("store.LoadSessionLineage: children rows: %w", err)
	}
	return v, nil
}
