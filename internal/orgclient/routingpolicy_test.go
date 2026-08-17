package orgclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// countRoutingStates returns how many mutually-exclusive top-level states the
// outcome asserts. Exactly one must hold for any emitted routing outcome. On the
// routing rail a RejectCode IS its own state (a reject carries no OK, unlike the
// guard rail's delivered-rejection), so it is counted here.
func countRoutingStates(o RoutingFetchOutcome) int {
	n := 0
	for _, b := range []bool{o.OK, o.Unreachable, o.AuthFailed, o.Indeterminate, o.RejectCode != RejectNone} {
		if b {
			n++
		}
	}
	return n
}

// TestRoutingFetchOutcomeIsTotal drives classifyRoutingFetch directly (P0-6
// §2.5b / §7.1). Every one of FetchRoutingPolicy's return sites maps to exactly
// ONE state, Reached is correct, and the specific misclassifications the plan
// forbids fail here:
//   - 401/403 must NOT be Unreachable (auth is reached);
//   - a decode / cache-read / pin-store-DB / reachable-non-auth-4xx branch must
//     NOT be Unreachable OR a Reject;
//   - a genuine key CHANGE / signature failure must NOT be Unreachable.
func TestRoutingFetchOutcomeIsTotal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		sig      routingFetchSignal
		want     RoutingFetchOutcome
		wantEmit bool
	}{
		{"context canceled skips", routingFetchSignal{stage: rfStageSkip}, RoutingFetchOutcome{}, false},
		{"pre-wire local indeterminate not reached", routingFetchSignal{stage: rfStagePreWire}, RoutingFetchOutcome{Indeterminate: true, Reached: false}, true},
		{"transport unreachable not reached", routingFetchSignal{stage: rfStageTransport}, RoutingFetchOutcome{Unreachable: true, Reached: false}, true},
		{"404 is OK reached", routingFetchSignal{stage: rfStageHTTPStatus, status: http.StatusNotFound}, RoutingFetchOutcome{OK: true, Reached: true}, true},
		{"401 is AuthFailed NOT Unreachable", routingFetchSignal{stage: rfStageHTTPStatus, status: http.StatusUnauthorized}, RoutingFetchOutcome{AuthFailed: true, Reached: true}, true},
		{"403 is AuthFailed NOT Unreachable", routingFetchSignal{stage: rfStageHTTPStatus, status: http.StatusForbidden}, RoutingFetchOutcome{AuthFailed: true, Reached: true}, true},
		{"500 is Unreachable", routingFetchSignal{stage: rfStageHTTPStatus, status: http.StatusInternalServerError}, RoutingFetchOutcome{Unreachable: true, Reached: false}, true},
		{"503 is Unreachable", routingFetchSignal{stage: rfStageHTTPStatus, status: http.StatusServiceUnavailable}, RoutingFetchOutcome{Unreachable: true, Reached: false}, true},
		{"429 reachable is Indeterminate reached NOT Unreachable", routingFetchSignal{stage: rfStageHTTPStatus, status: http.StatusTooManyRequests}, RoutingFetchOutcome{Indeterminate: true, Reached: true}, true},
		{"409 reachable is Indeterminate reached NOT Unreachable", routingFetchSignal{stage: rfStageHTTPStatus, status: http.StatusConflict}, RoutingFetchOutcome{Indeterminate: true, Reached: true}, true},
		{"decode is Indeterminate reached NOT Reject", routingFetchSignal{stage: rfStageDecode}, RoutingFetchOutcome{Indeterminate: true, Reached: true}, true},
		{"cache-read local is Indeterminate not reached", routingFetchSignal{stage: rfStageCacheReadLocal}, RoutingFetchOutcome{Indeterminate: true, Reached: false}, true},
		{"pin-store DB error is Indeterminate NOT Reject", routingFetchSignal{stage: rfStagePinStoreLocal}, RoutingFetchOutcome{Indeterminate: true, Reached: false}, true},
		{"key change is RejectKeyPinMismatch reached NOT Unreachable", routingFetchSignal{stage: rfStageKeyMismatch, version: 7}, RoutingFetchOutcome{RejectCode: RejectKeyPinMismatch, Reached: true, Version: 7}, true},
		{"sig invalid is RejectSigInvalid reached NOT Unreachable", routingFetchSignal{stage: rfStageSigInvalid, version: 7}, RoutingFetchOutcome{RejectCode: RejectSigInvalid, Reached: true, Version: 7}, true},
		{"cache-write after verified body is Indeterminate reached", routingFetchSignal{stage: rfStageCacheWriteLocal}, RoutingFetchOutcome{Indeterminate: true, Reached: true}, true},
		{"accepted is OK reached", routingFetchSignal{stage: rfStageAccepted, version: 9}, RoutingFetchOutcome{OK: true, Reached: true, Version: 9}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, emit := classifyRoutingFetch(tc.sig)
			if emit != tc.wantEmit {
				t.Fatalf("emit = %v, want %v", emit, tc.wantEmit)
			}
			if !emit {
				return
			}
			if got != tc.want {
				t.Fatalf("outcome = %+v, want %+v", got, tc.want)
			}
			if n := countRoutingStates(got); n != 1 {
				t.Fatalf("outcome asserts %d states, want exactly 1: %+v", n, got)
			}
		})
	}
}

// TestFetchRoutingPolicy_ContextCanceledFromLocalOpSkips pins Blocker 2
// (R5-NIT): a context.Canceled surfacing from a NON-Do-site local op (here,
// the very first call in fetchRoutingPolicy, LoadEnrolment) must still make
// the EXPORTED FetchRoutingPolicy wrapper skip (nil outcome) — not fall
// through to that branch's own Indeterminate classification. Before the fix,
// only the httpClient.Do error site was guarded against context.Canceled, so
// this local-op cancellation would have emitted a non-nil Indeterminate
// outcome instead of skipping.
func TestFetchRoutingPolicy_ContextCanceledFromLocalOpSkips(t *testing.T) {
	s := newAgentStore(t)
	c := newTestClient(t, s, &memBearerStore{bearer: "bearer-xyz"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled BEFORE the call: LoadEnrolment sees it immediately

	changed, outcome, err := c.FetchRoutingPolicy(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if outcome != nil {
		t.Fatalf("outcome = %+v, want nil (context.Canceled must always skip)", outcome)
	}
	if changed {
		t.Fatalf("changed = true, want false on a canceled/skipped fetch")
	}
}

// TestFetchRoutingPolicy_DeadlineExceededStaysUnreachable is the contrast
// case for the fix above: a genuine timeout on the Do path (context.
// DeadlineExceeded, NOT context.Canceled) must still classify as a decisive
// Unreachable outcome, never skip. errors.Is(err, context.Canceled) is
// correctly false for context.DeadlineExceeded, so the universal skip guard
// added for Blocker 2 must not swallow it.
func TestFetchRoutingPolicy_DeadlineExceededStaysUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(200 * time.Millisecond) // longer than the client timeout below
	}))
	defer srv.Close()

	s := newAgentStore(t)
	if err := s.WriteEnrolment(context.Background(), store.Enrolment{
		OrgID: "org-1", OrgName: "Acme", OrgServerURL: srv.URL,
		UserID: "scim-42", UserEmail: "dev@acme.example",
		EnrolledAt: time.Now().UTC().Format(time.RFC3339), BearerKeyID: "test",
	}); err != nil {
		t.Fatalf("WriteEnrolment: %v", err)
	}
	cfg := config.OrgClientConfig{
		Enabled: true, PushIntervalSeconds: config.DefaultPushIntervalSeconds,
		MaxPushBytes: config.DefaultMaxPushBytes, KeychainID: config.DefaultKeychainID,
	}
	hc := &http.Client{Timeout: 20 * time.Millisecond}
	c := New(cfg, s, &memBearerStore{bearer: "bearer-xyz"}, "test-version", hc, quietLogger())

	_, outcome, err := c.FetchRoutingPolicy(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if outcome == nil {
		t.Fatalf("outcome = nil, want non-nil Unreachable (a timeout is decisive, not a skip)")
	}
	if !outcome.Unreachable || outcome.Reached {
		t.Fatalf("outcome = %+v, want Unreachable{Reached:false}", outcome)
	}
}

// signedRoutingDoc builds a verifiable orgcontract.RoutingPolicyDoc the
// same way orgpin_test.go's routing-rail tests do (raw Ed25519 over the
// body bytes, hash = hex(sha256(body)) — the rail's OWN scheme, not the
// announcement rail's domain-tagged one).
func signedRoutingDoc(priv ed25519.PrivateKey, pub ed25519.PublicKey, version int64, body string) orgcontract.RoutingPolicyDoc {
	sum := sha256.Sum256([]byte(body))
	return orgcontract.RoutingPolicyDoc{
		Version:   version,
		Body:      body,
		BodyHash:  hex.EncodeToString(sum[:]),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(body))),
		PublicKey: base64.StdEncoding.EncodeToString(pub),
	}
}

// TestSetRoutingReloadSink_FiredOnAccept pins the P0-7 router-side
// wake-up contract (§7.2/§7.4): a fetch that ACCEPTS and caches a
// routing policy must invoke the installed reload sink, and the sink
// must observe the cache ALREADY updated when it runs — i.e. the sink
// fires strictly AFTER the cache upsert, never before or in place of
// it. Both accept arms (freshly-cached AND already-current) fire it.
//
// Mutation-ready: dropping either fireRoutingReload() call site in
// fetchRoutingPolicy (internal/orgclient/routingpolicy.go) drops the
// fired count for that arm to 0, failing this test. Reordering the
// newly-accepted arm's call to BEFORE the UpsertOrgRoutingPolicy call
// would make the sink observe an absent (ok=false) or stale cache row,
// failing the "sink saw version already cached" assertion below.
func TestSetRoutingReloadSink_FiredOnAccept(t *testing.T) {
	ctx := context.Background()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	body := "[routing]\n"
	doc := signedRoutingDoc(priv, pub, 1, body)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/routing-policy" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeTestJSON(w, http.StatusOK, doc)
	}))
	t.Cleanup(srv.Close)

	c, s, _ := enrolledClient(t, srv.URL)

	var fired int32
	var sinkSawCachedVersion atomic.Int64
	sinkSawCachedVersion.Store(-1)
	c.SetRoutingReloadSink(func(ctx context.Context) {
		atomic.AddInt32(&fired, 1)
		if row, ok, _ := s.GetOrgRoutingPolicy(context.Background()); ok {
			sinkSawCachedVersion.Store(row.Version)
		}
	})

	// Arm 1: a brand-new, never-before-seen version — the
	// newly-accepted branch of fetchRoutingPolicy.
	changed, outcome, err := c.FetchRoutingPolicy(ctx)
	if err != nil {
		t.Fatalf("FetchRoutingPolicy(new): err = %v", err)
	}
	if !changed || outcome == nil || !outcome.OK {
		t.Fatalf("FetchRoutingPolicy(new): changed=%v outcome=%+v, want changed OK", changed, outcome)
	}
	if got := atomic.LoadInt32(&fired); got != 1 {
		t.Fatalf("reload sink fired %d times on accept, want exactly 1", got)
	}
	if got := sinkSawCachedVersion.Load(); got != 1 {
		t.Fatalf("reload sink observed cached version %d, want 1 (the sink must fire AFTER the cache upsert)", got)
	}

	// Arm 2: same version again — the already-current branch. It must
	// also fire the sink (the running router still needs to be told
	// "nothing changed, but you're asked to converge").
	changed, outcome, err = c.FetchRoutingPolicy(ctx)
	if err != nil {
		t.Fatalf("FetchRoutingPolicy(already-current): err = %v", err)
	}
	if changed || outcome == nil || !outcome.OK {
		t.Fatalf("FetchRoutingPolicy(already-current): changed=%v outcome=%+v, want unchanged OK", changed, outcome)
	}
	if got := atomic.LoadInt32(&fired); got != 2 {
		t.Fatalf("reload sink fired %d times cumulatively after the already-current arm, want 2", got)
	}
}
