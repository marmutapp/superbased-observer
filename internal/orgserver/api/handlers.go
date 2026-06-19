package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	gen "github.com/marmutapp/superbased-observer/internal/orgserver/api/gen"
	"github.com/marmutapp/superbased-observer/internal/orgserver/auth"
	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
	"github.com/marmutapp/superbased-observer/internal/orgserver/ingest"
)

// Compile-time contracts: Handlers implements the generated agent-protocol
// server interface and the auth bridge interfaces.
var (
	_ gen.ServerInterface = (*Handlers)(nil)
	_ auth.BearerVerifier = (*Handlers)(nil)
	_ auth.UserResolver   = (*Handlers)(nil)
)

// maxPushBytes caps the decompressed push body to defend against zip bombs.
// The spec ceiling for an agent batch is 16 MiB; allow some headroom.
const maxPushBytes = 32 << 20 // 32 MiB

// Handlers implements the generated agent-protocol ServerInterface plus the
// admin enrolment-token mint endpoint, and bridges auth (BearerVerifier,
// UserResolver) to the DB.
type Handlers struct {
	store         *store
	issuer        *auth.Issuer
	org           orgdb.Org
	logger        *slog.Logger
	enrolTokenTTL time.Duration
	// policyPubKey is the base64url public half of the org POLICY
	// signing key (guard spec §14.2), delivered in EnrollResponse so
	// agents pin it. Empty when [policy].signing_key_path is not
	// configured — the field is then omitted from the response (the
	// pre-G13 wire shape). Set once at assembly via
	// SetOrgPolicyPublicKey, before serving starts.
	policyPubKey string
}

// SetOrgPolicyPublicKey installs the org policy public key delivered
// at enrolment (guard spec §14.2). Call before the server starts
// serving; not safe to call concurrently with requests.
func (h *Handlers) SetOrgPolicyPublicKey(b64 string) { h.policyPubKey = b64 }

// New constructs Handlers over the server DB. ttl is the default
// enrolment-token lifetime (defaults to 7 days when non-positive).
func New(db *sql.DB, issuer *auth.Issuer, org orgdb.Org, ttl time.Duration, logger *slog.Logger) *Handlers {
	if logger == nil {
		logger = slog.Default()
	}
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	return &Handlers{
		store:         newStore(db),
		issuer:        issuer,
		org:           org,
		logger:        logger,
		enrolTokenTTL: ttl,
	}
}

// EnrollAgent implements gen.ServerInterface: it exchanges a one-time
// enrolment token for a long-lived bearer (POST /api/agent/enroll).
func (h *Handlers) EnrollAgent(w http.ResponseWriter, r *http.Request) {
	var req orgcontract.EnrollRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		auth.WriteError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.OneTimeToken == "" || req.AgentPublicKey == "" {
		auth.WriteError(w, http.StatusBadRequest, "bad_request", "one_time_token and agent_public_key are required")
		return
	}
	ctx := r.Context()

	// Validate the agent's public key shape before touching the token, so a
	// malformed key is a 400 and does not burn the token.
	if _, err := auth.DecodePublicKey(req.AgentPublicKey); err != nil {
		auth.WriteError(w, http.StatusBadRequest, "bad_request", "agent_public_key is not a valid Ed25519 key")
		return
	}

	// Resolve + verify + burn the compound token; this yields the user id.
	userID, ok, err := h.resolveEnrolmentToken(ctx, req.OneTimeToken)
	if err != nil {
		h.logger.Error("enroll: token resolve", "err", err)
		auth.WriteError(w, http.StatusInternalServerError, "internal", "enrolment failed")
		return
	}
	if !ok {
		auth.WriteError(w, http.StatusUnauthorized, "unauthorized", "invalid or expired enrolment token")
		return
	}

	m, found, err := h.store.memberByID(ctx, userID)
	if err != nil {
		h.logger.Error("enroll: member lookup", "err", err)
		auth.WriteError(w, http.StatusInternalServerError, "internal", "enrolment failed")
		return
	}
	if !found || !m.Active {
		// Token was valid but the user was deprovisioned after it was minted.
		auth.WriteError(w, http.StatusUnauthorized, "unauthorized", "invalid or expired enrolment token")
		return
	}

	if err := h.store.bindAgentPublicKey(ctx, userID, req.AgentPublicKey); err != nil {
		h.logger.Error("enroll: bind pubkey", "err", err)
		auth.WriteError(w, http.StatusInternalServerError, "internal", "enrolment failed")
		return
	}

	bearer, claims, err := h.issuer.Mint(userID, h.org.OrgID)
	if err != nil {
		h.logger.Error("enroll: mint bearer", "err", err)
		auth.WriteError(w, http.StatusInternalServerError, "internal", "enrolment failed")
		return
	}
	// Index the jti for the dashboard's bearer list / admin Revoke. A failure
	// here must not fail the enrol (the bearer is already valid); log and go on.
	if err := h.store.recordIssuedBearer(ctx, claims.Jti, userID,
		time.Unix(claims.Iat, 0), time.Unix(claims.Exp, 0)); err != nil {
		h.logger.Error("enroll: record issued bearer", "err", err, "jti", claims.Jti)
	}
	h.logger.Info("agent enrolled", "user_id", userID, "jti", claims.Jti)

	writeJSON(w, http.StatusOK, orgcontract.EnrollResponse{
		Bearer:          bearer,
		BearerExpiresAt: time.Unix(claims.Exp, 0).UTC().Format(time.RFC3339),
		OrgID:           h.org.OrgID,
		OrgName:         h.org.OrgName,
		UserID:          userID,
		UserEmail:       m.Email,
		// Empty (and json-omitted) when no policy signing key is
		// configured — the agent then trust-on-first-fetch pins.
		OrgPolicyPublicKey: h.policyPubKey,
	})
}

// PushBatch implements gen.ServerInterface (POST /api/agent/push). The bearer
// is already validated by RequireBearer middleware; this handler additionally
// enforces the per-push Ed25519 proof (defence against a stolen bearer) and
// then ingests the content-free batch.
//
// The signature is verified over the EXACT wire bytes (the gzip-compressed
// body), so the raw bytes are read first and hashed before any decode. A
// missing/invalid signature, an out-of-skew timestamp, or an unbound/absent
// agent key are all auth failures (401), matching a missing bearer rather than
// the generic 400 the wrapper would return for a malformed body.
func (h *Handlers) PushBatch(w http.ResponseWriter, r *http.Request, params gen.PushBatchParams) {
	ctx := r.Context()
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok {
		auth.WriteError(w, http.StatusUnauthorized, "unauthorized", "missing bearer")
		return
	}

	// 1. The per-push proof headers are mandatory (declared optional in the
	//    OpenAPI so their absence is an auth failure here, not a 400).
	if params.XSBOTimestamp == nil || params.XSBOAgentSignature == nil {
		auth.WriteError(w, http.StatusUnauthorized, "unauthorized", "missing per-push signature")
		return
	}
	ts := *params.XSBOTimestamp

	// 2. Read the raw wire bytes (the signed gzip body) before any decode.
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxPushBytes))
	if err != nil {
		auth.WriteError(w, http.StatusBadRequest, "bad_request", "could not read body")
		return
	}
	if absInt64(time.Now().Unix()-ts) > orgcontract.PushSignatureSkewSeconds {
		auth.WriteError(w, http.StatusUnauthorized, "unauthorized", "timestamp outside allowed skew")
		return
	}

	// 3. Verify the signature against the key bound at enrolment.
	pub, bound, err := h.store.agentPublicKey(ctx, claims.Sub)
	if err != nil {
		h.logger.Error("push: agent key lookup", "err", err)
		auth.WriteError(w, http.StatusInternalServerError, "internal", "ingest failed")
		return
	}
	sig, decErr := base64.RawURLEncoding.DecodeString(*params.XSBOAgentSignature)
	if !bound || decErr != nil || !ed25519.Verify(pub, orgcontract.PushSigningMessage(ts, raw), sig) {
		auth.WriteError(w, http.StatusUnauthorized, "unauthorized", "invalid per-push signature")
		return
	}

	// 4. Decode the envelope from the verified bytes.
	env, err := decodePushBody(raw, r.Header.Get("Content-Encoding"))
	if err != nil {
		auth.WriteError(w, http.StatusBadRequest, "bad_request", "invalid push envelope")
		return
	}

	// 5. Ingest under the authenticated user (never client-supplied identity).
	res, err := ingest.Push(ctx, h.store.db, env, claims.Sub, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		h.logger.Error("push: ingest", "err", err, "user_id", claims.Sub)
		auth.WriteError(w, http.StatusInternalServerError, "internal", "ingest failed")
		return
	}
	h.logger.Info("push ingested", "user_id", claims.Sub,
		"accepted", res.Accepted, "deduped", res.Deduped,
		"sessions", len(env.Sessions), "actions", len(env.Actions),
		"api_turns", len(env.APITurns), "token_usage", len(env.TokenUsage))

	writeJSON(w, http.StatusOK, orgcontract.PushResponse{
		AcceptedRows: res.Accepted,
		DedupedRows:  res.Deduped,
		NextCursor:   env.CursorTo,
	})
}

// mintTokenRequest / mintTokenResponse are the admin enrolment-token mint
// contract (SAML-protected; M3 formalises it in the dashboard OpenAPI surface).
type mintTokenRequest struct {
	UserID  string `json:"user_id"`
	TTLDays int    `json:"ttl_days,omitempty"`
}

type mintTokenResponse struct {
	Token     string `json:"token"`
	TokenID   string `json:"token_id"`
	UserID    string `json:"user_id"`
	ExpiresAt string `json:"expires_at"`
}

// MintEnrolmentToken handles POST /api/org/enrolment-tokens. It is mounted
// behind RequireSAMLSession. It generates a 32-byte token, stores its
// argon2id hash with a TTL, and returns the cleartext exactly once.
func (h *Handlers) MintEnrolmentToken(w http.ResponseWriter, r *http.Request) {
	var req mintTokenRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		auth.WriteError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.UserID == "" {
		auth.WriteError(w, http.StatusBadRequest, "bad_request", "user_id is required")
		return
	}
	ctx := r.Context()

	ttl := h.enrolTokenTTL
	if req.TTLDays > 0 {
		ttl = time.Duration(req.TTLDays) * 24 * time.Hour
	}

	cleartext, tokenID, expiresAt, err := MintEnrolmentTokenForUser(ctx, h.store.db, req.UserID, ttl)
	switch {
	case errors.Is(err, ErrUserNotFound):
		auth.WriteError(w, http.StatusNotFound, "not_found", "no such user")
		return
	case err != nil:
		h.logger.Error("mint token", "err", err)
		auth.WriteError(w, http.StatusInternalServerError, "internal", "could not store token")
		return
	}
	h.logger.Info("enrolment token minted", "user_id", req.UserID, "token_id", tokenID)

	writeJSON(w, http.StatusCreated, mintTokenResponse{
		Token:     cleartext,
		TokenID:   tokenID,
		UserID:    req.UserID,
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
	})
}

// ErrUserNotFound is returned by MintEnrolmentTokenForUser when the target
// user does not exist.
var ErrUserNotFound = errors.New("api: no such user")

// MintEnrolmentTokenForUser generates a 32-byte one-time enrolment token for
// userID, stores its argon2id hash with the given TTL (default 7 days when
// non-positive), and returns the cleartext exactly once. Shared by the HTTP
// mint handler and the observer-org `new-enrolment-token` CLI command so
// there is one minting implementation.
func MintEnrolmentTokenForUser(ctx context.Context, db *sql.DB, userID string, ttl time.Duration) (cleartext, tokenID string, expiresAt time.Time, err error) {
	s := newStore(db)
	if _, ok, lookErr := s.memberByID(ctx, userID); lookErr != nil {
		return "", "", time.Time{}, lookErr
	} else if !ok {
		return "", "", time.Time{}, ErrUserNotFound
	}
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	secret, err := newCleartextToken(32)
	if err != nil {
		return "", "", time.Time{}, err
	}
	// Only the secret is hashed; the token_id is the (non-secret) lookup key.
	hash, err := hashToken(secret)
	if err != nil {
		return "", "", time.Time{}, err
	}
	tokenID, err = randID()
	if err != nil {
		return "", "", time.Time{}, err
	}
	expiresAt = s.now().Add(ttl)
	if err := s.insertEnrolmentToken(ctx, tokenID, hash, userID, expiresAt); err != nil {
		return "", "", time.Time{}, err
	}
	// The admin hands the developer this single compound string; the server
	// resolves the user from token_id at enrol, so no user_id is needed there.
	return tokenID + "." + secret, tokenID, expiresAt, nil
}

// resolveEnrolmentToken parses a compound "<token_id>.<secret>" token, looks up
// the row by token_id, verifies the secret (argon2id) and expiry, and burns it
// (atomic single-use). It returns the token's user_id on success. All failure
// modes return ok=false with no distinguishing error, so the endpoint cannot
// be used as a user/token enumeration oracle.
func (h *Handlers) resolveEnrolmentToken(ctx context.Context, compound string) (userID string, ok bool, err error) {
	id, secret, found := strings.Cut(compound, ".")
	if !found || id == "" || secret == "" {
		return "", false, nil
	}
	tok, found, err := h.store.tokenByID(ctx, id)
	if err != nil {
		return "", false, err
	}
	if !found || h.store.now().After(tok.ExpiresAt) || !verifyToken(secret, tok.Hash) {
		return "", false, nil
	}
	burned, err := h.store.burnToken(ctx, tok.ID)
	if err != nil {
		return "", false, err
	}
	if !burned {
		return "", false, nil // already used (single-use race)
	}
	return tok.UserID, true, nil
}

// VerifyBearer implements auth.BearerVerifier: stateless signature/expiry
// checks plus the DB-backed revocation and subject-active checks.
func (h *Handlers) VerifyBearer(ctx context.Context, raw string) (orgcontract.BearerClaims, error) {
	claims, err := h.issuer.Parse(raw)
	if err != nil {
		return orgcontract.BearerClaims{}, err
	}
	revoked, err := h.store.isRevoked(ctx, claims.Jti)
	if err != nil {
		return orgcontract.BearerClaims{}, err
	}
	if revoked {
		return orgcontract.BearerClaims{}, errors.New("api: bearer revoked")
	}
	m, ok, err := h.store.memberByID(ctx, claims.Sub)
	if err != nil {
		return orgcontract.BearerClaims{}, err
	}
	if !ok || !m.Active {
		return orgcontract.BearerClaims{}, errors.New("api: bearer subject inactive or missing")
	}
	return claims, nil
}

// ResolveSAMLUser implements auth.UserResolver.
func (h *Handlers) ResolveSAMLUser(ctx context.Context, id auth.SAMLIdentity) (string, error) {
	return h.store.upsertSAMLUser(ctx, id.Email, id.DisplayName)
}

// decodePushBody decodes a push envelope from the verified wire bytes. When the
// body was gzip-encoded (the agent always compresses), it is inflated through a
// LimitReader so a decompression bomb cannot exhaust memory beyond maxPushBytes.
func decodePushBody(raw []byte, contentEncoding string) (orgcontract.PushEnvelope, error) {
	var env orgcontract.PushEnvelope
	var rdr io.Reader = bytes.NewReader(raw)
	if contentEncoding == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return env, err
		}
		defer func() { _ = zr.Close() }()
		rdr = zr
	}
	if err := json.NewDecoder(io.LimitReader(rdr, maxPushBytes)).Decode(&env); err != nil {
		return env, err
	}
	return env, nil
}

// absInt64 returns the absolute value of n (for the timestamp skew check).
func absInt64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
