package scim

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	scimlib "github.com/elimity-com/scim"
	"github.com/elimity-com/scim/optional"
)

// Store is the SQLite-backed SCIM storage shared by the User and Group
// handlers. now is injectable for deterministic tests.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// NewStore constructs a Store over the given server DB handle.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db, now: func() time.Time { return time.Now().UTC() }}
}

// userRow mirrors a row of org_members.
type userRow struct {
	UserID      string
	ExternalID  string
	UserName    string
	Email       string
	DisplayName string
	Active      bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// teamRow mirrors a row of org_teams plus its members.
type teamRow struct {
	TeamID      string
	ExternalID  string
	DisplayName string
	Members     []string // user ids
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// isUniqueViolation reports whether err is a SQLite UNIQUE constraint failure.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// ---- User row ↔ SCIM resource ---------------------------------------------

func (s *Store) userResource(u userRow) scimlib.Resource {
	attrs := scimlib.ResourceAttributes{
		"userName": u.UserName,
		"active":   u.Active,
	}
	if u.DisplayName != "" {
		attrs["displayName"] = u.DisplayName
	}
	if u.Email != "" {
		attrs["emails"] = []interface{}{
			map[string]interface{}{"value": u.Email, "primary": true},
		}
	}
	created := u.CreatedAt
	modified := u.UpdatedAt
	res := scimlib.Resource{
		ID:         u.UserID,
		Attributes: attrs,
		Meta:       scimlib.Meta{Created: &created, LastModified: &modified, Version: etag(u.UpdatedAt)},
	}
	if u.ExternalID != "" {
		res.ExternalID = optional.NewString(u.ExternalID)
	}
	return res
}

func (s *Store) teamResource(t teamRow) scimlib.Resource {
	members := make([]interface{}, 0, len(t.Members))
	for _, uid := range t.Members {
		members = append(members, map[string]interface{}{"value": uid})
	}
	attrs := scimlib.ResourceAttributes{
		"displayName": t.DisplayName,
		"members":     members,
	}
	created := t.CreatedAt
	modified := t.UpdatedAt
	res := scimlib.Resource{
		ID:         t.TeamID,
		Attributes: attrs,
		Meta:       scimlib.Meta{Created: &created, LastModified: &modified, Version: etag(t.UpdatedAt)},
	}
	if t.ExternalID != "" {
		res.ExternalID = optional.NewString(t.ExternalID)
	}
	return res
}

// etag derives a weak version tag from the last-modified time.
func etag(t time.Time) string {
	return `W/"` + t.UTC().Format("20060102150405.000000000") + `"`
}

// ---- User DB operations ----------------------------------------------------

func (s *Store) insertUser(ctx context.Context, u userRow) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO org_members (user_id, external_id, user_name, email, display_name, active, created_at, updated_at)
		 VALUES (?, NULLIF(?, ''), ?, ?, NULLIF(?, ''), ?, ?, ?)`,
		u.UserID, u.ExternalID, u.UserName, u.Email, u.DisplayName, boolToInt(u.Active),
		u.CreatedAt.Format(time.RFC3339Nano), u.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

func (s *Store) getUser(ctx context.Context, id string) (userRow, bool, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		`SELECT user_id, COALESCE(external_id,''), user_name, email, COALESCE(display_name,''), active, created_at, updated_at
		 FROM org_members WHERE user_id = ?`, id))
}

func (s *Store) listUsers(ctx context.Context) ([]userRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT user_id, COALESCE(external_id,''), user_name, email, COALESCE(display_name,''), active, created_at, updated_at
		 FROM org_members ORDER BY created_at, user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []userRow
	for rows.Next() {
		u, _, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) replaceUser(ctx context.Context, u userRow) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE org_members
		 SET external_id = NULLIF(?, ''), user_name = ?, email = ?, display_name = NULLIF(?, ''), active = ?, updated_at = ?
		 WHERE user_id = ?`,
		u.ExternalID, u.UserName, u.Email, u.DisplayName, boolToInt(u.Active),
		u.UpdatedAt.Format(time.RFC3339Nano), u.UserID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) deleteUser(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM org_members WHERE user_id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanUser(sc rowScanner) (userRow, bool, error) {
	var u userRow
	var active int
	var created, updated string
	err := sc.Scan(&u.UserID, &u.ExternalID, &u.UserName, &u.Email, &u.DisplayName, &active, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return userRow{}, false, nil
	}
	if err != nil {
		return userRow{}, false, err
	}
	u.Active = active != 0
	u.CreatedAt = parseTime(created)
	u.UpdatedAt = parseTime(updated)
	return u, true, nil
}

// ---- Team DB operations ----------------------------------------------------

func (s *Store) insertTeam(ctx context.Context, t teamRow) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO org_teams (team_id, external_id, display_name, created_at, updated_at)
		 VALUES (?, NULLIF(?, ''), ?, ?, ?)`,
		t.TeamID, t.ExternalID, t.DisplayName,
		t.CreatedAt.Format(time.RFC3339Nano), t.UpdatedAt.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if err := replaceMembersTx(ctx, tx, t.TeamID, t.Members); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) getTeam(ctx context.Context, id string) (teamRow, bool, error) {
	var t teamRow
	var created, updated string
	err := s.db.QueryRowContext(ctx,
		`SELECT team_id, COALESCE(external_id,''), display_name, created_at, updated_at
		 FROM org_teams WHERE team_id = ?`, id).
		Scan(&t.TeamID, &t.ExternalID, &t.DisplayName, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return teamRow{}, false, nil
	}
	if err != nil {
		return teamRow{}, false, err
	}
	t.CreatedAt = parseTime(created)
	t.UpdatedAt = parseTime(updated)
	members, err := s.teamMembers(ctx, id)
	if err != nil {
		return teamRow{}, false, err
	}
	t.Members = members
	return t, true, nil
}

func (s *Store) teamMembers(ctx context.Context, teamID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT user_id FROM org_team_members WHERE team_id = ? ORDER BY user_id`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		out = append(out, uid)
	}
	return out, rows.Err()
}

func (s *Store) listTeams(ctx context.Context) ([]teamRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT team_id, COALESCE(external_id,''), display_name, created_at, updated_at
		 FROM org_teams ORDER BY created_at, team_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var teams []teamRow
	for rows.Next() {
		var t teamRow
		var created, updated string
		if err := rows.Scan(&t.TeamID, &t.ExternalID, &t.DisplayName, &created, &updated); err != nil {
			return nil, err
		}
		t.CreatedAt = parseTime(created)
		t.UpdatedAt = parseTime(updated)
		teams = append(teams, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range teams {
		m, err := s.teamMembers(ctx, teams[i].TeamID)
		if err != nil {
			return nil, err
		}
		teams[i].Members = m
	}
	return teams, nil
}

func (s *Store) replaceTeam(ctx context.Context, t teamRow) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`UPDATE org_teams SET external_id = NULLIF(?, ''), display_name = ?, updated_at = ? WHERE team_id = ?`,
		t.ExternalID, t.DisplayName, t.UpdatedAt.Format(time.RFC3339Nano), t.TeamID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if err := replaceMembersTx(ctx, tx, t.TeamID, t.Members); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) deleteTeam(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM org_teams WHERE team_id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// addMembers / removeMembers maintain org_team_members for Group PATCH. Add is
// idempotent (INSERT OR IGNORE); touchTeam bumps updated_at.
func (s *Store) addMembers(ctx context.Context, teamID string, userIDs []string) error {
	for _, uid := range userIDs {
		if _, err := s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO org_team_members (team_id, user_id, role) VALUES (?, ?, 'member')`,
			teamID, uid); err != nil {
			return err
		}
	}
	return s.touchTeam(ctx, teamID)
}

func (s *Store) removeMembers(ctx context.Context, teamID string, userIDs []string) error {
	for _, uid := range userIDs {
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM org_team_members WHERE team_id = ? AND user_id = ?`, teamID, uid); err != nil {
			return err
		}
	}
	return s.touchTeam(ctx, teamID)
}

func (s *Store) touchTeam(ctx context.Context, teamID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE org_teams SET updated_at = ? WHERE team_id = ?`,
		s.now().Format(time.RFC3339Nano), teamID)
	return err
}

func replaceMembersTx(ctx context.Context, tx *sql.Tx, teamID string, userIDs []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM org_team_members WHERE team_id = ?`, teamID); err != nil {
		return err
	}
	for _, uid := range userIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO org_team_members (team_id, user_id, role) VALUES (?, ?, 'member')`,
			teamID, uid); err != nil {
			return err
		}
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func parseTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}
