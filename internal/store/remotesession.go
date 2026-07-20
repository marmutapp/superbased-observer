package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/marmutapp/superbased-observer/internal/remoteauth"
)

// RemoteSessionPersister implements remoteauth.SessionPersister over the
// NODE-LOCAL remote_sessions + remote_session_state tables (migration 066), so
// a paired device survives a daemon restart without weakening the
// revoke/rotate/disable invariant. It stores only the sha256 HASH of each
// bearer (never the raw token). This is the ONE store seam for those tables;
// they are pinned out of the org-push wire in tests/invariant/privacy_test.go.
type RemoteSessionPersister struct{ db *sql.DB }

// NewRemoteSessionPersister builds a persister over the daemon DB. The returned
// value satisfies remoteauth.SessionPersister (injected via
// SessionParams.Persister).
func NewRemoteSessionPersister(db *sql.DB) *RemoteSessionPersister {
	return &RemoteSessionPersister{db: db}
}

// LoadAll returns the durable generation + every persisted session in ONE
// read transaction, so the generation and the rows are a single consistent
// snapshot even if a separate CLI process advances the fence concurrently (an
// unfenced pair of reads could otherwise mix a pre-advance gen with post-advance
// rows). A missing state row defaults the generation to 1 (fresh install).
// Times are stored as unix nanoseconds UTC.
func (p *RemoteSessionPersister) LoadAll(ctx context.Context) (uint64, []remoteauth.PersistedSession, error) {
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, nil, fmt.Errorf("store.RemoteSessionPersister.LoadAll begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var gen int64 = 1
	if err := tx.QueryRowContext(ctx, `SELECT gen FROM remote_session_state WHERE id = 1`).Scan(&gen); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, nil, fmt.Errorf("store.RemoteSessionPersister.LoadAll state: %w", err)
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id_hash, gen, created_at, last_seen FROM remote_sessions`)
	if err != nil {
		return 0, nil, fmt.Errorf("store.RemoteSessionPersister.LoadAll rows: %w", err)
	}
	defer rows.Close()
	var out []remoteauth.PersistedSession
	for rows.Next() {
		var idHash string
		var rowGen, createdNs, lastNs int64
		if err := rows.Scan(&idHash, &rowGen, &createdNs, &lastNs); err != nil {
			return 0, nil, fmt.Errorf("store.RemoteSessionPersister.LoadAll scan: %w", err)
		}
		out = append(out, remoteauth.PersistedSession{
			IDHash:    idHash,
			Gen:       uint64(rowGen),
			CreatedAt: time.Unix(0, createdNs).UTC(),
			LastSeen:  time.Unix(0, lastNs).UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("store.RemoteSessionPersister.LoadAll iterate: %w", err)
	}
	return uint64(gen), out, nil
}

// Save inserts/updates one session row (called durable-first by Create).
func (p *RemoteSessionPersister) Save(ctx context.Context, s remoteauth.PersistedSession) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO remote_sessions (id_hash, gen, created_at, last_seen) VALUES (?, ?, ?, ?)
		 ON CONFLICT(id_hash) DO UPDATE SET gen=excluded.gen, created_at=excluded.created_at, last_seen=excluded.last_seen`,
		s.IDHash, int64(s.Gen), s.CreatedAt.UTC().UnixNano(), s.LastSeen.UTC().UnixNano())
	if err != nil {
		return fmt.Errorf("store.RemoteSessionPersister.Save: %w", err)
	}
	return nil
}

// Touch is UPDATE-ONLY and generation-fenced — it never INSERTs, so a late
// touch cannot resurrect a deleted row.
func (p *RemoteSessionPersister) Touch(ctx context.Context, idHash string, gen uint64, lastSeen time.Time) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE remote_sessions SET last_seen=? WHERE id_hash=? AND gen=?`,
		lastSeen.UTC().UnixNano(), idHash, int64(gen))
	if err != nil {
		return fmt.Errorf("store.RemoteSessionPersister.Touch: %w", err)
	}
	return nil
}

// Delete removes one session row by hash.
func (p *RemoteSessionPersister) Delete(ctx context.Context, idHash string) error {
	_, err := p.db.ExecContext(ctx, `DELETE FROM remote_sessions WHERE id_hash=?`, idHash)
	if err != nil {
		return fmt.Errorf("store.RemoteSessionPersister.Delete: %w", err)
	}
	return nil
}

// Reset atomically clears every session row and advances the durable generation
// to at least gen (monotonic — MAX guards against a regression when a separate
// process advanced it further). Called durable-first by Rotate.
func (p *RemoteSessionPersister) Reset(ctx context.Context, gen uint64) error {
	return withImmediateTx(ctx, p.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM remote_sessions`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO remote_session_state (id, gen) VALUES (1, ?)
			 ON CONFLICT(id) DO UPDATE SET gen = MAX(remote_session_state.gen, excluded.gen)`,
			int64(gen))
		return err
	})
}

// AdvanceRemoteSessionGeneration is the CLI durable fence for `observer remote
// disable | rotate | enable` (persist-remote-sessions plan §5). Those verbs run
// in a SEPARATE process from the daemon, so they cannot use the live in-memory
// store; they must advance the shared durable generation + clear every
// persisted row in one transaction so that, after the next restart, no
// previously-paired cookie can re-validate. Returns the new generation.
// Monotonic (+1). Metadata-only; touches no bearer.
func (s *Store) AdvanceRemoteSessionGeneration(ctx context.Context) (uint64, error) {
	var newGen int64
	err := withImmediateTx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM remote_sessions`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO remote_session_state (id, gen) VALUES (1, 1)
			 ON CONFLICT(id) DO UPDATE SET gen = remote_session_state.gen + 1`); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT gen FROM remote_session_state WHERE id = 1`).Scan(&newGen)
	})
	if err != nil {
		return 0, fmt.Errorf("store.AdvanceRemoteSessionGeneration: %w", err)
	}
	return uint64(newGen), nil
}

// withImmediateTx runs fn inside a transaction, rolling back on error. The
// daemon DB is WAL with a 30s busy_timeout + immediate txlock, so a short
// cross-process write from the CLI safely waits out a concurrent daemon writer.
func withImmediateTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
