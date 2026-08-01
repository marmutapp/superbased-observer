// Package routingpolicy implements the org-distributed routing-policy
// registry (model-routing spec §R19.1–§R19.2): versioned, signed,
// audited policy documents the org admin publishes and agents fetch.
//
// The body is a TOML fragment in the [routing] vocabulary. Composition
// semantics live on the AGENT (routingconfig.ComposeOrgPolicy): org
// hard constraints filter FIRST and cannot be relaxed locally; org
// soft rules rank UNDER local ones; any enabled/mode key in the body
// is STRUCTURALLY ignored — enforcement is node-side opt-in by design
// (§R23: no remote enforce toggle exists, mirroring share.full_content).
package routingpolicy

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// Publish validates, signs, stores, and audits a new policy version.
// The body must parse as TOML (garbage is refused at the door); deeper
// semantic validation happens agent-side through the same lint path as
// local policies (a broken org rule fails open on the node, loudly).
func Publish(ctx context.Context, db *sql.DB, body, actor string) (orgcontract.RoutingPolicyDoc, error) {
	var probe map[string]any
	if err := toml.Unmarshal([]byte(body), &probe); err != nil {
		return orgcontract.RoutingPolicyDoc{}, fmt.Errorf("routingpolicy.Publish: body is not valid TOML: %w", err)
	}
	priv, pub, err := SigningKey(ctx, db)
	if err != nil {
		return orgcontract.RoutingPolicyDoc{}, err
	}
	sum := sha256.Sum256([]byte(body))
	doc := orgcontract.RoutingPolicyDoc{
		Body:      body,
		BodyHash:  hex.EncodeToString(sum[:]),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(body))),
		PublicKey: base64.StdEncoding.EncodeToString(pub),
	}
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return orgcontract.RoutingPolicyDoc{}, fmt.Errorf("routingpolicy.Publish: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var maxVersion sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(version) FROM org_routing_policies`).Scan(&maxVersion); err != nil {
		return orgcontract.RoutingPolicyDoc{}, fmt.Errorf("routingpolicy.Publish: version: %w", err)
	}
	doc.Version = maxVersion.Int64 + 1
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO org_routing_policies (version, body, body_hash, signature, created_by, created_at)
		 VALUES (?,?,?,?,?,?)`,
		doc.Version, doc.Body, doc.BodyHash, doc.Signature, actor, now); err != nil {
		return orgcontract.RoutingPolicyDoc{}, fmt.Errorf("routingpolicy.Publish: insert: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO routing_policy_audit (version, action, actor, at) VALUES (?,?,?,?)`,
		doc.Version, "publish", actor, now); err != nil {
		return orgcontract.RoutingPolicyDoc{}, fmt.Errorf("routingpolicy.Publish: audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return orgcontract.RoutingPolicyDoc{}, fmt.Errorf("routingpolicy.Publish: commit: %w", err)
	}
	return doc, nil
}

// Latest returns the newest published policy. ok=false when none.
func Latest(ctx context.Context, db *sql.DB) (orgcontract.RoutingPolicyDoc, bool, error) {
	var doc orgcontract.RoutingPolicyDoc
	err := db.QueryRowContext(ctx, `
		SELECT version, body, body_hash, signature
		FROM org_routing_policies ORDER BY version DESC LIMIT 1`).
		Scan(&doc.Version, &doc.Body, &doc.BodyHash, &doc.Signature)
	if errors.Is(err, sql.ErrNoRows) {
		return orgcontract.RoutingPolicyDoc{}, false, nil
	}
	if err != nil {
		return orgcontract.RoutingPolicyDoc{}, false, fmt.Errorf("routingpolicy.Latest: %w", err)
	}
	var pub string
	if err := db.QueryRowContext(ctx, `SELECT public_key FROM routing_policy_keys WHERE id = 1`).Scan(&pub); err == nil {
		doc.PublicKey = pub
	}
	return doc, true, nil
}

// SigningKey loads (or generates, once) the server's Ed25519
// distribution signing key.
//
// EXPORTED because it is the org server's ONE signing identity, not a
// routing-specific one: every signed-distribution rail shares it so an
// agent that has TOFU-pinned the key on one rail sees the same key on
// the others. Second consumer: orgserver/organnounce (rail R3 of the
// announcements plan). Keep it that way — a second key would mean a
// second pin, a second rotation story, and a second way to get it
// wrong. The row lives in routing_policy_keys (id=1) for continuity
// with the deployments that already have one; the table name is
// historical, its contents are org-wide.
func SigningKey(ctx context.Context, db *sql.DB) (ed25519.PrivateKey, ed25519.PublicKey, error) {
	var pubB64, privB64 string
	err := db.QueryRowContext(ctx, `SELECT public_key, private_key FROM routing_policy_keys WHERE id = 1`).
		Scan(&pubB64, &privB64)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		pub, priv, gerr := ed25519.GenerateKey(rand.Reader)
		if gerr != nil {
			return nil, nil, fmt.Errorf("routingpolicy: generate key: %w", gerr)
		}
		if _, ierr := db.ExecContext(ctx,
			`INSERT INTO routing_policy_keys (id, public_key, private_key, created_at) VALUES (1,?,?,?)`,
			base64.StdEncoding.EncodeToString(pub),
			base64.StdEncoding.EncodeToString(priv),
			time.Now().UTC().Format(time.RFC3339)); ierr != nil {
			return nil, nil, fmt.Errorf("routingpolicy: persist key: %w", ierr)
		}
		return priv, pub, nil
	case err != nil:
		return nil, nil, fmt.Errorf("routingpolicy: load key: %w", err)
	}
	pub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil {
		return nil, nil, fmt.Errorf("routingpolicy: decode public key: %w", err)
	}
	priv, err := base64.StdEncoding.DecodeString(privB64)
	if err != nil {
		return nil, nil, fmt.Errorf("routingpolicy: decode private key: %w", err)
	}
	return ed25519.PrivateKey(priv), ed25519.PublicKey(pub), nil
}

// Verify checks a policy doc's signature against a (pinned) public key.
func Verify(doc orgcontract.RoutingPolicyDoc, pinnedPubB64 string) error {
	return VerifySigned(doc.Body, doc.BodyHash, doc.Signature, pinnedPubB64)
}

// VerifySigned is the document-shape-independent core of Verify: hash
// match first, then Ed25519 over the BODY BYTES against the PINNED key.
//
// SECURITY NOTE (docs/security.md open ledger, ROUTING-SIG-1): the
// signature covers the body and NOTHING ELSE — not the version, not
// which rail the document arrived on. A captured signed policy can
// therefore be replayed at a different version number, and the org
// server's ONE signing identity means a signature minted on another
// body-signing rail would verify here too. This is a RELEASED wire
// format (agents in the field verify exactly these bytes), so it is
// recorded rather than changed in place; a fix is a versioned
// migration, not an edit.
//
// A NEW rail must NOT reuse this. The org-announcement rail
// (orgserver/organnounce, unreleased when it was hardened) signs
// orgcontract.AnnouncementSigningMessage — domain tag + version + body
// — precisely so neither replay works there, and it deliberately does
// not call this function.
func VerifySigned(body, bodyHash, signatureB64, pinnedPubB64 string) error {
	pub, err := base64.StdEncoding.DecodeString(pinnedPubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("routingpolicy.Verify: bad public key")
	}
	sig, err := base64.StdEncoding.DecodeString(signatureB64)
	if err != nil {
		return fmt.Errorf("routingpolicy.Verify: bad signature encoding")
	}
	sum := sha256.Sum256([]byte(body))
	if hex.EncodeToString(sum[:]) != bodyHash {
		return fmt.Errorf("routingpolicy.Verify: body hash mismatch")
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), []byte(body), sig) {
		return fmt.Errorf("routingpolicy.Verify: signature invalid")
	}
	return nil
}
