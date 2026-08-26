package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// Enrolment is the agent's singleton org_enrolment row (id = 1). It records
// which org this agent enrolled in and under which identity; the bearer and
// signing key themselves live in the OS keychain, not here — bearer_key_id is
// only the keychain service handle.
type Enrolment struct {
	OrgID        string
	OrgName      string
	OrgServerURL string
	UserID       string // SCIM user id
	UserEmail    string
	EnrolledAt   string // RFC3339
	BearerKeyID  string // keychain service handle (not the secret itself)
	// Tenancy is the enrolment class: "individual" (default/zero, BYO —
	// "Never server-forced" holds absolutely) or "managed" (org-provisioned,
	// opted into comprehensive admin control at enrolment). It is written only
	// here from the enrol response and read everywhere else; the empty string
	// is treated as "individual".
	Tenancy string
}

// IsManaged reports whether this enrolment opted into Enterprise-Managed
// Tenancy. Nil-safe: a nil Enrolment (not enrolled) is never managed. The
// canonical class strings live in internal/orgcontract (the wire package both
// the node and the org server share) so the two sides cannot drift.
func (e *Enrolment) IsManaged() bool {
	return e != nil && e.Tenancy == orgcontract.TenancyManaged
}

// WriteEnrolment upserts the singleton org_enrolment row. EnrolledAt defaults
// to now (UTC, RFC3339) when empty.
func (s *Store) WriteEnrolment(ctx context.Context, e Enrolment) error {
	if e.EnrolledAt == "" {
		e.EnrolledAt = time.Now().UTC().Format(time.RFC3339)
	}
	if e.Tenancy == "" {
		e.Tenancy = orgcontract.TenancyIndividual
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO org_enrolment
		   (id, org_id, org_name, org_server_url, user_id, user_email, enrolled_at, bearer_key_id, tenancy)
		 VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   org_id         = excluded.org_id,
		   org_name       = excluded.org_name,
		   org_server_url = excluded.org_server_url,
		   user_id        = excluded.user_id,
		   user_email     = excluded.user_email,
		   enrolled_at    = excluded.enrolled_at,
		   bearer_key_id  = excluded.bearer_key_id,
		   tenancy        = excluded.tenancy`,
		e.OrgID, e.OrgName, e.OrgServerURL, e.UserID, e.UserEmail, e.EnrolledAt, e.BearerKeyID, e.Tenancy)
	if err != nil {
		return fmt.Errorf("store.WriteEnrolment: %w", err)
	}
	return nil
}

// LoadEnrolment returns the singleton org_enrolment row, or (nil, nil) when the
// agent is not enrolled (no row, or a pre-028 schema with no table). The
// not-enrolled path is never an error — org mode being absent is the default.
func (s *Store) LoadEnrolment(ctx context.Context) (*Enrolment, error) {
	var e Enrolment
	err := s.db.QueryRowContext(ctx,
		`SELECT org_id, org_name, org_server_url, user_id, user_email, enrolled_at, bearer_key_id, tenancy
		   FROM org_enrolment WHERE id = 1`).
		Scan(&e.OrgID, &e.OrgName, &e.OrgServerURL, &e.UserID, &e.UserEmail, &e.EnrolledAt, &e.BearerKeyID, &e.Tenancy)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store.LoadEnrolment: %w", err)
	}
	if e.Tenancy == "" {
		e.Tenancy = orgcontract.TenancyIndividual
	}
	return &e, nil
}

// DeleteEnrolment removes the org_enrolment row (used by `observer unenroll`)
// and every piece of node-local state that belongs to the org being left:
//
//   - the stored last-push payload (the transparency artifact is no longer
//     relevant once unenrolled);
//   - the org_announcements cache (rail R3) — otherwise the departed org's
//     banner keeps rendering on the dashboard after unenrolment, and its
//     TOFU-pinned key outlives the enrolment that established it, so
//     re-enrolling into a DIFFERENT org is refused as a "key CHANGED"
//     attack by the very rail that org would publish on;
//   - the org_routing_policies cache (§R19.1) — same two reasons: leaving
//     an org must drop its policy layer (the CLI already does exactly this
//     for the guard bundle file), and its pin must not poison re-enrolment.
//     Both pins are ONE org identity (orgclient/orgpin.go), so clearing one
//     without the other would leave the cross-rail check comparing against
//     a dead org's key.
//
// Absence is not an error. The push cursor in schema_meta is intentionally
// left intact: a later re-enrol reseeds it from CurrentMaxIDs, so activity
// in the unenrolled gap is never retroactively shared.
func (s *Store) DeleteEnrolment(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM org_enrolment WHERE id = 1`); err != nil {
		return fmt.Errorf("store.DeleteEnrolment: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM schema_meta WHERE key = ?`, lastPushPayloadKey); err != nil {
		return fmt.Errorf("store.DeleteEnrolment: clear payload: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM org_announcements`); err != nil {
		return fmt.Errorf("store.DeleteEnrolment: clear announcements: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM org_routing_policies`); err != nil {
		return fmt.Errorf("store.DeleteEnrolment: clear routing policy: %w", err)
	}
	return nil
}
