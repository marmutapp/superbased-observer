package orgcontract

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
)

func testGrant() EnrolmentGrant {
	return EnrolmentGrant{
		OrgID:        "org-1",
		OrgServerURL: "https://org.example.com",
		KeyPinSHA256: "deadbeef",
		Authority:    []string{"dashboard.visibility"},
		GrantedAt:    "2026-08-15T10:00:00Z",
		ExpiresAt:    "2026-09-14T10:00:00Z",
	}
}

func TestEnrolmentGrantSignVerify(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	g := testGrant()
	g.Signature = SignEnrolmentGrant(priv, g)
	if err := VerifyEnrolmentGrant(g, pub); err != nil {
		t.Fatalf("VerifyEnrolmentGrant: %v", err)
	}

	// Authority order must not change the signature: the message is built
	// over the canonical (sorted, deduplicated) token list.
	g2 := g
	g2.Authority = []string{"settings.pin", "dashboard.visibility"}
	g2.Signature = SignEnrolmentGrant(priv, g2)
	g3 := g2
	g3.Authority = []string{"dashboard.visibility", "settings.pin", "dashboard.visibility"}
	if err := VerifyEnrolmentGrant(g3, pub); err != nil {
		t.Fatalf("reordered/duplicated authority broke verification: %v", err)
	}
}

// TestEnrolmentGrantTamperDetected walks every semantic field: a grant that
// claims MORE authority (or a different key pin, org, or TTL) than the org
// signed must fail. This is the evidence property the whole consent story
// rests on.
func TestEnrolmentGrantTamperDetected(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	base := testGrant()
	base.Signature = SignEnrolmentGrant(priv, base)

	mutate := map[string]func(*EnrolmentGrant){
		"widened authority": func(g *EnrolmentGrant) {
			g.Authority = append(g.Authority, "capture.raise")
		},
		"different org":     func(g *EnrolmentGrant) { g.OrgID = "org-2" },
		"different server":  func(g *EnrolmentGrant) { g.OrgServerURL = "https://evil.example" },
		"different key pin": func(g *EnrolmentGrant) { g.KeyPinSHA256 = "cafe" },
		"extended TTL":      func(g *EnrolmentGrant) { g.ExpiresAt = "2099-01-01T00:00:00Z" },
		"backdated grant":   func(g *EnrolmentGrant) { g.GrantedAt = "2020-01-01T00:00:00Z" },
	}
	for name, m := range mutate {
		t.Run(name, func(t *testing.T) {
			g := base
			g.Authority = append([]string(nil), base.Authority...)
			m(&g)
			if err := VerifyEnrolmentGrant(g, pub); err == nil {
				t.Fatalf("tampered grant (%s) verified", name)
			}
		})
	}

	t.Run("wrong key", func(t *testing.T) {
		otherPub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("keygen: %v", err)
		}
		if err := VerifyEnrolmentGrant(base, otherPub); err == nil {
			t.Fatal("grant verified under a key it was not signed with")
		}
	})

	t.Run("no key at all", func(t *testing.T) {
		err := VerifyEnrolmentGrant(base, nil)
		if err == nil || !strings.Contains(err.Error(), "no org policy public key") {
			t.Fatalf("err = %v, want a NAMED no-key error (a silent skip would enrol a node with unverified governance)", err)
		}
	})

	t.Run("no signature", func(t *testing.T) {
		g := base
		g.Signature = ""
		if err := VerifyEnrolmentGrant(g, pub); err == nil {
			t.Fatal("unsigned grant verified")
		}
	})
}

// TestEnrolmentGrantSigningIsDomainSeparated proves a grant signature cannot
// be replayed from another rail signed by the same org key.
func TestEnrolmentGrantSigningIsDomainSeparated(t *testing.T) {
	g := testGrant()
	msg := string(EnrolmentGrantSigningMessage(g))
	if msg == string(PolicyBundleSigningMessage(1, []byte(g.OrgID))) {
		t.Fatal("grant and policy-bundle signing messages collide")
	}
	if msg == string(AnnouncementSigningMessage(1, g.OrgID)) {
		t.Fatal("grant and announcement signing messages collide")
	}
}
