package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	gen "github.com/marmutapp/superbased-observer/internal/orgserver/api/gen"
	"github.com/marmutapp/superbased-observer/internal/orgserver/auth"
	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
)

func newTestHandlers(t *testing.T) (*Handlers, *sql.DB) {
	t.Helper()
	d, err := orgdb.Open(context.Background(), orgdb.Options{Path: t.TempDir() + "/server.db"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	org, err := orgdb.EnsureOrg(context.Background(), d, "https://org.example")
	if err != nil {
		t.Fatal(err)
	}
	priv, _, err := auth.GenerateSigningKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := auth.NewIssuer(priv, org.ExternalURL, 90*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return New(d, issuer, org, 7*24*time.Hour, nil), d
}

// seedMember inserts an active member directly.
func seedMember(t *testing.T, d *sql.DB, userID, email string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := d.Exec(
		`INSERT INTO org_members (user_id, user_name, email, active, created_at, updated_at) VALUES (?,?,?,1,?,?)`,
		userID, email, email, now, now,
	); err != nil {
		t.Fatal(err)
	}
}

func TestEnrollFlow(t *testing.T) {
	h, d := newTestHandlers(t)
	seedMember(t, d, "user-1", "dev@acme.example")

	// Mint an enrolment token via the admin endpoint.
	mintRec := httptest.NewRecorder()
	h.MintEnrolmentToken(mintRec, httptest.NewRequest(http.MethodPost, "/api/org/enrolment-tokens",
		bytes.NewReader([]byte(`{"user_id":"user-1"}`))))
	if mintRec.Code != http.StatusCreated {
		t.Fatalf("mint: code=%d body=%s", mintRec.Code, mintRec.Body.String())
	}
	var mint mintTokenResponse
	if err := json.Unmarshal(mintRec.Body.Bytes(), &mint); err != nil {
		t.Fatal(err)
	}
	if mint.Token == "" {
		t.Fatal("mint returned empty token")
	}

	// Enrol with the cleartext token + a fresh agent public key.
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	enrollBody, _ := json.Marshal(orgcontract.EnrollRequest{
		OneTimeToken:   mint.Token, // compound "<token_id>.<secret>"
		AgentPublicKey: auth.EncodePublicKey(pub),
	})
	rec := httptest.NewRecorder()
	h.EnrollAgent(rec, httptest.NewRequest(http.MethodPost, "/api/agent/enroll", bytes.NewReader(enrollBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp orgcontract.EnrollResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Bearer == "" || resp.UserEmail != "dev@acme.example" || resp.OrgID != h.org.OrgID || resp.UserID != "user-1" {
		t.Fatalf("unexpected enroll response: %+v", resp)
	}

	// The minted bearer must validate end-to-end.
	if _, err := h.VerifyBearer(context.Background(), resp.Bearer); err != nil {
		t.Errorf("VerifyBearer rejected the freshly minted bearer: %v", err)
	}

	// Single-use: re-enrolling with the same token fails 401.
	rec2 := httptest.NewRecorder()
	h.EnrollAgent(rec2, httptest.NewRequest(http.MethodPost, "/api/agent/enroll", bytes.NewReader(enrollBody)))
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("token reuse: code=%d, want 401", rec2.Code)
	}
}

func TestEnrollRejections(t *testing.T) {
	h, d := newTestHandlers(t)
	seedMember(t, d, "user-1", "dev@acme.example")
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	pubStr := auth.EncodePublicKey(pub)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"missing fields", `{"user_id":"user-1"}`, http.StatusBadRequest},
		{"bad json", `{`, http.StatusBadRequest},
		{"bad pubkey", `{"user_id":"user-1","one_time_token":"x","agent_public_key":"notakey"}`, http.StatusBadRequest},
		{"unknown user", `{"user_id":"ghost","one_time_token":"x","agent_public_key":"` + pubStr + `"}`, http.StatusUnauthorized},
		{"wrong token", `{"user_id":"user-1","one_time_token":"nope","agent_public_key":"` + pubStr + `"}`, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.EnrollAgent(rec, httptest.NewRequest(http.MethodPost, "/api/agent/enroll", bytes.NewReader([]byte(tc.body))))
			if rec.Code != tc.want {
				t.Errorf("code=%d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestMintRejectsUnknownUser(t *testing.T) {
	h, _ := newTestHandlers(t)
	rec := httptest.NewRecorder()
	h.MintEnrolmentToken(rec, httptest.NewRequest(http.MethodPost, "/api/org/enrolment-tokens",
		bytes.NewReader([]byte(`{"user_id":"ghost"}`))))
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d, want 404", rec.Code)
	}
}

// signedPush builds a gzip-compressed, Ed25519-signed push request for userID
// against the handler, with claims already in context (as RequireBearer would
// leave them). ts overrides the signing timestamp when non-zero.
func signedPush(t *testing.T, h *Handlers, userID string, key ed25519.PrivateKey, env orgcontract.PushEnvelope, ts int64) (*http.Request, gen.PushBatchParams) {
	t.Helper()
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	wire := buf.Bytes()
	if ts == 0 {
		ts = time.Now().Unix()
	}
	sig := ed25519.Sign(key, orgcontract.PushSigningMessage(ts, wire))

	req := httptest.NewRequest(http.MethodPost, "/api/agent/push", bytes.NewReader(wire))
	req.Header.Set("Content-Encoding", "gzip")
	req = req.WithContext(auth.ContextWithClaims(req.Context(), orgcontract.BearerClaims{Sub: userID, Aud: h.org.OrgID}))
	sigStr := base64.RawURLEncoding.EncodeToString(sig)
	return req, gen.PushBatchParams{XSBOTimestamp: &ts, XSBOAgentSignature: &sigStr}
}

func TestPushBatchIngests(t *testing.T) {
	h, d := newTestHandlers(t)
	seedMember(t, d, "user-1", "dev@acme.example")

	// Bind an agent key the way enrolment would.
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	if err := h.store.bindAgentPublicKey(context.Background(), "user-1", auth.EncodePublicKey(pub)); err != nil {
		t.Fatal(err)
	}

	env := orgcontract.PushEnvelope{
		AgentVersion: "test", CursorFrom: 5, CursorTo: 10,
		Sessions: []orgcontract.SessionRow{{
			ID: "s1", Tool: "claude-code", StartedAt: "2026-05-26T10:00:00Z",
			OrgID: h.org.OrgID, UserEmail: "dev@acme.example",
		}},
		Actions: []orgcontract.ActionRow{{
			SessionID: "s1", SourceFile: "f.jsonl", SourceEventID: "e1", Timestamp: "2026-05-26T10:00:01Z",
			Tool: "claude-code", ActionType: "read_file", Success: true,
			OrgID: h.org.OrgID, UserEmail: "dev@acme.example",
		}},
	}

	// First push ingests both rows and advances the cursor to cursor_to.
	req, params := signedPush(t, h, "user-1", key, env, 0)
	rec := httptest.NewRecorder()
	h.PushBatch(rec, req, params)
	if rec.Code != http.StatusOK {
		t.Fatalf("push: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp orgcontract.PushResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.AcceptedRows != 2 || resp.DedupedRows != 0 || resp.NextCursor != 10 {
		t.Fatalf("first push response = %+v, want accepted=2 deduped=0 next=10", resp)
	}
	// Rows landed tagged with the authenticated pusher.
	var n int
	_ = d.QueryRow(`SELECT COUNT(*) FROM actions WHERE pushed_by_user_id = 'user-1' AND user_id = 'user-1'`).Scan(&n)
	if n != 1 {
		t.Errorf("actions rows for user-1 = %d, want 1", n)
	}

	// Re-pushing the same batch deduplicates (idempotent).
	req2, params2 := signedPush(t, h, "user-1", key, env, 0)
	rec2 := httptest.NewRecorder()
	h.PushBatch(rec2, req2, params2)
	_ = json.Unmarshal(rec2.Body.Bytes(), &resp)
	if rec2.Code != http.StatusOK || resp.AcceptedRows != 0 || resp.DedupedRows != 2 {
		t.Fatalf("re-push response = %d %+v, want 200 accepted=0 deduped=2", rec2.Code, resp)
	}
}

func TestPushBatchAuthFailures(t *testing.T) {
	h, d := newTestHandlers(t)
	seedMember(t, d, "user-1", "dev@acme.example")
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	if err := h.store.bindAgentPublicKey(context.Background(), "user-1", auth.EncodePublicKey(pub)); err != nil {
		t.Fatal(err)
	}
	env := orgcontract.PushEnvelope{AgentVersion: "test", CursorTo: 1}

	tests := []struct {
		name   string
		mutate func(req *http.Request, p *gen.PushBatchParams)
		want   int
	}{
		{"missing signature headers", func(_ *http.Request, p *gen.PushBatchParams) {
			p.XSBOTimestamp, p.XSBOAgentSignature = nil, nil
		}, http.StatusUnauthorized},
		{"timestamp out of skew", func(req *http.Request, p *gen.PushBatchParams) {
			// Re-sign with an old timestamp so the signature is valid but stale.
			old := time.Now().Unix() - orgcontract.PushSignatureSkewSeconds - 60
			r2, p2 := signedPush(t, h, "user-1", key, env, old)
			*req = *r2
			*p = p2
		}, http.StatusUnauthorized},
		{"tampered signature", func(_ *http.Request, p *gen.PushBatchParams) {
			bad := "AAAA" + (*p.XSBOAgentSignature)[4:]
			p.XSBOAgentSignature = &bad
		}, http.StatusUnauthorized},
		{"wrong signing key", func(req *http.Request, p *gen.PushBatchParams) {
			_, other, _ := ed25519.GenerateKey(rand.Reader)
			r2, p2 := signedPush(t, h, "user-1", other, env, 0)
			*req = *r2
			*p = p2
		}, http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, params := signedPush(t, h, "user-1", key, env, 0)
			tc.mutate(req, &params)
			rec := httptest.NewRecorder()
			h.PushBatch(rec, req, params)
			if rec.Code != tc.want {
				t.Errorf("code=%d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	// A user with no bound agent key cannot push (401).
	seedMember(t, d, "user-2", "two@acme.example")
	req, params := signedPush(t, h, "user-2", key, env, 0)
	rec := httptest.NewRecorder()
	h.PushBatch(rec, req, params)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unbound user push: code=%d, want 401", rec.Code)
	}
}

func TestVerifyBearerRevocationAndActive(t *testing.T) {
	h, d := newTestHandlers(t)
	seedMember(t, d, "user-1", "dev@acme.example")
	bearer, claims, err := h.issuer.Mint("user-1", h.org.OrgID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.VerifyBearer(context.Background(), bearer); err != nil {
		t.Fatalf("valid bearer rejected: %v", err)
	}

	// Revoke by jti.
	if _, err := d.Exec(`INSERT INTO revoked_bearers (jti, revoked_at) VALUES (?, ?)`, claims.Jti, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.VerifyBearer(context.Background(), bearer); err == nil {
		t.Error("revoked bearer accepted")
	}

	// Inactive subject.
	bearer2, _, _ := h.issuer.Mint("user-1", h.org.OrgID)
	if _, err := d.Exec(`UPDATE org_members SET active = 0 WHERE user_id = 'user-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.VerifyBearer(context.Background(), bearer2); err == nil {
		t.Error("bearer for inactive user accepted")
	}
}

func TestResolveSAMLUser(t *testing.T) {
	h, d := newTestHandlers(t)
	// JIT-create on first login.
	id1, err := h.ResolveSAMLUser(context.Background(), auth.SAMLIdentity{Email: "new@acme.example", DisplayName: "New"})
	if err != nil {
		t.Fatalf("resolve 1: %v", err)
	}
	// Idempotent on second login (same id).
	id2, err := h.ResolveSAMLUser(context.Background(), auth.SAMLIdentity{Email: "new@acme.example", DisplayName: "New Renamed"})
	if err != nil {
		t.Fatalf("resolve 2: %v", err)
	}
	if id1 != id2 {
		t.Errorf("resolve not idempotent: %s != %s", id1, id2)
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM org_members WHERE email = 'new@acme.example'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("JIT created %d rows, want 1", n)
	}
	if _, err := h.ResolveSAMLUser(context.Background(), auth.SAMLIdentity{Email: ""}); err == nil {
		t.Error("expected error resolving empty email")
	}
}

func TestRateLimit(t *testing.T) {
	mw := RateLimit(0.0001, 2) // tiny refill, burst 2
	called := 0
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called++; w.WriteHeader(200) }))
	codes := make([]int, 0, 3)
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		h.ServeHTTP(rec, req)
		codes = append(codes, rec.Code)
	}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != http.StatusTooManyRequests {
		t.Errorf("rate limit codes = %v, want [200 200 429]", codes)
	}
}
