package orgclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// enrolledJudgeRelayClient wires an enrolled Client against srv with a fresh
// signing key — mirrors enrolledPolicyStateClient (policystate_test.go).
func enrolledJudgeRelayClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	s := newAgentStore(t)
	if err := s.WriteEnrolment(context.Background(), store.Enrolment{
		OrgID: "org-1", OrgName: "Acme", OrgServerURL: srv.URL,
		UserID: "scim-42", UserEmail: "dev@acme.example", BearerKeyID: "test",
	}); err != nil {
		t.Fatalf("WriteEnrolment: %v", err)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	bs := &memBearerStore{bearer: "bearer-xyz", key: priv}
	cfg := config.OrgClientConfig{Enabled: true, KeychainID: config.DefaultKeychainID}
	return New(cfg, s, bs, "test-version", srv.Client(), quietLogger())
}

// TestJudgeRelay_SignsPlainJSONNoGzip — the request carries the Authorization
// bearer + timestamped Ed25519 signature exactly like PostPolicyState, but
// UNLIKE PostPolicyState the wire body is plain JSON: no Content-Encoding
// header, and the signature verifies over PushSigningMessage(ts, rawBody)
// where rawBody is the UNCOMPRESSED marshalled request (§1 wire contract).
func TestJudgeRelay_SignsPlainJSONNoGzip(t *testing.T) {
	var (
		sawAuth, sawGzipHeader bool
		sigVerified            bool
		gotReq                 judgeRelayRequest
	)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization") == "Bearer bearer-xyz"
		_, sawGzipHeader = r.Header["Content-Encoding"]
		tsStr := r.Header.Get(orgcontract.HeaderTimestamp)
		sigB64 := r.Header.Get(orgcontract.HeaderAgentSignature)
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotReq)
		if tsStr != "" && sigB64 != "" {
			ts, _ := strconv.ParseInt(tsStr, 10, 64)
			sig, _ := base64.RawURLEncoding.DecodeString(sigB64)
			sigVerified = ed25519.Verify(pub, orgcontract.PushSigningMessage(ts, raw), sig)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(judgeRelayResponse{Text: "OK", Model: "gpt-4o-mini"})
	}))
	defer srv.Close()

	s := newAgentStore(t)
	if err := s.WriteEnrolment(context.Background(), store.Enrolment{
		OrgID: "org-1", OrgServerURL: srv.URL, UserID: "scim-42", UserEmail: "dev@acme.example", BearerKeyID: "test",
	}); err != nil {
		t.Fatalf("WriteEnrolment: %v", err)
	}
	c := New(config.OrgClientConfig{Enabled: true}, s, &memBearerStore{bearer: "bearer-xyz", key: priv}, "v", srv.Client(), quietLogger())

	reply, err := c.JudgeRelay(context.Background(), "admission", "judge this", "gpt-4o")
	if err != nil {
		t.Fatalf("JudgeRelay: %v", err)
	}
	if !sawAuth || sawGzipHeader {
		t.Fatalf("headers: auth=%v gzipHeaderPresent=%v — want auth set and NO Content-Encoding (plain JSON, §1)", sawAuth, sawGzipHeader)
	}
	if !sigVerified {
		t.Fatal("signature did not verify over PushSigningMessage(ts, rawBody)")
	}
	if gotReq.Purpose != "admission" || gotReq.Prompt != "judge this" || gotReq.ModelHint != "gpt-4o" {
		t.Fatalf("wire body = %+v, want purpose=admission prompt=%q model_hint=gpt-4o", gotReq, "judge this")
	}
	if reply.Text != "OK" || reply.Model != "gpt-4o-mini" {
		t.Fatalf("reply = %+v, want {OK gpt-4o-mini}", reply)
	}
}

// TestJudgeRelay_OmitsEmptyModelHint — an empty modelHint must not appear on
// the wire at all (json:",omitempty" — §1 "optional, recorded only").
func TestJudgeRelay_OmitsEmptyModelHint(t *testing.T) {
	var sawKey bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		_, sawKey = m["model_hint"]
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(judgeRelayResponse{Text: "OK"})
	}))
	defer srv.Close()

	c := enrolledJudgeRelayClient(t, srv)
	if _, err := c.JudgeRelay(context.Background(), "eval", "prompt", ""); err != nil {
		t.Fatalf("JudgeRelay: %v", err)
	}
	if sawKey {
		t.Fatal("model_hint key present on the wire for an empty hint, want omitted")
	}
}

// TestJudgeRelay_404And405LatchOff — an older server without the endpoint
// returns ErrJudgeRelayUnsupported for BOTH 404 and 405, so the caller can
// latch the relay off for the daemon lifetime (§3) regardless of which the
// server's router chooses.
func TestJudgeRelay_404And405LatchOff(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()

			c := enrolledJudgeRelayClient(t, srv)
			_, err := c.JudgeRelay(context.Background(), "admission", "p", "")
			if !errors.Is(err, ErrJudgeRelayUnsupported) {
				t.Fatalf("status %d: err = %v, want ErrJudgeRelayUnsupported", code, err)
			}
		})
	}
}

// TestJudgeRelay_AuthFailed — 401/403 map to ErrAuthFailed.
func TestJudgeRelay_AuthFailed(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()

			c := enrolledJudgeRelayClient(t, srv)
			_, err := c.JudgeRelay(context.Background(), "admission", "p", "")
			if !errors.Is(err, ErrAuthFailed) {
				t.Fatalf("status %d: err = %v, want ErrAuthFailed", code, err)
			}
		})
	}
}

// TestJudgeRelay_GenericErrorBoundedExcerpt — any other non-200 (e.g. 502/503)
// returns a generic error carrying the status code and a body excerpt bounded
// to judgeRelayMaxBodyExcerpt characters, even when the server sends a much
// longer body (§3 "bounded 300 chars").
func TestJudgeRelay_GenericErrorBoundedExcerpt(t *testing.T) {
	longBody := strings.Repeat("x", judgeRelayMaxBodyExcerpt*3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(longBody))
	}))
	defer srv.Close()

	c := enrolledJudgeRelayClient(t, srv)
	_, err := c.JudgeRelay(context.Background(), "admission", "p", "")
	if err == nil {
		t.Fatal("want a non-nil error for 502")
	}
	if errors.Is(err, ErrJudgeRelayUnsupported) || errors.Is(err, ErrAuthFailed) {
		t.Fatalf("502 must not map to a sentinel, got %v", err)
	}
	if !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v, want it to mention status 502", err)
	}
	if len(err.Error()) > judgeRelayMaxBodyExcerpt+200 { // status text + wrapper prefix headroom
		t.Fatalf("error message length %d looks unbounded (excerpt cap = %d): %v", len(err.Error()), judgeRelayMaxBodyExcerpt, err)
	}
}

// TestJudgeRelay_NotEnrolled — a store with no enrolment row returns
// ErrNotEnrolled without attempting any network call.
func TestJudgeRelay_NotEnrolled(t *testing.T) {
	s := newAgentStore(t)
	c := New(config.OrgClientConfig{Enabled: true}, s, &memBearerStore{}, "v", http.DefaultClient, quietLogger())
	_, err := c.JudgeRelay(context.Background(), "admission", "p", "")
	if !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("err = %v, want ErrNotEnrolled", err)
	}
}

// TestJudgeRelay_CtxCancelStopsSend — the request is built with the caller
// ctx, so a cancelled context aborts the in-flight POST (mirrors
// TestPostPolicyState_CtxCancelStopsSend).
func TestJudgeRelay_CtxCancelStopsSend(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(release)

	c := enrolledJudgeRelayClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := c.JudgeRelay(ctx, "admission", "p", "")
	if err == nil {
		t.Fatal("want a context-cancellation error, got nil")
	}
}

// TestJudgeRelay_NoInternalRetry — the sender makes exactly one attempt per
// call regardless of outcome (§3 "no internal retry; the admission pipeline
// owns fail posture").
func TestJudgeRelay_NoInternalRetry(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := enrolledJudgeRelayClient(t, srv)
	if _, err := c.JudgeRelay(context.Background(), "admission", "p", ""); err == nil {
		t.Fatal("want a non-nil error for 500")
	}
	if calls != 1 {
		t.Fatalf("server calls = %d, want exactly 1 (no retry inside the sender)", calls)
	}
}
