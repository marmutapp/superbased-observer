package pidbridge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Entry is one bridge row.
type Entry struct {
	PID       int
	SessionID string
	Tool      string
	CWD       string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Store wraps the session_pid_bridge table.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// New returns a Store backed by db. db is expected to already have
// migration 004 applied.
func New(db *sql.DB) *Store {
	return &Store{db: db, now: time.Now}
}

// SetClock overrides time.Now for tests. Not safe for concurrent use with
// Write/Prune; call it once after construction.
func (s *Store) SetClock(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

// Write upserts an Entry. pid is the primary key; on conflict the
// session_id, tool, cwd, and updated_at are refreshed.
func (s *Store) Write(ctx context.Context, e Entry) error {
	if e.PID <= 0 {
		return errors.New("pidbridge.Write: PID must be > 0")
	}
	if e.SessionID == "" {
		return errors.New("pidbridge.Write: SessionID required")
	}
	if e.Tool == "" {
		return errors.New("pidbridge.Write: Tool required")
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	created := now
	if !e.CreatedAt.IsZero() {
		created = e.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO session_pid_bridge (pid, session_id, tool, cwd, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(pid) DO UPDATE SET
		   session_id = excluded.session_id,
		   tool       = excluded.tool,
		   cwd        = COALESCE(NULLIF(excluded.cwd, ''), session_pid_bridge.cwd),
		   updated_at = excluded.updated_at`,
		e.PID, e.SessionID, e.Tool, nullableString(e.CWD), created, now,
	)
	if err != nil {
		return fmt.Errorf("pidbridge.Write: %w", err)
	}
	return nil
}

// Lookup returns the bridge entry for pid, if any. ok=false on a clean
// miss (no row); err is only set on I/O failures.
func (s *Store) Lookup(ctx context.Context, pid int) (Entry, bool, error) {
	if pid <= 0 {
		return Entry{}, false, nil
	}
	var (
		e         Entry
		cwd       sql.NullString
		createdAt string
		updatedAt string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT pid, session_id, tool, cwd, created_at, updated_at
		 FROM session_pid_bridge WHERE pid = ?`, pid,
	).Scan(&e.PID, &e.SessionID, &e.Tool, &cwd, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("pidbridge.Lookup: %w", err)
	}
	if cwd.Valid {
		e.CWD = cwd.String
	}
	e.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	e.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	return e, true, nil
}

// Prune deletes rows whose updated_at is older than now-olderThan. Returns
// the count deleted. A non-positive olderThan is a no-op.
func (s *Store) Prune(ctx context.Context, olderThan time.Duration) (int, error) {
	if olderThan <= 0 {
		return 0, nil
	}
	cutoff := s.now().UTC().Add(-olderThan).Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM session_pid_bridge WHERE updated_at < ?`, cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("pidbridge.Prune: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// List returns every row ordered by updated_at DESC. Intended for doctor
// and tests, not the proxy hot path.
func (s *Store) List(ctx context.Context) ([]Entry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT pid, session_id, tool, cwd, created_at, updated_at
		 FROM session_pid_bridge ORDER BY updated_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("pidbridge.List: %w", err)
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var (
			e         Entry
			cwd       sql.NullString
			createdAt string
			updatedAt string
		)
		if err := rows.Scan(&e.PID, &e.SessionID, &e.Tool, &cwd, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("pidbridge.List: scan: %w", err)
		}
		if cwd.Valid {
			e.CWD = cwd.String
		}
		e.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		e.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Resolver is the proxy-facing contract for converting a client's TCP
// remote address ("127.0.0.1:54321") into a session_id. A clean miss
// returns ("", false, nil). An I/O or parse failure returns a non-nil
// error; callers should treat that as a miss.
type Resolver interface {
	Resolve(ctx context.Context, remoteAddr string) (sessionID string, ok bool, err error)
}

// NopResolver implements [Resolver] and always reports a miss. Useful as
// a zero value when the pid bridge is disabled.
type NopResolver struct{}

// Resolve implements [Resolver].
func (NopResolver) Resolve(context.Context, string) (string, bool, error) {
	return "", false, nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
