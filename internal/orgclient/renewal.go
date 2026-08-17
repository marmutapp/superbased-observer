package orgclient

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgclient/gen"
	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// Admin-controlled Plane B, Phase 1b: grant renewal
// (docs/plans/admin-controlled-plane-b-phase-1b-mini-spec-2026-08-15.md §4).
//
// The rule, in its closed form (operator ruling R4 / parent amendment A14):
//
//	RENEWAL SIGNAL = any response on an AUTHENTICATED agent endpoint whose
//	status is 2xx, 304 or 404, where the request carried the node's bearer.
//
// 304 is included because it is the overwhelmingly common steady-state
// response on the policy polls (an If-None-Match hit), and a naive
// "2xx only" rule would silently expire every converged node. 404 is
// included because it is the CORRECT response for an org that governs
// nobody, and for a new agent against an old server.
//
// 401/403 are excluded because the authorization signal is the whole point
// (R5). 5xx and transport errors are excluded because they do not prove the
// server accepted the credential.
//
// This is ONE seam, not five call sites: a single classifier here, in the
// package that already knows a request carried the bearer.

// RenewalPath distinguishes the PUSH path from every other authenticated
// agent path. It exists for exactly one rule (§4.3 / parent §11.3): the
// authDenied latch clears ONLY on an authorized response on the push path,
// because the poll is the path an org might leave open while revoking the
// rest — which is the case the requirement was written for.
type RenewalPath string

const (
	// RenewalPathPush is POST /api/agent/push (and its authorization probe).
	RenewalPathPush RenewalPath = "push"
	// RenewalPathOther is every other authenticated agent endpoint.
	RenewalPathOther RenewalPath = "other"
)

// RenewalOutcome is one classified response.
type RenewalOutcome struct {
	// Authorized is true for 2xx / 304 / 404 — the server accepted the
	// bearer and answered.
	Authorized bool
	// Denied is true for 401 / 403.
	Denied bool
	// Path records which agent path produced it.
	Path RenewalPath
}

// SetRenewalSink installs the renewal observer. Nil = today's exact
// behaviour (no renewal, no latch), so a build with no governance wiring is
// unchanged.
func (c *Client) SetRenewalSink(fn func(RenewalOutcome)) {
	if c == nil {
		return
	}
	c.renewalSink = fn
}

// classifyRenewal maps a response status (or a transport error) onto a
// renewal outcome. ok=false means "no signal": neither a renewal nor a
// denial, so the latch and the clock are both left alone.
//
// It is a table, one row per status class, with one test per row (CLAUDE.md
// #5) so a future "simplification" to 2xx-only fails loudly rather than
// silently offboarding every converged node.
func classifyRenewal(status int, err error) (RenewalOutcome, bool) {
	if err != nil {
		// A transport error proves nothing about the server's opinion of
		// the credential.
		return RenewalOutcome{}, false
	}
	switch {
	case status >= 200 && status <= 299:
		return RenewalOutcome{Authorized: true}, true
	case status == http.StatusNotModified:
		return RenewalOutcome{Authorized: true}, true
	case status == http.StatusNotFound:
		return RenewalOutcome{Authorized: true}, true
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return RenewalOutcome{Denied: true}, true
	default:
		return RenewalOutcome{}, false
	}
}

// noteRenewal classifies one authenticated response and forwards it. Every
// authenticated agent path calls exactly this.
func (c *Client) noteRenewal(path RenewalPath, status int, err error) {
	if c == nil || c.renewalSink == nil {
		return
	}
	out, ok := classifyRenewal(status, err)
	if !ok {
		return
	}
	out.Path = path
	c.renewalSink(out)
}

// noteRenewalFromResponse is the http.Response-shaped convenience the raw
// agent paths call. A nil response with a nil error cannot happen, but it is
// handled rather than dereferenced: this runs inside every poll loop, and a
// panic here would take the daemon down for a diagnostic.
func (c *Client) noteRenewalFromResponse(path RenewalPath, resp *http.Response, err error) {
	if resp == nil {
		c.noteRenewal(path, 0, err)
		return
	}
	c.noteRenewal(path, resp.StatusCode, err)
}

// RenewalTracker holds the authDenied latch (§4.3).
//
// CONTEXT, so a future reader does not think the permission split was
// re-discovered by evidence: parent §13.2's amendment A14 concluded that the
// push/poll permission split §11.3 worried about DOES NOT EXIST under
// today's auth model (one bearer, one RequireBearer, member-level
// revocation). The latch is therefore DEFENSIVE-ONLY. It costs almost
// nothing and is correct in advance of any future split.
//
// The latch is in-memory and deliberately NOT persisted: a persisted deny
// latch would be a second, node-local revocation clock that an admin cannot
// clear.
type RenewalTracker struct {
	mu        sync.Mutex
	denied    bool
	lastProbe time.Time
}

// Observe folds one outcome into the latch.
func (t *RenewalTracker) Observe(o RenewalOutcome) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case o.Denied:
		// A 401/403 on ANY authenticated path latches renewal off. While
		// latched, no renewal is written no matter how many 2xx or 304
		// responses arrive from other paths.
		t.denied = true
	case o.Authorized && o.Path == RenewalPathPush:
		t.denied = false
	}
}

// Latched reports whether renewal is currently stopped by a denial.
func (t *RenewalTracker) Latched() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.denied
}

// ShouldProbe reports whether the caller should issue an explicit
// authorization probe on the push path, and records the attempt.
//
// This is review M2's fix. As first specified the clear rule was
// UNIMPLEMENTABLE: PushOnce returns early on an empty batch, BEFORE any
// request is sent, so a node whose scope filters everything out — or whose
// developer is on leave — performs zero push round trips. One transient 401
// (a server restart mid-key-rotation, a clock skew, a 403 from an unrelated
// path) would then latch renewal off for the daemon's lifetime, and an
// availability blip would silently offboard a compliant node up to 30 days
// later.
//
// The probe fires ONLY while latched and at most once per interval, so a
// healthy idle node stays completely silent and an idle fleet does not
// become chatty. The rejected alternative was an empty-batch heartbeat that
// always performs a round trip, which costs the same information for a
// permanently chattier fleet.
func (t *RenewalTracker) ShouldProbe(now time.Time, minInterval time.Duration) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.denied {
		return false
	}
	if !t.lastProbe.IsZero() && now.Sub(t.lastProbe) < minInterval {
		return false
	}
	t.lastProbe = now
	return true
}

// ProbePushAuthorization asks the org server, on the PUSH path and with the
// node's own bearer, whether this node is still authorized. It is the §4.3
// idle-node probe.
//
// It sends a fully-signed but EMPTY push envelope. It deliberately does NOT
// use a HEAD or a bare GET: the server registers only `POST /api/agent/push`
// on a Go 1.22 ServeMux, so any other method is answered 405 by the mux
// BEFORE the bearer middleware runs — a probe that could never tell
// "authorized" from "denied", which is the one thing it exists to do.
//
// It has no side effects on the push state: no cursor is advanced, no
// push-log row is written, and the server ingests zero rows.
func (c *Client) ProbePushAuthorization(ctx context.Context) error {
	enr, err := c.store.LoadEnrolment(ctx)
	if err != nil {
		return err
	}
	if enr == nil {
		return ErrNotEnrolled
	}
	bearer, err := c.bearers.LoadBearer()
	if errors.Is(err, ErrNoSecret) {
		return ErrNotEnrolled
	}
	if err != nil {
		return err
	}
	signKey, err := c.bearers.LoadAgentKey()
	if errors.Is(err, ErrNoSecret) {
		return ErrNotEnrolled
	}
	if err != nil {
		return err
	}
	raw, err := json.Marshal(orgcontract.PushEnvelope{AgentVersion: c.agentVersion})
	if err != nil {
		return err
	}
	wire, err := gzipBytes(raw)
	if err != nil {
		return err
	}
	gc, err := c.genClient(enr.OrgServerURL)
	if err != nil {
		return err
	}
	ts := time.Now().Unix()
	sig := ed25519.Sign(signKey, orgcontract.PushSigningMessage(ts, wire))
	params := &gen.PushBatchParams{
		XSBOTimestamp:      &ts,
		XSBOAgentSignature: strPtr(base64.RawURLEncoding.EncodeToString(sig)),
	}
	resp, derr := gc.PushBatchWithBodyWithResponse(ctx, params, "application/json", bytes.NewReader(wire),
		bearerEditor(bearer), gzipEncodingEditor)
	if derr != nil {
		c.noteRenewal(RenewalPathPush, 0, derr)
		return derr
	}
	c.noteRenewal(RenewalPathPush, resp.StatusCode(), nil)
	return nil
}
