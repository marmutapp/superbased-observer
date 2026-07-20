package store

import (
	"context"
	"fmt"
	"time"
)

// RemoteAuditEvent is one row of the NODE-LOCAL remote-access audit log
// (remote-dashboard-access plan §4.8, migration 063). Metadata only: session
// ids and enums, NEVER secrets (no pairing secret, session cookie, CSRF token,
// or execute-capability token). This is the ONE store seam for remote_audit;
// the table is sentinel-pinned out of the org-push wire
// (tests/invariant/privacy_test.go). It is NOT compliance-immutable — a local
// owner can mutate SQLite; documented as a residual, not over-claimed.
type RemoteAuditEvent struct {
	// TS defaults to now (UTC) when zero.
	TS time.Time
	// Kind is the event type: http_request | session_paired | session_revoked
	// | auth_failed | ws_attach | execute_action.
	Kind string
	// SessionID is the device-session id (not a secret) or "".
	SessionID string
	// Principal is the resolved capability: public | view | execute | anonymous.
	Principal string
	// RemoteAddr is the best-effort peer host.
	RemoteAddr string
	// Route is the matched route pattern / path.
	Route string
	// Decision is allow | deny | ok | fail.
	Decision string
	// Detail is a short, bounded, non-sensitive descriptor.
	Detail string
}

// InsertRemoteAudit appends one remote-access audit event. Best-effort by
// contract: callers on the request hot path should not fail a request because
// the audit write failed (they log and continue), so this returns the error but
// the wiring treats it as non-fatal.
func (s *Store) InsertRemoteAudit(ctx context.Context, ev RemoteAuditEvent) error {
	ts := ev.TS
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO remote_audit (ts, kind, session_id, principal, remote_addr, route, decision, detail)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		ts.UTC().Format(time.RFC3339Nano), ev.Kind, ev.SessionID, ev.Principal,
		ev.RemoteAddr, ev.Route, ev.Decision, ev.Detail)
	if err != nil {
		return fmt.Errorf("store.InsertRemoteAudit: %w", err)
	}
	return nil
}

// RecentRemoteAudit returns the most recent audit events (newest first),
// capped at limit, for `observer remote status`. Metadata only.
func (s *Store) RecentRemoteAudit(ctx context.Context, limit int) ([]RemoteAuditEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts, kind, session_id, principal, remote_addr, route, decision, detail
		 FROM remote_audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store.RecentRemoteAudit: %w", err)
	}
	defer rows.Close()
	var out []RemoteAuditEvent
	for rows.Next() {
		var ev RemoteAuditEvent
		var tsStr string
		if err := rows.Scan(&tsStr, &ev.Kind, &ev.SessionID, &ev.Principal,
			&ev.RemoteAddr, &ev.Route, &ev.Decision, &ev.Detail); err != nil {
			return nil, fmt.Errorf("store.RecentRemoteAudit scan: %w", err)
		}
		ev.TS, _ = time.Parse(time.RFC3339Nano, tsStr)
		out = append(out, ev)
	}
	return out, rows.Err()
}
