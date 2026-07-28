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
	// ErrStandingUnavailable — the standing credential leg denied for a reason
	// that does NOT blame the secret: standing access is currently switched off,
	// or the attempt was rate-limited. The refusal is exactly as hard as
	// ErrCapabilityRejected (no lease is granted), but it carries the fact that
	// the presented secret was never judged, so a caller must not react by
	// destroying it. Introduced 2026-07-25 (mobile terminal-continuity arc): the
	// device UI clears its saved standing secret on a credential rejection, and
	// folding these transient refusals into ErrCapabilityRejected made a
	// momentary rate-limit or a disabled-then-re-enabled toggle wipe a perfectly
	// valid secret, forcing the operator to mint a new one.
	ErrStandingUnavailable = errors.New("termlease: standing terminal-control temporarily unavailable")
	// ErrStandingRevoked — the standing credential leg denied because NO
	// standing secret exists on this server at all: it was revoked (the revoke
	// path DELETES the hash at rest) and never re-provisioned. Unlike
	// ErrStandingUnavailable this is a PERMANENT judgement about the presented
	// secret — not because the secret was compared (it wasn't; there is nothing
	// to compare it against) but because no secret a device is holding can ever
	// be accepted again: the only way back is a fresh mint, which issues a
	// DIFFERENT secret. A device may therefore discard its saved secret here,
	// exactly as it does for ErrCapabilityRejected.
	//
	// It is deliberately distinct from ErrStandingUnavailable, which covers the
	// states that can flip back on their own (standing access temporarily
	// switched off with the secret still at rest, rate limiting): discarding a
	// still-valid secret costs the operator a manual re-mint, so only a state
	// the server can prove is terminal for that secret may trigger it.
	// Introduced 2026-07-25 (operator decision A2).
	ErrStandingRevoked = errors.New("termlease: standing terminal-control secret has been revoked")
)

// StandingDenial classifies WHY a standing-credential verify denied. It exists
// so one verify call reports both the outcome and its blame — a separate "why
// did the last one fail?" query would race concurrent acquires.
//
// The classification changes NO authorization outcome (every non-None value
// denies exactly as hard); it decides only whether the DEVICE should destroy
// its saved standing secret, which is irreversible for the user (the operator
// must mint and convey a new one).
type StandingDenial uint8

const (
	// StandingDenialNone is the zero value: no denial. Meaningful only when the
	// verify reported ok — it must never be used to describe a refusal.
	StandingDenialNone StandingDenial = iota
	// StandingDenialUnavailable — the verifier refused WITHOUT ever judging the
	// presented secret, in a state that can flip back on its own: standing
	// access temporarily switched off (secret still at rest), or rate limiting.
	// The device MUST keep its secret. This is the fail-safe default for any
	// condition a verifier cannot positively classify.
	StandingDenialUnavailable
	// StandingDenialBadSecret — the verifier JUDGED the presented value and
	// rejected it (wrong/rotated secret, or an undecodable one). The device may
	// discard it.
	StandingDenialBadSecret
	// StandingDenialRevoked — no standing secret exists at rest at all: it was
	// revoked and never re-provisioned. No secret any device holds can be
	// accepted again (a fresh mint issues a different one), so the device may
	// discard it. Reserved for a state the server can PROVE — never inferred
	// from a merely-disabled gate.
	StandingDenialRevoked
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

// ReasoningStandingVerifier is an ADDITIVE optional interface a StandingVerifier
// may also implement to report, in the SAME call, HOW a denial should be blamed.
// It exists so the device UI can distinguish "your secret is dead — stop using
// it" from "not right now — keep it and retry"; without it every denial looks
// like a credential rejection and the device throws away a still-valid secret.
//
// Returning the classification from the verify call itself (rather than a
// separate "why did the last one fail?" query) keeps it race-free under
// concurrent acquires. It changes NO authorization outcome — every denial
// refuses the lease exactly as hard; only the sentinel, and with it the
// device's keep-or-discard decision, differs. A verifier that does not
// implement it keeps the previous behaviour (every denial reported as a
// credential rejection), so existing fakes are unaffected.
type ReasoningStandingVerifier interface {
	// VerifyStandingTerminalControlReason performs the same verification as
	// VerifyStandingTerminalControl and additionally classifies a denial:
	// StandingDenialBadSecret when the presented value was itself judged and
	// rejected (wrong / rotated / undecodable); StandingDenialRevoked when the
	// server can PROVE no standing secret exists at rest at all; and
	// StandingDenialUnavailable for every state refusal that never judged the
	// secret and may flip back on its own (standing access temporarily off,
	// rate-limited) — which is also the required fail-safe for any condition the
	// verifier cannot positively classify. The denial is meaningless when ok is
	// true.
	VerifyStandingTerminalControlReason(secret, deviceSessionID, handle string) (ok bool, denial StandingDenial)
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
	//
	// A denial carries one of three sentinels, chosen by the verifier's own
	// classification. All three deny identically — no lease is granted — and they
	// differ ONLY in the downstream "should the device discard its saved secret?"
	// decision:
	//
	//	StandingDenialBadSecret   → ErrCapabilityRejected  (judged and rejected)
	//	StandingDenialRevoked     → ErrStandingRevoked     (no secret at rest at all)
	//	StandingDenialUnavailable → ErrStandingUnavailable (never judged; may return)
	if ok, denial := verifyStanding(standing, req); !ok {
		switch denial {
		case StandingDenialBadSecret:
			return WriterGrant{}, ErrCapabilityRejected
		case StandingDenialRevoked:
			return WriterGrant{}, ErrStandingRevoked
		default:
			// Unavailable AND any value this build does not recognise: keep the
			// secret. Discarding one is irreversible for the user, so an
			// unclassifiable denial must fall on the preserving side.
			return WriterGrant{}, ErrStandingUnavailable
		}
	}
	return WriterGrant{
		authorized: true,
		handle:     req.Handle,
		holder:     fingerprint(req.DeviceSessionID),
		holderKey:  holderKeyOf(req.DeviceSessionID),
		standing:   true,
	}, nil
}

// verifyStanding runs the standing credential leg, preferring the richer
// ReasoningStandingVerifier when the injected verifier offers it. A verifier
// without it is treated exactly as before: a denial blames the secret.
func verifyStanding(standing StandingVerifier, req AuthorizeRequest) (ok bool, denial StandingDenial) {
	if reasoning, isReasoning := standing.(ReasoningStandingVerifier); isReasoning {
		return reasoning.VerifyStandingTerminalControlReason(req.CapabilityToken, req.DeviceSessionID, req.Handle)
	}
	if standing.VerifyStandingTerminalControl(req.CapabilityToken, req.DeviceSessionID, req.Handle) {
		return true, StandingDenialNone
	}
	return false, StandingDenialBadSecret
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
