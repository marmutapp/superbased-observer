package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	gen "github.com/marmutapp/superbased-observer/internal/orgserver/api/gen"
	"github.com/marmutapp/superbased-observer/internal/orgserver/auth"
)

const escalatingOrgTOML = "[[override]]\nrule = \"R-110\"\ndecision = \"deny\"\nenforce = true\n"

// policyTestKey generates an Ed25519 policy signing key for tests.
func policyTestKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	return priv
}

// TestPolicyBundle_PublishAndServe covers the server half of the §14.2
// channel end to end: publish assigns monotonic versions, the endpoint
// serves the LATEST version with a verifiable signature + a strong
// ETag, and If-None-Match yields 304.
func TestPolicyBundle_PublishAndServe(t *testing.T) {
	h, d := newTestHandlers(t)
	priv := policyTestKey(t)
	ctx := context.Background()

	v1, err := PublishPolicyBundle(ctx, d, priv, escalatingOrgTOML, "ops@acme", "initial floor")
	if err != nil {
		t.Fatalf("publish v1: %v", err)
	}
	v2TOML := escalatingOrgTOML + "\n[[override]]\nrule = \"R-101\"\ndecision = \"deny\"\nenforce = true\n"
	v2, err := PublishPolicyBundle(ctx, d, priv, v2TOML, "ops@acme", "add delete floor")
	if err != nil {
		t.Fatalf("publish v2: %v", err)
	}
	if v1 != 1 || v2 != 2 {
		t.Fatalf("versions = %d, %d, want 1, 2", v1, v2)
	}

	rec := httptest.NewRecorder()
	h.GetPolicyBundle(rec, httptest.NewRequest(http.MethodGet, "/api/v1/policy-bundle", nil), gen.GetPolicyBundleParams{})
	if rec.Code != http.StatusOK {
		t.Fatalf("get: code=%d body=%s", rec.Code, rec.Body.String())
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag header")
	}
	var b orgcontract.PolicyBundle
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if b.Version != 2 || b.BundleTOML != v2TOML || b.Description != "add delete floor" {
		t.Fatalf("served bundle = %+v, want v2", b)
	}

	// The served envelope verifies under the publish key — the exact
	// check the agent runs.
	pub, err := orgcontract.VerifyPolicyBundle(b)
	if err != nil {
		t.Fatalf("served bundle does not verify: %v", err)
	}
	wantPub := priv.Public().(ed25519.PublicKey)
	if !pub.Equal(wantPub) {
		t.Error("served public key is not the publish key")
	}

	// Conditional revalidation: matching If-None-Match → 304, no body.
	rec304 := httptest.NewRecorder()
	h.GetPolicyBundle(rec304, httptest.NewRequest(http.MethodGet, "/api/v1/policy-bundle", nil),
		gen.GetPolicyBundleParams{IfNoneMatch: &etag})
	if rec304.Code != http.StatusNotModified || rec304.Body.Len() != 0 {
		t.Fatalf("conditional get: code=%d len=%d, want 304 empty", rec304.Code, rec304.Body.Len())
	}

	// Version history lists newest first.
	metas, err := ListPolicyBundles(ctx, d)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(metas) != 2 || metas[0].Version != 2 || metas[1].Version != 1 || metas[0].CreatedBy != "ops@acme" {
		t.Fatalf("history = %+v", metas)
	}
	old, err := PolicyBundleByVersion(ctx, d, 1)
	if err != nil || old.BundleTOML != escalatingOrgTOML {
		t.Fatalf("v1 by version = %+v err=%v", old, err)
	}
}

// TestPolicyBundle_NoBundle404 pins the empty-channel shape: 404 with
// the no_policy_bundle code — indistinguishable from a pre-G13 server,
// which is exactly the §14.2 compat posture.
func TestPolicyBundle_NoBundle404(t *testing.T) {
	h, _ := newTestHandlers(t)
	rec := httptest.NewRecorder()
	h.GetPolicyBundle(rec, httptest.NewRequest(http.MethodGet, "/api/v1/policy-bundle", nil), gen.GetPolicyBundleParams{})
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no_policy_bundle") {
		t.Fatalf("code=%d body=%s, want 404 no_policy_bundle", rec.Code, rec.Body.String())
	}
}

// TestPublishPolicyBundle_Refusals is the publish-gate table: a
// floor-violating or unparseable bundle is never signed, and a missing
// key refuses outright.
func TestPublishPolicyBundle_Refusals(t *testing.T) {
	_, d := newTestHandlers(t)
	priv := policyTestKey(t)
	ctx := context.Background()

	cases := []struct {
		name string
		toml string
		key  ed25519.PrivateKey
	}{
		{"relaxing override violates the org floor", "[[override]]\nrule = \"R-110\"\ndecision = \"allow\"\n", priv},
		{"unparseable TOML", "not toml at all [", priv},
		{"missing signing key", escalatingOrgTOML, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PublishPolicyBundle(ctx, d, tc.key, tc.toml, "ops@acme", "")
			if err == nil {
				t.Fatal("publish succeeded, want refusal")
			}
			if tc.key != nil && !errors.Is(err, ErrBundleInvalid) {
				t.Fatalf("err = %v, want ErrBundleInvalid", err)
			}
		})
	}
	if metas, _ := ListPolicyBundles(ctx, d); len(metas) != 0 {
		t.Fatalf("refused publishes left rows: %+v", metas)
	}
}

// TestEnroll_DeliversPolicyKey pins the enrol-time key delivery: with
// a policy key configured the response carries its base64url public
// half; without one the field is absent (pre-G13 wire shape).
func TestEnroll_DeliversPolicyKey(t *testing.T) {
	h, d := newTestHandlers(t)
	seedMember(t, d, "user-1", "dev@acme.example")
	priv := policyTestKey(t)
	wantKey := auth.EncodePublicKey(priv.Public().(ed25519.PublicKey))
	h.SetOrgPolicyPublicKey(wantKey)

	mintRec := httptest.NewRecorder()
	h.MintEnrolmentToken(mintRec, httptest.NewRequest(http.MethodPost, "/api/org/enrolment-tokens",
		bytes.NewReader([]byte(`{"user_id":"user-1"}`))))
	if mintRec.Code != http.StatusCreated {
		t.Fatalf("mint: code=%d body=%s", mintRec.Code, mintRec.Body.String())
	}
	var mint mintTokenResponse
	if err := json.Unmarshal(mintRec.Body.Bytes(), &mint); err != nil {
		t.Fatal(err)
	}

	agentPub, _, _ := ed25519.GenerateKey(rand.Reader)
	enrollBody, _ := json.Marshal(orgcontract.EnrollRequest{
		OneTimeToken:   mint.Token,
		AgentPublicKey: auth.EncodePublicKey(agentPub),
	})
	rec := httptest.NewRecorder()
	h.EnrollAgent(rec, httptest.NewRequest(http.MethodPost, "/api/agent/enroll", bytes.NewReader(enrollBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp orgcontract.EnrollResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OrgPolicyPublicKey != wantKey {
		t.Fatalf("org_policy_public_key = %q, want %q", resp.OrgPolicyPublicKey, wantKey)
	}
}
