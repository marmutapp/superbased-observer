package orgclient

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// signedRoutingDocV2 builds a document carrying BOTH signature rails,
// exactly as the org server's Publish mints one since server migration 078.
// Every attack below starts from a document the org GENUINELY signed — the
// guard, not the setup, is what must refuse it.
func signedRoutingDocV2(priv ed25519.PrivateKey, pub ed25519.PublicKey, version int64, body string) orgcontract.RoutingPolicyDoc {
	doc := signedRoutingDoc(priv, pub, version, body)
	doc.SignatureV2 = orgcontract.SignRoutingPolicyV2(priv, version, body)
	return doc
}

// routingDocServer serves whatever document the returned pointer holds, so a
// test can swap the served document between polls (which is exactly what a
// compromised/substituted org server does).
type routingDocServer struct {
	mu  sync.Mutex
	doc orgcontract.RoutingPolicyDoc
}

func (d *routingDocServer) set(doc orgcontract.RoutingPolicyDoc) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.doc = doc
}

func (d *routingDocServer) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/routing-policy" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		d.mu.Lock()
		doc := d.doc
		d.mu.Unlock()
		writeTestJSON(w, http.StatusOK, doc)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// enrolledClientWithLog is enrolledClient with a CAPTURING logger, so tests
// can assert on the version-regression WARN (the mitigation that holds
// regardless of which signature rail a document rides).
func enrolledClientWithLog(t *testing.T, srvURL string) (*Client, *store.Store, *syncBuffer) {
	t.Helper()
	s := newAgentStore(t)
	if err := s.WriteEnrolment(context.Background(), store.Enrolment{
		OrgID: "org-1", OrgName: "Acme", OrgServerURL: srvURL,
		UserID: "scim-42", UserEmail: "dev@acme.example",
		EnrolledAt: time.Now().UTC().Format(time.RFC3339), BearerKeyID: "test",
	}); err != nil {
		t.Fatalf("WriteEnrolment: %v", err)
	}
	buf := &syncBuffer{}
	cfg := config.OrgClientConfig{
		Enabled: true, PushIntervalSeconds: config.DefaultPushIntervalSeconds,
		MaxPushBytes: config.DefaultMaxPushBytes, KeychainID: config.DefaultKeychainID,
	}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := New(cfg, s, &memBearerStore{bearer: "bearer-xyz"}, "test-version", http.DefaultClient, logger)
	return c, s, buf
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestFetchRoutingPolicy_V2RefusesVersionReplayFreeze is THE end-to-end
// regression test for docs/security.md ROUTING-SIG-1, at the layer where the
// finding actually bites: the node's fetch loop.
//
// The attack, replayed here verbatim: a party who can serve
// GET /api/agent/routing-policy takes the org's own GENUINELY SIGNED policy
// and re-presents it under an INFLATED version. Under the v1 body-only
// signature that verified, the node cached it, and every later genuine
// publish then lost the monotonic `cached.Version >= doc.Version`
// short-circuit — the fleet was frozen on an attacker-chosen version forever.
//
// With v2 the replay must be REFUSED at the signature gate, the cache must be
// untouched, and the NEXT genuine publish must still land. That last
// assertion is the one that matters: refusing the replay is only useful if it
// leaves the node able to converge.
func TestFetchRoutingPolicy_V2RefusesVersionReplayFreeze(t *testing.T) {
	ctx := context.Background()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	genuine := signedRoutingDocV2(priv, pub, 1, "[routing]\n# v1 policy\n")
	srvDoc := &routingDocServer{}
	srvDoc.set(genuine)
	c, s, _ := enrolledClientWithLog(t, srvDoc.start(t))

	// 1. The genuine v1 document is accepted and cached.
	changed, outcome, err := c.FetchRoutingPolicy(ctx)
	if err != nil || !changed || outcome == nil || !outcome.OK {
		t.Fatalf("genuine fetch: changed=%v outcome=%+v err=%v", changed, outcome, err)
	}
	if row, ok, _ := s.GetOrgRoutingPolicy(ctx); !ok || row.Version != 1 {
		t.Fatalf("cache after genuine fetch = %+v ok=%v, want version 1", row, ok)
	}

	// 2. THE REPLAY: same body, same (genuine) signatures, inflated version.
	replay := genuine
	replay.Version = 1 << 40
	srvDoc.set(replay)

	changed, outcome, err = c.FetchRoutingPolicy(ctx)
	if err == nil {
		t.Fatal("VERSION REPLAY ACCEPTED — the node would now be frozen at version 2^40 against every future genuine publish (ROUTING-SIG-1)")
	}
	if changed {
		t.Error("the replay changed the cache")
	}
	if outcome == nil || outcome.RejectCode != RejectSigInvalid {
		t.Errorf("outcome = %+v, want RejectSigInvalid (a served body failed a gate, not a transport problem)", outcome)
	}
	if row, ok, _ := s.GetOrgRoutingPolicy(ctx); !ok || row.Version != 1 {
		t.Fatalf("cache after replay = %+v ok=%v, want the untouched version 1", row, ok)
	}

	// 3. CONVERGENCE: the next GENUINE publish must still land. Without
	// this, refusing the replay would be indistinguishable from the freeze
	// it prevents.
	genuine2 := signedRoutingDocV2(priv, pub, 2, "[routing]\n# v2 policy\n")
	srvDoc.set(genuine2)
	changed, outcome, err = c.FetchRoutingPolicy(ctx)
	if err != nil || !changed || outcome == nil || !outcome.OK {
		t.Fatalf("genuine v2 publish after the refused replay: changed=%v outcome=%+v err=%v", changed, outcome, err)
	}
	if row, ok, _ := s.GetOrgRoutingPolicy(ctx); !ok || row.Version != 2 {
		t.Fatalf("cache after genuine v2 = %+v ok=%v, want version 2", row, ok)
	}
}

// TestFetchRoutingPolicy_V2NeverFallsBackToV1 pins the ONE-WAY rail
// selection. A document that CARRIES a v2 signature is verified on v2 and, if
// that fails, is REFUSED — it must never be re-tried on the legacy rail. The
// document here has a perfectly valid v1 signature, so a fallback would
// accept it, which would hand the whole finding back: an attacker who cannot
// forge v2 would just serve a broken one.
func TestFetchRoutingPolicy_V2NeverFallsBackToV1(t *testing.T) {
	ctx := context.Background()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	doc := signedRoutingDocV2(priv, pub, 1, "[routing]\n")
	// Corrupt ONLY the v2 signature; v1 stays genuine and verifiable.
	raw, derr := base64.StdEncoding.DecodeString(doc.SignatureV2)
	if derr != nil {
		t.Fatalf("decode: %v", derr)
	}
	raw[0] ^= 0xFF
	doc.SignatureV2 = base64.StdEncoding.EncodeToString(raw)
	if err := orgcontract.VerifyRoutingPolicy(doc, doc.PublicKey); err != nil {
		t.Fatalf("fixture broken: the v1 signature must still be VALID for this test to prove anything: %v", err)
	}

	srvDoc := &routingDocServer{}
	srvDoc.set(doc)
	c, s, _ := enrolledClientWithLog(t, srvDoc.start(t))

	changed, outcome, err := c.FetchRoutingPolicy(ctx)
	if err == nil {
		t.Fatal("a document with a BAD v2 signature was accepted by falling back to its (valid) v1 signature — the downgrade the v2 rail exists to refuse")
	}
	if changed {
		t.Error("the rejected document changed the cache")
	}
	if outcome == nil || outcome.RejectCode != RejectSigInvalid {
		t.Errorf("outcome = %+v, want RejectSigInvalid", outcome)
	}
	if _, ok, _ := s.GetOrgRoutingPolicy(ctx); ok {
		t.Error("a rejected document was cached")
	}
}

// TestFetchRoutingPolicy_PreV2ServerStillVerifies pins the backward-compat
// direction: an org server that predates migration 078 serves no v2 signature
// at all, and a v2-capable agent must still accept its documents on the
// legacy rail. Cache-side, the empty v2 signature must round-trip as empty —
// never invented.
//
// It also pins the ACCEPTED residual recorded as ledger ROUTING-SIG-2: this
// same leg is the downgrade window, because an attacker who strips
// signature_v2 is indistinguishable from a pre-078 server until v2 becomes
// REQUIRED after a deprecation window.
func TestFetchRoutingPolicy_PreV2ServerStillVerifies(t *testing.T) {
	ctx := context.Background()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	old := signedRoutingDoc(priv, pub, 5, "[routing]\n# from a pre-078 server\n")
	if old.SignatureV2 != "" {
		t.Fatal("fixture broken: a pre-078 document must carry no v2 signature")
	}
	srvDoc := &routingDocServer{}
	srvDoc.set(old)
	c, s, _ := enrolledClientWithLog(t, srvDoc.start(t))

	changed, outcome, err := c.FetchRoutingPolicy(ctx)
	if err != nil || !changed || outcome == nil || !outcome.OK {
		t.Fatalf("pre-078 document rejected: changed=%v outcome=%+v err=%v", changed, outcome, err)
	}
	row, ok, err := s.GetOrgRoutingPolicy(ctx)
	if err != nil || !ok {
		t.Fatalf("GetOrgRoutingPolicy: ok=%v err=%v", ok, err)
	}
	if row.Version != 5 {
		t.Errorf("cached version = %d, want 5", row.Version)
	}
	if row.SignatureV2 != "" {
		t.Errorf("cached signature_v2 = %q, want empty — nothing may be invented for a pre-078 document", row.SignatureV2)
	}
	if row.Signature != old.Signature {
		t.Errorf("cached v1 signature = %q, want %q", row.Signature, old.Signature)
	}
}

// TestFetchRoutingPolicy_CachesV2Signature pins that an accepted v2 document's
// signature reaches the node cache. The cache exists so the node can
// RE-VERIFY the policy it is about to compose without a live server; if the
// v2 signature were dropped there, that offline re-verification would
// silently fall back to the body-only rail — the finding, reintroduced one
// layer down.
func TestFetchRoutingPolicy_CachesV2Signature(t *testing.T) {
	ctx := context.Background()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	doc := signedRoutingDocV2(priv, pub, 3, "[routing]\n")
	srvDoc := &routingDocServer{}
	srvDoc.set(doc)
	c, s, _ := enrolledClientWithLog(t, srvDoc.start(t))

	if _, _, err := c.FetchRoutingPolicy(ctx); err != nil {
		t.Fatalf("FetchRoutingPolicy: %v", err)
	}
	row, ok, err := s.GetOrgRoutingPolicy(ctx)
	if err != nil || !ok {
		t.Fatalf("GetOrgRoutingPolicy: ok=%v err=%v", ok, err)
	}
	if row.SignatureV2 != doc.SignatureV2 {
		t.Fatalf("cached signature_v2 = %q, want %q", row.SignatureV2, doc.SignatureV2)
	}
	// The cached row must be re-verifiable OFFLINE on the v2 rail, which is
	// the whole reason it is stored.
	cachedDoc := orgcontract.RoutingPolicyDoc{
		Version: row.Version, Body: row.Body, BodyHash: row.BodyHash,
		Signature: row.Signature, SignatureV2: row.SignatureV2,
	}
	if err := orgcontract.VerifyRoutingPolicyV2(cachedDoc, row.ServerPubkey); err != nil {
		t.Fatalf("the cached document does not re-verify on the v2 rail: %v", err)
	}
}

// TestFetchRoutingPolicy_WarnsOnVersionRegression pins the INDEPENDENT
// mitigation: a served version LOWER than the cached one is WARN-logged
// before the monotonic short-circuit swallows it. It fires regardless of
// signature rail — including against a pre-078 server, where v2 cannot help —
// so a frozen or rolled-back fleet is visible in the routing logs rather than
// silent.
func TestFetchRoutingPolicy_WarnsOnVersionRegression(t *testing.T) {
	ctx := context.Background()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	srvDoc := &routingDocServer{}
	srvDoc.set(signedRoutingDocV2(priv, pub, 9, "[routing]\n# current\n"))
	c, s, logs := enrolledClientWithLog(t, srvDoc.start(t))

	if _, _, err := c.FetchRoutingPolicy(ctx); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if strings.Contains(logs.String(), "REGRESSION") {
		t.Fatal("a first, forward fetch logged a version regression")
	}

	// Same version again — NOT a regression, and must not warn (a noisy
	// warning is a warning nobody reads).
	if _, _, err := c.FetchRoutingPolicy(ctx); err != nil {
		t.Fatalf("already-current fetch: %v", err)
	}
	if strings.Contains(logs.String(), "REGRESSION") {
		t.Fatal("an already-current fetch logged a version regression")
	}

	// Now the server serves an OLDER version — a rollback, or a replay of
	// an old genuine document.
	srvDoc.set(signedRoutingDocV2(priv, pub, 4, "[routing]\n# old\n"))
	if _, _, err := c.FetchRoutingPolicy(ctx); err != nil {
		t.Fatalf("regressed fetch: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, "REGRESSION") {
		t.Errorf("a served version LOWER than the cached one was swallowed silently by the monotonic short-circuit; logs:\n%s", out)
	}
	if !strings.Contains(out, "served_version=4") || !strings.Contains(out, "cached_version=9") {
		t.Errorf("the regression warning does not name both versions; logs:\n%s", out)
	}
	// The short-circuit must still do its job: the NEWER cached policy wins.
	if row, ok, _ := s.GetOrgRoutingPolicy(ctx); !ok || row.Version != 9 {
		t.Errorf("cache = %+v ok=%v, want the newer version 9 retained", row, ok)
	}
}

// TestFetchRoutingPolicy_V2TamperedBodyRefused pins the ordinary integrity
// leg on the new rail: re-hashing a tampered body does not rescue it, because
// BodyHash authorizes nothing — the signature does.
func TestFetchRoutingPolicy_V2TamperedBodyRefused(t *testing.T) {
	ctx := context.Background()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	doc := signedRoutingDocV2(priv, pub, 1, "[routing]\n")
	doc.Body += "\n# evil\n"
	sum := sha256.Sum256([]byte(doc.Body))
	doc.BodyHash = hex.EncodeToString(sum[:])

	srvDoc := &routingDocServer{}
	srvDoc.set(doc)
	c, s, _ := enrolledClientWithLog(t, srvDoc.start(t))

	_, outcome, err := c.FetchRoutingPolicy(ctx)
	if err == nil {
		t.Fatal("a tampered body with a matching hash was accepted")
	}
	if outcome == nil || outcome.RejectCode != RejectSigInvalid {
		t.Errorf("outcome = %+v, want RejectSigInvalid", outcome)
	}
	if _, ok, _ := s.GetOrgRoutingPolicy(ctx); ok {
		t.Error("a rejected document was cached")
	}
}
