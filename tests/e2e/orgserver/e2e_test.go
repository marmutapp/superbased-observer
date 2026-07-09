// Package e2e exercises the org server end to end through its real HTTP
// handler: SCIM-provision a user, log that user in via a real signed SAML
// assertion (an in-process crewjam/saml IdP), mint an enrolment token with
// the resulting dashboard session, exchange the token for an Ed25519 bearer,
// and use the bearer on the push endpoint. Every step goes through the
// assembled orgserver.Server.Handler() with its real auth gates.
package e2e

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"html"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewjam/saml"
	"github.com/crewjam/saml/logger"
	"github.com/crewjam/saml/samlsp"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	"github.com/marmutapp/superbased-observer/internal/orgserver"
	"github.com/marmutapp/superbased-observer/internal/orgserver/auth"
	"github.com/marmutapp/superbased-observer/internal/orgserver/config"
)

const (
	spExternalURL = "http://sp.example"
	testEmail     = "dev@acme.example"
	scimToken     = "e2e-scim-token"
)

// fixedSessionProvider makes the IdP authenticate a fixed user without a login
// form (saml.SessionProvider).
type fixedSessionProvider struct{ session *saml.Session }

func (f fixedSessionProvider) GetSession(_ http.ResponseWriter, _ *http.Request, _ *saml.IdpAuthnRequest) *saml.Session {
	return f.session
}

// spProvider returns the SP metadata to the IdP (saml.ServiceProviderProvider).
type spProvider struct{ md *saml.EntityDescriptor }

func (p spProvider) GetServiceProvider(_ *http.Request, _ string) (*saml.EntityDescriptor, error) {
	return p.md, nil
}

func TestEnrolmentE2E(t *testing.T) {
	ctx := context.Background()

	// --- IdP keypair + in-process IdP serving signed assertions ------------
	idpKey, idpCert := genRSACert(t, "e2e-idp")
	idp := &saml.IdentityProvider{
		Key:         idpKey,
		Certificate: idpCert,
		Logger:      logger.DefaultLogger,
		SessionProvider: fixedSessionProvider{session: &saml.Session{
			ID:         "sess-1",
			CreateTime: time.Now(),
			ExpireTime: time.Now().Add(time.Hour),
			NameID:     testEmail,
			UserName:   testEmail,
			UserEmail:  testEmail,
		}},
	}
	idpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		out, err := xml.MarshalIndent(idp.Metadata(), "", "  ")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/samlmetadata+xml")
		_, _ = w.Write(out)
	}))
	defer idpServer.Close()
	idpURL, _ := url.Parse(idpServer.URL)
	idp.MetadataURL = *idpURL
	idp.SSOURL = *mustURL(idpServer.URL + "/sso")

	// --- org server config + secrets ---------------------------------------
	cfg := writeServerConfig(t, idpServer.URL)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := orgserver.New(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("orgserver.New: %v", err)
	}
	// Close the server's DB handle before the test returns, so t.TempDir's
	// RemoveAll can delete server.db on Windows (this test drives Handler()
	// directly and never calls Run, which is where the DB is otherwise closed).
	defer func() { _ = srv.Close() }()
	spServer := httptest.NewServer(srv.Handler())
	defer spServer.Close()

	client := spServer.Client()
	// Don't auto-follow the post-ACS redirect; we want to capture the cookie.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	// Resolve the SP metadata (advertises the http://sp.example URLs) and hand
	// it to the IdP so it can target the right ACS/audience.
	spMD := fetchSPMetadata(t, spServer.URL)
	idp.ServiceProviderProvider = spProvider{md: spMD}

	// --- 1. SCIM-provision the user ----------------------------------------
	scimUserID := scimProvision(t, spServer.URL, client)
	t.Logf("SCIM provisioned user_id=%s", scimUserID)

	// --- 2. Real SAML login: IdP makes a signed assertion, POST it to ACS ---
	sessionCookie := samlLogin(t, idp, spMD, spServer.URL, client)
	if sessionCookie == nil {
		t.Fatal("SAML login produced no session cookie")
	}
	t.Logf("SAML login set cookie %s", sessionCookie.Name)

	// --- 3. Mint an enrolment token (SAML session) -------------------------
	token := mintToken(t, spServer.URL, client, sessionCookie, scimUserID)

	// --- 4. Exchange the token for a bearer --------------------------------
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	enroll := enrollAgent(t, spServer.URL, client, token, auth.EncodePublicKey(pub))
	if enroll.Bearer == "" || enroll.UserEmail != testEmail || enroll.UserID != scimUserID {
		t.Fatalf("unexpected enroll response: %+v", enroll)
	}

	// --- 5. The bearer + per-push signature ingest a batch (200) -----------
	env := orgcontract.PushEnvelope{
		AgentVersion: "e2e", CursorFrom: 0, CursorTo: 2,
		Sessions: []orgcontract.SessionRow{{
			ID: "sess-e2e", Tool: "claude-code", StartedAt: "2026-05-26T10:00:00Z",
			OrgID: enroll.OrgID, UserEmail: testEmail,
		}},
		Actions: []orgcontract.ActionRow{{
			SessionID: "sess-e2e", SourceFile: "f.jsonl", SourceEventID: "e1",
			Timestamp: "2026-05-26T10:00:01Z", Tool: "claude-code", ActionType: "read_file", Success: true,
			OrgID: enroll.OrgID, UserEmail: testEmail,
		}},
	}
	status, resp := pushSigned(t, spServer.URL, client, enroll.Bearer, priv, env)
	if status != http.StatusOK {
		t.Fatalf("push with bearer+signature: status=%d, want 200", status)
	}
	if resp.AcceptedRows != 2 || resp.NextCursor != 2 {
		t.Fatalf("push response = %+v, want accepted=2 next=2", resp)
	}
	// Re-pushing the same batch is deduplicated server-side (idempotent).
	if _, resp2 := pushSigned(t, spServer.URL, client, enroll.Bearer, priv, env); resp2.AcceptedRows != 0 || resp2.DedupedRows != 2 {
		t.Fatalf("re-push response = %+v, want accepted=0 deduped=2", resp2)
	}

	// Negative: a valid bearer but NO per-push signature → 401.
	if s := pushWithBearer(t, spServer.URL, client, enroll.Bearer); s != http.StatusUnauthorized {
		t.Errorf("push without signature: status=%d, want 401", s)
	}
	// Negative: no bearer → 401.
	if s := pushWithBearer(t, spServer.URL, client, ""); s != http.StatusUnauthorized {
		t.Errorf("push without bearer: status=%d, want 401", s)
	}
}

// ---- step helpers ----------------------------------------------------------

func scimProvision(t *testing.T, base string, client *http.Client) string {
	t.Helper()
	body := `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"` + testEmail +
		`","emails":[{"value":"` + testEmail + `","primary":true}],"active":true}`
	req, _ := http.NewRequest(http.MethodPost, base+"/scim/v2/Users", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+scimToken)
	req.Header.Set("Content-Type", "application/scim+json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("scim create: status=%d body=%s", resp.StatusCode, raw)
	}
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatal("scim create returned no id")
	}
	return id
}

func samlLogin(t *testing.T, idp *saml.IdentityProvider, spMD *saml.EntityDescriptor, base string, client *http.Client) *http.Cookie {
	t.Helper()
	// Drive the IdP IDP-initiated flow to produce the auto-POST HTML form.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, idp.SSOURL.String(), nil)
	idp.ServeIDPInitiated(rec, r, spMD.EntityID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ServeIDPInitiated: code=%d body=%s", rec.Code, rec.Body.String())
	}
	samlResponse := extractField(t, rec.Body.String(), "SAMLResponse")

	// POST the signed assertion to the SP ACS.
	form := url.Values{"SAMLResponse": {samlResponse}, "RelayState": {""}}
	resp, err := client.PostForm(base+"/saml/acs", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("ACS: status=%d body=%s", resp.StatusCode, raw)
	}
	for _, c := range resp.Cookies() {
		if c.Name == auth.DefaultSessionCookie {
			return c
		}
	}
	return nil
}

func mintToken(t *testing.T, base string, client *http.Client, cookie *http.Cookie, userID string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/api/org/enrolment-tokens",
		strings.NewReader(`{"user_id":"`+userID+`"}`))
	req.AddCookie(cookie)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("mint: status=%d body=%s", resp.StatusCode, raw)
	}
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	tok, _ := m["token"].(string)
	if tok == "" {
		t.Fatal("mint returned no token")
	}
	return tok
}

func enrollAgent(t *testing.T, base string, client *http.Client, token, pubKey string) orgcontract.EnrollResponse {
	t.Helper()
	body, _ := json.Marshal(orgcontract.EnrollRequest{OneTimeToken: token, AgentPublicKey: pubKey})
	resp, err := client.Post(base+"/api/agent/enroll", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("enroll: status=%d body=%s", resp.StatusCode, raw)
	}
	var out orgcontract.EnrollResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

// pushWithBearer sends an unsigned, uncompressed push with only a bearer (or
// none). It exercises the auth-failure paths: a missing bearer (401 at the
// middleware) and a present bearer without the per-push signature (401 at the
// handler).
func pushWithBearer(t *testing.T, base string, client *http.Client, bearer string) int {
	t.Helper()
	body, _ := json.Marshal(orgcontract.PushEnvelope{AgentVersion: "e2e", CursorFrom: 0, CursorTo: 1})
	req, _ := http.NewRequest(http.MethodPost, base+"/api/agent/push", strings.NewReader(string(body)))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// pushSigned gzips + signs env with the enrol-time private key exactly as the
// agent does, and posts it with the bearer + per-push signature headers.
func pushSigned(t *testing.T, base string, client *http.Client, bearer string, key ed25519.PrivateKey, env orgcontract.PushEnvelope) (int, orgcontract.PushResponse) {
	t.Helper()
	raw, _ := json.Marshal(env)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	wire := buf.Bytes()
	ts := time.Now().Unix()
	sig := ed25519.Sign(key, orgcontract.PushSigningMessage(ts, wire))

	req, _ := http.NewRequest(http.MethodPost, base+"/api/agent/push", bytes.NewReader(wire))
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("X-SBO-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-SBO-Agent-Signature", base64.RawURLEncoding.EncodeToString(sig))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out orgcontract.PushResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// ---- setup helpers ---------------------------------------------------------

func writeServerConfig(t *testing.T, idpMetadataURL string) config.Config {
	t.Helper()
	dir := t.TempDir()
	spKey, spCert := genRSACert(t, "e2e-sp")
	writePEM(t, filepath.Join(dir, "sp.crt"), "CERTIFICATE", spCert.Raw)
	spKeyDER, _ := x509.MarshalPKCS8PrivateKey(spKey)
	writePEM(t, filepath.Join(dir, "sp.key"), "PRIVATE KEY", spKeyDER)

	_, bearerPEM, err := auth.GenerateSigningKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "bearer.key"), bearerPEM)
	mustWrite(t, filepath.Join(dir, "session.key"), []byte(strings.Repeat("e2e-session-key-", 2))) // 32 bytes
	mustWrite(t, filepath.Join(dir, "scim.token"), []byte(scimToken))
	_ = os.Chmod(filepath.Join(dir, "scim.token"), 0o600)

	cfg := config.Default()
	cfg.Server.Listen = "127.0.0.1:0"
	cfg.Server.ExternalURL = spExternalURL
	cfg.Server.DBPath = filepath.Join(dir, "server.db")
	cfg.Server.SessionKeyPath = filepath.Join(dir, "session.key")
	cfg.Server.LogLevel = "error"
	cfg.SAML.SPEntityID = spExternalURL + "/saml/metadata"
	cfg.SAML.SPCertPath = filepath.Join(dir, "sp.crt")
	cfg.SAML.SPKeyPath = filepath.Join(dir, "sp.key")
	cfg.SAML.IDPMetadataURL = idpMetadataURL
	// The default assertion maker emits UserEmail with FriendlyName "mail".
	cfg.SAML.AttributeMapping = map[string]string{"email": "mail", "display_name": "displayName", "groups": "eduPersonAffiliation"}
	cfg.SCIM.AuthTokenPath = filepath.Join(dir, "scim.token")
	cfg.Bearer.SigningKeyPath = filepath.Join(dir, "bearer.key")
	return cfg
}

func fetchSPMetadata(t *testing.T, base string) *saml.EntityDescriptor {
	t.Helper()
	resp, err := http.Get(base + "/saml/metadata")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	md, err := samlsp.ParseMetadata(raw)
	if err != nil {
		t.Fatalf("parse SP metadata: %v", err)
	}
	return md
}

var fieldRE = regexp.MustCompile(`name="([^"]+)" value="([^"]*)"`)

func extractField(t *testing.T, body, field string) string {
	t.Helper()
	for _, m := range fieldRE.FindAllStringSubmatch(body, -1) {
		if m[1] == field {
			// crewjam renders the form with html/template, which escapes the
			// base64 (e.g. '+' → "&#43;"); unescape before use.
			return html.UnescapeString(m[2])
		}
	}
	t.Fatalf("field %q not found in IdP response form:\n%s", field, body)
	return ""
}

func genRSACert(t *testing.T, cn string) (*rsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return key, cert
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	mustWrite(t, path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}))
}

func mustWrite(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}
