package orgcontract

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Enrolment-grant signing (admin-controlled Plane B,
// docs/plans/admin-controlled-plane-b-spec-2026-08-15.md §2.4). The org
// server signs the grant it offers with the SAME Ed25519 policy key the
// policy-resource rail uses, so there is exactly one org signing identity
// and one agent-side TOFU pin.
//
// HONESTY ABOUT WHAT THE SIGNATURE BUYS (adversarial review A1). In the
// Phase-1a flow (enrolment tokens) the verification key arrives in the SAME
// HTTP response as the grant, so the signature provides ZERO anti-forgery
// value at first enrolment: whoever controls the enrolment endpoint controls
// both. Its real value is (a) NON-REPUDIATION / evidence — the node keeps a
// verifiable record of what the organization actually asked for — and (b)
// anti-substitution ON EVERY LATER RESOLVE, because internal/govern compares
// the grant's bound key-pin hash against the live TOFU pin. Committing the
// key OUT OF BAND before the node ever talks to the server is the MDM flow's
// job (spec §2.1 step 4) and is Phase 4. Phase 1a therefore trusts the
// enrolment endpoint exactly as much as it already trusts it for the bearer:
// no more, no less.

// enrolmentGrantSigningDomain domain-separates grant signatures from every
// other Ed25519 use in the protocol (policy bundles, policy resources,
// announcements, push proofs): a signature minted for one purpose can never
// verify for another.
const enrolmentGrantSigningDomain = "sbo-enrolment-grant-v1"

// EnrolmentGrant is the OPTIONAL grant object on the enrol response. Absent
// (omitempty) means an ordinary, ungoverned "reporting-only" enrolment —
// which is what every pre-governance server returns and what a governance
// server returns for a token minted without authority.
//
// It deliberately carries NO target_group (adversarial review A17/D-A): the
// grant records AUTHORITY, never AUDIENCE. Audience is resolved server-side
// from the subject's authoritative attributes (P0-10), so a group
// reassignment cannot leave a node permanently rejecting its own correctly
// targeted policy.
type EnrolmentGrant struct {
	// OrgID / OrgServerURL bind the grant to the identity it was offered
	// for; the node checks both against the enrolment it is completing.
	OrgID        string `json:"org_id"`
	OrgServerURL string `json:"org_server_url"`
	// KeyPinSHA256 is the hex sha256 of the org policy signing key this
	// grant is bound to. The node refuses a grant whose pin does not match
	// the key it pinned during THIS enrolment, and internal/govern re-checks
	// it against the live pin on every resolve.
	KeyPinSHA256 string `json:"key_pin_sha256"`
	// Authority is the closed-vocabulary token list (see internal/govern).
	Authority []string `json:"authority"`
	// GrantedAt / ExpiresAt are RFC3339. ExpiresAt is the §5.3 TTL: an org
	// that stops authorizing a node stops governing it.
	GrantedAt string `json:"granted_at"`
	ExpiresAt string `json:"expires_at"`
	// ConsentMode / ConsentActor are the ACP-P6c consent EVIDENCE: the
	// server's own statement of how this enrolment was consented to, bound
	// into the signature so the node records what the organization actually
	// asserted rather than what an envelope field claimed. Populated only for
	// a mint on the IdP device-code rail ("idp" plus the verified address of
	// the member who approved the pairing); every token-rail grant leaves
	// both empty and is byte-identical to a pre-P6c grant on the wire and in
	// the signing message.
	ConsentMode  string `json:"consent_mode,omitempty"`
	ConsentActor string `json:"consent_actor,omitempty"`
	// Signature is base64url(Ed25519) over EnrolmentGrantSigningMessage.
	Signature string `json:"signature"`
}

// CanonicalAuthority returns the grant's tokens sorted and deduplicated —
// the form the signing message is built over, so two orderings of the same
// authority set produce the same signature input.
func CanonicalAuthority(tokens []string) []string {
	seen := make(map[string]bool, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// EnrolmentGrantSigningMessage returns the canonical bytes signed over a
// grant: a fixed domain prefix plus every semantic field, NUL-separated so
// no field can shift a boundary into another. The signature does NOT cover
// itself, obviously; every other field is bound.
//
// PRESENCE-VERSIONED (ACP-P6c §3e). The consent evidence is appended, marker-
// prefixed like authority, ONLY when ConsentMode is non-empty. A grant with
// no consent evidence — every token-rail grant, which is every grant minted
// before P6c — therefore produces BYTE-IDENTICAL bytes to the pre-P6c
// algorithm, so no existing signature stops verifying and no version
// negotiation is needed. A golden test pins that equality.
//
// The compat consequence is deliberate and is the honest degradation, not an
// oversight: only an idp-minted grant carries the fields, and only a P6c-aware
// agent can initiate the device-code flow that produces one. If an OLD binary
// somehow redeemed a leaked idp enrolment code, it would compute the message
// without the consent writes, VerifyEnrolmentGrant would fail, and
// evaluateGrantOffer would refuse the grant through its existing named-error
// path — enrolling the node ungoverned and LOUDLY. An old binary can never be
// handed a managed grant silently.
func EnrolmentGrantSigningMessage(g EnrolmentGrant) []byte {
	h := sha256.New()
	write := func(s string) {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	write(enrolmentGrantSigningDomain)
	write(g.OrgID)
	write(g.OrgServerURL)
	write(g.KeyPinSHA256)
	for _, a := range CanonicalAuthority(g.Authority) {
		write("authority")
		write(a)
	}
	write(g.GrantedAt)
	write(g.ExpiresAt)
	if g.ConsentMode != "" {
		write("consent_mode")
		write(g.ConsentMode)
		write("consent_actor")
		write(g.ConsentActor)
	}
	return h.Sum(nil)
}

// SignEnrolmentGrant signs the canonical grant message and returns the
// base64url signature the wire carries. The org server's enrol path is the
// only caller.
func SignEnrolmentGrant(priv ed25519.PrivateKey, g EnrolmentGrant) string {
	sig := ed25519.Sign(priv, EnrolmentGrantSigningMessage(g))
	return base64.RawURLEncoding.EncodeToString(sig)
}

// VerifyEnrolmentGrant checks a grant's signature against pub. It returns a
// NAMED error for every failure so the CLI can tell the developer exactly
// why a grant was refused rather than silently enrolling ungoverned
// (adversarial review A3).
func VerifyEnrolmentGrant(g EnrolmentGrant, pub ed25519.PublicKey) error {
	if len(pub) != ed25519.PublicKeySize {
		return errors.New("orgcontract.VerifyEnrolmentGrant: no org policy public key to verify the grant against")
	}
	if g.Signature == "" {
		return errors.New("orgcontract.VerifyEnrolmentGrant: grant carries no signature")
	}
	sig, err := base64.RawURLEncoding.DecodeString(g.Signature)
	if err != nil {
		return fmt.Errorf("orgcontract.VerifyEnrolmentGrant: decode signature: %w", err)
	}
	if !ed25519.Verify(pub, EnrolmentGrantSigningMessage(g), sig) {
		return errors.New("orgcontract.VerifyEnrolmentGrant: signature verification failed")
	}
	return nil
}

// EnrolmentGrantReceiptHash is the node's own content address for a grant —
// what `observer org grant show` prints and what an auditor can recompute
// from the signed document.
func EnrolmentGrantReceiptHash(g EnrolmentGrant) string {
	sum := sha256.Sum256(append(EnrolmentGrantSigningMessage(g), []byte(g.Signature)...))
	return hex.EncodeToString(sum[:])
}
