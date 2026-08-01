package organnounce

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/announce"
	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
	"github.com/marmutapp/superbased-observer/internal/orgserver/routingpolicy"
)

// openDB opens a throwaway org-server database with every migration
// applied (including 022, which creates the announcement tables).
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := orgdb.Open(context.Background(), orgdb.Options{Path: filepath.Join(t.TempDir(), "org.db")})
	if err != nil {
		t.Fatalf("orgdb.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

// validBody is one well-formed §1 announcement document.
func validBody(t *testing.T, id string) string {
	t.Helper()
	body, err := announce.Encode([]announce.Announcement{{
		ID:        id,
		Severity:  announce.SeverityNotice,
		Title:     "Fleet maintenance",
		Body:      "The build cluster is down for maintenance until 14:00 UTC.",
		ExpiresAt: "2030-01-01T00:00:00Z",
		Source:    announce.SourceOrg,
	}})
	if err != nil {
		t.Fatalf("announce.Encode: %v", err)
	}
	return body
}

// TestPublishLatestVerify pins the server arc: publish validates +
// versions + signs + audits; Latest serves the doc; Verify accepts the
// genuine signature and rejects tampering.
func TestPublishLatestVerify(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := openDB(t)

	doc, err := Publish(ctx, database, validBody(t, "2026-08-01-maint"), "admin@acme")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if doc.Version != 1 || doc.Signature == "" || doc.PublicKey == "" || doc.BodyHash == "" {
		t.Fatalf("doc = %+v", doc)
	}

	doc2, err := Publish(ctx, database, validBody(t, "2026-08-02-maint"), "admin@acme")
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
	tampered.Body = validBody(t, "evil-injected")
	if err := Verify(tampered, latest.PublicKey); err == nil {
		t.Error("tampered body verified")
	}

	var n int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM org_announcement_audit WHERE action='publish'`).Scan(&n); err != nil || n != 2 {
		t.Errorf("audit rows = %d err=%v, want 2", n, err)
	}
}

// TestSigningKeyIsSharedWithRoutingPolicy pins the reuse decision: one
// org server has ONE distribution signing identity, so an agent that
// TOFU-pinned the key on the routing rail sees the same key here. A
// second key would silently mean a second pin and a second rotation
// story.
func TestSigningKeyIsSharedWithRoutingPolicy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := openDB(t)

	ann, err := Publish(ctx, database, validBody(t, "2026-08-01-x"), "admin@acme")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	pol, err := routingpolicy.Publish(ctx, database, "[routing]\n", "admin@acme")
	if err != nil {
		t.Fatalf("routingpolicy.Publish: %v", err)
	}
	if ann.PublicKey != pol.PublicKey {
		t.Errorf("announcement key %q != routing key %q — the rails must share one signing identity",
			ann.PublicKey, pol.PublicKey)
	}
}

// TestRetractionIsAPublishedEmptyDocument pins the retraction shape: an
// empty body is signed, versioned, audited as "retract", and served by
// Latest with ok=true. A node MUST receive it (and bump its cached
// version) to clear the banner it is currently showing, which a delete
// could never achieve given the monotonic-version short-circuit.
func TestRetractionIsAPublishedEmptyDocument(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := openDB(t)

	if _, err := Publish(ctx, database, validBody(t, "2026-08-01-x"), "admin@acme"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	retracted, err := Publish(ctx, database, "", "admin@acme")
	if err != nil {
		t.Fatalf("retract: %v", err)
	}
	if retracted.Version != 2 || retracted.Body != "" || retracted.Signature == "" {
		t.Fatalf("retraction doc = %+v", retracted)
	}
	if err := Verify(retracted, retracted.PublicKey); err != nil {
		t.Errorf("retraction failed verification: %v", err)
	}
	latest, ok, err := Latest(ctx, database)
	if err != nil || !ok || latest.Version != 2 || latest.Body != "" {
		t.Fatalf("Latest after retraction = %+v ok=%v err=%v", latest, ok, err)
	}
	var action string
	if err := database.QueryRowContext(ctx, `SELECT action FROM org_announcement_audit WHERE version=2`).Scan(&action); err != nil {
		t.Fatalf("audit read: %v", err)
	}
	if action != "retract" {
		t.Errorf("audit action = %q, want retract", action)
	}
}

// TestValidateBodyRejections is the gate that matters most: nothing
// invalid may ever be signed, so every refusal leg is enumerated.
func TestValidateBodyRejections(t *testing.T) {
	t.Parallel()
	good := validBody(t, "2026-08-01-ok")
	tests := []struct {
		name string
		body string
	}{
		{"not JSON at all", "just some text"},
		{"object with no expiry", `{"id":"x","severity":"info","title":"t","body":"b","source":"org"}`},
		{"unknown severity", `{"id":"x","severity":"urgent","title":"t","body":"b","expires_at":"2030-01-01T00:00:00Z","source":"org"}`},
		{"http url", `{"id":"x","severity":"info","title":"t","body":"b","url":"http://x.example","expires_at":"2030-01-01T00:00:00Z","source":"org"}`},
		{"source not org", `{"id":"x","severity":"info","title":"t","body":"b","expires_at":"2030-01-01T00:00:00Z","source":"release"}`},
		{"newline in body", `{"id":"x","severity":"info","title":"t","body":"line1\nline2","expires_at":"2030-01-01T00:00:00Z","source":"org"}`},
		{"duplicate ids in array", "[" + good + "," + good + "]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ValidateBody(tc.body); !errors.Is(err, ErrInvalidBody) {
				t.Errorf("ValidateBody accepted (or wrong error class): %v", err)
			}
		})
	}
}

// TestPublishRefusesInvalidBodyBeforeSigning pins the ordering: a
// refused publish leaves NO row, NO audit row — and crucially no
// signature over a body the fleet would have to drop.
func TestPublishRefusesInvalidBodyBeforeSigning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := openDB(t)

	if _, err := Publish(ctx, database, `{"id":"x"}`, "admin@acme"); !errors.Is(err, ErrInvalidBody) {
		t.Fatalf("Publish accepted an invalid body: %v", err)
	}
	for _, table := range []string{"org_announcements", "org_announcement_audit"} {
		var n int
		if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s rows = %d err=%v, want 0", table, n, err)
		}
	}
}

// TestValidateBodyAcceptsArray pins the forward-compatible shape: a
// JSON array of announcements is accepted from day one so a multi-
// announcement composer later needs no wire change.
func TestValidateBodyAcceptsArray(t *testing.T) {
	t.Parallel()
	body := "[" + validBody(t, "a-one") + "," + validBody(t, "b-two") + "]"
	list, err := ValidateBody(body)
	if err != nil {
		t.Fatalf("ValidateBody(array): %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("decoded %d announcements, want 2", len(list))
	}
}

// TestLatestEmpty pins the no-announcement-yet shape: ok=false, no
// error — the handler turns this into a 404, never a 500.
func TestLatestEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doc, ok, err := Latest(ctx, openDB(t))
	if err != nil {
		t.Fatalf("Latest on empty registry: %v", err)
	}
	if ok || doc.Version != 0 {
		t.Errorf("Latest = %+v ok=%v, want ok=false zero doc", doc, ok)
	}
}

// TestVerifyRejectsWrongKey pins the TOFU refusal leg the node depends
// on: a well-formed but DIFFERENT pinned key must not verify.
func TestVerifyRejectsWrongKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doc, err := Publish(ctx, openDB(t), validBody(t, "2026-08-01-k"), "admin@acme")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	other := base64.StdEncoding.EncodeToString(make([]byte, 32))
	if err := Verify(doc, other); err == nil {
		t.Error("Verify accepted a wrong pinned key")
	}
}

// TestVerifyRefusesVersionBumpedReplay is security finding 1 at the
// verification core. The document is exactly the one the server signed —
// same body, same hash, same signature — with the version raised, which
// is all an eavesdropper needs to freeze (or clear) a fleet's banner
// through the node's monotonic-version short-circuit.
func TestVerifyRefusesVersionBumpedReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doc, err := Publish(ctx, openDB(t), validBody(t, "2026-08-01-v"), "admin@acme")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := Verify(doc, doc.PublicKey); err != nil {
		t.Fatalf("genuine doc failed verification: %v", err)
	}
	for _, v := range []int64{doc.Version + 1, doc.Version - 1, 1 << 40} {
		replay := doc
		replay.Version = v
		if err := Verify(replay, doc.PublicKey); err == nil {
			t.Errorf("Verify accepted the same signed document at version %d", v)
		}
	}
}

// TestVerifyRefusesBodyOnlySignature is the cross-rail half of finding
// 1. The org server has ONE distribution signing identity (shared with
// the routing-policy rail by design), so the only thing keeping a
// captured routing-policy document from being served as an announcement
// is that this rail's signature covers a domain tag the other rail's
// does not.
func TestVerifyRefusesBodyOnlySignature(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := openDB(t)
	body := validBody(t, "2026-08-01-x")

	// A genuine routing-policy publish: same DB, same key, signature
	// over the bare body bytes. Its body is announcement JSON here to
	// make the confusion attack as easy as it can possibly be.
	pol, err := routingpolicy.Publish(ctx, database, "# not toml relevant\n", "admin@acme")
	if err != nil {
		t.Fatalf("routingpolicy.Publish: %v", err)
	}
	priv, pub, err := routingpolicy.SigningKey(ctx, database)
	if err != nil {
		t.Fatalf("SigningKey: %v", err)
	}
	sum := sha256.Sum256([]byte(body))
	crossRail := orgcontract.OrgAnnouncementDoc{
		Version:   1,
		Body:      body,
		BodyHash:  hex.EncodeToString(sum[:]),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(body))),
		PublicKey: base64.StdEncoding.EncodeToString(pub),
	}
	if crossRail.PublicKey != pol.PublicKey {
		t.Fatalf("test setup: the rails are not sharing a key (%q vs %q)", crossRail.PublicKey, pol.PublicKey)
	}
	if err := Verify(crossRail, crossRail.PublicKey); err == nil {
		t.Error("Verify accepted a body-only (routing-rail-shaped) signature by the org's own key")
	}
}

// TestSignatureBindsTheVersionItWasPublishedAt pins the mechanism the
// two tests above depend on: each published version carries a DIFFERENT
// signature over an unchanged body. Equal signatures would mean the
// version is not in the signed message at all.
func TestSignatureBindsTheVersionItWasPublishedAt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := openDB(t)
	body := validBody(t, "2026-08-01-same")

	v1, err := Publish(ctx, database, body, "admin@acme")
	if err != nil {
		t.Fatalf("Publish v1: %v", err)
	}
	v2, err := Publish(ctx, database, body, "admin@acme")
	if err != nil {
		t.Fatalf("Publish v2: %v", err)
	}
	if v1.BodyHash != v2.BodyHash {
		t.Fatal("test setup: the bodies differ")
	}
	if v1.Signature == v2.Signature {
		t.Error("the same signature covers two versions — the version is not signed")
	}
	if err := Verify(v2, v2.PublicKey); err != nil {
		t.Errorf("v2 failed verification: %v", err)
	}
}

// TestPublishRefusesAmbiguousRetractionSpellings is security finding 6
// at the publish gate: "[]" and "null" are not retractions. The empty
// body is, and it is the only one — so a retraction has one hash and
// one signature no matter who authored it.
func TestPublishRefusesAmbiguousRetractionSpellings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := openDB(t)

	for _, body := range []string{"[]", "null", " [] "} {
		if _, err := Publish(ctx, database, body, "admin@acme"); !errors.Is(err, ErrInvalidBody) {
			t.Errorf("Publish(%q) err = %v, want ErrInvalidBody", body, err)
		}
	}
	// The one true retraction still publishes.
	doc, err := Publish(ctx, database, "", "admin@acme")
	if err != nil || doc.Body != "" {
		t.Fatalf("empty-body retraction refused: %+v err=%v", doc, err)
	}
	var n int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM org_announcements`).Scan(&n); err != nil || n != 1 {
		t.Errorf("rows = %d err=%v, want 1 (only the real retraction landed)", n, err)
	}
}
