package emit

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/provenance"
	"github.com/marmutapp/superbased-observer/internal/selfobs/run"
)

// Credential fixtures — declared ONCE and reused so the expected header is
// derived from the same values passed to Config (no duplicated literals that
// could drift). authScheme is the package const under test.
const (
	testKeyID = "kid-0001"
	testSec   = "shhh-0002"
	testTok   = "tok-0003"
)

func wantHeader(keyID, sec string) string { return authScheme + keyID + "." + sec }

// authRecorder captures the Authorization header (and TLS state) seen by a test
// server, concurrency-safe.
type authRecorder struct {
	mu      sync.Mutex
	seen    bool
	authHdr string
	overTLS bool
}

func (a *authRecorder) record(r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = true
	a.authHdr = r.Header.Get(headerAuthorization)
	a.overTLS = r.TLS != nil
}

func (a *authRecorder) snapshot() (seen bool, hdr string, overTLS bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.seen, a.authHdr, a.overTLS
}

func diagRun() run.DecisionRun {
	return run.DecisionRun{
		RunID:       "run-1",
		TraceID:     "corr-1",
		Component:   "test-component",
		Trigger:     "manual",
		Outcome:     "verified",
		LatencyMS:   3,
		InitiatedBy: provenance.ActorHuman,
	}
}

func TestNopNeverPanics(t *testing.T) {
	t.Parallel()

	sink := Nop()
	ctx := context.Background()
	sink.Emit(ctx, diagRun())
	if err := sink.ForceFlush(ctx); err != nil {
		t.Errorf("Nop ForceFlush: %v", err)
	}
	if err := sink.Shutdown(ctx); err != nil {
		t.Errorf("Nop Shutdown: %v", err)
	}
}

func TestNewReturnsNopOnEmpty(t *testing.T) {
	t.Parallel()

	// Empty endpoint.
	s, err := New(Config{KeyID: testKeyID, Secret: testSec})
	if err != nil {
		t.Fatalf("New empty endpoint err: %v", err)
	}
	if s == nil {
		t.Fatal("New returned a nil interface (must be Nop)")
	}
	s.Emit(context.Background(), diagRun()) // must not panic

	// Empty credential.
	s2, err := New(Config{Endpoint: "https://example.com:4318"})
	if err != nil {
		t.Fatalf("New empty cred err: %v", err)
	}
	if s2 == nil {
		t.Fatal("New returned a nil interface on empty credential (must be Nop)")
	}
	s2.Emit(context.Background(), diagRun()) // must not panic
}

func TestRejectsBadScheme(t *testing.T) {
	t.Parallel()

	if _, err := New(Config{Endpoint: "ftp://host:1234", KeyID: testKeyID, Secret: testSec}); err == nil {
		t.Error("New accepted an ftp:// endpoint; want error")
	}
	if _, err := New(Config{Endpoint: "http://host:1234", KeyID: testKeyID, Secret: testSec}); err == nil {
		t.Error("New accepted a plaintext http:// endpoint without Insecure; want error")
	}
	if _, err := New(Config{Endpoint: "http://host:1234", KeyID: testKeyID, Secret: testSec, Insecure: true}); err != nil {
		t.Errorf("New rejected http:// WITH Insecure: %v", err)
	}
}

// TestEmitSendsAuthHeader is the auth-header gate: the gateway must receive
// the composed bearer credential.
func TestEmitSendsAuthHeader(t *testing.T) {
	t.Parallel()

	rec := &authRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink, err := New(Config{Endpoint: srv.URL, KeyID: testKeyID, Secret: testSec, Insecure: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = sink.Shutdown(context.Background()) }()

	ctx := context.Background()
	sink.Emit(ctx, diagRun())
	if err := sink.ForceFlush(ctx); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}

	seen, hdr, _ := rec.snapshot()
	if !seen {
		t.Fatal("gateway never received the export")
	}
	if want := wantHeader(testKeyID, testSec); hdr != want {
		t.Errorf("Authorization = %q, want %q", hdr, want)
	}
}

// TestEmitTokenWinsOverKeyIDSecret pins Token precedence over KeyID+Secret.
func TestEmitTokenWinsOverKeyIDSecret(t *testing.T) {
	t.Parallel()

	rec := &authRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink, err := New(Config{Endpoint: srv.URL, KeyID: testKeyID, Secret: testSec, Token: testTok, Insecure: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = sink.Shutdown(context.Background()) }()

	sink.Emit(context.Background(), diagRun())
	if err := sink.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
	if _, hdr, _ := rec.snapshot(); hdr != authScheme+testTok {
		t.Errorf("Authorization = %q, want %q", hdr, authScheme+testTok)
	}
}

// TestRedirectDoesNotLeakCredential is the Mut B target: a 307 redirect must
// NOT cause the credential to be resent to the redirect target.
func TestRedirectDoesNotLeakCredential(t *testing.T) {
	t.Parallel()

	target := &authRecorder{}
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer targetSrv.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, targetSrv.URL+"/v1/traces", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	sink, err := New(Config{Endpoint: redirector.URL, KeyID: testKeyID, Secret: testSec, Insecure: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = sink.Shutdown(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sink.Emit(ctx, diagRun())
	// ForceFlush may return an error (the 307 is not a success) — that is fine.
	_ = sink.ForceFlush(ctx)

	if seen, hdr, _ := target.snapshot(); seen {
		t.Errorf("credential leaked to redirect target: received Authorization=%q", hdr)
	}
}

// TestEnvCannotDowngradeToPlaintext is the executable gate for R5-B1: with both
// OTEL_EXPORTER_OTLP_INSECURE and OTEL_EXPORTER_OTLP_TRACES_INSECURE set to
// "true", a SECURE https endpoint must still export over TLS — the endpoint
// scheme (via WithEndpointURL) overrides the env. A naive WithEndpoint build
// would downgrade to plaintext, the TLS server would reject the handshake, and
// the handler would never fire.
func TestEnvCannotDowngradeToPlaintext(t *testing.T) {
	// NOT parallel: mutates process env.
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_INSECURE", "true")

	rec := &authRecorder{}
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer tlsSrv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(tlsSrv.Certificate())
	tlsCfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}

	// Secure endpoint (https), Insecure:false — must stay TLS despite the env.
	sink, err := New(Config{Endpoint: tlsSrv.URL, KeyID: testKeyID, Secret: testSec, TLSConfig: tlsCfg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = sink.Shutdown(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sink.Emit(ctx, diagRun())
	if err := sink.ForceFlush(ctx); err != nil {
		t.Fatalf("ForceFlush over TLS failed (env downgrade suspected): %v", err)
	}

	seen, hdr, overTLS := rec.snapshot()
	if !seen {
		t.Fatal("TLS gateway never received the export — env downgraded the credential path to plaintext (R5-B1 regression)")
	}
	if !overTLS {
		t.Error("request did not arrive over TLS (R5-B1 regression)")
	}
	if want := wantHeader(testKeyID, testSec); hdr != want {
		t.Errorf("Authorization = %q, want %q", hdr, want)
	}
}

// TestHTTPSEndpointStaysTLSDespiteInsecureFlag is the executable gate for R6-B1:
// an https:// endpoint given TOGETHER with Insecure:true is a self-contradiction,
// and the scheme must win (coerce-to-TLS). A build that passed Insecure:true
// straight through to the factory would append WithInsecure() AFTER
// WithEndpointURL, downgrade the credential-bearing export to plaintext, the TLS
// handshake would fail, and the handler would never fire.
func TestHTTPSEndpointStaysTLSDespiteInsecureFlag(t *testing.T) {
	t.Parallel()

	rec := &authRecorder{}
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer tlsSrv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(tlsSrv.Certificate())
	tlsCfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}

	// https endpoint WITH Insecure:true — the scheme is authoritative (R6-B1),
	// so the credential MUST still travel over TLS.
	sink, err := New(Config{Endpoint: tlsSrv.URL, KeyID: testKeyID, Secret: testSec, Insecure: true, TLSConfig: tlsCfg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = sink.Shutdown(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sink.Emit(ctx, diagRun())
	if err := sink.ForceFlush(ctx); err != nil {
		t.Fatalf("ForceFlush over TLS failed (Insecure:true downgraded the https endpoint to plaintext — R6-B1 regression): %v", err)
	}

	seen, hdr, overTLS := rec.snapshot()
	if !seen {
		t.Fatal("TLS gateway never received the export — Insecure:true downgraded the credential path to plaintext (R6-B1 regression)")
	}
	if !overTLS {
		t.Error("request did not arrive over TLS despite an https endpoint (R6-B1 regression)")
	}
	if want := wantHeader(testKeyID, testSec); hdr != want {
		t.Errorf("Authorization = %q, want %q", hdr, want)
	}
}

// TestBuildEndpointURLValidation pins R6-SF1: BOTH the bare host:port branch
// and the full-URL branch converge on one parse+validate path, so a URL
// carrying userinfo (a credential-leak vector), an empty/port-only host, a bad
// port, and a bad/plaintext scheme are all rejected identically — while a valid
// endpoint resolves to the right scheme.
func TestBuildEndpointURLValidation(t *testing.T) {
	t.Parallel()

	// Rejections — each is a credential-leak or malformed-endpoint vector that
	// the bare branch previously let through unparsed.
	reject := []struct {
		name     string
		endpoint string
		insecure bool
	}{
		{"bare userinfo", "trusted@evil.example", false},
		{"scheme userinfo", "https://trusted@evil.example", false},
		{"bare port-only", ":4318", false},
		{"scheme port-only", "https://:4318", false},
		{"bare bad port", "collector:bad", false},
		{"scheme bad port", "https://collector:bad", false},
		{"bad scheme", "ftp://host", false},
		{"http without insecure", "http://host", false},
		// The pre-existing with-scheme cases.
		{"scheme userinfo short", "https://a@b", false},
		{"scheme empty host", "https:///x", false},
	}
	for _, tc := range reject {
		tc := tc
		t.Run("reject/"+tc.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := buildEndpointURL(tc.endpoint, tc.insecure); err == nil {
				t.Errorf("buildEndpointURL(%q, insecure=%v) = nil err; want rejection", tc.endpoint, tc.insecure)
			}
		})
	}

	// Acceptances — the resolved scheme must be correct.
	accept := []struct {
		name       string
		endpoint   string
		insecure   bool
		wantScheme string
		wantFull   string
	}{
		{"bare host:port defaults to https", "host:4318", false, "https", "https://host:4318"},
		{"bare host:port insecure downgrades to http", "host:4318", true, "http", "http://host:4318"},
		{"https scheme stays https despite insecure", "https://host:4318", true, "https", "https://host:4318"},
		{"http scheme with insecure stays http", "http://host:4318", true, "http", "http://host:4318"},
	}
	for _, tc := range accept {
		tc := tc
		t.Run("accept/"+tc.name, func(t *testing.T) {
			t.Parallel()
			full, scheme, err := buildEndpointURL(tc.endpoint, tc.insecure)
			if err != nil {
				t.Fatalf("buildEndpointURL(%q, insecure=%v): unexpected error: %v", tc.endpoint, tc.insecure, err)
			}
			if scheme != tc.wantScheme {
				t.Errorf("scheme = %q, want %q", scheme, tc.wantScheme)
			}
			if full != tc.wantFull {
				t.Errorf("full = %q, want %q", full, tc.wantFull)
			}
		})
	}
}
