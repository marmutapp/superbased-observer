package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
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
