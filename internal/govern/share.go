package govern

import "sort"

// The share algebra (Phase-1b mini-spec §2.1, operator ruling R6).
//
// The parent spec's §3.6 is SUPERSEDED on direction. It specified a
// raise-only merge under an authority token literally named capture.raise.
// Phase 1b implements the opposite:
//
//	The organization may LOWER a node's sharing, or pin it at the level the
//	node operator already consented to. It may NEVER raise it.
//
// Formally, with ⊑ meaning "shares no more than":
//
//	effective ⊑ local     for all org directives, always
//
//	boolean keys:  effective = local AND org
//	list keys:     effective = intersection(local, org)
//
// So an org directive of true on a key the node already has true is a PIN
// (the node's toggle goes read-only and the org is shown as a co-source); an
// org directive of true on a key the node has false is a NO-OP; an org
// directive of false LOWERS and takes effect at the next push.
//
// # Why this needs no change at the org-push seam
//
// internal/store.ShareOptions has exactly ONE non-test construction site in
// the whole tree (internal/orgclient/client.go), and shipsRawContent()
// (internal/store/orgpush.go) reads only that struct's own fields. A
// lowering merge applied UPSTREAM of that single constructor therefore needs
// no change at the seam, in any shipsRawContent() call site, or in
// tests/invariant/privacy_test.go — which is why orgpush.go and the privacy
// sentinel are byte-identical after Phase 1b.
//
// Because the merge can only move a boolean true → false and never
// false → true, shipsRawContent() can only go true → false. There is no code
// path, under any org body, any grant, any authority token, or any
// compromise of the org signing key, by which a node that has not locally
// set full_content or admin_managed ships raw content. The CLAUDE.md
// invariant — "privacy posture is node-side opt-in, never server-forced" —
// therefore needs no amendment: 1b does not widen it, it adds an org-side
// ability to NARROW what a consenting node ships.

// LowerBool merges the org's directive for a boolean share key with the
// node's own local value. The result is never MORE sharing than local.
//
// A dormant posture, an unauthorized share class, an absent key and an
// org `true` all return local unchanged — the four ways "the org said
// nothing that reduces this" can arise, deliberately collapsing to one
// answer.
func (e Effective) LowerBool(key string, local bool) bool {
	if !local {
		// Already at the floor. Nothing the org can say lowers it further,
		// and nothing it can say raises it.
		return false
	}
	v, ok := e.Share[key]
	if !ok {
		return local
	}
	org, isBool := v.(bool)
	if !isBool {
		// A malformed directive is not authority to change anything. The
		// fail-safe direction for a sharing key is the node's own choice.
		return local
	}
	return local && org
}

// LowerList merges the org's directive for a list-valued share key by
// INTERSECTION, so the effective list can only shrink.
//
// The nil/empty distinction matters and is preserved: an empty local list
// means "nothing extra is allowed", and the intersection of anything with it
// is still empty.
func (e Effective) LowerList(key string, local []string) []string {
	v, ok := e.Share[key]
	if !ok {
		return local
	}
	org, isList := v.([]string)
	if !isList {
		return local
	}
	allow := make(map[string]bool, len(org))
	for _, s := range org {
		allow[s] = true
	}
	out := make([]string, 0, len(local))
	for _, s := range local {
		if allow[s] {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	if len(out) == 0 && local == nil {
		return nil
	}
	return out
}

// RaiseBool is the Enterprise-Managed Tenancy inverse of LowerBool: it lets
// the org RAISE a boolean share tier to true. It is the sanctioned lift of the
// lowering-only algebra for the MANAGED plane only, and it is structurally
// inert anywhere else:
//
//   - it no-ops unless e.Managed (the resolver sets Managed only under
//     managed-class consent, ManagedConsent), so an individual / BYO node can
//     never raise, whatever org body or signing key it is handed;
//   - the push seam additionally gates each tier on its own
//     GrantsXxxExtraction predicate (Managed AND the matching extract.* token,
//     or the extract.managed umbrella), so even a managed node raises nothing
//     unless the admin was granted the extraction authority for that tier.
//
// current already true short-circuits (the org cannot un-raise here — that is
// LowerBool's job, applied first). A missing or malformed directive leaves
// current unchanged.
func (e Effective) RaiseBool(key string, current bool) bool {
	if current {
		return true
	}
	if !e.Managed {
		// Raise is managed-only, structurally. An individual node reaching
		// here (it should not) changes nothing.
		return current
	}
	v, ok := e.Share[key]
	if !ok {
		return current
	}
	org, isBool := v.(bool)
	if !isBool {
		return current
	}
	return org
}

// ShareSource classifies who decided a share key's effective value, for the
// Privacy page's Source column. It is resolved HERE, at the boundary, and
// never assembled as a string in the SPA.
type ShareSource string

const (
	// ShareSourceLocal — the org published nothing for this key, or what it
	// published is structurally inert on this node.
	ShareSourceLocal ShareSource = "you"
	// ShareSourceOrg — the org LOWERED this key below the node's setting.
	ShareSourceOrg ShareSource = "org"
	// ShareSourceBoth — the org pinned this key at the value the node had
	// already set.
	ShareSourceBoth ShareSource = "both"
	// ShareSourceOrgRaised — the org RAISED this key ABOVE what the node
	// operator set locally.
	//
	// This value exists because raising exists. It did not when the enum was
	// written: the comment here used to say an "org increased it" value was
	// deliberately absent because it "structurally cannot" happen. That
	// stopped being true when the Enterprise-Managed Tenancy lift shipped —
	// RaiseBool turns a tier ON remotely, gated on e.Managed, so on a MANAGED
	// node an org `true` over a local `false` really does take effect.
	//
	// Leaving the enum three-valued did not prevent the raise; it only
	// prevented SAYING it, and the default branch then attributed the org's
	// decision to the developer ("You"). Naming it is the honest-copy rule
	// applied to an attribution column. Raising remains managed-only: on the
	// individual plane RaiseBool no-ops and this value is unreachable.
	ShareSourceOrgRaised ShareSource = "org_raised"
)

// MergeBool resolves a boolean share key end to end, in the SAME order the
// push seam applies the two halves of the algebra
// (cmd/observer.lowerShareOptions): lower first, then the managed raise.
//
// It exists so a transparency surface and the push seam cannot disagree about
// what is in force — the Privacy page's "In force" column is this function,
// and its "Source" column is SourceForBool over the same (key, local).
//
// KNOWN OVER-REPORT: the push seam additionally gates each raise on that
// tier's own GrantsXxxExtraction predicate (Managed AND the matching
// extract.* token), while this function — like RaiseBool itself — gates only
// on Managed. On a managed node whose grant carries some but not all
// extraction tokens, a tier the org body asked to raise reads here as raised
// even though the seam will not raise it. The direction is deliberate: a
// privacy surface that over-states what an organisation asked for is a false
// alarm, one that under-states it is a false assurance.
//
// MergeBoolGated (below) closes this over-report for any caller that can
// afford to be exact about a specific [org_client.share] key (W-8) —
// prefer it for any NEW transparency or wire surface. MergeBool itself is
// left as-is: it is a documented, deliberately-conservative approximation
// still used by callers that predate sharetiers.go's per-key gate, and
// narrowing its own behavior out from under them is a separate change.
func (e Effective) MergeBool(key string, local bool) bool {
	return e.RaiseBool(key, e.LowerBool(key, local))
}

// MergeBoolGated is MergeBool's exact-answer twin (W-8): it resolves the
// SAME lower-then-raise algebra, but additionally requires
// ExtractionAuthorized(e, key) before letting the raise take effect — the
// same per-tier gate governance_wire.lowerShareOptions applies via each
// GrantsXxxExtraction predicate. It closes MergeBool's documented KNOWN
// OVER-REPORT: on a managed node whose grant authorizes some but not all
// extraction tiers, a key outside the granted tier reads here as still
// lowered, matching what the push seam will actually ship.
//
// key must be a shareTierTable / shareTierExempt key (§ sharetiers.go) for
// the gate to have any effect; an unmapped key is never authorized by
// ExtractionAuthorized, so it behaves like an ungated key on the individual
// plane — the raise never fires.
func (e Effective) MergeBoolGated(key string, local bool) bool {
	lowered := e.LowerBool(key, local)
	if !ExtractionAuthorized(e, key) {
		return lowered
	}
	return e.RaiseBool(key, lowered)
}

// SourceForBool reports who decided a boolean share key.
//
// All four outcomes are reachable, ShareSourceOrgRaised included: see that
// constant for why the enum grew a fourth value. The tenancy gate is the same
// one RaiseBool applies — without e.Managed an org `true` over a local
// `false` moves nothing, so the source stays local.
func (e Effective) SourceForBool(key string, local bool) ShareSource {
	v, ok := e.Share[key]
	if !ok {
		return ShareSourceLocal
	}
	org, isBool := v.(bool)
	if !isBool {
		// A malformed directive changes nothing (LowerBool and RaiseBool both
		// hand back the value they were given), so neither may it change the
		// attribution.
		return ShareSourceLocal
	}
	switch {
	case local && !org:
		return ShareSourceOrg
	case local && org:
		return ShareSourceBoth
	case !local && org && e.Managed:
		return ShareSourceOrgRaised
	default:
		// local is false and either the org agreed, or it said true on a node
		// where raising is structurally inert. Nothing moved, so the node's
		// own setting is the whole story.
		return ShareSourceLocal
	}
}

// SourceForBoolGated is SourceForBool's exact-answer twin, paired with
// MergeBoolGated (W-8). It reports the same attribution EXCEPT it never
// attributes ShareSourceOrgRaised unless ExtractionAuthorized(e, key) — the
// same per-tier gate MergeBoolGated applies to the value itself, so a
// transparency surface's "Source" column can never claim the org raised a
// key that MergeBoolGated (and the real push seam) left untouched. When the
// gate fails, the honest attribution reverts to ShareSourceLocal: the org's
// directive did not move anything, so the node's own setting is the whole
// story, exactly as SourceForBool already treats every other inert case.
func (e Effective) SourceForBoolGated(key string, local bool) ShareSource {
	src := e.SourceForBool(key, local)
	if src == ShareSourceOrgRaised && !ExtractionAuthorized(e, key) {
		return ShareSourceLocal
	}
	return src
}
