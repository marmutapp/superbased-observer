package identity

import (
	"context"
	"database/sql"
)

// OrgRow is implemented by rows that can carry org attribution. The
// implementations are thin adapters in internal/store wrapping the
// concrete *models row types, so identity never imports internal/models.
type OrgRow interface {
	// SetOrg stamps the org id and user email onto the underlying row.
	SetOrg(orgID, userEmail string)
}

// Stamper carries the agent's enrolled identity and stamps it onto rows
// before insert. The zero value, a nil *Stamper, and a Stamper built from
// an empty (or absent) org_enrolment table are all valid no-ops.
type Stamper struct {
	orgID     string
	userEmail string
}

// NewStamper reads the singleton org_enrolment row (id = 1) and returns a
// Stamper bound to that identity. A solo-local install has no such row —
// and a database migrated below 028 has no such table — both of which are
// reported as a no-op Stamper with a nil error, never a failure: org mode
// being absent is the default, not an error condition. Callers therefore
// never need to special-case the unenrolled path.
func NewStamper(ctx context.Context, db *sql.DB) (*Stamper, error) {
	s := &Stamper{}
	if db == nil {
		return s, nil
	}
	var orgID, userEmail string
	err := db.QueryRowContext(
		ctx,
		`SELECT org_id, user_email FROM org_enrolment WHERE id = 1`,
	).Scan(&orgID, &userEmail)
	if err != nil {
		// sql.ErrNoRows (not enrolled) and "no such table: org_enrolment"
		// (pre-028 schema) both mean "solo-local" — return a no-op stamper
		// rather than blocking ingest. Any other transient read error is
		// treated the same way for safety: stamping is best-effort.
		return s, nil
	}
	s.orgID = orgID
	s.userEmail = userEmail
	return s, nil
}

// OrgID returns the enrolled organisation id, or "" when not enrolled.
func (s *Stamper) OrgID() string {
	if s == nil {
		return ""
	}
	return s.orgID
}

// UserEmail returns the enrolled user email, or "" when not enrolled.
func (s *Stamper) UserEmail() string {
	if s == nil {
		return ""
	}
	return s.userEmail
}

// IsEnrolled reports whether the agent is enrolled in an org.
func (s *Stamper) IsEnrolled() bool {
	return s != nil && s.orgID != ""
}

// Stamp writes the enrolled org id and user email onto row. It is a no-op
// when the Stamper is nil or unenrolled, so the default solo-local call
// sites (a nil *Stamper held by the store) need no guard of their own.
func (s *Stamper) Stamp(row OrgRow) {
	if !s.IsEnrolled() {
		return
	}
	row.SetOrg(s.orgID, s.userEmail)
}
