package rollup

import (
	"context"
	"database/sql"
	"fmt"
)

// ListActiveMembers returns the active SCIM-provisioned org members, sorted
// case-insensitively by email. It powers the admin Invite dropdown
// (GET /api/org/members) so an admin can pick a target user_id instead of
// pasting a UUID.
func ListActiveMembers(ctx context.Context, db *sql.DB) (MembersResult, error) {
	res := MembersResult{Members: []Member{}}
	rows, err := db.QueryContext(ctx, `
SELECT user_id, user_name, email, COALESCE(display_name, '')
  FROM org_members
 WHERE active = 1
 ORDER BY LOWER(email)`)
	if err != nil {
		return MembersResult{}, fmt.Errorf("rollup.ListActiveMembers: query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.UserName, &m.Email, &m.DisplayName); err != nil {
			return MembersResult{}, fmt.Errorf("rollup.ListActiveMembers: scan: %w", err)
		}
		res.Members = append(res.Members, m)
	}
	if err := rows.Err(); err != nil {
		return MembersResult{}, fmt.Errorf("rollup.ListActiveMembers: rows: %w", err)
	}
	return res, nil
}
