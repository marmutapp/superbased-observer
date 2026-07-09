package api

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/marmutapp/superbased-observer/internal/guard"
	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	gen "github.com/marmutapp/superbased-observer/internal/orgserver/api/gen"
	"github.com/marmutapp/superbased-observer/internal/orgserver/auth"
)

// Org guard-policy bundle channel, server side (guard spec §14.2,
// G13). The serve path (GetPolicyBundle) only READS — bundles are
// signed at publish time by an operator holding the policy signing
// key, so the long-running server process never needs the private key
// for serving. Publishing is `observer-org policy publish` (direct-DB,
// the MintEnrolmentTokenForUser pattern); dashboard authoring joins
// with G14's RBAC on top of the same PublishPolicyBundle gate.

// ErrNoPolicyBundle is returned by latestPolicyBundle when no bundle
// has been published.
var ErrNoPolicyBundle = errors.New("orgserver/api: no policy bundle published")

// ErrBundleInvalid is returned by PublishPolicyBundle when the TOML
// fails the org-layer policy lint (parse errors or floor violations).
var ErrBundleInvalid = errors.New("orgserver/api: bundle does not lint as an org policy file")

// GetPolicyBundle implements gen.ServerInterface (GET
// /api/v1/policy-bundle): it serves the latest published bundle with a
// strong ETag, honouring If-None-Match with a 304. Bearer auth is
// enforced by the bearerSecurity middleware (the operation carries the
// bearerAuth scope marker). 404 when no bundle was ever published —
// indistinguishable from a pre-G13 server on purpose: agents treat
// both as "run local-only policy".
func (h *Handlers) GetPolicyBundle(w http.ResponseWriter, r *http.Request, params gen.GetPolicyBundleParams) {
	b, err := latestPolicyBundle(r.Context(), h.store.db)
	if errors.Is(err, ErrNoPolicyBundle) {
		auth.WriteError(w, http.StatusNotFound, "no_policy_bundle", "no policy bundle has been published")
		return
	}
	if err != nil {
		h.logger.Error("policy-bundle: read", "err", err)
		auth.WriteError(w, http.StatusInternalServerError, "internal", "policy bundle read failed")
		return
	}
	etag := policyBundleETag(b)
	if params.IfNoneMatch != nil && *params.IfNoneMatch == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	writeJSON(w, http.StatusOK, b)
}

// policyBundleETag builds the strong validator for a bundle version:
// version + content hash, so a (hypothetical) re-signed identical
// version still matches and a content change never does.
func policyBundleETag(b orgcontract.PolicyBundle) string {
	sum := sha256.Sum256([]byte(b.BundleTOML))
	return fmt.Sprintf(`"pb-v%d-%s"`, b.Version, hex.EncodeToString(sum[:8]))
}

// latestPolicyBundle reads the highest-version bundle row.
func latestPolicyBundle(ctx context.Context, db *sql.DB) (orgcontract.PolicyBundle, error) {
	var b orgcontract.PolicyBundle
	err := db.QueryRowContext(ctx, `
		SELECT version, bundle_toml, signature, public_key, signed_at, description
		FROM org_policy_bundles ORDER BY version DESC LIMIT 1`).
		Scan(&b.Version, &b.BundleTOML, &b.Signature, &b.PublicKey, &b.SignedAt, &b.Description)
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNoPolicyBundle
	}
	if err != nil {
		return b, fmt.Errorf("orgserver/api.latestPolicyBundle: %w", err)
	}
	return b, nil
}

// PublishPolicyBundle validates, signs and stores bundleTOML as the
// next bundle version, returning the assigned version. The lint gate
// (guard.Lint with the org layer's escalate-only floor checks) runs
// HERE so every authoring surface — the G13 CLI and G14's dashboard —
// goes through the same refusal: a bundle that would not load on an
// agent, or that tries to RELAX below built-in strictness, is never
// signed. The version is read and the row written in one immediate
// transaction so concurrent publishes serialize and versions stay
// strictly monotonic.
func PublishPolicyBundle(ctx context.Context, db *sql.DB, priv ed25519.PrivateKey, bundleTOML, createdBy, description string) (int64, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return 0, errors.New("orgserver/api.PublishPolicyBundle: policy signing key required")
	}
	if problems := guard.Lint([]byte(bundleTOML), "org"); len(problems) > 0 {
		return 0, fmt.Errorf("%w: %s", ErrBundleInvalid, problems[0])
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("orgserver/api.PublishPolicyBundle: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var version int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) + 1 FROM org_policy_bundles`).Scan(&version); err != nil {
		return 0, fmt.Errorf("orgserver/api.PublishPolicyBundle: next version: %w", err)
	}
	signedAt := time.Now().UTC().Format(time.RFC3339)
	sig := orgcontract.SignPolicyBundle(priv, version, []byte(bundleTOML))
	pub := auth.EncodePublicKey(priv.Public().(ed25519.PublicKey))
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO org_policy_bundles (version, bundle_toml, signature, public_key, signed_at, created_by, description)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		version, bundleTOML, sig, pub, signedAt, createdBy, description); err != nil {
		return 0, fmt.Errorf("orgserver/api.PublishPolicyBundle: insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("orgserver/api.PublishPolicyBundle: commit: %w", err)
	}
	return version, nil
}

// PolicyBundleMeta is one version-history row for `observer-org policy
// list` (content omitted; `policy show` reads the full row).
type PolicyBundleMeta struct {
	Version     int64
	SignedAt    string
	CreatedBy   string
	Description string
	TOMLBytes   int
}

// ListPolicyBundles returns the version history, newest first.
func ListPolicyBundles(ctx context.Context, db *sql.DB) ([]PolicyBundleMeta, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT version, signed_at, created_by, description, LENGTH(bundle_toml)
		FROM org_policy_bundles ORDER BY version DESC`)
	if err != nil {
		return nil, fmt.Errorf("orgserver/api.ListPolicyBundles: %w", err)
	}
	defer rows.Close()
	var out []PolicyBundleMeta
	for rows.Next() {
		var m PolicyBundleMeta
		if err := rows.Scan(&m.Version, &m.SignedAt, &m.CreatedBy, &m.Description, &m.TOMLBytes); err != nil {
			return nil, fmt.Errorf("orgserver/api.ListPolicyBundles: scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("orgserver/api.ListPolicyBundles: rows: %w", err)
	}
	return out, nil
}

// PolicyBundleByVersion reads one historical bundle row (latest when
// version <= 0). ErrNoPolicyBundle when absent.
func PolicyBundleByVersion(ctx context.Context, db *sql.DB, version int64) (orgcontract.PolicyBundle, error) {
	if version <= 0 {
		return latestPolicyBundle(ctx, db)
	}
	var b orgcontract.PolicyBundle
	err := db.QueryRowContext(ctx, `
		SELECT version, bundle_toml, signature, public_key, signed_at, description
		FROM org_policy_bundles WHERE version = ?`, version).
		Scan(&b.Version, &b.BundleTOML, &b.Signature, &b.PublicKey, &b.SignedAt, &b.Description)
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNoPolicyBundle
	}
	if err != nil {
		return b, fmt.Errorf("orgserver/api.PolicyBundleByVersion: %w", err)
	}
	return b, nil
}
