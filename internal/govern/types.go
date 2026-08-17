package govern

import (
	"time"

	"github.com/marmutapp/superbased-observer/internal/policyfam/nodegov"
)

// The CLOSED authority vocabulary a grant may carry (spec §2.4). Only
// AuthorityDashboardVisibility is CONSUMED in Phase 1a — the other three are
// declared because the vocabulary is part of the signed grant and must be
// stable across the phases that add their directive classes, and because a
// node must be able to display the authority it granted even for a class
// this build cannot yet act on.
//
// A token outside this set in a delivered grant is IGNORED and REPORTED
// (Effective.UnknownAuthority), never an enrolment failure: forward compat
// requires an older agent to enrol against a newer server that offered a
// token it has never heard of.
const (
	// AuthorityDashboardVisibility lets the org hide or read-only-lock node
	// dashboard pages and Settings sections. It grants NO read of node data.
	AuthorityDashboardVisibility = "dashboard.visibility"
	// AuthoritySettingsPin (Phase 1b) lets the org pin a closed set of
	// [config] keys — the `pinned` directive class.
	AuthoritySettingsPin = "settings.pin"
	// AuthorityCaptureRaise is RETIRED and PERMANENTLY INERT.
	//
	// It was declared in Phase 1a for a raise-only share merge. Phase 1b
	// reversed that direction (operator ruling R6): the organization may
	// LOWER a node's sharing or pin it at the level the node operator
	// already consented to, and may never raise it. A token whose NAME says
	// "raise" cannot survive as the token for a directive class that can
	// only lower, and it is already in the closed vocabulary of a shipped
	// build, so it cannot simply be renamed.
	//
	// It therefore stays in KnownAuthority for wire stability — an older
	// grant carrying it is not an enrolment failure, and the developer can
	// still read what their machine handed over — but it grants NOTHING.
	// Widening authority to capture.pin requires a fresh `observer enroll`,
	// which is a new node-side act. That is the design working as intended.
	AuthorityCaptureRaise = "capture.raise"
	// AuthorityCapturePin (Phase 1b) lets the org REDUCE, or lock at its
	// current level, what this machine shares. It can never increase it.
	AuthorityCapturePin = "capture.pin"
	// AuthorityFeatureLock (Phase 1b) lets the org force a feature subsystem
	// on or off — the `features` directive class, which is a compile-time
	// alias over `pinned`.
	AuthorityFeatureLock = "feature.lock"
)

// KnownAuthority reports whether tok is in the closed vocabulary.
//
// capture.raise is deliberately still known: forward/backward compat
// requires an agent to be able to READ a token a differently-versioned
// server offered. Known is not the same as honoured — see
// AuthorityCaptureRaise.
func KnownAuthority(tok string) bool {
	switch tok {
	case AuthorityDashboardVisibility, AuthoritySettingsPin,
		AuthorityCaptureRaise, AuthorityCapturePin, AuthorityFeatureLock:
		return true
	}
	return false
}

// RetiredAuthority reports whether tok is a known token that grants nothing
// in this build. It is surfaced by the CLI and the Enrolment page so a
// developer holding an older grant can see WHY a directive did not take.
func RetiredAuthority(tok string) bool { return tok == AuthorityCaptureRaise }

// ConsentMode records HOW the node came to hold this grant (spec §2). Only
// ConsentInteractive is produced in Phase 1a (flow (c), enrolment tokens);
// the other two are the Phase 3/4 flows.
const (
	ConsentInteractive = "interactive"
	ConsentIdP         = "idp"
	ConsentManaged     = "managed"
)

// Grant is the node-side record of the bounded authority this machine handed
// to an organization at enrolment. It is the consent boundary: a directive
// outside it is dropped and reported, so widening authority requires a new
// enrolment, which requires a new node-side act.
//
// It is written by cmd/observer at enrolment (never by internal/orgclient —
// orgclient has no TTY and has already committed the bearer by the time a
// human could answer), stored by internal/store/orggrant.go, and deleted by
// `observer unenroll`.
type Grant struct {
	OrgKey       string
	Generation   int64
	OrgID        string
	OrgName      string
	OrgServerURL string
	// KeyPinSHA256 is the org policy signing key hash bound at grant time.
	// Resolve compares it against the LIVE TOFU pin (rule 4): a grant signed
	// under a key the node no longer pins is not authority, it is a
	// substitution attempt.
	KeyPinSHA256 string
	Authority    []string
	ConsentMode  string
	ConsentActor string
	GrantedAt    time.Time
	ExpiresAt    time.Time
	// Signature is the base64url Ed25519 signature over the grant's canonical
	// signing message. Kept as EVIDENCE (an operator, an auditor, or a court
	// can verify what the organization actually asked for) — it is verified
	// at write time against the pinned key, not re-verified on every Resolve.
	Signature   string
	ReceiptHash string
}

// LiveIdentity is the node's CURRENT enrolment identity plus its live org
// policy key pin — the facts a grant is checked against on every resolve, so
// a raced unenrol/re-enrol (or a key substitution) wins immediately rather
// than at the next restart.
type LiveIdentity struct {
	Enrolled     bool
	OrgKey       string
	Generation   int64
	KeyPinSHA256 string
}

// Delivered is the org-published resource the node accepted for the
// node.governance family, or the zero value when nothing is installed.
type Delivered struct {
	// Present is false when no resource is installed (404/withdrawn/never
	// published, or an install the accept gates refused).
	Present bool
	Version int64
	// BodyHash is the SIGNED body hash — the org-rail wire identity this
	// family reports as EffectiveHash, exactly like admitter/egress/gateway.
	BodyHash string
	Spec     nodegov.PolicySpec
	// InertReason is the accept-path's own inert verdict (e.g. the
	// [org_client.policy].preauthorize_enforce gate refused to let the body
	// run). Non-empty means the node accepted the resource but must not
	// apply it.
	InertReason string
}

// State is the resolver's verdict — the ONE value the whole node branches
// on. It is deliberately coarser than the wire status enum: the mapping onto
// (status, reason) lives at the policystate boundary, not here.
type State string

const (
	// StateNoGrant — this node holds no enrolment grant. The delivered body,
	// however valid, is ignored entirely. This is the solo/unenrolled/
	// enrolled-but-ungoverned node, and it is the zero value.
	StateNoGrant State = "no_grant"
	// StateGrantExpired — the grant's TTL lapsed; the node has reverted to
	// its local settings and says so.
	StateGrantExpired State = "grant_expired"
	// StateIdentityChanged — the grant belongs to an enrolment identity or
	// generation the node no longer holds (a raced unenrol/re-enrol).
	StateIdentityChanged State = "identity_changed"
	// StateKeyPinMismatch — the grant was bound to an org signing key the
	// node no longer pins (adversarial review A2 / spec §3.7 row 3b).
	StateKeyPinMismatch State = "key_pin_mismatch"
	// StateNoPolicy — a live grant, but the org has published no governance.
	StateNoPolicy State = "no_policy"
	// StateInert — a live grant and a delivered body, but nothing was
	// applied: the grant does not carry the authority the body needs, or the
	// accept path already marked it inert.
	StateInert State = "inert"
	// StateApplied — the delivered body was applied in full.
	StateApplied State = "applied"
)

// Reason values are the BOUNDED reason enum carried in Dropped and mapped at
// the policystate boundary onto the orgcontract wire reasons. The literals
// match orgcontract's, duplicated here for the same dependency-graph reason
// internal/policystate duplicates its family names: this is a pure decision
// package, not a wire package.
const (
	ReasonNotPreauthorized = "not_preauthorized"
	ReasonGrantExpired     = "grant_expired"
	ReasonIdentityChanged  = "identity_changed"
	ReasonKeyPinMismatch   = "key_pin_mismatch"
	// ReasonAuthorityRetired records a directive dropped because the only
	// authority the grant carries for it is a RETIRED token (capture.raise).
	//
	// Adding a reason is wire-safe HERE and only here (review n5): the
	// server validates ack reasons against a closed set and 400s the WHOLE
	// report on an unknown one — the same trap that keeps break_glass
	// unemitted. This reason is node-local BY CONSTRUCTION because
	// cmd/observer's governanceFacts never forwards a govern reason to the
	// wire; it collapses every non-applied resolution to
	// not_preauthorized. A future reader who "helpfully" plumbs the richer
	// reason through to the ack will take the fleet's reporting down. Do
	// not.
	ReasonAuthorityRetired = "authority_retired"
)

// Dropped records one directive class the node refused to apply, and why.
//
// Its PRESENCE is what forces accepted_inert / not_preauthorized instead of
// effective, so a partial application is visible in fleet state. Its
// CONTENT is not: the wire carries only (status, reason, hash), and
// cmd/observer's Facts() collapses every non-applied resolution to the
// single reason not_preauthorized (review M4). Dropped is therefore
// NODE-LOCAL — rendered on the developer's own Enrolment page and by
// `observer org grant show`, and folded into Effective.Hash so a partial
// application can never hash-match the delivered body — but the admin sees
// only that SOMETHING was refused, never which class. Per-directive drop
// visibility is a Phase-2 wire item.
type Dropped struct {
	Directive string `json:"directive"`
	Reason    string `json:"reason"`
}

// Effective is what the node ACTUALLY applies. It is never the delivered
// body: it is the delivered body INTERSECTED with the grant's authority,
// with every dropped directive recorded.
type Effective struct {
	// Active is true when this node is GOVERNED: a live grant AND an
	// installed body. It is the single predicate every UI surface and the
	// dashboard's route guard short-circuit on, so an ungoverned node's
	// request path never changes shape.
	Active  bool   `json:"active"`
	State   State  `json:"state"`
	Version int64  `json:"version"`
	OrgName string `json:"org_name,omitempty"`

	HiddenSections   []string `json:"hidden_sections"`
	ReadOnlySections []string `json:"read_only_sections"`
	HiddenSettings   []string `json:"hidden_settings"`
	ReadOnlySettings []string `json:"read_only_settings"`

	// Pinned is the settings.pin + feature.lock directive classes MERGED
	// into one map — dotted config path → typed value. It is what the
	// daemon materializes into the sidecar, and therefore what every
	// process on the machine reads through config.Load.
	//
	// A feature lock is a compile-time alias over a pin, so there is exactly
	// one enforcement path and a feature can never drift from the pin
	// implementing it. The two are gated on their OWN authority tokens
	// before they are merged here.
	Pinned map[string]any `json:"pinned,omitempty"`
	// Share is the org's capture.pin directive, as delivered. It is NOT the
	// effective share posture: the lowering merge against the node's own
	// local [org_client.share] happens at the org-push seam, because only
	// there is the local value known (§2.2). The org can only ever REDUCE
	// or pin; it can never raise.
	Share map[string]any `json:"share,omitempty"`
	// Features is the DERIVED, display-only mirror (§3), so the developer's
	// Enrolment page can say "your organization requires the guard to be on"
	// rather than "guard.enabled = true".
	Features map[string]bool `json:"features,omitempty"`

	Notice Notice `json:"notice"`

	// Authority is the grant's own token list, surfaced so the developer can
	// always read what this machine handed over.
	Authority []string `json:"authority,omitempty"`
	// UnknownAuthority holds grant tokens outside this build's closed
	// vocabulary: honoured by nothing, reported so the admin can see the node
	// did not act on them.
	UnknownAuthority []string `json:"unknown_authority,omitempty"`
	// Dropped lists the directive classes the node refused.
	Dropped []Dropped `json:"dropped,omitempty"`

	ExpiresAt time.Time `json:"expires_at,omitempty"`
	// Hash is the content address of THIS resolved posture (what is running),
	// which by construction differs from the delivered BodyHash whenever
	// anything was dropped.
	Hash string `json:"hash,omitempty"`

	// featurePins is the feature block's expansion, held separately only
	// between its authority gate and the merge into Pinned. Unexported so no
	// surface can mistake it for a second enforcement path.
	featurePins map[string]any
}

// IsPinned reports whether the org has fixed a config key, and what to.
func (e Effective) IsPinned(key string) (any, bool) {
	v, ok := e.Pinned[key]
	return v, ok
}

// ShareDirective returns the org's directive for a share key, if any. It is
// the org's REQUEST, not the effective posture: the lowering merge against
// the node's own local value happens at the org-push seam, which is the only
// place the local value is known.
func (e Effective) ShareDirective(key string) (any, bool) {
	v, ok := e.Share[key]
	return v, ok
}

// Notice is the honesty copy rendered verbatim by the node.
type Notice struct {
	OrgDisplayName string `json:"org_display_name,omitempty"`
	Contact        string `json:"contact,omitempty"`
	PolicyURL      string `json:"policy_url,omitempty"`
}

// IsNavSectionHidden reports whether the resolved posture hides a nav
// section. Nil-safe on the zero value (nothing hidden).
func (e Effective) IsNavSectionHidden(id string) bool { return contains(e.HiddenSections, id) }

// IsNavSectionReadOnly reports whether the resolved posture locks a nav
// section read-only.
func (e Effective) IsNavSectionReadOnly(id string) bool { return contains(e.ReadOnlySections, id) }

// IsSettingsSectionHidden / IsSettingsSectionReadOnly are the Settings
// sub-section twins.
func (e Effective) IsSettingsSectionHidden(id string) bool { return contains(e.HiddenSettings, id) }

func (e Effective) IsSettingsSectionReadOnly(id string) bool {
	return contains(e.ReadOnlySettings, id)
}

func contains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
