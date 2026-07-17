//go:build !no_obs

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/db"
	obsstore "github.com/marmutapp/superbased-observer/internal/obs/store"
)

// TestEgress_AdviseModeEndToEnd is the G22 advise-mode integration: the REAL
// daemon path (buildProxy → wireAdmission → obsAdmitter) drives a flagged
// request whose egress policy would route it to a local target — but in ADVISE
// mode the directive is evaluated, logged, and RECORDED to obs_egress_decisions
// yet NEVER applied, so the request still forwards to the DEFAULT upstream
// (design §10 P4/§6 advise posture). This is the "flagged-to-local, advise"
// close-out the task asks for.
func TestEgress_AdviseModeEndToEnd(t *testing.T) {
	ctx := context.Background()

	var defaultHit, targetHit bool
	defaultUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defaultHit = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","model":"claude-opus-4-8","usage":{"input_tokens":5,"output_tokens":3},"content":[]}`))
	}))
	t.Cleanup(defaultUp.Close)
	targetUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHit = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_2","model":"local","usage":{"input_tokens":5,"output_tokens":3},"content":[]}`))
	}))
	t.Cleanup(targetUp.Close)

	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "observer.db")
	configPath := filepath.Join(tmp, "config.toml")
	configBody := fmt.Sprintf(`
[observer]
db_path = %q
log_level = "warn"

[proxy]
enabled = true
port = 8820
anthropic_upstream = %q
openai_upstream = "http://127.0.0.1:1"
chatgpt_upstream = "http://127.0.0.1:2"
prewarm_targets = []

[observability]
enabled = true

[observability.admission]
enabled = true
mode = "observe"

[observability.admission.prefilter]
deny = ["forbidden-topic"]

[observability.egress]
enabled = true
mode = "advise"

[[observability.egress.targets]]
id = "local"
url = %q
shape = "anthropic"

[[observability.egress.rules]]
name = "flagged-to-local"
when = { verdict_at_least = "flag" }
action = { route_to_upstream = "local" }
on_unavailable = "deny"
reason_code = "egress_flagged_local"
`, dbPath, defaultUp.URL, targetUp.URL)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	p, cleanup, _, _, _, err := buildProxy(ctx, configPath, "", 0, "127.0.0.1")
	if err != nil {
		t.Fatalf("buildProxy: %v", err)
	}
	t.Cleanup(cleanup)
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)

	// A request whose last user text trips the prefilter deny → verdict deny →
	// the flagged-to-local egress rule fires (advise).
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages",
		strings.NewReader(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"please help with forbidden-topic"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	// Advise never applies: the request must reach the DEFAULT upstream, NOT
	// the egress target.
	if !defaultHit {
		t.Error("advise mode did not forward to the default upstream")
	}
	if targetHit {
		t.Error("advise mode APPLIED the route — the request reached the egress target (must only log/record)")
	}

	// The egress directive must be RECORDED (mode advise, not applied).
	conn2, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("db.Open (inspect): %v", err)
	}
	defer func() { _ = conn2.Close() }()
	os2, err := obsstore.Open(ctx, conn2)
	if err != nil {
		t.Fatalf("obsstore.Open (inspect): %v", err)
	}
	rows, err := os2.ListEgressDecisions(ctx, 10)
	if err != nil {
		t.Fatalf("ListEgressDecisions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want exactly 1 recorded egress decision, got %d", len(rows))
	}
	r0 := rows[0]
	if r0.Mode != "advise" || r0.Applied {
		t.Errorf("advise decision recorded wrong: mode=%q applied=%v", r0.Mode, r0.Applied)
	}
	if r0.RuleName != "flagged-to-local" || r0.Action != "route_upstream" || r0.ReasonCode != "egress_flagged_local" {
		t.Errorf("recorded directive fields wrong: %+v", r0)
	}
	if r0.RequestID == "" {
		t.Error("egress decision has no request_id — the P0 stable id did not propagate")
	}
	if cr, err := os2.VerifyEgressChain(ctx); err != nil || !cr.OK {
		t.Errorf("egress chain not intact: %+v (err=%v)", cr, err)
	}
}

// TestEgress_EnforceRealizedOutcomeEndToEnd is the G22 wave-2 close-out: the
// REAL daemon path drives a flagged request in ENFORCE mode, the proxy routes
// it to the on-prem target AND reports the realized outcome back through the
// obs_wire EgressReporter seam, so the audit row's `applied` records what
// ACTUALLY happened on the wire (not mere intent). The chain stays intact after
// the in-place realized-outcome update.
func TestEgress_EnforceRealizedOutcomeEndToEnd(t *testing.T) {
	ctx := context.Background()

	var defaultHit, targetHit bool
	defaultUp := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		defaultHit = true
		t.Errorf("default upstream hit despite an enforce route_upstream: %s", r.URL.Path)
	}))
	t.Cleanup(defaultUp.Close)
	targetUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHit = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_2","model":"local","usage":{"input_tokens":5,"output_tokens":3},"content":[]}`))
	}))
	t.Cleanup(targetUp.Close)

	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "observer.db")
	configPath := filepath.Join(tmp, "config.toml")
	configBody := fmt.Sprintf(`
[observer]
db_path = %q
log_level = "warn"

[proxy]
enabled = true
port = 8820
anthropic_upstream = %q
openai_upstream = "http://127.0.0.1:1"
chatgpt_upstream = "http://127.0.0.1:2"
prewarm_targets = []

[observability]
enabled = true

[observability.admission]
enabled = true
mode = "observe"

[observability.admission.prefilter]
deny = ["forbidden-topic"]

[observability.egress]
enabled = true
mode = "enforce"

[[observability.egress.targets]]
id = "local"
url = %q
shape = "anthropic"

[[observability.egress.rules]]
name = "flagged-to-local"
when = { verdict_at_least = "flag" }
action = { route_to_upstream = "local" }
on_unavailable = "deny"
reason_code = "egress_flagged_local"
`, dbPath, defaultUp.URL, targetUp.URL)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	p, cleanup, _, _, _, err := buildProxy(ctx, configPath, "", 0, "127.0.0.1")
	if err != nil {
		t.Fatalf("buildProxy: %v", err)
	}
	t.Cleanup(cleanup)
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages",
		strings.NewReader(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"please help with forbidden-topic"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if !targetHit {
		t.Error("enforce mode did not route the flagged request to the on-prem target")
	}
	if defaultHit {
		t.Error("enforce route leaked to the default upstream")
	}

	conn2, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("db.Open (inspect): %v", err)
	}
	defer func() { _ = conn2.Close() }()
	os2, err := obsstore.Open(ctx, conn2)
	if err != nil {
		t.Fatalf("obsstore.Open (inspect): %v", err)
	}
	rows, err := os2.ListEgressDecisions(ctx, 10)
	if err != nil {
		t.Fatalf("ListEgressDecisions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want exactly 1 recorded egress decision, got %d", len(rows))
	}
	r0 := rows[0]
	if r0.Mode != "enforce" || r0.Action != "route_upstream" {
		t.Errorf("recorded directive fields wrong: %+v", r0)
	}
	// The realized-outcome callback must have flipped `applied` true and stamped
	// the realized_outcome label — the row records what actually happened.
	if !r0.Applied || r0.RealizedOutcome != "applied" {
		t.Errorf("realized outcome not recorded: applied=%v outcome=%q", r0.Applied, r0.RealizedOutcome)
	}
	if r0.FailClosed {
		t.Errorf("route succeeded but fail_closed was set: %+v", r0)
	}
	if cr, err := os2.VerifyEgressChain(ctx); err != nil || !cr.OK {
		t.Errorf("egress chain not intact after realized-outcome update: %+v (err=%v)", cr, err)
	}
}

// egressRefusalShape decodes a provider-shaped (Anthropic-lane) refusal body
// and asserts it is the admission 403 JSON — {"type":"error","error":
// {"type":"permission_error",...}} — never a raw proxy 502/plain-text error.
func egressRefusalShape(t *testing.T, resp *http.Response, body []byte) {
	t.Helper()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("want provider-shaped 403 refusal, got status %d (body %q)", resp.StatusCode, body)
	}
	if resp.StatusCode == http.StatusBadGateway {
		t.Error("fail-closed leaked a raw 502 to the client")
	}
	var shaped struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &shaped); err != nil {
		t.Errorf("refusal body is not JSON: %v (body %q)", err, body)
		return
	}
	if shaped.Type != "error" || shaped.Error.Type != "permission_error" {
		t.Errorf("refusal not provider-shaped: %+v", shaped)
	}
}

// egressE2EConfig renders the shared enforce-mode daemon config for the
// fail-posture e2e tests: prefilter-denied text trips the flagged rule whose
// route_to_upstream points at targetURL with the given on_unavailable posture.
func egressE2EConfig(dbPath, defaultURL, targetURL, onUnavailable string) string {
	return fmt.Sprintf(`
[observer]
db_path = %q
log_level = "warn"

[proxy]
enabled = true
port = 8820
anthropic_upstream = %q
openai_upstream = "http://127.0.0.1:1"
chatgpt_upstream = "http://127.0.0.1:2"
prewarm_targets = []

[observability]
enabled = true

[observability.admission]
enabled = true
mode = "observe"

[observability.admission.prefilter]
deny = ["forbidden-topic"]

[observability.egress]
enabled = true
mode = "enforce"

[[observability.egress.targets]]
id = "local"
url = %q
shape = "anthropic"

[[observability.egress.rules]]
name = "flagged-to-local"
when = { verdict_at_least = "flag" }
action = { route_to_upstream = "local" }
on_unavailable = %q
reason_code = "egress_flagged_local"
`, dbPath, defaultURL, targetURL, onUnavailable)
}

// postFlagged drives one prefilter-flagged Anthropic-shaped request through
// the proxy under test and returns the response + body.
func postFlagged(t *testing.T, base string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/messages",
		strings.NewReader(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"please help with forbidden-topic"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, body
}

// TestEgress_EnforceFailClosedEndToEnd is the G22 wave-2 enforce live-e2e
// (deferred item 2), fail-closed half: the REAL daemon path (buildProxy →
// wireAdmission → obsAdmitter → proxy → EgressReporter) drives flagged
// requests whose PINNED locality target (on_unavailable = deny ⇒
// MustUseTarget) is DEAD. Every request must fail CLOSED with the
// provider-shaped 403 — never a raw 502, never a leak to the default upstream
// — first via runtime dial failure (upstream_error), then, once the breaker
// opens at 3 consecutive failures, via the pre-dial short-circuit
// (breaker_open). The realized-outcome callback must land on each decision
// row, and the hash chain must verify over all produced rows (the in-place
// realized updates are outside the preimage).
func TestEgress_EnforceFailClosedEndToEnd(t *testing.T) {
	ctx := context.Background()

	defaultUp := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("fail-closed leaked to the default upstream: %s", r.URL.Path)
	}))
	t.Cleanup(defaultUp.Close)
	// A dead pinned target: allocate a real listener, then close it so the
	// port is refused at dial time (no external network involved).
	deadTarget := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	deadURL := deadTarget.URL
	deadTarget.Close()

	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "observer.db")
	configPath := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(configPath, []byte(egressE2EConfig(dbPath, defaultUp.URL, deadURL, "deny")), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	p, cleanup, _, _, _, err := buildProxy(ctx, configPath, "", 0, "127.0.0.1")
	if err != nil {
		t.Fatalf("buildProxy: %v", err)
	}
	t.Cleanup(cleanup)
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)

	// Requests 1-3 dial the dead target (fail closed, upstream_error) and
	// train the breaker to its 3-failure threshold; request 4 must be refused
	// BEFORE the dial (breaker_open) — still the same provider-shaped 403.
	const total = 4
	for i := 0; i < total; i++ {
		resp, body := postFlagged(t, srv.URL)
		egressRefusalShape(t, resp, body)
	}

	conn2, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("db.Open (inspect): %v", err)
	}
	defer func() { _ = conn2.Close() }()
	os2, err := obsstore.Open(ctx, conn2)
	if err != nil {
		t.Fatalf("obsstore.Open (inspect): %v", err)
	}
	rows, err := os2.ListEgressDecisions(ctx, 10)
	if err != nil {
		t.Fatalf("ListEgressDecisions: %v", err)
	}
	if len(rows) != total {
		t.Fatalf("want %d recorded egress decisions, got %d", total, len(rows))
	}
	for i, r0 := range rows {
		if r0.Mode != "enforce" || r0.Action != "route_upstream" || !r0.MustUseTarget {
			t.Errorf("row %d directive fields wrong: %+v", i, r0)
		}
		// The realized-outcome callback must have landed on EVERY row: never
		// applied, always fail-closed.
		if r0.Applied {
			t.Errorf("row %d marked applied on a dead pinned target: %+v", i, r0)
		}
		if !r0.FailClosed {
			t.Errorf("row %d not marked fail_closed: %+v", i, r0)
		}
		if r0.RequestID == "" {
			t.Errorf("row %d has no request_id", i)
		}
	}
	// rows are newest-first: the LAST request short-circuited on the open
	// breaker; the first three failed at the dial.
	if got := rows[0].RealizedOutcome; got != "breaker_open" {
		t.Errorf("newest row realized_outcome = %q, want breaker_open (pre-dial short-circuit)", got)
	}
	for i := 1; i < total; i++ {
		if got := rows[i].RealizedOutcome; got != "upstream_error" {
			t.Errorf("row %d realized_outcome = %q, want upstream_error (runtime dial failure)", i, got)
		}
	}
	if cr, err := os2.VerifyEgressChain(ctx); err != nil || !cr.OK || cr.Rows != total {
		t.Errorf("egress chain not intact over %d realized-updated rows: %+v (err=%v)", total, cr, err)
	}
}

// TestEgress_EnforceFailOpenEndToEnd is the fail-open half of the enforce
// live-e2e: the same dead target but on_unavailable = fail_open (a
// cost/cohort convenience route, NOT pinned). The proxy dials the target,
// observes the transport failure, and re-forwards ONCE to the default
// upstream — the client gets the default upstream's 200, the decision row
// records applied=false + realized_outcome=fallback_open (the directed target
// was never reached), and the chain verifies.
func TestEgress_EnforceFailOpenEndToEnd(t *testing.T) {
	ctx := context.Background()

	var defaultHits int
	defaultUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defaultHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_fb","model":"claude-opus-4-8","usage":{"input_tokens":5,"output_tokens":3},"content":[]}`))
	}))
	t.Cleanup(defaultUp.Close)
	deadTarget := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	deadURL := deadTarget.URL
	deadTarget.Close()

	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "observer.db")
	configPath := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(configPath, []byte(egressE2EConfig(dbPath, defaultUp.URL, deadURL, "fail_open")), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	p, cleanup, _, _, _, err := buildProxy(ctx, configPath, "", 0, "127.0.0.1")
	if err != nil {
		t.Fatalf("buildProxy: %v", err)
	}
	t.Cleanup(cleanup)
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)

	resp, body := postFlagged(t, srv.URL)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("fail-open did not serve the default upstream's response: status %d (body %q)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "msg_fb") {
		t.Errorf("response body is not the default upstream's: %q", body)
	}
	if defaultHits != 1 {
		t.Errorf("default upstream hit %d times, want exactly 1 (one clean re-forward)", defaultHits)
	}

	conn2, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("db.Open (inspect): %v", err)
	}
	defer func() { _ = conn2.Close() }()
	os2, err := obsstore.Open(ctx, conn2)
	if err != nil {
		t.Fatalf("obsstore.Open (inspect): %v", err)
	}
	rows, err := os2.ListEgressDecisions(ctx, 10)
	if err != nil {
		t.Fatalf("ListEgressDecisions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want exactly 1 recorded egress decision, got %d", len(rows))
	}
	r0 := rows[0]
	if r0.Mode != "enforce" || r0.Action != "route_upstream" || r0.MustUseTarget {
		t.Errorf("directive fields wrong (fail_open must not pin): %+v", r0)
	}
	if r0.Applied || r0.FailClosed || r0.RealizedOutcome != "fallback_open" {
		t.Errorf("realized outcome wrong: applied=%v fail_closed=%v outcome=%q (want fallback_open)",
			r0.Applied, r0.FailClosed, r0.RealizedOutcome)
	}
	if cr, err := os2.VerifyEgressChain(ctx); err != nil || !cr.OK {
		t.Errorf("egress chain not intact: %+v (err=%v)", cr, err)
	}
}
