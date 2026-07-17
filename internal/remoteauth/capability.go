package remoteauth

import (
	"crypto/subtle"
	"sync"
	"time"
)

// DefaultCapabilityTTL is the execute-capability lifetime — deliberately short
// (plan §4.2: single-use, short-lived, session- AND action-bound).
const DefaultCapabilityTTL = 2 * time.Minute

// ActionTerminalControl is the fixed action a terminal-control capability is
// minted against (plan §4.γ.1). The capability is additionally bound to the
// specific terminal handle so it cannot be replayed against another terminal.
const ActionTerminalControl = "terminal.control"

// executeCap is one minted execute capability. handle + confirm are populated
// only for terminal-control capabilities (§4.γ) and are zero for the plain
// session+action capabilities minted via Mint.
type executeCap struct {
	sessionID string
	action    string
	handle    string // terminal handle binding (terminal-control only)
	confirm   string // co-stored confirm nonce (terminal-control only)
	expiresAt time.Time
}

// CapabilityStore mints and consumes single-use execute capabilities (plan
// §4.2 / codex P0 #6 — an execute grant is NEVER a reusable token). A
// capability is bound to (session id, action) and expires; Consume removes it
// (single use). Revoking the owning session drops all its capabilities. Safe
// for concurrent use.
type CapabilityStore struct {
	mu   sync.Mutex
	ttl  time.Duration
	now  func() time.Time
	caps map[string]executeCap
}

// NewCapabilityStore builds a store. ttl<=0 uses DefaultCapabilityTTL; now nil
// uses time.Now.UTC.
func NewCapabilityStore(ttl time.Duration, now func() time.Time) *CapabilityStore {
	if ttl <= 0 {
		ttl = DefaultCapabilityTTL
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &CapabilityStore{ttl: ttl, now: now, caps: map[string]executeCap{}}
}

// Mint issues a fresh single-use execute capability bound to sessionID+action,
// returning its opaque token. Called only after a LOCAL approval step (plan
// §4.7) — this package does not perform the approval, it just represents its
// result.
func (c *CapabilityStore) Mint(sessionID, action string) (string, error) {
	tok, err := randToken(32)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.caps[tok] = executeCap{sessionID: sessionID, action: action, expiresAt: c.now().Add(c.ttl)}
	return tok, nil
}

// Consume validates and CONSUMES a capability: it must exist, be unexpired, and
// match sessionID+action exactly. It is removed on any lookup hit (single use),
// so a replay after a successful OR failed match cannot reuse it. Returns true
// only on a fully valid, first-use match.
func (c *CapabilityStore) Consume(token, sessionID, action string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	cap, ok := c.caps[token]
	if !ok {
		return false
	}
	delete(c.caps, token) // single-use: burn on any hit
	if c.now().After(cap.expiresAt) {
		return false
	}
	return cap.sessionID == sessionID && cap.action == action
}

// MintTerminalControl issues a single-use terminal-control capability bound to
// (sessionID + ActionTerminalControl + terminal handle), together with a
// co-stored server-minted confirm nonce (plan §4.γ). Both the capability token
// and the confirm are returned to the LOCAL approve-execute handler ONLY; they
// are memory-only, restart-invalidated, and never persisted or logged. The
// confirm is checked bound to THIS exact capability at consume time — a confirm
// minted for capability A cannot satisfy capability B.
func (c *CapabilityStore) MintTerminalControl(sessionID, handle string) (token, confirm string, err error) {
	token, err = randToken(32)
	if err != nil {
		return "", "", err
	}
	confirm, err = randToken(32)
	if err != nil {
		return "", "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.caps[token] = executeCap{
		sessionID: sessionID,
		action:    ActionTerminalControl,
		handle:    handle,
		confirm:   confirm,
		expiresAt: c.now().Add(c.ttl),
	}
	return token, confirm, nil
}

// ConsumeTerminalControl validates and CONSUMES a terminal-control capability
// atomically with its bound confirm (plan §4.γ.2). It returns true only when
// the token exists, the confirm matches the co-stored nonce (constant-time),
// the capability is unexpired, and it is bound to exactly
// (sessionID + ActionTerminalControl + handle).
//
// A WRONG confirm consumes NOTHING (the one deliberate exception to burn-on-hit,
// §4.γ.2): the confirm is a server-minted random nonce, not brute-forceable, so
// leaving the capability intact on a confirm miss creates no usable oracle while
// letting consume + confirm-check + lease-grant be one transaction (a failed
// confirm never burns the capability). Once the confirm matches, the capability
// is burned regardless of the binding outcome (single use).
func (c *CapabilityStore) ConsumeTerminalControl(token, confirm, sessionID, handle string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec, ok := c.caps[token]
	if !ok {
		return false
	}
	// Confirm first — a mismatch burns nothing.
	if subtle.ConstantTimeCompare([]byte(rec.confirm), []byte(confirm)) != 1 || rec.confirm == "" {
		return false
	}
	delete(c.caps, token) // burn on a confirmed hit (single use)
	if c.now().After(rec.expiresAt) {
		return false
	}
	return rec.sessionID == sessionID &&
		rec.action == ActionTerminalControl &&
		rec.handle == handle
}

// RevokeSession drops every capability minted for sessionID (session logout /
// rotation). A leaked-but-unused capability dies with its session.
func (c *CapabilityStore) RevokeSession(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for tok, cap := range c.caps {
		if cap.sessionID == sessionID {
			delete(c.caps, tok)
		}
	}
}

// RevokeAll drops every capability (global rotate).
func (c *CapabilityStore) RevokeAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.caps = map[string]executeCap{}
}
