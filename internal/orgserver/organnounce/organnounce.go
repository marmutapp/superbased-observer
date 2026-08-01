// Package organnounce implements the org announcement registry — rail
// R3 of docs/plans/dashboard-announcements-banner-plan-2026-07-31.md
// §4: versioned, signed, audited announcement documents the org admin
// publishes and enrolled agents fetch on the push cycle they already
// run.
//
// It is deliberately the same mechanism as orgserver/routingpolicy: the
// Ed25519 key (routingpolicy.SigningKey) is SHARED, so there is exactly
// one signing identity per org server and a node that pinned it on one
// rail sees the same key here (orgclient/orgpin.go enforces that both
// ways).
//
// The verification core is NOT shared, and that is the security-
// relevant difference. routingpolicy signs the bare body bytes; this
// rail signs orgcontract.AnnouncementSigningMessage — a domain tag plus
// the version plus the body — so a captured document cannot be replayed
// at another version, and a document signed on the other rail by the
// same key cannot be presented here. The routing rail is released and
// keeps its shape (docs/security.md, ROUTING-SIG-1). What else lives
// here is what actually differs: the table, the body semantics, and the
// validation gate.
//
// Body semantics: the plan §1 announcement JSON (one object or an array
// of them), validated through internal/announce.Validate BEFORE it is
// signed, so no malformed announcement can ever reach a fleet with a
// good signature on it. An EMPTY body is the RETRACTION document — a
// real, signed, version-bumped instruction to show nothing (see
// orgcontract.OrgAnnouncementDoc for why retraction is a publish and
// not a delete).
//
// What this rail can do is bounded by construction: put dismissible
// plain text in a banner. It carries no toggle and no code, the node's
// [dashboard].org_announcements switch silences it locally, and no
// server-side override for that switch exists (same posture as
// [org_client.share].full_content).
package organnounce

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/marmutapp/superbased-observer/internal/announce"
	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	"github.com/marmutapp/superbased-observer/internal/orgserver/routingpolicy"
)

// ErrInvalidBody is returned when the body is not a decodable
// announcement document or carries an announcement that fails the §1
// rules. Handlers map it to 400 — a publish attempt with a bad body is
// the admin's mistake, not a server fault.
var ErrInvalidBody = errors.New("organnounce: invalid announcement body")

// ValidateBody parses and validates a candidate document body. It
// accepts the retraction (empty body) and every shape announce.Decode
// accepts, and returns the decoded announcements so a caller that also
// wants to display them need not decode twice.
//
// Source is checked, not coerced: coercing here would let a body whose
// stored bytes say "release" be served to nodes that then render it as
// org content, and the SIGNED bytes are what the node reads. Callers
// build the document with Source=SourceOrg (the handler does) and this
// refuses anything else, so the signature always covers honest
// provenance.
func ValidateBody(body string) ([]announce.Announcement, error) {
	list, err := announce.Decode(body)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidBody, err)
	}
	seen := make(map[string]struct{}, len(list))
	for i, a := range list {
		if err := announce.Validate(a); err != nil {
			return nil, fmt.Errorf("%w: announcement %d: %w", ErrInvalidBody, i, err)
		}
		if a.Source != announce.SourceOrg {
			return nil, fmt.Errorf("%w: announcement %d: source must be %q, got %q",
				ErrInvalidBody, i, announce.SourceOrg, a.Source)
		}
		if _, dup := seen[a.ID]; dup {
			return nil, fmt.Errorf("%w: duplicate announcement id %q (a repeated id is a silently-unshown banner)",
				ErrInvalidBody, a.ID)
		}
		seen[a.ID] = struct{}{}
	}
	return list, nil
}

// Publish validates, signs, stores, and audits a new announcement
// version. The EMPTY body — and only the empty body — publishes the
// retraction: announce.Decode refuses "[]" and "null", so a retraction
// has one byte representation, one hash, and one signature, and
// `action = "retract"` below is decided by a body that is genuinely
// empty rather than by one of several spellings of nothing.
//
// Validation happens BEFORE the key is touched: the invariant worth
// having is that a signature never exists over a body the fleet would
// have to drop.
//
// The signature is minted inside the transaction, once MAX(version)+1
// is known, because it BINDS the version (see Verify).
func Publish(ctx context.Context, db *sql.DB, body, actor string) (orgcontract.OrgAnnouncementDoc, error) {
	list, err := ValidateBody(body)
	if err != nil {
		return orgcontract.OrgAnnouncementDoc{}, err
	}
	action := "publish"
	if len(list) == 0 {
		action = "retract"
	}
	priv, pub, err := routingpolicy.SigningKey(ctx, db)
	if err != nil {
		return orgcontract.OrgAnnouncementDoc{}, err
	}
	sum := sha256.Sum256([]byte(body))
	doc := orgcontract.OrgAnnouncementDoc{
		Body:      body,
		BodyHash:  hex.EncodeToString(sum[:]),
		PublicKey: base64.StdEncoding.EncodeToString(pub),
	}
	// Signature is minted BELOW, once the version is known: the signed
	// message binds the version (orgcontract.AnnouncementSigningMessage),
	// so it cannot be computed before MAX(version)+1 is resolved inside
	// the transaction. BodyHash stays the plain body hash — it is the
	// display/dedup value, never the thing that authorizes a document.
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return orgcontract.OrgAnnouncementDoc{}, fmt.Errorf("organnounce.Publish: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var maxVersion sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(version) FROM org_announcements`).Scan(&maxVersion); err != nil {
		return orgcontract.OrgAnnouncementDoc{}, fmt.Errorf("organnounce.Publish: version: %w", err)
	}
	doc.Version = maxVersion.Int64 + 1
	doc.Signature = base64.StdEncoding.EncodeToString(
		ed25519.Sign(priv, orgcontract.AnnouncementSigningMessage(doc.Version, doc.Body)),
	)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO org_announcements (version, body, body_hash, signature, created_by, created_at)
		 VALUES (?,?,?,?,?,?)`,
		doc.Version, doc.Body, doc.BodyHash, doc.Signature, actor, now); err != nil {
		return orgcontract.OrgAnnouncementDoc{}, fmt.Errorf("organnounce.Publish: insert: %w", err)
	}
	// Audited in the SAME transaction as the insert (routingpolicy's
	// discipline): a published version with no audit row must be
	// impossible, not merely unlikely.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO org_announcement_audit (version, action, actor, at) VALUES (?,?,?,?)`,
		doc.Version, action, actor, now); err != nil {
		return orgcontract.OrgAnnouncementDoc{}, fmt.Errorf("organnounce.Publish: audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return orgcontract.OrgAnnouncementDoc{}, fmt.Errorf("organnounce.Publish: commit: %w", err)
	}
	return doc, nil
}

// Latest returns the newest published announcement document. ok=false
// when none has ever been published — the handler turns that into a
// 404, never a 500.
//
// A RETRACTION is ok=true with an empty Body: it is a published
// document and the node must receive it (and bump its cached version)
// to stop showing the previous one.
func Latest(ctx context.Context, db *sql.DB) (orgcontract.OrgAnnouncementDoc, bool, error) {
	var doc orgcontract.OrgAnnouncementDoc
	err := db.QueryRowContext(ctx, `
		SELECT version, body, body_hash, signature
		FROM org_announcements ORDER BY version DESC LIMIT 1`).
		Scan(&doc.Version, &doc.Body, &doc.BodyHash, &doc.Signature)
	if errors.Is(err, sql.ErrNoRows) {
		return orgcontract.OrgAnnouncementDoc{}, false, nil
	}
	if err != nil {
		return orgcontract.OrgAnnouncementDoc{}, false, fmt.Errorf("organnounce.Latest: %w", err)
	}
	var pub string
	if err := db.QueryRowContext(ctx, `SELECT public_key FROM routing_policy_keys WHERE id = 1`).Scan(&pub); err == nil {
		doc.PublicKey = pub
	}
	return doc, true, nil
}

// Verify checks an announcement doc against a (pinned) public key.
//
// What the signature covers is the security-relevant part, and it is
// NOT the body alone: verification runs over
// orgcontract.AnnouncementSigningMessage(doc.Version, doc.Body), which
// binds both the announcement RAIL (a domain tag) and the VERSION.
// Consequences, each one a real attack this refuses:
//
//   - A captured signed document cannot be replayed at a different
//     version. Bumping Version on a valid capture used to let an
//     eavesdropper freeze a fleet's cache at a huge version (no later
//     genuine announcement would ever pass the node's monotonic
//     short-circuit), or clear every banner by replaying an old
//     retraction. Both now fail signature verification.
//   - A routing-policy document cannot be presented as an announcement
//     (or vice versa) even though ONE org key signs both rails: the
//     routing rail signs the bare body, this rail signs the tagged
//     message, so neither signature verifies on the other's input.
//
// BodyHash is still checked, but only as what it is — an integrity/
// dedup value for display. It authorizes nothing on its own, so it is
// verified BEFORE the signature only to give the clearer error.
//
// The routing-policy rail is deliberately NOT changed to match: it is
// released, its signature shape is a compat surface, and the residual
// is recorded in docs/security.md's open ledger instead.
func Verify(doc orgcontract.OrgAnnouncementDoc, pinnedPubB64 string) error {
	pub, err := base64.StdEncoding.DecodeString(pinnedPubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("organnounce.Verify: bad public key")
	}
	sig, err := base64.StdEncoding.DecodeString(doc.Signature)
	if err != nil {
		return errors.New("organnounce.Verify: bad signature encoding")
	}
	sum := sha256.Sum256([]byte(doc.Body))
	if hex.EncodeToString(sum[:]) != doc.BodyHash {
		return errors.New("organnounce.Verify: body hash mismatch")
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), orgcontract.AnnouncementSigningMessage(doc.Version, doc.Body), sig) {
		return errors.New("organnounce.Verify: signature invalid (it must cover this rail's domain tag AND this version)")
	}
	return nil
}
