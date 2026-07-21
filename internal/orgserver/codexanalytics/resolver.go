package codexanalytics

import (
	"context"
	"database/sql"
	"strings"
)

// ResolveOrgUserID maps a stored analytics user_key to an org member's user_id.
// The two surfaces differ in how their key reaches an org member's email, so the
// resolution path is surface-specific — but both end at the SAME join key
// (org_members.email, case-insensitive), mirroring ccanalytics.ResolveOrgUserID.
//
// This is consumed by the Phase-3 spendCTE merge (gated on a live cross-check),
// not by the poller. It returns ok=false for any actor that cannot be resolved
// to an org member (automation, workspace-aggregate, unmatched) — the caller
// buckets those rather than dropping them.
//
//   - SurfaceChatGPTEnterprise: user_key is an email "where the workspace
//     permits" → one-step case-insensitive email join. A workspace user id (no
//     '@') cannot be joined → ok=false.
//   - SurfaceOpenAIOrg: user_key is an OpenAI user_id (e.g. "user_…"), NOT an
//     email → it needs a TWO-step resolve (Admin Users API user_id→email, then
//     the email join). The Users-API map is passed in by the caller (the poller
//     side that holds the admin key); this function does the email join.
func ResolveOrgUserID(ctx context.Context, db *sql.DB, surface Surface, actorType, userKey string, userIDToEmail map[string]string) (string, bool) {
	if actorType != ActorUser || userKey == "" {
		return "", false
	}
	email := userKey
	if surface == SurfaceOpenAIOrg {
		// OpenAI-org keys are user_ids; resolve to email via the caller-supplied
		// Admin Users map. Absent a mapping (or a non-user_id key), unresolved.
		resolved, ok := userIDToEmail[userKey]
		if !ok || resolved == "" {
			return "", false
		}
		email = resolved
	} else if !strings.Contains(email, "@") {
		// ChatGPT surface fell back to a workspace user id (email withheld).
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
