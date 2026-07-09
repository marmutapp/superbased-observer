package orgclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// signedTestBundle builds a verifiable envelope around bundleTOML.
func signedTestBundle(t *testing.T, priv ed25519.PrivateKey, pub ed25519.PublicKey, version int64, bundleTOML string) orgcontract.PolicyBundle {
	t.Helper()
	return orgcontract.PolicyBundle{
		Version:    version,
		BundleTOML: bundleTOML,
		Signature:  orgcontract.SignPolicyBundle(priv, version, []byte(bundleTOML)),
		PublicKey:  base64.RawURLEncoding.EncodeToString(pub),
		SignedAt:   "2026-06-11T09:00:00Z",
	}
}

// policyBundleServer serves GET /api/v1/policy-bundle with ETag/304
// semantics over a mutable bundle pointer (nil = 404). It records the
// If-None-Match values it saw.
type policyBundleServer struct {
	srv    *httptest.Server
	bundle *orgcontract.PolicyBundle
	seen   []string
	hits   chan struct{}
}

func newPolicyBundleServer(t *testing.T) *policyBundleServer {
	t.Helper()
	ps := &policyBundleServer{hits: make(chan struct{}, 64)}
	ps.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case ps.hits <- struct{}{}:
		default:
		}
		ps.seen = append(ps.seen, r.Header.Get("If-None-Match"))
		if ps.bundle == nil {
			writeTestJSON(w, http.StatusNotFound, map[string]string{"error": "no_policy_bundle"})
			return
		}
		etag := `"pb-v` + string(rune('0'+ps.bundle.Version)) + `"`
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		writeTestJSON(w, http.StatusOK, ps.bundle)
	}))
	t.Cleanup(ps.srv.Close)
	return ps
}

// enrolledClient returns a Client whose store carries an enrolment row
// pointing at srvURL plus a loaded bearer, and a cache path under a
// temp dir.
func enrolledClient(t *testing.T, srvURL string) (*Client, *store.Store, string) {
	t.Helper()
	s := newAgentStore(t)
	if err := s.WriteEnrolment(context.Background(), store.Enrolment{
		OrgID: "org-1", OrgName: "Acme", OrgServerURL: srvURL,
		UserID: "scim-42", UserEmail: "dev@acme.example",
		EnrolledAt: time.Now().UTC().Format(time.RFC3339), BearerKeyID: "test",
	}); err != nil {
		t.Fatalf("WriteEnrolment: %v", err)
	}
	bs := &memBearerStore{bearer: "bearer-xyz"}
	return newTestClient(t, s, bs), s, filepath.Join(t.TempDir(), "org-policy-bundle.json")
}

// orgStateRow returns the latest guard_policy_state row for (org, path),
// or nil.
func orgStateRow(t *testing.T, s *store.Store, path string) *store.GuardPolicyStateRow {
	t.Helper()
	states, err := s.LatestGuardPolicyStates(context.Background())
	if err != nil {
		t.Fatalf("LatestGuardPolicyStates: %v", err)
	}
	for i := range states {
		if states[i].Layer == "org" && states[i].Path == path {
			return &states[i]
		}
	}
	return nil
}

const escalatingTOML = "[[override]]\nrule = \"R-110\"\ndecision = \"deny\"\nenforce = true\n"

// TestFetchPolicyBundle_AppliedThenUnchanged covers the happy path end
// to end: a verified bundle is cached atomically, the TOFU key pin and
// the version row land in guard_policy_state, the ETag persists, and
// the second poll rides If-None-Match into a 304.
func TestFetchPolicyBundle_AppliedThenUnchanged(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	ps := newPolicyBundleServer(t)
	b := signedTestBundle(t, priv, pub, 3, escalatingTOML)
	ps.bundle = &b

	c, s, cachePath := enrolledClient(t, ps.srv.URL)
	res, err := c.FetchPolicyBundle(context.Background(), cachePath)
	if err != nil {
		t.Fatalf("FetchPolicyBundle: %v", err)
	}
	if res.Status != PolicyApplied || res.Version != 3 {
		t.Fatalf("result = %+v, want applied v3", res)
	}

	// Cache holds the envelope verbatim.
	raw, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("cache not written: %v", err)
	}
	var cached orgcontract.PolicyBundle
	if err := json.Unmarshal(raw, &cached); err != nil || cached.Version != 3 || cached.BundleTOML != escalatingTOML {
		t.Fatalf("cache mismatch: %+v err=%v", cached, err)
	}

	// TOFU pin row + bundle version row.
	pin := orgStateRow(t, s, PolicyKeyPinPath(ps.srv.URL))
	if pin == nil || pin.ContentHash != orgcontract.PublicKeyPinHash(pub) {
		t.Fatalf("key pin row = %+v, want hash of the served key", pin)
	}
	st := orgStateRow(t, s, ps.srv.URL+"/api/v1/policy-bundle")
	if st == nil || st.Version != "3" || st.Signature != b.Signature {
		t.Fatalf("bundle state row = %+v, want version 3 + signature", st)
	}

	// Second poll: If-None-Match → 304 → unchanged.
	res, err = c.FetchPolicyBundle(context.Background(), cachePath)
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if res.Status != PolicyUnchanged {
		t.Fatalf("second result = %+v, want unchanged", res)
	}
	if len(ps.seen) != 2 || ps.seen[1] == "" {
		t.Errorf("If-None-Match not sent on second poll: %q", ps.seen)
	}
}

// TestFetchPolicyBundle_Rejections is the acceptance-gate table: each
// failing gate yields PolicyRejected with a naming detail, writes NO
// cache, and never errors (rejection is a result the caller turns into
// R-205).
func TestFetchPolicyBundle_Rejections(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)

	cases := []struct {
		name       string
		bundle     func(t *testing.T) orgcontract.PolicyBundle
		prePin     ed25519.PublicKey // pre-recorded enrolment pin (nil = none)
		wantDetail string
	}{
		{
			name: "tampered body breaks the signature",
			bundle: func(t *testing.T) orgcontract.PolicyBundle {
				b := signedTestBundle(t, priv, pub, 3, escalatingTOML)
				b.BundleTOML += "# evil\n"
				return b
			},
			wantDetail: "signature verification failed",
		},
		{
			name: "key does not match the enrolment pin",
			bundle: func(t *testing.T) orgcontract.PolicyBundle {
				return signedTestBundle(t, priv, pub, 3, escalatingTOML)
			},
			prePin:     otherPub,
			wantDetail: "does not match the enrolment pin",
		},
		{
			name: "floor-violating TOML fails the org lint",
			bundle: func(t *testing.T) orgcontract.PolicyBundle {
				return signedTestBundle(t, priv, pub, 3, "[[override]]\nrule = \"R-110\"\ndecision = \"allow\"\n")
			},
			wantDetail: "does not lint as an org policy file",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ps := newPolicyBundleServer(t)
			b := tc.bundle(t)
			ps.bundle = &b
			c, s, cachePath := enrolledClient(t, ps.srv.URL)
			if tc.prePin != nil {
				if _, err := s.RecordGuardPolicyState(context.Background(), store.GuardPolicyStateRow{
					Layer: "org", Path: PolicyKeyPinPath(ps.srv.URL),
					ContentHash: orgcontract.PublicKeyPinHash(tc.prePin),
					LoadedAt:    time.Now().UTC(),
				}); err != nil {
					t.Fatalf("pre-pin: %v", err)
				}
			}
			res, err := c.FetchPolicyBundle(context.Background(), cachePath)
			if err != nil {
				t.Fatalf("FetchPolicyBundle: %v", err)
			}
			if res.Status != PolicyRejected || !strings.Contains(res.Detail, tc.wantDetail) {
				t.Fatalf("result = %+v, want rejected with %q", res, tc.wantDetail)
			}
			if _, serr := os.Stat(cachePath); !os.IsNotExist(serr) {
				t.Error("rejected bundle must not be cached")
			}
		})
	}
}

// TestFetchPolicyBundle_VersionRegression pins gate 3: after applying
// v5, a served (validly signed) v3 is rejected and the v5 cache stays.
func TestFetchPolicyBundle_VersionRegression(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	ps := newPolicyBundleServer(t)
	v5 := signedTestBundle(t, priv, pub, 5, escalatingTOML)
	ps.bundle = &v5

	c, _, cachePath := enrolledClient(t, ps.srv.URL)
	if res, err := c.FetchPolicyBundle(context.Background(), cachePath); err != nil || res.Status != PolicyApplied {
		t.Fatalf("apply v5: %+v err=%v", res, err)
	}

	v3 := signedTestBundle(t, priv, pub, 3, escalatingTOML)
	ps.bundle = &v3
	res, err := c.FetchPolicyBundle(context.Background(), cachePath)
	if err != nil {
		t.Fatalf("fetch v3: %v", err)
	}
	if res.Status != PolicyRejected || !strings.Contains(res.Detail, "version regression") {
		t.Fatalf("result = %+v, want version-regression rejection", res)
	}
	var cached orgcontract.PolicyBundle
	raw, _ := os.ReadFile(cachePath)
	if err := json.Unmarshal(raw, &cached); err != nil || cached.Version != 5 {
		t.Errorf("cache no longer holds v5: %+v err=%v", cached, err)
	}
}

// TestFetchPolicyBundle_NoBundleAndAuth covers the non-200 statuses:
// 404 → PolicyNone (old server / nothing published; an existing cache
// stays), 401 → ErrAuthFailed.
func TestFetchPolicyBundle_NoBundleAndAuth(t *testing.T) {
	ps := newPolicyBundleServer(t) // bundle nil → 404
	c, _, cachePath := enrolledClient(t, ps.srv.URL)
	res, err := c.FetchPolicyBundle(context.Background(), cachePath)
	if err != nil {
		t.Fatalf("404 fetch: %v", err)
	}
	if res.Status != PolicyNone {
		t.Fatalf("result = %+v, want none", res)
	}

	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, http.StatusUnauthorized, map[string]string{"error": "bearer_invalid"})
	}))
	defer authSrv.Close()
	c2, _, cache2 := enrolledClient(t, authSrv.URL)
	if _, err := c2.FetchPolicyBundle(context.Background(), cache2); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("401 fetch err = %v, want ErrAuthFailed", err)
	}
}

// TestFetchPolicyBundle_NotEnrolled pins the idle path.
func TestFetchPolicyBundle_NotEnrolled(t *testing.T) {
	s := newAgentStore(t)
	c := newTestClient(t, s, &memBearerStore{})
	if _, err := c.FetchPolicyBundle(context.Background(), filepath.Join(t.TempDir(), "b.json")); !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("err = %v, want ErrNotEnrolled", err)
	}
}

// TestPolicyPollLoop_ImmediateFirstFetch pins the §14.2 "on observer
// start" half: the loop fetches once BEFORE the first interval wait
// (interval here is an hour — only the immediate cycle can be the one
// that lands) and reports through onResult.
func TestPolicyPollLoop_ImmediateFirstFetch(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	ps := newPolicyBundleServer(t)
	b := signedTestBundle(t, priv, pub, 1, escalatingTOML)
	ps.bundle = &b

	c, _, cachePath := enrolledClient(t, ps.srv.URL)
	results := make(chan PolicyResult, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- c.PolicyPollLoop(ctx, cachePath, func(r PolicyResult) {
			select {
			case results <- r:
			default:
			}
		})
	}()
	select {
	case r := <-results:
		if r.Status != PolicyApplied || r.Version != 1 {
			t.Fatalf("first poll result = %+v, want applied v1", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("immediate first fetch never happened")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("loop returned %v, want context.Canceled", err)
	}
}

// TestEnroll_PinsPolicyKey covers the enrol-time pin: a server that
// delivers org_policy_public_key gets its key hash recorded under the
// "#policy-key" guard_policy_state row.
func TestEnroll_PinsPolicyKey(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	s := newAgentStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, http.StatusOK, orgcontract.EnrollResponse{
			Bearer: "bearer-xyz", BearerExpiresAt: "2026-08-23T00:00:00Z",
			OrgID: "org-1", OrgName: "Acme", UserID: "scim-42", UserEmail: "dev@acme.example",
			OrgPolicyPublicKey: base64.RawURLEncoding.EncodeToString(pub),
		})
	}))
	defer srv.Close()

	c := newTestClient(t, s, &memBearerStore{})
	if _, err := c.Enroll(context.Background(), srv.URL, "tok_id.secret"); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	pin := orgStateRow(t, s, PolicyKeyPinPath(srv.URL))
	if pin == nil || pin.ContentHash != orgcontract.PublicKeyPinHash(pub) {
		t.Fatalf("pin row = %+v, want enrol-time key hash", pin)
	}
}
