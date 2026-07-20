package termlease

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// Authorization errors. Every miss fails closed with a distinct sentinel so the
// caller can audit WHICH leg of the conjunction rejected (metadata only — never
// the secret itself).
var (
	// ErrNotRemoteExposed — the request did not arrive on a remotely-exposed
	// execute-classified route (provenance check, defence-in-depth over the
	// route registry).
	ErrNotRemoteExposed = errors.New("termlease: request is not on a remote-exposed execute route")
	// ErrTerminalDisabled — remote.allow_terminal is false. Enforced HERE at the
	// grant boundary, not only in remoteAuthz at upgrade (plan §4.δ).
	ErrTerminalDisabled = errors.New("termlease: remote terminal control is disabled (remote.allow_terminal=false)")
	// ErrNoDeviceSession — the device session failed validation (unknown,
	// expired, idle-timed-out, or superseded generation).
	ErrNoDeviceSession = errors.New("termlease: no valid device session")
	// ErrPolicyDenied — the [terminal.launch] policy does not admit driving this
	// terminal handle (tool/project-root allow-list).
	ErrPolicyDenied = errors.New("termlease: terminal handle not permitted by launch policy")
	// ErrCapabilityRejected — the single-use capability + bound confirm did not
	// validate (unknown/expired/replayed capability, wrong binding, or a confirm
	// that does not match the exact capability). A failed confirm consumes
	// NOTHING.
	ErrCapabilityRejected = errors.New("termlease: execute capability or bound confirm rejected")
	// ErrMissingField — a required identity field on the request was empty.
	ErrMissingField = errors.New("termlease: missing required authorization field")
)

// WriterGrant is the unforgeable proof that the full §4.δ conjunction held for
// a specific (terminal handle, device session). It has no exported fields and
// no constructor other than Authorize, so a caller cannot fabricate an
// authorized grant — the same structural guarantee as
// internal/aggregateclient's Gate. A zero-value grant is never authorized.
type WriterGrant struct {
	authorized bool
	handle     string
	holder     string
	holderKey  string
	standing   bool
}

// Authorized reports whether this grant proves a successful §4.δ conjunction.
// A zero-value grant returns false.
func (g WriterGrant) Authorized() bool { return g.authorized }

// Handle is the terminal handle the grant is bound to.
func (g WriterGrant) Handle() string { return g.handle }

// Holder is the device-session identity (fingerprint) recorded on the grant for
// the local-UI display + audit. Never the full bearer secret.
func (g WriterGrant) Holder() string { return g.holder }

// HolderKey is the FULL, non-reversible device-session key (the complete sha256
// hex of the device-session id) — the writer-lease HOLDER IDENTITY the manager
// stores and matches a per-device revoke against. It is byte-identical to the
// dashboard's deviceSessionKey and remoteauth's SessionInfo.ID, so a revoke that
// resolves a device to its full session hash targets exactly one lease with no
// 8-char-prefix collision. Distinct from Holder (the 8-char display prefix); the
// RAW session id is never recorded. Empty for a zero-value grant.
func (g WriterGrant) HolderKey() string { return g.holderKey }

// Standing reports which credential leg minted this grant: true for the
// reusable standing terminal-control secret (AuthorizeStanding), false for a
// single-use capability (Authorize) or a zero-value grant. Provenance only —
// it grants nothing by itself; the manager records it on the lease so the
// OPT-IN revoke-standing-on-local-takeover policy can tell a standing-secret
// writer apart from a single-use one at supersede time.
func (g WriterGrant) Standing() bool { return g.standing }

// SessionValidator validates a device-session id (satisfied by
// *remoteauth.SessionStore). Injected so this package never imports remoteauth.
type SessionValidator interface {
	Validate(deviceSessionID string) error
}

// LaunchPolicy reports whether the [terminal.launch] policy admits driving a
// given terminal handle. Satisfied by a cmd/dashboard adapter that resolves the
// handle to its tool/project-root and checks the operator's allow-list.
type LaunchPolicy interface {
	Allowed(handle string) bool
}

// CapabilityConsumer consumes a single-use execute capability together with its
// bound confirm nonce, atomically. It returns true ONLY on a fully valid,
// first-use match bound to exactly (token, confirm, deviceSessionID, handle);
// a failed confirm MUST consume nothing (plan §4.γ.2). Satisfied by a
// remoteauth.CapabilityStore adapter.
type CapabilityConsumer interface {
	ConsumeTerminalControl(token, confirm, deviceSessionID, handle string) bool
}

// StandingVerifier verifies a REUSABLE standing terminal-control secret bound
// to a device session + handle — the OPT-IN alternative to the single-use
// capability leg (standing-terminal-access §B). Unlike CapabilityConsumer it
// does NOT consume (the secret is reusable until the operator revokes it); the
// implementation MUST itself enforce the standing-access enabled gate, a
// constant-time hash compare, per-device rate limiting, and audit every attempt
// (success + failure). A false result denies the writer lease exactly like a
// rejected capability. Satisfied by the dashboard remote controller.
type StandingVerifier interface {
	VerifyStandingTerminalControl(secret, deviceSessionID, handle string) bool
}

// AuthorizeRequest carries the raw, request-derived inputs to the single
// authorization function. The booleans are resolved by the caller AT THE
// BOUNDARY from listener provenance + the live config snapshot (CLAUDE.md #3 —
// branch on capability, not source identity); the tokens are the client-
// supplied secrets validated here.
type AuthorizeRequest struct {
	// Handle is the terminal session handle the writer lease is for.
	Handle string
	// DeviceSessionID is the authenticated device session id (from the cookie).
	DeviceSessionID string
	// CapabilityToken is the single-use execute capability minted at
	// approve-execute.
	CapabilityToken string
	// Confirm is the capability-bound confirm nonce minted at approve-execute.
	Confirm string
	// RemoteExposed is true when the request arrived on a remotely-exposed
	// execute-classified route (provenance-classified at construction, §4.4).
	RemoteExposed bool
	// AllowTerminal is remote.allow_terminal from the live config snapshot.
	AllowTerminal bool
}

// Authorize is the SINGLE authorization function for a remote writer lease. It
// atomically validates the entire §4.δ conjunction and, ONLY on success, mints
// the unforgeable WriterGrant that termsession.AcquireWriterRemote requires.
//
// The conjunction is checked in fail-fast order with the single-use capability
// consumption LAST, so a miss on any earlier leg never burns the capability
// (a failed confirm consumes nothing, plan §4.γ.2):
//
//	remote-exposed listener
//	AND remote.allow_terminal == true
//	AND authenticated device session
//	AND applicable [terminal.launch] policy
//	AND exact single-use capability (handle+action+device-session-bound)
//	AND bound confirmation
func Authorize(req AuthorizeRequest, sessions SessionValidator, policy LaunchPolicy, caps CapabilityConsumer) (WriterGrant, error) {
	if err := checkConjunctionPrefix(req, sessions, policy); err != nil {
		return WriterGrant{}, err
	}
	// Consume the single-use capability + bound confirm LAST and atomically, so
	// no earlier failing leg ever consumes it.
	if !caps.ConsumeTerminalControl(req.CapabilityToken, req.Confirm, req.DeviceSessionID, req.Handle) {
		return WriterGrant{}, ErrCapabilityRejected
	}
	return WriterGrant{
		authorized: true,
		handle:     req.Handle,
		holder:     fingerprint(req.DeviceSessionID),
		holderKey:  holderKeyOf(req.DeviceSessionID),
	}, nil
}

// AuthorizeStanding is the SINGLE authorization function for a remote writer
// lease acquired via the OPT-IN standing terminal-control secret (§B). It runs
// the IDENTICAL §4.δ conjunction as Authorize — remote-exposed listener AND
// remote.allow_terminal AND authenticated device session AND applicable launch
// policy — and differs ONLY in the final credential leg: instead of consuming a
// single-use capability + bound confirm, it verifies the reusable standing
// secret carried in req.CapabilityToken. It is NOT a parallel authorization
// path: it shares checkConjunctionPrefix with Authorize and mints the same
// unforgeable WriterGrant, so the manager's write-side boundary is unchanged.
// The standing verifier itself enforces the enabled gate, constant-time compare,
// rate limiting, and per-attempt audit; a false result denies exactly like a
// rejected capability.
func AuthorizeStanding(req AuthorizeRequest, sessions SessionValidator, policy LaunchPolicy, standing StandingVerifier) (WriterGrant, error) {
	if err := checkConjunctionPrefix(req, sessions, policy); err != nil {
		return WriterGrant{}, err
	}
	// Verify the reusable standing secret LAST (the CapabilityToken field carries
	// the wire-encoded standing secret in this path). It is not consumed.
	if !standing.VerifyStandingTerminalControl(req.CapabilityToken, req.DeviceSessionID, req.Handle) {
		return WriterGrant{}, ErrCapabilityRejected
	}
	return WriterGrant{
		authorized: true,
		handle:     req.Handle,
		holder:     fingerprint(req.DeviceSessionID),
		holderKey:  holderKeyOf(req.DeviceSessionID),
		standing:   true,
	}, nil
}

// checkConjunctionPrefix runs every §4.δ conjunction leg EXCEPT the final
// credential leg (single-use capability vs standing secret), in fail-fast order.
// Sharing it keeps Authorize and AuthorizeStanding one path that branches only
// on the last leg — a credential DIFFERENCE resolved at the boundary, never a
// second copy of the conjunction (CLAUDE.md #1/#3).
func checkConjunctionPrefix(req AuthorizeRequest, sessions SessionValidator, policy LaunchPolicy) error {
	if req.Handle == "" || req.DeviceSessionID == "" {
		return ErrMissingField
	}
	if !req.RemoteExposed {
		return ErrNotRemoteExposed
	}
	if !req.AllowTerminal {
		return ErrTerminalDisabled
	}
	if err := sessions.Validate(req.DeviceSessionID); err != nil {
		return ErrNoDeviceSession
	}
	if !policy.Allowed(req.Handle) {
		return ErrPolicyDenied
	}
	return nil
}

// fingerprint returns a short, non-bearer display token for a device-session id
// (the first 8 hex chars of its sha256 hash) — byte-identical to the dashboard's
// deviceFingerprint and remoteauth's SessionInfo.Fingerprint so grant.Holder()
// correlates on ONE device token across the lease-audit and control-audit rows.
// The RAW session id (and any prefix of it) is NEVER recorded on the grant.
func fingerprint(id string) string {
	if id == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])[:8]
}

// holderKeyOf returns the FULL sha256-hex of a device-session id — the complete
// hash of which fingerprint is the first 8 chars. It is byte-identical to
// remoteauth.HashSessionID (and the dashboard's deviceSessionKey /
// SessionInfo.ID), so the writer-lease holder identity a per-device revoke
// matches on is the same full key the session store and sensitive-viewer
// registry key on — closing the 8-char-prefix over-revoke hole. The RAW session
// id is never recorded on the grant.
func holderKeyOf(id string) string {
	if id == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}
