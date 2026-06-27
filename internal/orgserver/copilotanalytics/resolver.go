package copilotanalytics

import (
	"context"
	"database/sql"
	"strings"
)

// ResolveOrgUserID maps a stored analytics user_key (a GitHub login) to an org
// member's user_id. Copilot's identity is the GitHub login, NOT an email
// (instance §5.3) — and GitHub emails are frequently private — so the CC/Codex
// one-step email join does not transfer. The resolution is therefore TWO-step:
// login → email (via the caller-supplied map) → org_members email join.
//
// loginToEmail is built server-side by the caller that holds the admin token
// (either a GitHub Users-API lookup or an admin-supplied login↔email map). When a
// login is absent from the map (or the row is org-aggregate/automation), it stays
// non-enrolled coverage — ok=false, the caller buckets it rather than dropping it.
//
// This is consumed by the SIBLING cost rollup / per-developer attribution, NOT by
// the poller. (A future enhancement stores `github_login` as a first-class
// org_members column for a one-step join; until then the map is the bridge.)
func ResolveOrgUserID(ctx context.Context, db *sql.DB, actorType, login string, loginToEmail map[string]string) (string, bool) {
	if actorType != ActorUser || login == "" || login == orgAggregateKey {
		return "", false
	}
	email, ok := loginToEmail[login]
	if !ok || strings.TrimSpace(email) == "" {
		return "", false
	}
	return emailJoin(ctx, db, email)
}

// emailJoin is the shared final step: case-insensitive org_members.email lookup.
func emailJoin(ctx context.Context, db *sql.DB, email string) (string, bool) {
	var userID string
	err := db.QueryRowContext(ctx,
		`SELECT user_id FROM org_members WHERE lower(email) = lower(?) LIMIT 1`,
		strings.TrimSpace(email)).Scan(&userID)
	if err != nil {
		return "", false
	}
	return userID, true
}
