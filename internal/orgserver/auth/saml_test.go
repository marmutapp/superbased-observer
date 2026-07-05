package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/crewjam/saml"
)

func TestExtractIdentity(t *testing.T) {
	mapping := AttributeMapping{Email: "Email", DisplayName: "DisplayName", Groups: "Groups"}
	assertion := &saml.Assertion{
		AttributeStatements: []saml.AttributeStatement{{
			Attributes: []saml.Attribute{
				{Name: "Email", Values: []saml.AttributeValue{{Value: "dev@acme.example"}}},
				{FriendlyName: "DisplayName", Values: []saml.AttributeValue{{Value: "Dev Eloper"}}},
				{Name: "Groups", Values: []saml.AttributeValue{{Value: "platform"}, {Value: "backend"}}},
				{Name: "Unmapped", Values: []saml.AttributeValue{{Value: "ignore"}}},
			},
		}},
	}
	id := extractIdentity(assertion, mapping)
	if id.Email != "dev@acme.example" {
		t.Errorf("email = %q", id.Email)
	}
	if id.DisplayName != "Dev Eloper" {
		t.Errorf("display name = %q (matched by FriendlyName)", id.DisplayName)
	}
	if len(id.Groups) != 2 || id.Groups[0] != "platform" || id.Groups[1] != "backend" {
		t.Errorf("groups = %v", id.Groups)
	}
}

func TestExtractIdentityNilSafe(t *testing.T) {
	if id := extractIdentity(nil, AttributeMapping{Email: "Email"}); id.Email != "" {
		t.Errorf("nil assertion should yield empty identity, got %+v", id)
	}
}

// genSelfSigned writes a throwaway RSA cert+key PEM pair to a temp dir and
// returns the paths.
func genSelfSigned(t *testing.T) (certPath, keyPath string, certPEM, keyPEM []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-sp"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return "", "", certPEM, keyPEM
}

func TestParseSPKeypair(t *testing.T) {
	_, _, certPEM, keyPEM := genSelfSigned(t)
	cert, signer, err := parseSPKeypair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parseSPKeypair: %v", err)
	}
	if cert.Subject.CommonName != "test-sp" {
		t.Errorf("cert CN = %q", cert.Subject.CommonName)
	}
	if signer == nil {
		t.Fatal("nil signer")
	}
}

func TestParseSPKeypairRejectsGarbage(t *testing.T) {
	if _, _, err := parseSPKeypair([]byte("not pem"), []byte("not pem")); err == nil {
		t.Error("expected error on garbage PEM")
	}
}

// stubResolver records the identity it was asked to resolve.
type stubResolver struct{ lastEmail string }

func (s *stubResolver) ResolveSAMLUser(_ context.Context, id SAMLIdentity) (string, error) {
	s.lastEmail = id.Email
	return "user-for-" + id.Email, nil
}

const idpMetadataXML = `<?xml version="1.0"?>
<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="https://idp.example/metadata">
  <IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">
    <SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="https://idp.example/sso"/>
  </IDPSSODescriptor>
</EntityDescriptor>`

func TestNewSAMLAndMetadata(t *testing.T) {
	_, _, certPEM, keyPEM := genSelfSigned(t)
	dir := t.TempDir()
	certPath := dir + "/sp.crt"
	keyPath := dir + "/sp.key"
	mustWrite(t, certPath, certPEM)
	mustWrite(t, keyPath, keyPEM)

	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/samlmetadata+xml")
		_, _ = w.Write([]byte(idpMetadataXML))
	}))
	defer idp.Close()

	sessions, err := NewSessionManager([]byte(strings.Repeat("k", 32)), time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &stubResolver{}
	sp, err := NewSAML(context.Background(), SAMLOptions{
		ExternalURL:    "https://org.example",
		EntityID:       "https://org.example/saml/metadata",
		SPCertPath:     certPath,
		SPKeyPath:      keyPath,
		IDPMetadataURL: idp.URL,
		Mapping:        AttributeMapping{Email: "Email", DisplayName: "DisplayName", Groups: "Groups"},
		HTTPClient:     idp.Client(),
	}, sessions, resolver, nil)
	if err != nil {
		t.Fatalf("NewSAML: %v", err)
	}

	// Metadata endpoint serves SP metadata XML naming our entity ID.
	rec := httptest.NewRecorder()
	sp.Metadata(rec, httptest.NewRequest(http.MethodGet, "/saml/metadata", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("metadata status = %d", rec.Code)
	}
	if !strings.Contains(body, "org.example/saml/metadata") {
		t.Errorf("metadata missing entity ID:\n%s", body)
	}
	if !strings.Contains(body, "org.example/saml/acs") {
		t.Errorf("metadata missing ACS URL:\n%s", body)
	}

	// SLO clears the session cookie.
	rec = httptest.NewRecorder()
	sp.SLO(rec, httptest.NewRequest(http.MethodGet, "/saml/slo", nil))
	if rec.Code != http.StatusFound {
		t.Errorf("SLO status = %d, want 302", rec.Code)
	}
}

func TestNewSAMLRejectsBadConfig(t *testing.T) {
	sessions, _ := NewSessionManager([]byte(strings.Repeat("k", 32)), time.Hour, false)
	_, err := NewSAML(context.Background(), SAMLOptions{
		ExternalURL: "not-a-url",
	}, sessions, &stubResolver{}, nil)
	if err == nil {
		t.Error("expected error for invalid external URL")
	}
}

func mustWrite(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// ctxRecordingResolver captures the Err() state of every context it's
// asked to resolve under. Used by TestCreateSessionDetachesContext to
// prove the SAML session creator hands the DB resolver a fresh
// non-canceled context even when the inbound HTTP request context has
// already been canceled (browser disconnect, write-timeout, etc.).
type ctxRecordingResolver struct {
	calls       int
	lastCtxErr  error
	lastEmail   string
	returnedErr error
}

func (r *ctxRecordingResolver) ResolveSAMLUser(ctx context.Context, id SAMLIdentity) (string, error) {
	r.calls++
	r.lastCtxErr = ctx.Err()
	r.lastEmail = id.Email
	return "uid-" + id.Email, r.returnedErr
}

// TestCreateSessionDetachesContext is the regression test for Issue 5b
// in docs/teams-test-regression-2026-06-03.md: when r.Context() was
// canceled mid-ACS (browser disconnect / write-timeout), the SAML
// resolver call inherited the cancellation, memberByEmail returned
// `context canceled`, CreateSession failed, crewjam returned 403, and
// the browser retried in an infinite SSO loop. The detach in saml.go
// ensures the resolver sees a fresh non-canceled context.
func TestCreateSessionDetachesContext(t *testing.T) {
	sessions, err := NewSessionManager([]byte(strings.Repeat("k", 32)), time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &ctxRecordingResolver{}
	sess := &samlSession{
		sessions: sessions,
		resolver: resolver,
		mapping:  AttributeMapping{Email: "Email"},
		logger:   testLogger(t),
	}
	assertion := &saml.Assertion{
		AttributeStatements: []saml.AttributeStatement{{
			Attributes: []saml.Attribute{
				{Name: "Email", Values: []saml.AttributeValue{{Value: "user1@example.com"}}},
			},
		}},
	}

	// Build a request whose context is ALREADY canceled — modelling the
	// browser-disconnect / write-timeout race.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodPost, "/saml/acs", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	if err := sess.CreateSession(rec, r, assertion); err != nil {
		t.Fatalf("CreateSession with canceled r.Context() returned error: %v", err)
	}
	if resolver.calls != 1 {
		t.Fatalf("resolver call count = %d, want 1", resolver.calls)
	}
	if resolver.lastEmail != "user1@example.com" {
		t.Errorf("resolver email = %q", resolver.lastEmail)
	}
	if resolver.lastCtxErr != nil {
		t.Errorf("resolver inbound ctx.Err() = %v, want nil (detach failed — would surface as 5b SSO loop in prod)", resolver.lastCtxErr)
	}
	// Session cookie was issued — the detach happy path.
	if cookies := rec.Result().Cookies(); len(cookies) == 0 {
		t.Error("no session cookie set on response")
	}
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestSSOShortCircuitsWhenAuthenticated regresses Issue 5b's loop half:
// when a user with a valid sbo_org_session hits /saml/sso (the URL our
// requireSAMLWeb middleware redirects unauthenticated visitors to, and
// the URL crewjam records as the post-ACS return target), the handler
// must 302 to "/" instead of re-initiating SSO. Without this, the
// browser bounces between /saml/sso → IdP → /saml/acs → /saml/sso →
// ... at ~10 round-trips/sec until the tab crashes.
func TestSSOShortCircuitsWhenAuthenticated(t *testing.T) {
	sessions, err := NewSessionManager([]byte(strings.Repeat("k", 32)), time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}

	// Build a stub *SAML that only exercises the SSO short-circuit path.
	// crewjam's middleware isn't required — the short-circuit fires
	// before it.
	sp := &SAML{sessions: sessions, logger: testLogger(t)}

	// First: no cookie → handler should fall through to crewjam.
	// crewjam isn't wired here, so we expect a panic on nil mw — capture
	// it as the proof that the fall-through path was taken.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/saml/sso", nil)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("unauthenticated SSO did not reach crewjam (mw nil) — short-circuit fired without a session?")
			}
		}()
		sp.SSO(rec, r)
	}()

	// Now: issue a real session cookie and hit /saml/sso again — must
	// short-circuit to /.
	cookieRec := httptest.NewRecorder()
	if err := sessions.Issue(cookieRec, "user-uid-1"); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	cookies := cookieRec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no session cookie issued")
	}

	rec = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/saml/sso", nil)
	r.AddCookie(cookies[0])
	sp.SSO(rec, r)

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want /", loc)
	}
}
