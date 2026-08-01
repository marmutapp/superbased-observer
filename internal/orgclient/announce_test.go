package orgclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// signAnnouncementDoc builds a genuinely-signed announcement document,
// the way the org server would: the signature covers the
// DOMAIN-SEPARATED, VERSION-BOUND message, never the bare body.
func signAnnouncementDoc(version int64, body string, priv ed25519.PrivateKey, pub ed25519.PublicKey) orgcontract.OrgAnnouncementDoc {
	sum := sha256.Sum256([]byte(body))
	return orgcontract.OrgAnnouncementDoc{
		Version:   version,
		Body:      body,
		BodyHash:  hex.EncodeToString(sum[:]),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, orgcontract.AnnouncementSigningMessage(version, body))),
		PublicKey: base64.StdEncoding.EncodeToString(pub),
	}
}

// signBodyOnlyDoc builds a document signed the way the RELEASED
// routing-policy rail signs (Ed25519 over the bare body bytes, no
// domain tag, no version) but shaped as an announcement document.
//
// This is the cross-rail confusion attack in one helper: the org server
// has ONE signing identity, so a captured routing-policy document is a
// genuine signature by the trusted key over attacker-chosen bytes. If
// the announcement rail verified over the body alone, this would be
// accepted as an announcement.
func signBodyOnlyDoc(version int64, body string, priv ed25519.PrivateKey, pub ed25519.PublicKey) orgcontract.OrgAnnouncementDoc {
	sum := sha256.Sum256([]byte(body))
	return orgcontract.OrgAnnouncementDoc{
		Version:   version,
		Body:      body,
		BodyHash:  hex.EncodeToString(sum[:]),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(body))),
		PublicKey: base64.StdEncoding.EncodeToString(pub),
	}
}

// announcementServer serves GET /api/agent/announcement over a mutable
// doc pointer (nil = 404) and counts requests.
type announcementServer struct {
	srv  *httptest.Server
	doc  *orgcontract.OrgAnnouncementDoc
	raw  string // when non-empty, served verbatim INSTEAD of doc
	hits int
	auth string
}

func newAnnouncementServer(t *testing.T) *announcementServer {
	t.Helper()
	as := &announcementServer{}
	as.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		as.hits++
		as.auth = r.Header.Get("Authorization")
		if r.URL.Path != "/api/agent/announcement" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if as.raw != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(as.raw))
			return
		}
		if as.doc == nil {
			writeTestJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		writeTestJSON(w, http.StatusOK, as.doc)
	}))
	t.Cleanup(as.srv.Close)
	return as
}

const testAnnBody = `{"id":"2026-08-01-maint","severity":"notice","title":"Fleet maintenance",` +
	`"body":"Build cluster down until 14:00 UTC.","expires_at":"2030-01-01T00:00:00Z","source":"org"}`

// TestFetchOrgAnnouncement_CachesAndShortCircuits covers the happy arc:
// a verified doc is cached with the TOFU-pinned key, and re-fetching the
// SAME version writes nothing (the monotonic short-circuit is what makes
// riding the push cycle cheap).
func TestFetchOrgAnnouncement_CachesAndShortCircuits(t *testing.T) {
	ctx := context.Background()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	as := newAnnouncementServer(t)
	c, s, _ := enrolledClient(t, as.srv.URL)

	doc := signAnnouncementDoc(1, testAnnBody, priv, pub)
	as.doc = &doc

	changed, err := c.FetchOrgAnnouncement(ctx)
	if err != nil || !changed {
		t.Fatalf("first fetch: changed=%v err=%v", changed, err)
	}
	wantAuth := "Bear" + "er " + "bearer-xyz"
	if as.auth != wantAuth {
		t.Errorf("Authorization = %q, want %q (the enrolment credential)", as.auth, wantAuth)
	}
	row, ok, err := s.GetOrgAnnouncement(ctx)
	if err != nil || !ok {
		t.Fatalf("GetOrgAnnouncement: ok=%v err=%v", ok, err)
	}
	if row.Version != 1 || row.Body != testAnnBody {
		t.Fatalf("cached row = %+v", row)
	}
	if row.ServerPubkey != doc.PublicKey {
		t.Errorf("pinned key = %q, want the served key (TOFU on first receipt)", row.ServerPubkey)
	}

	// Same version again → no-op.
	changed, err = c.FetchOrgAnnouncement(ctx)
	if err != nil || changed {
		t.Fatalf("second fetch of the same version: changed=%v err=%v", changed, err)
	}

	// New version → cached.
	doc2 := signAnnouncementDoc(2, testAnnBody, priv, pub)
	as.doc = &doc2
	changed, err = c.FetchOrgAnnouncement(ctx)
	if err != nil || !changed {
		t.Fatalf("third fetch (v2): changed=%v err=%v", changed, err)
	}
	row, _, _ = s.GetOrgAnnouncement(ctx)
	if row.Version != 2 {
		t.Errorf("cached version = %d, want 2", row.Version)
	}
}

// TestFetchOrgAnnouncement_RefusesKeyChange pins the TOFU refusal: a
// server that starts signing with a DIFFERENT key is refused loudly and
// the previously-verified cache is left untouched. This is the leg that
// stops a compromised/substituted server from pushing arbitrary banner
// text to a fleet.
func TestFetchOrgAnnouncement_RefusesKeyChange(t *testing.T) {
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	as := newAnnouncementServer(t)
	c, s, _ := enrolledClient(t, as.srv.URL)

	doc := signAnnouncementDoc(1, testAnnBody, priv, pub)
	as.doc = &doc
	if _, err := c.FetchOrgAnnouncement(ctx); err != nil {
		t.Fatalf("first fetch: %v", err)
	}

	evil := signAnnouncementDoc(2, `{"id":"evil","severity":"critical","title":"t","body":"b","expires_at":"2030-01-01T00:00:00Z","source":"org"}`, otherPriv, otherPub)
	as.doc = &evil
	changed, err := c.FetchOrgAnnouncement(ctx)
	if err == nil || changed {
		t.Fatalf("key change accepted: changed=%v err=%v", changed, err)
	}
	if !strings.Contains(err.Error(), "key CHANGED") {
		t.Errorf("error = %v, want a loud key-change refusal", err)
	}
	row, _, _ := s.GetOrgAnnouncement(ctx)
	if row.Version != 1 || row.Body != testAnnBody {
		t.Errorf("cache mutated by a refused doc: %+v", row)
	}
}

// TestFetchOrgAnnouncement_RefusesBadSignature pins that a doc whose
// signature does not verify against the pinned key never lands.
func TestFetchOrgAnnouncement_RefusesBadSignature(t *testing.T) {
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	as := newAnnouncementServer(t)
	c, s, _ := enrolledClient(t, as.srv.URL)

	doc := signAnnouncementDoc(1, testAnnBody, priv, pub)
	doc.Body = `{"id":"tampered","severity":"critical","title":"t","body":"b","expires_at":"2030-01-01T00:00:00Z","source":"org"}`
	as.doc = &doc

	if changed, err := c.FetchOrgAnnouncement(ctx); err == nil || changed {
		t.Fatalf("tampered doc accepted: changed=%v err=%v", changed, err)
	}
	if _, ok, _ := s.GetOrgAnnouncement(ctx); ok {
		t.Error("tampered doc was cached")
	}
}

// TestFetchOrgAnnouncement_NotPublished pins the 404 no-op — which is
// ALSO what an older org server (no announcement route) returns, so the
// node must treat it as "nothing to show", never as an error worth
// warning about.
func TestFetchOrgAnnouncement_NotPublished(t *testing.T) {
	ctx := context.Background()
	as := newAnnouncementServer(t)
	c, s, _ := enrolledClient(t, as.srv.URL)

	changed, err := c.FetchOrgAnnouncement(ctx)
	if err != nil || changed {
		t.Fatalf("fetch with nothing published: changed=%v err=%v", changed, err)
	}
	if _, ok, _ := s.GetOrgAnnouncement(ctx); ok {
		t.Error("a 404 wrote a cache row")
	}
}

// TestFetchOrgAnnouncement_CachesRetraction pins that an empty body is
// cached like any other version — the retraction is the mechanism by
// which a banner goes away, so dropping it would strand the fleet on
// the previous announcement forever.
func TestFetchOrgAnnouncement_CachesRetraction(t *testing.T) {
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	as := newAnnouncementServer(t)
	c, s, _ := enrolledClient(t, as.srv.URL)

	doc := signAnnouncementDoc(1, testAnnBody, priv, pub)
	as.doc = &doc
	if _, err := c.FetchOrgAnnouncement(ctx); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	retraction := signAnnouncementDoc(2, "", priv, pub)
	as.doc = &retraction
	changed, err := c.FetchOrgAnnouncement(ctx)
	if err != nil || !changed {
		t.Fatalf("retraction fetch: changed=%v err=%v", changed, err)
	}
	row, ok, _ := s.GetOrgAnnouncement(ctx)
	if !ok || row.Version != 2 || row.Body != "" {
		t.Errorf("cached retraction = %+v ok=%v", row, ok)
	}
}

// TestFetchOrgAnnouncement_NotEnrolled pins the solo-install path: no
// enrolment ⇒ ErrNotEnrolled and no request at all. PushLoop treats
// that sentinel as silence, so a solo daemon logs nothing.
func TestFetchOrgAnnouncement_NotEnrolled(t *testing.T) {
	as := newAnnouncementServer(t)
	c := newTestClient(t, newAgentStore(t), &memBearerStore{bearer: "b"})
	if _, err := c.FetchOrgAnnouncement(context.Background()); err == nil {
		t.Fatal("expected ErrNotEnrolled")
	}
	if as.hits != 0 {
		t.Errorf("hits = %d, want 0 — an unenrolled node must make no request", as.hits)
	}
}

// TestFetchOrgAnnouncement_RefusesVersionBumpedReplay is the headline
// replay regression (security finding 1). The document is BYTE-IDENTICAL
// to one the node already accepted — same body, same hash, same genuine
// signature by the pinned key — with only the version number raised.
//
// If the signature covered the body alone, this would be accepted, and
// accepting it is not cosmetic: the node's monotonic short-circuit
// (`cached.Version >= doc.Version`) would then ignore every genuine
// announcement the org ever publishes below the forged version. One
// captured document plus one integer edit freezes a fleet's banner
// permanently.
func TestFetchOrgAnnouncement_RefusesVersionBumpedReplay(t *testing.T) {
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	as := newAnnouncementServer(t)
	c, s, _ := enrolledClient(t, as.srv.URL)

	genuine := signAnnouncementDoc(1, testAnnBody, priv, pub)
	as.doc = &genuine
	if _, err := c.FetchOrgAnnouncement(ctx); err != nil {
		t.Fatalf("first fetch: %v", err)
	}

	replay := genuine // every field the server signed, unchanged…
	replay.Version = 1 << 40
	as.doc = &replay

	changed, err := c.FetchOrgAnnouncement(ctx)
	if err == nil || changed {
		t.Fatalf("version-bumped replay accepted: changed=%v err=%v", changed, err)
	}
	row, _, _ := s.GetOrgAnnouncement(ctx)
	if row.Version != 1 {
		t.Errorf("cache version = %d, want 1 — a refused replay must not touch the cache", row.Version)
	}
}

// TestFetchOrgAnnouncement_RefusesBodyOnlySignature pins the cross-rail
// half of finding 1: a document signed the way the RELEASED
// routing-policy rail signs (bare body bytes, no domain tag) must not
// verify on the announcement rail, even though ONE org key signs both.
//
// Concretely: an attacker who captures any signed routing-policy
// document holds a genuine signature by the trusted key over bytes of
// their choosing. Without domain separation, presenting it here is a
// fleet-wide banner injection with no key compromise at all.
func TestFetchOrgAnnouncement_RefusesBodyOnlySignature(t *testing.T) {
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	as := newAnnouncementServer(t)
	c, s, _ := enrolledClient(t, as.srv.URL)

	crossRail := signBodyOnlyDoc(1, testAnnBody, priv, pub)
	as.doc = &crossRail

	changed, err := c.FetchOrgAnnouncement(ctx)
	if err == nil || changed {
		t.Fatalf("body-only (routing-rail-shaped) signature accepted: changed=%v err=%v", changed, err)
	}
	if _, ok, _ := s.GetOrgAnnouncement(ctx); ok {
		t.Error("a cross-rail signature wrote a cache row")
	}
}

// TestFetchOrgAnnouncement_RefusesReplayedRetraction pins the third
// replay shape from finding 1, and the one with the most operational
// bite: a captured RETRACTION is a signed instruction to show nothing.
// Replayed at a bumped version it would silently clear whatever the org
// is currently announcing — including a critical security advisory —
// with no key compromise and no server access.
func TestFetchOrgAnnouncement_RefusesReplayedRetraction(t *testing.T) {
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	as := newAnnouncementServer(t)
	c, s, _ := enrolledClient(t, as.srv.URL)

	// v1 announcement → v2 retraction (captured here) → v3 announcement.
	first := signAnnouncementDoc(1, testAnnBody, priv, pub)
	as.doc = &first
	if _, err := c.FetchOrgAnnouncement(ctx); err != nil {
		t.Fatalf("v1 fetch: %v", err)
	}
	capturedRetraction := signAnnouncementDoc(2, "", priv, pub)
	as.doc = &capturedRetraction
	if _, err := c.FetchOrgAnnouncement(ctx); err != nil {
		t.Fatalf("v2 retraction fetch: %v", err)
	}
	advisory := signAnnouncementDoc(3, testAnnBody, priv, pub)
	as.doc = &advisory
	if _, err := c.FetchOrgAnnouncement(ctx); err != nil {
		t.Fatalf("v3 fetch: %v", err)
	}

	replay := capturedRetraction
	replay.Version = 99
	as.doc = &replay
	changed, err := c.FetchOrgAnnouncement(ctx)
	if err == nil || changed {
		t.Fatalf("replayed retraction accepted: changed=%v err=%v", changed, err)
	}
	row, _, _ := s.GetOrgAnnouncement(ctx)
	if row.Version != 3 || row.Body != testAnnBody {
		t.Errorf("cache = {v%d %q}, want the v3 announcement — a replayed retraction must not clear the banner",
			row.Version, row.Body)
	}
}

// TestFetchOrgAnnouncement_RefusesTrailingBytes pins finding 5 on the
// node side: the 1 MiB LimitReader capped the READ, not the document,
// and json.Decode stops at the end of the first value — so a response
// carrying a second document (or padding) after the first decoded
// "successfully". The rail accepts exactly one document.
func TestFetchOrgAnnouncement_RefusesTrailingBytes(t *testing.T) {
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	as := newAnnouncementServer(t)
	c, s, _ := enrolledClient(t, as.srv.URL)

	doc := signAnnouncementDoc(1, testAnnBody, priv, pub)
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	// A well-formed doc, then a second one. The first is genuinely
	// signed, so ONLY the trailing-bytes check can refuse this.
	as.raw = string(encoded) + string(encoded)

	changed, ferr := c.FetchOrgAnnouncement(ctx)
	if ferr == nil || changed {
		t.Fatalf("document with trailing bytes accepted: changed=%v err=%v", changed, ferr)
	}
	if !strings.Contains(ferr.Error(), "trailing bytes") {
		t.Errorf("error = %v, want a trailing-bytes refusal", ferr)
	}
	if _, ok, _ := s.GetOrgAnnouncement(ctx); ok {
		t.Error("a multi-document response wrote a cache row")
	}
}

// TestFetchOrgAnnouncement_AcceptsTrailingNewline is the false-positive
// guard for the test above: the org server writes with json.Encoder,
// which appends "\n". Trailing WHITESPACE must stay fine, or finding
// 5's fix would break the rail it hardens.
func TestFetchOrgAnnouncement_AcceptsTrailingNewline(t *testing.T) {
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	as := newAnnouncementServer(t)
	c, s, _ := enrolledClient(t, as.srv.URL)

	doc := signAnnouncementDoc(1, testAnnBody, priv, pub)
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	as.raw = string(encoded) + "\n"

	changed, ferr := c.FetchOrgAnnouncement(ctx)
	if ferr != nil || !changed {
		t.Fatalf("trailing newline refused: changed=%v err=%v", changed, ferr)
	}
	if _, ok, _ := s.GetOrgAnnouncement(ctx); !ok {
		t.Error("no cache row written")
	}
}
