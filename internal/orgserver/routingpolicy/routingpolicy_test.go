package routingpolicy

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
)

// TestPublishLatestVerify pins the §R19.1/§R19.2 server arc: publish
// validates + versions + signs + audits; Latest serves the doc; Verify
// accepts the genuine signature and rejects tampering; garbage TOML is
// refused at the door.
func TestPublishLatestVerify(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, err := orgdb.Open(ctx, orgdb.Options{Path: filepath.Join(t.TempDir(), "org.db")})
	if err != nil {
		t.Fatalf("orgdb.Open: %v", err)
	}
	defer database.Close()

	body := "[routing]\n[[routing.privacy.rules]]\nproject = \"eu\"\nlocal_only = true\n"
	doc, err := Publish(ctx, database, body, "admin@acme")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if doc.Version != 1 || doc.Signature == "" || doc.PublicKey == "" || doc.BodyHash == "" {
		t.Fatalf("doc = %+v", doc)
	}

	if _, err := Publish(ctx, database, "not = = toml", "admin@acme"); err == nil {
		t.Error("garbage TOML accepted")
	}

	doc2, err := Publish(ctx, database, body+"# v2\n", "admin@acme")
	if err != nil || doc2.Version != 2 {
		t.Fatalf("second publish: %+v err=%v", doc2, err)
	}

	latest, ok, err := Latest(ctx, database)
	if err != nil || !ok || latest.Version != 2 {
		t.Fatalf("Latest = %+v ok=%v err=%v", latest, ok, err)
	}
	if err := Verify(latest, latest.PublicKey); err != nil {
		t.Errorf("genuine doc failed verification: %v", err)
	}
	tampered := latest
	tampered.Body += "\n# evil"
	if err := Verify(tampered, latest.PublicKey); err == nil {
		t.Error("tampered body verified")
	}

	// Audit rows recorded.
	var n int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM routing_policy_audit`).Scan(&n); err != nil || n != 2 {
		t.Errorf("audit rows = %d err=%v, want 2", n, err)
	}
}

// TestLatestEmpty pins the no-policy-yet shape: ok=false, no error —
// the handler turns this into a 404, never a 500.
func TestLatestEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, err := orgdb.Open(ctx, orgdb.Options{Path: filepath.Join(t.TempDir(), "org.db")})
	if err != nil {
		t.Fatalf("orgdb.Open: %v", err)
	}
	defer database.Close()

	doc, ok, err := Latest(ctx, database)
	if err != nil {
		t.Fatalf("Latest on empty registry: %v", err)
	}
	if ok || doc.Version != 0 {
		t.Errorf("Latest = %+v ok=%v, want ok=false zero doc", doc, ok)
	}
}

// TestVerifyRejectsBadInputs pins each Verify refusal leg: undecodable
// or wrong-size pinned key, undecodable signature, and a wrong (but
// well-formed) key — the TOFU pin's key-change refusal path.
func TestVerifyRejectsBadInputs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, err := orgdb.Open(ctx, orgdb.Options{Path: filepath.Join(t.TempDir(), "org.db")})
	if err != nil {
		t.Fatalf("orgdb.Open: %v", err)
	}
	defer database.Close()

	doc, err := Publish(ctx, database, "[routing]\n", "admin@acme")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	otherKey := base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))
	badSig := doc
	badSig.Signature = "%%not-base64%%"

	tests := []struct {
		name string
		doc  orgcontract.RoutingPolicyDoc
		pin  string
	}{
		{"pinned key not base64", doc, "%%not-base64%%"},
		{"pinned key wrong size", doc, base64.StdEncoding.EncodeToString([]byte("short"))},
		{"signature not base64", badSig, doc.PublicKey},
		{"wrong pinned key", doc, otherKey},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := Verify(tc.doc, tc.pin); err == nil {
				t.Error("Verify accepted, want refusal")
			}
		})
	}
}
