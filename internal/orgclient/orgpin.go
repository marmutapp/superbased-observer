package orgclient

import (
	"context"
	"fmt"
)

// Rail names used in key-identity refusals. They exist only to make the
// message name the rail that holds the conflicting pin, which is the
// one thing an operator needs in order to act on it.
const (
	routingPolicyRail = "routing-policy"
	announcementRail  = "announcement"
)

// railPin is one signed-distribution rail's node-local pin of the org's
// signing key.
type railPin struct {
	rail string
	key  string
}

// loadRailPins returns the org signing key as pinned by every
// signed-distribution rail's node-local cache.
//
// There is exactly ONE org distribution identity — the org server signs
// routing policy and announcements with the same Ed25519 key
// (orgserver/routingpolicy.SigningKey, deliberately shared) — so the
// pins are not independent trust decisions and must never be allowed to
// disagree. Reading them as a set is what lets a rail's FIRST fetch be
// checked against trust the node already established elsewhere, instead
// of being a fresh TOFU that a network attacker can win.
func (c *Client) loadRailPins(ctx context.Context) ([]railPin, error) {
	var pins []railPin
	rp, ok, err := c.store.GetOrgRoutingPolicy(ctx)
	if err != nil {
		return nil, err
	}
	if ok && rp.ServerPubkey != "" {
		pins = append(pins, railPin{rail: routingPolicyRail, key: rp.ServerPubkey})
	}
	ann, ok, err := c.store.GetOrgAnnouncement(ctx)
	if err != nil {
		return nil, err
	}
	if ok && ann.ServerPubkey != "" {
		pins = append(pins, railPin{rail: announcementRail, key: ann.ServerPubkey})
	}
	return pins, nil
}

// checkOrgKeyIdentity refuses an offered signing key that disagrees
// with a pin held by ANOTHER rail. The caller's own rail is skipped:
// it owns that comparison itself (it is fused with the monotonic-
// version short-circuit) and duplicating it here would only make two
// places able to disagree about the refusal wording.
//
// No pin anywhere ⇒ TOFU, exactly as before: the first key a freshly
// enrolled node ever sees is trusted, because the enrolment channel is
// the trust root. The change is that "first" now means first across the
// org, not first per rail.
//
// The refusal is loud and terminal for this cycle (no cache write, no
// silent downgrade), the same class as the per-rail key-change refusal,
// and the remedy is the same: re-enrol to rotate trust.
func (c *Client) checkOrgKeyIdentity(ctx context.Context, ownRail, offered string) error {
	pins, err := c.loadRailPins(ctx)
	if err != nil {
		return err
	}
	for _, p := range pins {
		if p.rail == ownRail || p.key == offered {
			continue
		}
		return fmt.Errorf("org distribution key CHANGED (pinned %s… by the %s rail, got %s…) — refusing; one org has one signing key, re-enrol to rotate trust",
			prefix8(p.key), p.rail, prefix8(offered))
	}
	return nil
}

// maxOrgDocBytes caps a signed distribution document read from the org
// server. Enforced as a DOCUMENT cap by orgcontract.DecodeCapped (a
// bare io.LimitReader caps the read, not the document, and says nothing
// about what follows the value).
const maxOrgDocBytes = 1 << 20
