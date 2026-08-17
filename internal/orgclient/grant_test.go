package orgclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// The enrolment-grant accept gates (admin-controlled Plane B §2.3,
// adversarial review A3): a grant is authority, so every gate REFUSES rather
// than degrades, and a refusal enrols the node ungoverned instead of storing
// unverified authority. Enroll never writes the grant at all — it RETURNS a
// verified offer for cmd/observer/org.go to confirm and store (A4).

func grantEnrolServer(t *testing.T, key string, grant *orgcontract.EnrolmentGrant) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, http.StatusOK, orgcontract.EnrollResponse{
			Bearer: "bearer-xyz", BearerExpiresAt: "2026-08-23T00:00:00Z",
			OrgID: "org-1", OrgName: "Acme", UserID: "scim-42", UserEmail: "dev@acme.example",
			OrgPolicyPublicKey: key,
			Grant:              grant,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func signedGrant(t *testing.T, priv ed25519.PrivateKey, keyPin, orgURL string) *orgcontract.EnrolmentGrant {
	t.Helper()
	g := orgcontract.EnrolmentGrant{
		OrgID:        "org-1",
		OrgServerURL: orgURL,
		KeyPinSHA256: keyPin,
		Authority:    []string{"dashboard.visibility"},
		GrantedAt:    time.Now().UTC().Format(time.RFC3339),
		ExpiresAt:    time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339),
	}
	g.Signature = orgcontract.SignEnrolmentGrant(priv, g)
	return &g
}

func TestEnrollAcceptsAVerifiedGrant(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	keyB64 := base64.RawURLEncoding.EncodeToString(pub)
	keyPin := orgcontract.PublicKeyPinHash(pub)

	// The server URL is only known once the server exists, and the grant is
	// signed over it, so mint the grant lazily through a pointer the handler
	// closes over.
	var grant *orgcontract.EnrolmentGrant
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, http.StatusOK, orgcontract.EnrollResponse{
			Bearer: "bearer-xyz", BearerExpiresAt: "2026-08-23T00:00:00Z",
			OrgID: "org-1", OrgName: "Acme", UserID: "scim-42", UserEmail: "dev@acme.example",
			OrgPolicyPublicKey: keyB64, Grant: grant,
		})
	}))
	defer srv.Close()
	grant = signedGrant(t, priv, keyPin, srv.URL)

	s := newAgentStore(t)
	c := newTestClient(t, s, &memBearerStore{})
	_, offer, err := c.Enroll(context.Background(), srv.URL, "tok_id.secret")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if offer == nil {
		t.Fatal("Enroll returned no grant offer for a correctly signed, correctly pinned grant")
	}
	if offer.KeyPinSHA256 != keyPin {
		t.Fatalf("offer.KeyPinSHA256 = %q, want the pin recorded during this enrolment (%q)", offer.KeyPinSHA256, keyPin)
	}
	if offer.Generation <= 0 || offer.OrgKey == "" {
		t.Fatalf("offer identity = gen %d org_key %q, want the live enrolment identity", offer.Generation, offer.OrgKey)
	}
	if offer.ReceiptHash == "" {
		t.Fatal("offer carries no receipt hash — `observer org grant show` has nothing to print")
	}

	// Enroll must NOT have written it: the confirmation lives in cmd.
	if _, ok, err := s.LoadEnrolmentGrant(context.Background(), offer.OrgKey); err != nil || ok {
		t.Fatalf("Enroll wrote the grant itself (ok=%v err=%v) — orgclient has no TTY and cannot obtain consent", ok, err)
	}
}

// TestEnrollRefusesUngroundedGrants is the mutation-killing table: each row
// is a way a grant could be authority bound to nothing, and every one must
// enrol the node UNGOVERNED.
func TestEnrollRefusesUngroundedGrants(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	otherPub, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	keyB64 := base64.RawURLEncoding.EncodeToString(pub)
	keyPin := orgcontract.PublicKeyPinHash(pub)

	cases := []struct {
		name string
		key  string // org_policy_public_key on the response
		mut  func(g *orgcontract.EnrolmentGrant, priv ed25519.PrivateKey, orgURL string)
	}{
		{
			// The A3 case: a server that mints a grant but sends no key.
			// Without an explicit refusal the node would hold authority it
			// can never bind to a signing identity.
			name: "no org policy public key at all",
			key:  "",
			mut:  func(*orgcontract.EnrolmentGrant, ed25519.PrivateKey, string) {},
		},
		{
			name: "grant names a different key than the one pinned",
			key:  keyB64,
			mut: func(g *orgcontract.EnrolmentGrant, priv ed25519.PrivateKey, _ string) {
				g.KeyPinSHA256 = orgcontract.PublicKeyPinHash(otherPub)
				g.Signature = orgcontract.SignEnrolmentGrant(priv, *g)
			},
		},
		{
			name: "signature minted by a different key",
			key:  keyB64,
			mut: func(g *orgcontract.EnrolmentGrant, _ ed25519.PrivateKey, _ string) {
				g.Signature = orgcontract.SignEnrolmentGrant(otherPriv, *g)
			},
		},
		{
			name: "authority widened after signing",
			key:  keyB64,
			mut: func(g *orgcontract.EnrolmentGrant, _ ed25519.PrivateKey, _ string) {
				g.Authority = append(g.Authority, "capture.raise")
			},
		},
		{
			name: "grant names a different org",
			key:  keyB64,
			mut: func(g *orgcontract.EnrolmentGrant, priv ed25519.PrivateKey, _ string) {
				g.OrgID = "org-2"
				g.Signature = orgcontract.SignEnrolmentGrant(priv, *g)
			},
		},
		{
			name: "grant names a different server",
			key:  keyB64,
			mut: func(g *orgcontract.EnrolmentGrant, priv ed25519.PrivateKey, _ string) {
				g.OrgServerURL = "https://evil.example"
				g.Signature = orgcontract.SignEnrolmentGrant(priv, *g)
			},
		},
		{
			name: "already expired",
			key:  keyB64,
			mut: func(g *orgcontract.EnrolmentGrant, priv ed25519.PrivateKey, _ string) {
				g.ExpiresAt = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
				g.Signature = orgcontract.SignEnrolmentGrant(priv, *g)
			},
		},
		{
			name: "unsigned",
			key:  keyB64,
			mut: func(g *orgcontract.EnrolmentGrant, _ ed25519.PrivateKey, _ string) {
				g.Signature = ""
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var grant *orgcontract.EnrolmentGrant
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeTestJSON(w, http.StatusOK, orgcontract.EnrollResponse{
					Bearer: "bearer-xyz", BearerExpiresAt: "2026-08-23T00:00:00Z",
					OrgID: "org-1", OrgName: "Acme", UserID: "scim-42", UserEmail: "dev@acme.example",
					OrgPolicyPublicKey: tc.key, Grant: grant,
				})
			}))
			defer srv.Close()
			g := signedGrant(t, priv, keyPin, srv.URL)
			tc.mut(g, priv, srv.URL)
			grant = g

			c := newTestClient(t, newAgentStore(t), &memBearerStore{})
			enr, offer, err := c.Enroll(context.Background(), srv.URL, "tok_id.secret")
			if err != nil {
				t.Fatalf("Enroll must still SUCCEED (ungoverned), got: %v", err)
			}
			if enr == nil {
				t.Fatal("Enroll returned no enrolment")
			}
			if offer != nil {
				t.Fatalf("Enroll accepted a grant it should have refused (%s) — the node would hold authority bound to nothing", tc.name)
			}
		})
	}
}

// TestEnrollWithoutGrantIsUnchanged pins the compat direction: a
// pre-governance server (no grant key at all) enrols exactly as before.
func TestEnrollWithoutGrantIsUnchanged(t *testing.T) {
	srv := grantEnrolServer(t, "", nil)
	c := newTestClient(t, newAgentStore(t), &memBearerStore{})
	enr, offer, err := c.Enroll(context.Background(), srv.URL, "tok_id.secret")
	if err != nil || enr == nil {
		t.Fatalf("Enroll: enr=%v err=%v", enr, err)
	}
	if offer != nil {
		t.Fatal("a server that offered no grant produced an offer")
	}
}
