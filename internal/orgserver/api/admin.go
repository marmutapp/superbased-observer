package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ProvisionUser ensures an org_member exists for email, creating one if
// absent (the server-side equivalent of a SCIM POST /Users, so the
// observer-org `users create` / `invite` CLI commands don't require a
// running server + a raw SCIM curl). Idempotent: an existing email
// returns its user_id with created=false. Shared minting + provisioning
// keep org_members as the single user table.
func ProvisionUser(ctx context.Context, db *sql.DB, email, displayName string) (userID string, created bool, err error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return "", false, errors.New("api.ProvisionUser: empty email")
	}
	s := newStore(db)
	if m, ok, lookErr := s.memberByEmail(ctx, email); lookErr != nil {
		return "", false, lookErr
	} else if ok {
		return m.UserID, false, nil
	}

	id, err := randID()
	if err != nil {
		return "", false, err
	}
	now := s.now().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO org_members (user_id, user_name, email, display_name, active, created_at, updated_at)
		 VALUES (?, ?, ?, NULLIF(?, ''), 1, ?, ?)`,
		id, email, email, displayName, now, now); err != nil {
		return "", false, fmt.Errorf("api.ProvisionUser: insert: %w", err)
	}
	return id, true, nil
}

// EnrolmentTokenInfo is one row of ListEnrolmentTokens. UsedAt is nil for
// a token that hasn't been redeemed yet.
type EnrolmentTokenInfo struct {
	TokenID   string
	UserEmail string
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    *time.Time
}

// Redeemed reports whether the token has been consumed.
func (e EnrolmentTokenInfo) Redeemed() bool { return e.UsedAt != nil }

// Expired reports whether the token's expiry is in the past relative to now.
func (e EnrolmentTokenInfo) Expired(now time.Time) bool { return now.After(e.ExpiresAt) }

// ListEnrolmentTokens returns every enrolment token joined to its member
// email, newest first. Server-side read for `observer-org tokens list`,
// replacing hand-querying the SQLite DB. The token secret is never
// stored (only its argon2id hash) so it cannot be listed — only the
// non-secret token_id, owner, and lifecycle timestamps.
func ListEnrolmentTokens(ctx context.Context, db *sql.DB) ([]EnrolmentTokenInfo, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT et.id, m.email, et.created_at, et.expires_at, et.used_at
		   FROM enrolment_tokens et
		   JOIN org_members m ON m.user_id = et.user_id
		  ORDER BY et.created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("api.ListEnrolmentTokens: %w", err)
	}
	defer rows.Close()

	var out []EnrolmentTokenInfo
	for rows.Next() {
		var (
			info             EnrolmentTokenInfo
			created, expires string
			used             sql.NullString
		)
		if err := rows.Scan(&info.TokenID, &info.UserEmail, &created, &expires, &used); err != nil {
			return nil, fmt.Errorf("api.ListEnrolmentTokens: scan: %w", err)
		}
		info.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		info.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires)
		if used.Valid && used.String != "" {
			if t, perr := time.Parse(time.RFC3339Nano, used.String); perr == nil {
				info.UsedAt = &t
			}
		}
		out = append(out, info)
	}
	return out, rows.Err()
}
