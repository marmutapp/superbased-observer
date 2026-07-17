package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgserver/auth"
)

// store wraps the server DB with the queries the API handlers need:
// enrolment-token lifecycle, member lookups, agent-pubkey binding, bearer
// revocation, and SAML user resolution. It is intentionally separate from
// the SCIM store (different concern, different package).
type store struct {
	db  *sql.DB
	now func() time.Time
}

func newStore(db *sql.DB) *store {
	return &store{db: db, now: func() time.Time { return time.Now().UTC() }}
}

type member struct {
	UserID    string
	Email     string
	Active    bool
	PublicKey string
}

// memberByID returns the member, or (zero,false) if absent.
func (s *store) memberByID(ctx context.Context, userID string) (member, bool, error) {
	var m member
	var active int
	var pub sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id, email, active, agent_public_key FROM org_members WHERE user_id = ?`, userID).
		Scan(&m.UserID, &m.Email, &active, &pub)
	if errors.Is(err, sql.ErrNoRows) {
		return member{}, false, nil
	}
	if err != nil {
		return member{}, false, fmt.Errorf("api.store.memberByID: %w", err)
	}
	m.Active = active != 0
	m.PublicKey = pub.String
	return m, true, nil
}

// memberByEmail returns the first member with the given email.
func (s *store) memberByEmail(ctx context.Context, email string) (member, bool, error) {
	var m member
	var active int
	var pub sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id, email, active, agent_public_key FROM org_members WHERE email = ? ORDER BY created_at LIMIT 1`, email).
		Scan(&m.UserID, &m.Email, &active, &pub)
	if errors.Is(err, sql.ErrNoRows) {
		return member{}, false, nil
	}
	if err != nil {
		return member{}, false, fmt.Errorf("api.store.memberByEmail: %w", err)
	}
	m.Active = active != 0
	m.PublicKey = pub.String
	return m, true, nil
}

// upsertSAMLUser resolves a SAML identity to a user_id. If a member with the
// email exists, its display name is refreshed and the id returned; otherwise
// a member is JIT-created (SCIM remains the authoritative provisioning path,
// but a SAML login for a not-yet-provisioned user should still work).
func (s *store) upsertSAMLUser(ctx context.Context, email, displayName string) (string, error) {
	if email == "" {
		return "", errors.New("api.store.upsertSAMLUser: empty email")
	}
	m, ok, err := s.memberByEmail(ctx, email)
	if err != nil {
		return "", err
	}
	now := s.now().Format(time.RFC3339Nano)
	if ok {
		if displayName != "" {
			if _, err := s.db.ExecContext(ctx,
				`UPDATE org_members SET display_name = ?, updated_at = ? WHERE user_id = ?`,
				displayName, now, m.UserID); err != nil {
				return "", fmt.Errorf("api.store.upsertSAMLUser: refresh: %w", err)
			}
		}
		return m.UserID, nil
	}
	id, err := randID()
	if err != nil {
		return "", err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO org_members (user_id, user_name, email, display_name, active, created_at, updated_at)
		 VALUES (?, ?, ?, NULLIF(?, ''), 1, ?, ?)`,
		id, email, email, displayName, now, now); err != nil {
		return "", fmt.Errorf("api.store.upsertSAMLUser: insert: %w", err)
	}
	return id, nil
}

// insertEnrolmentToken stores an argon2id-hashed one-time token for a user.
func (s *store) insertEnrolmentToken(ctx context.Context, id, hash, userID string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO enrolment_tokens (id, token_hash, user_id, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?)`,
		id, hash, userID, s.now().Format(time.RFC3339Nano), expiresAt.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("api.store.insertEnrolmentToken: %w", err)
	}
	return nil
}

type enrolmentToken struct {
	ID        string
	Hash      string
	UserID    string
	ExpiresAt time.Time
}

// tokenByID returns the enrolment token row with the given id, regardless of
// used/expired state (the caller checks expiry and burnToken enforces
// single-use atomically). ok is false when no such id exists.
func (s *store) tokenByID(ctx context.Context, id string) (enrolmentToken, bool, error) {
	var t enrolmentToken
	var exp string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, token_hash, user_id, expires_at FROM enrolment_tokens WHERE id = ?`, id).
		Scan(&t.ID, &t.Hash, &t.UserID, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return enrolmentToken{}, false, nil
	}
	if err != nil {
		return enrolmentToken{}, false, fmt.Errorf("api.store.tokenByID: %w", err)
	}
	t.ExpiresAt = parseRFC3339(exp)
	return t, true, nil
}

// burnToken marks an enrolment token used. Returns false if it was already
// burned concurrently (the UPDATE ... WHERE used_at IS NULL affected 0 rows),
// which the caller treats as a failed enrol (single-use guarantee).
func (s *store) burnToken(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE enrolment_tokens SET used_at = ? WHERE id = ? AND used_at IS NULL`,
		s.now().Format(time.RFC3339Nano), id)
	if err != nil {
		return false, fmt.Errorf("api.store.burnToken: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// bindAgentPublicKey records the agent's Ed25519 public key on the member.
func (s *store) bindAgentPublicKey(ctx context.Context, userID, pubKey string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE org_members SET agent_public_key = ?, updated_at = ? WHERE user_id = ?`,
		pubKey, s.now().Format(time.RFC3339Nano), userID)
	if err != nil {
		return fmt.Errorf("api.store.bindAgentPublicKey: %w", err)
	}
	return nil
}

// agentPublicKey returns the Ed25519 public key bound to userID at enrolment,
// decoded and ready for ed25519.Verify. ok is false when the user is unknown
// or has no key bound (e.g. enrolment never completed), which the push handler
// treats as an auth failure.
func (s *store) agentPublicKey(ctx context.Context, userID string) (ed25519.PublicKey, bool, error) {
	var pub sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT agent_public_key FROM org_members WHERE user_id = ?`, userID).Scan(&pub)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("api.store.agentPublicKey: %w", err)
	}
	if !pub.Valid || pub.String == "" {
		return nil, false, nil
	}
	key, err := auth.DecodePublicKey(pub.String)
	if err != nil {
		return nil, false, fmt.Errorf("api.store.agentPublicKey: decode: %w", err)
	}
	return key, true, nil
}

// recordIssuedBearer indexes a freshly minted bearer's jti so the dashboard
// can list a developer's live bearers and an admin can revoke one. It stores
// no secret — only the public jti and its lifetime. A duplicate jti (vanishing
// probability) is ignored.
func (s *store) recordIssuedBearer(ctx context.Context, jti, userID string, issuedAt, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO issued_bearers (jti, user_id, issued_at, expires_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(jti) DO NOTHING`,
		jti, userID, issuedAt.UTC().Format(time.RFC3339), expiresAt.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("api.store.recordIssuedBearer: %w", err)
	}
	return nil
}

// isRevoked reports whether a bearer jti is on the revocation list.
func (s *store) isRevoked(ctx context.Context, jti string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM revoked_bearers WHERE jti = ?`, jti).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("api.store.isRevoked: %w", err)
	}
	return true, nil
}

func randID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func parseRFC3339(s string) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}
