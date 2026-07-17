package m365copilotanalytics

import (
	"context"
	"database/sql"
	"strings"
)

// ResolveUserIDs returns the set of Graph user identities Rail A should poll.
// getAllEnterpriseInteractions is a PER-USER endpoint, so the poller needs an
// explicit user list; this derives it from the org member store (the emails /
// userPrincipalNames the SCIM/SAML enrolment already knows). A real deployment
// filters to M365-Copilot-licensed users via a User.Read.All license lookup
// (LICENSE GATE — an unlicensed user's call fails); absent that lookup this
// returns every member email and the poller tolerates per-user 4xx by skipping.
//
// The returned identities are userPrincipalNames/emails (Graph accepts either the
// Entra object id or the UPN in the {id} path segment). Empty emails are skipped.
func ResolveUserIDs(ctx context.Context, db *sql.DB, orgID string) ([]string, error) {
	q := `SELECT DISTINCT email FROM org_members WHERE email IS NOT NULL AND email != ''`
	args := []any{}
	if orgID != "" {
		q += ` AND org_id = ?`
		args = append(args, orgID)
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return nil, err
		}
		if e := strings.TrimSpace(email); e != "" {
			out = append(out, e)
		}
	}
	return out, rows.Err()
}

// ResolveOrgUserID maps a stored analytics user_key (a Graph identity: an Entra
// object id, a userPrincipalName, or an email) to an org member's user_id via the
// case-insensitive org_members.email join, mirroring
// codexanalytics.ResolveOrgUserID. It returns ok=false for a key that is not an
// email/UPN (e.g. a bare object id with no '@') or that matches no member — the
// caller buckets those rather than dropping them. Non-user actors never resolve.
func ResolveOrgUserID(ctx context.Context, db *sql.DB, actorType, userKey string) (string, bool) {
	if actorType != ActorUser || strings.TrimSpace(userKey) == "" {
		return "", false
	}
	if !strings.Contains(userKey, "@") {
		// A bare Entra object id (no UPN/email) cannot join org_members.email.
		return "", false
	}
	var userID string
	err := db.QueryRowContext(ctx,
		`SELECT user_id FROM org_members WHERE lower(email) = lower(?) LIMIT 1`,
		strings.TrimSpace(userKey)).Scan(&userID)
	if err != nil {
		return "", false
	}
	return userID, true
}
