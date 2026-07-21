package auth

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/crewjam/saml"
	"github.com/crewjam/saml/samlsp"
)

// resolveSAMLUserTimeout bounds the DB resolver call inside CreateSession.
// The query is a single SELECT (microseconds) but we cap it so a runaway
// DB stall can't pin the request.
const resolveSAMLUserTimeout = 10 * time.Second

// AttributeMapping names the SAML assertion attributes that carry the
// canonical user fields. Values are matched against either the attribute's
// Name or FriendlyName, so an IdP emitting "Email" or "urn:oid:0.9.2342..."
// both work as long as the mapping matches one of them.
type AttributeMapping struct {
	Email       string
	DisplayName string
	Groups      string
}

// SAMLIdentity is the verified identity extracted from a SAML assertion.
// Groups is carried for the dashboard's future use; M1 relies on SCIM for
// authoritative team membership, so the resolver may ignore it.
type SAMLIdentity struct {
	Email       string
	DisplayName string
	Groups      []string
}

// UserResolver maps a verified SAML identity to a session user_id, upserting
// or refreshing the user record. It is implemented by the server layer
// against the DB; keeping it an interface keeps package auth free of a DB
// dependency. An error (e.g. empty email) aborts the login.
type UserResolver interface {
	ResolveSAMLUser(ctx context.Context, id SAMLIdentity) (userID string, err error)
}

// SAMLOptions configures the SAML SP.
type SAMLOptions struct {
	ExternalURL    string // server external URL; SP ACS/metadata/SLO derive from it
	EntityID       string // sp_entity_id
	SPCertPath     string
	SPKeyPath      string
	IDPMetadataURL string
	Mapping        AttributeMapping
	HTTPClient     *http.Client // optional; used to fetch IdP metadata (injected in tests)
}

// SAML wraps crewjam/saml's samlsp.Middleware with our own HMAC-cookie
// session and DB-backed user resolution. It exposes the four SP endpoints
// (metadata, sso, acs, slo) as plain http.HandlerFuncs the server mounts.
type SAML struct {
	mw       *samlsp.Middleware
	sessions *SessionManager
	logger   *slog.Logger
}

// NewSAML builds the SP: it loads the SP keypair, fetches the IdP metadata
// over the network (once, at boot), and constructs the middleware with our
// custom session provider. A metadata-fetch failure is fatal — the server
// cannot authenticate anyone without it.
func NewSAML(ctx context.Context, opts SAMLOptions, sessions *SessionManager, resolver UserResolver, logger *slog.Logger) (*SAML, error) {
	if sessions == nil || resolver == nil {
		return nil, errors.New("auth.NewSAML: sessions and resolver are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	root, err := url.Parse(opts.ExternalURL)
	if err != nil || root.Scheme == "" || root.Host == "" {
		return nil, fmt.Errorf("auth.NewSAML: external URL %q invalid", opts.ExternalURL)
	}
	cert, key, err := loadSPKeypair(opts.SPCertPath, opts.SPKeyPath)
	if err != nil {
		return nil, err
	}
	metaURL, err := url.Parse(opts.IDPMetadataURL)
	if err != nil || metaURL.Scheme == "" {
		return nil, fmt.Errorf("auth.NewSAML: idp_metadata_url %q invalid", opts.IDPMetadataURL)
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	idpMeta, err := samlsp.FetchMetadata(ctx, httpClient, *metaURL)
	if err != nil {
		return nil, fmt.Errorf("auth.NewSAML: fetch IdP metadata from %s: %w", opts.IDPMetadataURL, err)
	}

	mw, err := samlsp.New(samlsp.Options{
		EntityID:          opts.EntityID,
		URL:               *root,
		Key:               key,
		Certificate:       cert,
		IDPMetadata:       idpMeta,
		AllowIDPInitiated: true,
		CookieSameSite:    http.SameSiteLaxMode,
	})
	if err != nil {
		return nil, fmt.Errorf("auth.NewSAML: build middleware: %w", err)
	}
	// crewjam redirects to DefaultRedirectURI ("/") after a successful ACS;
	// our placeholder dashboard lives there.
	mw.ServiceProvider.DefaultRedirectURI = "/"
	mw.Session = &samlSession{
		sessions: sessions,
		resolver: resolver,
		mapping:  opts.Mapping,
		logger:   logger,
	}

	return &SAML{mw: mw, sessions: sessions, logger: logger}, nil
}

// Metadata serves the SP metadata XML (GET /saml/metadata).
func (s *SAML) Metadata(w http.ResponseWriter, r *http.Request) { s.mw.ServeMetadata(w, r) }

// ACS handles the AssertionConsumerService POST-back (POST /saml/acs). On a
// valid assertion it resolves the user, sets the session cookie, and
// redirects to the dashboard.
func (s *SAML) ACS(w http.ResponseWriter, r *http.Request) { s.mw.ServeACS(w, r) }

// SSO initiates SP-initiated SSO (GET /saml/sso), redirecting to the IdP.
// Short-circuits to "/" when the caller already has a valid session cookie:
// without this, crewjam records /saml/sso as the RelayState entry URL (it
// was the user-visible URL when SSO was started), and the post-ACS redirect
// lands the now-authed user right back on /saml/sso — which re-initiates
// SSO unconditionally, creating an infinite loop. The redirect-target bug
// is Issue 5b's loop half in docs/teams-test-regression-v1.8.2-2026-06-04.md.
func (s *SAML) SSO(w http.ResponseWriter, r *http.Request) {
	if _, err := s.sessions.UserID(r); err == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	s.mw.HandleStartAuthFlow(w, r)
}

// SLO performs a local logout (GET /saml/slo): it clears the session cookie
// and redirects home. Full IdP-initiated single logout is deferred to a
// later milestone; clearing the local session is the M1 contract.
func (s *SAML) SLO(w http.ResponseWriter, r *http.Request) {
	s.sessions.Clear(w)
	http.Redirect(w, r, "/", http.StatusFound)
}

// samlSession implements samlsp.SessionProvider, bridging crewjam's ACS flow
// to our HMAC cookie + DB user resolution.
type samlSession struct {
	sessions *SessionManager
	resolver UserResolver
	mapping  AttributeMapping
	logger   *slog.Logger
}

func (a *samlSession) CreateSession(w http.ResponseWriter, r *http.Request, assertion *saml.Assertion) error {
	id := extractIdentity(assertion, a.mapping)
	if id.Email == "" {
		return errors.New("auth: SAML assertion carried no email attribute")
	}
	// Detach from r.Context() with a short bounded timeout. A browser
	// disconnect or the server's WriteTimeout firing mid-ACS would
	// otherwise cancel ResolveSAMLUser, fail CreateSession, and trigger
	// crewjam's 403 → infinite SSO retry loop seen as Issue 5b in
	// docs/teams-test-regression-2026-06-03.md. Mirrors the v1.7.3
	// proxy-insert detach pattern (feedback_proxy_detached_insert_context).
	ctx, cancel := context.WithTimeout(context.Background(), resolveSAMLUserTimeout)
	defer cancel()
	userID, err := a.resolver.ResolveSAMLUser(ctx, id)
	if err != nil {
		return fmt.Errorf("auth: resolve SAML user %q: %w", id.Email, err)
	}
	a.logger.Info("saml login", "email", id.Email, "user_id", userID)
	return a.sessions.Issue(w, userID)
}

func (a *samlSession) DeleteSession(w http.ResponseWriter, _ *http.Request) error {
	a.sessions.Clear(w)
	return nil
}

func (a *samlSession) GetSession(r *http.Request) (samlsp.Session, error) {
	uid, err := a.sessions.UserID(r)
	if err != nil {
		return nil, samlsp.ErrNoSession
	}
	return uid, nil
}

// extractIdentity walks the assertion's attribute statements and pulls the
// mapped fields. Matching is by Name or FriendlyName.
func extractIdentity(assertion *saml.Assertion, m AttributeMapping) SAMLIdentity {
	var id SAMLIdentity
	if assertion == nil {
		return id
	}
	for _, stmt := range assertion.AttributeStatements {
		for _, attr := range stmt.Attributes {
			switch {
			case matches(attr, m.Email):
				if v := firstValue(attr); v != "" {
					id.Email = v
				}
			case matches(attr, m.DisplayName):
				if v := firstValue(attr); v != "" {
					id.DisplayName = v
				}
			case matches(attr, m.Groups):
				id.Groups = append(id.Groups, allValues(attr)...)
			}
		}
	}
	return id
}

func matches(attr saml.Attribute, name string) bool {
	return name != "" && (attr.Name == name || attr.FriendlyName == name)
}

func firstValue(attr saml.Attribute) string {
	for _, v := range attr.Values {
		if v.Value != "" {
			return v.Value
		}
	}
	return ""
}

func allValues(attr saml.Attribute) []string {
	out := make([]string, 0, len(attr.Values))
	for _, v := range attr.Values {
		if v.Value != "" {
			out = append(out, v.Value)
		}
	}
	return out
}

// loadSPKeypair reads the SP certificate (PEM) and its private key (PEM,
// PKCS#8 or PKCS#1). The key must implement crypto.Signer (RSA or ECDSA),
// which crewjam/saml uses to sign AuthnRequests and the session JWT it would
// otherwise issue (we replace the latter with our own cookie).
func loadSPKeypair(certPath, keyPath string) (*x509.Certificate, crypto.Signer, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, fmt.Errorf("auth.loadSPKeypair: read cert %s: %w", certPath, err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("auth.loadSPKeypair: read key %s: %w", keyPath, err)
	}
	return parseSPKeypair(certPEM, keyPEM)
}

func parseSPKeypair(certPEM, keyPEM []byte) (*x509.Certificate, crypto.Signer, error) {
	cblock, _ := pem.Decode(certPEM)
	if cblock == nil {
		return nil, nil, errors.New("auth.loadSPKeypair: no PEM block in certificate")
	}
	cert, err := x509.ParseCertificate(cblock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("auth.loadSPKeypair: parse certificate: %w", err)
	}

	kblock, _ := pem.Decode(keyPEM)
	if kblock == nil {
		return nil, nil, errors.New("auth.loadSPKeypair: no PEM block in key")
	}
	key, err := parsePrivateKey(kblock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, nil, fmt.Errorf("auth.loadSPKeypair: key type %T does not implement crypto.Signer", key)
	}
	return cert, signer, nil
}

func parsePrivateKey(der []byte) (any, error) {
	if k, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return k, nil
	}
	if k, err := x509.ParseECPrivateKey(der); err == nil {
		return k, nil
	}
	return nil, errors.New("auth.loadSPKeypair: unsupported private key format (want PKCS#8, PKCS#1, or SEC1)")
}
