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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// P0-10 Phase B agent-side targeting tests
// (docs/plans/policy-targeting-rollback-design-2026-08-13.md §2 + §6 cases
// 2 and 9). Two independent gates are exercised here:
//
//   - the canonical-form gate (grammar): selectors_json must be byte-
//     identical to its canonical rendering, or the envelope is a
//     closed_envelope_violation exactly as a targeted envelope was before
//     this rail existed;
//   - the targeting corroboration (semantics): a signed selector that
//     CONTRADICTS a locally-configured attribute rejects selector_mismatch
//     and keeps the prior LKG, while an attribute the node has not
//     configured is accepted and logged.

// signedTestResourceWithSelectors builds a verifiable envelope carrying an
// arbitrary (possibly non-canonical) selectors_json — signed AFTER the field
// is set, so the signature is always valid over exactly the bytes served.
// That is what isolates the agent's grammar gate from gate 1: a
// non-canonical spelling must be rejected even though nothing was tampered
// with in flight.
func signedTestResourceWithSelectors(t *testing.T, priv ed25519.PrivateKey, pub ed25519.PublicKey, family string, version int64, body, selectorsJSON string) orgcontract.SignedPolicyResource {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	r := orgcontract.SignedPolicyResource{
		ID:              "default",
		Version:         version,
		Family:          family,
		CompilerVersion: "v1",
		Body:            body,
		BodyHash:        hex.EncodeToString(sum[:]),
		SelectorsJSON:   selectorsJSON,
		PublicKey:       base64.RawURLEncoding.EncodeToString(pub),
		SignedAt:        "2026-08-13T00:00:00Z",
	}
	sig, err := orgcontract.SignPolicyResource(priv, r)
	if err != nil {
		t.Fatalf("SignPolicyResource: %v", err)
	}
	r.Signature = sig
	return r
}

// TestFetchAndAcceptPolicyResource_SelectorCanonicalGate is design §6 case 9:
// semantically-equal but non-canonical selectors (unsorted keys, padded
// whitespace, an unknown key, an explicit empty value, oversize) are
// closed_envelope_violation — the field's VALUE opened, its GRAMMAR did not.
// The canonical spellings in the same table apply cleanly, proving the gate
// discriminates on form, not on "is it targeted at all".
func TestFetchAndAcceptPolicyResource_SelectorCanonicalGate(t *testing.T) {
	cases := []struct {
		name      string
		selectors string
		attrs     orgcontract.Selectors
		wantCode  PolicyResourceRejectCode // "" = expect PRApplied
	}{
		{name: "canonical match-all applies", selectors: "{}"},
		{
			name:      "canonical single key applies",
			selectors: `{"environment":"prod"}`,
			attrs:     orgcontract.Selectors{Environment: "prod"},
		},
		{
			name:      "canonical three keys apply",
			selectors: `{"environment":"prod","service":"api","workspace":"acme"}`,
			attrs:     orgcontract.Selectors{Workspace: "acme", Environment: "prod", Service: "api"},
		},
		{name: "unsorted keys rejected", selectors: `{"workspace":"acme","environment":"prod"}`, wantCode: PRRejectClosedEnvelope},
		{name: "padded document rejected", selectors: ` {"environment":"prod"} `, wantCode: PRRejectClosedEnvelope},
		{name: "padded value rejected", selectors: `{"environment":" prod "}`, wantCode: PRRejectClosedEnvelope},
		{name: "spaced separator rejected", selectors: `{"environment": "prod"}`, wantCode: PRRejectClosedEnvelope},
		{name: "explicit empty value rejected", selectors: `{"environment":""}`, wantCode: PRRejectClosedEnvelope},
		{name: "unknown key rejected", selectors: `{"team":"platform"}`, wantCode: PRRejectClosedEnvelope},
		{name: "empty string rejected", selectors: ``, wantCode: PRRejectClosedEnvelope},
		{
			name:      "oversize rejected",
			selectors: `{"workspace":"` + strings.Repeat("a", orgcontract.MaxPolicyResourceSelectorsBytes) + `"}`,
			wantCode:  PRRejectClosedEnvelope,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pub, priv, _ := ed25519.GenerateKey(rand.Reader)
			ps := newPolicyResourceServer(t)
			r := signedTestResourceWithSelectors(t, priv, pub, policyfamAdmissionInput, 1, observeAdmissionBody, tc.selectors)
			ps.resources[policyfamAdmissionInput] = &r

			c, _, cacheDir := enrolledPRClient(t, ps.srv.URL)
			opts := PolicyResourceOptions{
				CacheDir:       cacheDir,
				AcceptFamilies: []string{policyfamAdmissionInput},
				NodeAttrs:      tc.attrs,
			}
			res, err := c.FetchAndAcceptPolicyResource(context.Background(), policyfamAdmissionInput, opts)
			if err != nil {
				t.Fatalf("FetchAndAcceptPolicyResource: %v", err)
			}
			cachePath := filepath.Join(cacheDir, orgKeyForTest(t, c), "1", policyfamAdmissionInput+".json")
			if tc.wantCode == "" {
				if res.Status != PRApplied {
					t.Fatalf("result = %+v, want applied", res)
				}
				if _, statErr := os.Stat(cachePath); statErr != nil {
					t.Errorf("applied resource not cached: %v", statErr)
				}
				return
			}
			if res.Status != PRRejected || res.RejectCode != tc.wantCode {
				t.Fatalf("result = %+v, want rejected/%s", res, tc.wantCode)
			}
			if _, statErr := os.Stat(cachePath); !os.IsNotExist(statErr) {
				t.Errorf("rejected resource must not be cached (stat err = %v)", statErr)
			}
		})
	}
}

// TestFetchAndAcceptPolicyResource_TargetingCorroboration is design §6 case
// 2: a node with matching config applies; with contradicting config it
// rejects selector_mismatch; with no configured attrs it accepts (logging
// the envelope as uncorroborated). Every case runs against the SAME signed
// envelope, so the only variable is the node's own configuration.
func TestFetchAndAcceptPolicyResource_TargetingCorroboration(t *testing.T) {
	const targeted = `{"environment":"prod","workspace":"acme"}`
	cases := []struct {
		name       string
		attrs      orgcontract.Selectors
		wantStatus PolicyResourceStatus
		wantCode   PolicyResourceRejectCode
	}{
		{
			name:       "every targeted attribute matches",
			attrs:      orgcontract.Selectors{Workspace: "acme", Environment: "prod"},
			wantStatus: PRApplied,
		},
		{
			name:       "matching attributes plus an untargeted extra",
			attrs:      orgcontract.Selectors{Workspace: "acme", Environment: "prod", Service: "anything"},
			wantStatus: PRApplied,
		},
		{
			name:       "no attributes configured is uncorroborated, not blocking",
			attrs:      orgcontract.Selectors{},
			wantStatus: PRApplied,
		},
		{
			name:       "partially configured: the configured half matches",
			attrs:      orgcontract.Selectors{Environment: "prod"},
			wantStatus: PRApplied,
		},
		{
			name:       "configured attribute contradicts",
			attrs:      orgcontract.Selectors{Environment: "dev"},
			wantStatus: PRRejected,
			wantCode:   PRRejectSelectorMismatch,
		},
		{
			name:       "one of two configured attributes contradicts",
			attrs:      orgcontract.Selectors{Workspace: "acme", Environment: "dev"},
			wantStatus: PRRejected,
			wantCode:   PRRejectSelectorMismatch,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pub, priv, _ := ed25519.GenerateKey(rand.Reader)
			ps := newPolicyResourceServer(t)
			// v1 is untargeted and applies for EVERY node: it is the prior LKG
			// a selector rejection must leave standing.
			r1 := signedTestResource(t, priv, pub, policyfamAdmissionInput, 1, observeAdmissionBody, nil)
			ps.resources[policyfamAdmissionInput] = &r1

			c, s, cacheDir := enrolledPRClient(t, ps.srv.URL)
			opts := PolicyResourceOptions{
				CacheDir:       cacheDir,
				AcceptFamilies: []string{policyfamAdmissionInput},
				NodeAttrs:      tc.attrs,
			}
			if res, err := c.FetchAndAcceptPolicyResource(context.Background(), policyfamAdmissionInput, opts); err != nil || res.Status != PRApplied {
				t.Fatalf("seed LKG v1: %+v err=%v", res, err)
			}

			const altBody = `{"mode":"observe","strict":true}`
			r2 := signedTestResourceWithSelectors(t, priv, pub, policyfamAdmissionInput, 2, altBody, targeted)
			ps.resources[policyfamAdmissionInput] = &r2

			res, err := c.FetchAndAcceptPolicyResource(context.Background(), policyfamAdmissionInput, opts)
			if err != nil {
				t.Fatalf("FetchAndAcceptPolicyResource: %v", err)
			}
			if res.Status != tc.wantStatus || res.RejectCode != tc.wantCode {
				t.Fatalf("result = %+v, want status=%s code=%q", res, tc.wantStatus, tc.wantCode)
			}

			// Whatever the outcome, the durable state and the on-disk cache
			// must agree with it: an applied v2 advances the floor, a
			// selector_mismatch leaves the v1 LKG entirely intact.
			wantVersion := int64(2)
			wantHash := r2.BodyHash
			if tc.wantStatus == PRRejected {
				wantVersion, wantHash = 1, r1.BodyHash
			}
			enr, _ := s.LoadEnrolment(context.Background())
			orgKey := OrgKey(enr.OrgServerURL, enr.OrgID)
			st, ok, serr := s.LoadPolicyResourceState(context.Background(), orgKey, policyfamAdmissionInput)
			if serr != nil || !ok || st.FloorVersion != wantVersion || st.BodyHash != wantHash {
				t.Fatalf("durable state = %+v ok=%v err=%v, want floor=%d hash=%s", st, ok, serr, wantVersion, wantHash)
			}
			raw, rerr := os.ReadFile(filepath.Join(cacheDir, orgKeyForTest(t, c), "1", policyfamAdmissionInput+".json"))
			if rerr != nil {
				t.Fatalf("read cache: %v", rerr)
			}
			var cached orgcontract.SignedPolicyResource
			if err := json.Unmarshal(raw, &cached); err != nil || cached.Version != wantVersion {
				t.Fatalf("cached envelope = %+v err=%v, want version %d", cached, err, wantVersion)
			}
		})
	}
}

// TestPolicyResourceFetchURL_AdvertisesSelectorCapability pins the design's
// R2 mixed-fleet compatibility marker at the URL-composition seam.
func TestPolicyResourceFetchURL_AdvertisesSelectorCapability(t *testing.T) {
	got := policyResourceFetchURL("https://org.example/", "admission.input")
	want := "https://org.example/api/agent/policy/admission.input?policy_caps=selectors"
	if got != want {
		t.Fatalf("policyResourceFetchURL = %q, want %q", got, want)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Query().Get("policy_caps") != PolicyResourceSelectorCapability {
		t.Fatalf("policy_caps = %q, want %q", u.Query().Get("policy_caps"), PolicyResourceSelectorCapability)
	}
}

// TestFetchAndAcceptPolicyResource_SendsSelectorCapability proves the marker
// actually rides the live request (not just the URL helper): a server that
// only serves the targeted resource to selector-capable subjects — exactly
// the R2 gate the org server will implement — gets the marker on every poll.
func TestFetchAndAcceptPolicyResource_SendsSelectorCapability(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	r := signedTestResourceWithSelectors(t, priv, pub, policyfamAdmissionInput, 1, observeAdmissionBody, `{"environment":"prod"}`)

	var sawCaps []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		sawCaps = append(sawCaps, req.URL.Query().Get("policy_caps"))
		if req.URL.Query().Get("policy_caps") != PolicyResourceSelectorCapability {
			// Stand in for a server serving an old agent: no targeted resource.
			writeTestJSON(w, http.StatusNotFound, map[string]string{"error": "no_policy_resource"})
			return
		}
		digest, derr := orgcontract.PolicyResourceMessageDigest(r)
		if derr != nil {
			t.Errorf("PolicyResourceMessageDigest: %v", derr)
		}
		w.Header().Set("ETag", `"`+digest+`"`)
		writeTestJSON(w, http.StatusOK, r)
	}))
	t.Cleanup(srv.Close)

	c, _, cacheDir := enrolledPRClient(t, srv.URL)
	opts := PolicyResourceOptions{
		CacheDir:       cacheDir,
		AcceptFamilies: []string{policyfamAdmissionInput},
		NodeAttrs:      orgcontract.Selectors{Environment: "prod"},
	}
	res, err := c.FetchAndAcceptPolicyResource(context.Background(), policyfamAdmissionInput, opts)
	if err != nil {
		t.Fatalf("FetchAndAcceptPolicyResource: %v", err)
	}
	if res.Status != PRApplied {
		t.Fatalf("result = %+v, want applied (the capability marker should have unlocked the targeted resource)", res)
	}
	if len(sawCaps) != 1 || sawCaps[0] != PolicyResourceSelectorCapability {
		t.Fatalf("server saw policy_caps = %q, want exactly one %q", sawCaps, PolicyResourceSelectorCapability)
	}
}
