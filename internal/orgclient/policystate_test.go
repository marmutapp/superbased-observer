package orgclient

import (
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// enrolledPolicyStateClient wires an enrolled Client against srv with a fresh
// signing key.
func enrolledPolicyStateClient(t *testing.T, srv *httptest.Server) *Client {
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

func sampleReport() orgcontract.PolicyStateReport {
	return orgcontract.PolicyStateReport{
		AgentVersion: "test-version",
		ReportSeq:    1,
		Rows: []orgcontract.PolicyStateRow{
			{Family: "guard.coding", EnforcementPoint: "guard", Status: "none", Reason: "no_policy", Mode: "off", LastSeen: time.Now().UTC().Format(time.RFC3339)},
		},
	}
}

// TestReportSeqPositive — the first issued seq is > 0 (R4-S1).
func TestReportSeqPositive(t *testing.T) {
	c := NewReportSeqCounter(filepath.Join(t.TempDir(), "seq"))
	first, err := c.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if first <= 0 {
		t.Fatalf("first seq = %d, want > 0", first)
	}
}

// TestReportSeqIsPersistedMonotonic — the persisted 0600 counter continues
// strictly upward across a simulated restart, and is IMMUNE to a regressed
// wall clock (the R4-B6 bug: a time.Now().UnixNano() source would hand a
// genuinely-later report a LOWER seq after an NTP correction toward -skew).
func TestReportSeqIsPersistedMonotonic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seq")

	c1 := NewReportSeqCounter(path)
	a, _ := c1.Next()
	b, _ := c1.Next()
	if a != 1 || b != 2 {
		t.Fatalf("in-process seqs = %d,%d, want 1,2", a, b)
	}

	// Simulate a daemon restart: a brand-new counter over the SAME sidecar
	// path, with NO in-process memory. A clock-based source would now regress;
	// the file-based counter continues strictly upward.
	c2 := NewReportSeqCounter(path)
	cVal, err := c2.Next()
	if err != nil {
		t.Fatalf("post-restart Next: %v", err)
	}
	if cVal != 3 {
		t.Fatalf("post-restart seq = %d, want strictly-upward 3", cVal)
	}
	if cVal <= b {
		t.Fatalf("post-restart seq %d not strictly greater than pre-restart %d — the clock-regression bug", cVal, b)
	}
}

// TestPostPolicyState_GatedOffByDefault — the opt-in defaults OFF: the reporter
// is only ever driven when the operator sets [org_client.share].policy_state.
func TestPostPolicyState_GatedOffByDefault(t *testing.T) {
	if config.Default().OrgClient.Share.PolicyState {
		t.Fatal("share.policy_state must default to false (node-side opt-in)")
	}
}

// TestPostPolicyState_EmptyAttributionOnWire — populate OrgID/UserEmail on a
// row; the sender must strip them so the WIRE carries empty attribution
// (server-stamped, R2-S2).
func TestPostPolicyState_EmptyAttributionOnWire(t *testing.T) {
	var gotOrg, gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gz, _ := gzip.NewReader(r.Body)
		raw, _ := io.ReadAll(gz)
		var rep orgcontract.PolicyStateReport
		_ = json.Unmarshal(raw, &rep)
		if len(rep.Rows) > 0 {
			gotOrg, gotUser = rep.Rows[0].OrgID, rep.Rows[0].UserEmail
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := enrolledPolicyStateClient(t, srv)
	rep := sampleReport()
	rep.Rows[0].OrgID = "leaked-org"
	rep.Rows[0].UserEmail = "leaked@user.example"
	if err := c.PostPolicyState(context.Background(), rep); err != nil {
		t.Fatalf("PostPolicyState: %v", err)
	}
	if gotOrg != "" || gotUser != "" {
		t.Fatalf("wire attribution = %q/%q, want empty/empty (stripped)", gotOrg, gotUser)
	}
}

// TestPostPolicyState_SignsLikePush — the request carries the Authorization
// bearer, the timestamped Ed25519 signature, and gzip Content-Encoding, and
// the signature verifies over PushSigningMessage(ts, wireBody) exactly like
// PushOnce. Dropping the signature path would make the server 401.
func TestPostPolicyState_SignsLikePush(t *testing.T) {
	var (
		sawAuth, sawSig, sawTS, sawGzip bool
		sigVerified                     bool
	)
	// Capture the client's public key so we can verify the signature.
	s := newAgentStore(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization") == "Bearer bearer-xyz"
		sawGzip = r.Header.Get("Content-Encoding") == "gzip"
		tsStr := r.Header.Get(orgcontract.HeaderTimestamp)
		sigB64 := r.Header.Get(orgcontract.HeaderAgentSignature)
		sawTS = tsStr != ""
		sawSig = sigB64 != ""
		wire, _ := io.ReadAll(r.Body)
		if sawTS && sawSig {
			ts, _ := strconv.ParseInt(tsStr, 10, 64)
			sig, _ := base64.RawURLEncoding.DecodeString(sigB64)
			sigVerified = ed25519.Verify(pub, orgcontract.PushSigningMessage(ts, wire), sig)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := s.WriteEnrolment(context.Background(), store.Enrolment{
		OrgID: "org-1", OrgServerURL: srv.URL, UserID: "scim-42", UserEmail: "dev@acme.example", BearerKeyID: "test",
	}); err != nil {
		t.Fatalf("WriteEnrolment: %v", err)
	}
	c := New(config.OrgClientConfig{Enabled: true}, s, &memBearerStore{bearer: "bearer-xyz", key: priv}, "v", srv.Client(), quietLogger())

	if err := c.PostPolicyState(context.Background(), sampleReport()); err != nil {
		t.Fatalf("PostPolicyState: %v", err)
	}
	if !sawAuth || !sawSig || !sawTS || !sawGzip {
		t.Fatalf("headers: auth=%v sig=%v ts=%v gzip=%v — all must be set like PushOnce", sawAuth, sawSig, sawTS, sawGzip)
	}
	if !sigVerified {
		t.Fatal("signature did not verify over PushSigningMessage(ts, wireBody)")
	}
}

// TestPostPolicyState_404IsNonFatalNoRetry — a pre-P0-6 server (404/405)
// returns the ErrPolicyAckUnsupported sentinel so the caller latches off; it is
// non-fatal and makes no second attempt (S8).
func TestPostPolicyState_404IsNonFatalNoRetry(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := enrolledPolicyStateClient(t, srv)
	err := c.PostPolicyState(context.Background(), sampleReport())
	if !errors.Is(err, ErrPolicyAckUnsupported) {
		t.Fatalf("err = %v, want ErrPolicyAckUnsupported", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("server calls = %d, want exactly 1 (no retry inside the sender)", calls.Load())
	}
}

// TestPostPolicyState_CtxCancelStopsSend — the request is built with the caller
// ctx, so a cancelled context aborts the in-flight POST (R2-S4 lifecycle proof
// for the reporter's report(ctx)). A context.Background()-built request would
// ignore the cancel and hang until the slow server responds.
func TestPostPolicyState_CtxCancelStopsSend(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(release)

	c := enrolledPolicyStateClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	err := c.PostPolicyState(ctx, sampleReport())
	if err == nil {
		t.Fatal("want a context-cancellation error, got nil")
	}
}

// TestPostPolicyState_RejectsNonPositiveSeq — the sender refuses a report with
// a non-positive ReportSeq before touching the wire (R4-S1 defense in depth).
func TestPostPolicyState_RejectsNonPositiveSeq(t *testing.T) {
	c := New(config.OrgClientConfig{}, nil, &memBearerStore{}, "v", http.DefaultClient, quietLogger())
	rep := sampleReport()
	rep.ReportSeq = 0
	if err := c.PostPolicyState(context.Background(), rep); err == nil {
		t.Fatal("want an error for report_seq = 0")
	}
}
