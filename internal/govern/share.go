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

// ShareSource classifies who decided a share key's effective value, for the
// Privacy page's Source column. It is resolved HERE, at the boundary, and
// never assembled as a string in the SPA.
type ShareSource string

const (
	// ShareSourceLocal — the org published nothing for this key.
	ShareSourceLocal ShareSource = "you"
	// ShareSourceOrg — the org LOWERED this key below the node's setting.
	ShareSourceOrg ShareSource = "org"
	// ShareSourceBoth — the org pinned this key at the value the node had
	// already set.
	ShareSourceBoth ShareSource = "both"
)

// SourceForBool reports who decided a boolean share key.
//
// There is deliberately no value of this enum that means "the organization
// increased what is shared", because it structurally cannot — and a value
// that could say it would eventually be rendered by a bug.
func (e Effective) SourceForBool(key string, local bool) ShareSource {
	v, ok := e.Share[key]
	if !ok {
		return ShareSourceLocal
	}
	org, isBool := v.(bool)
	if !isBool {
		return ShareSourceLocal
	}
	switch {
	case local && !org:
		return ShareSourceOrg
	case local && org:
		return ShareSourceBoth
	default:
		// local is already false: the org directive is a no-op either way,
		// so the node's own setting is the whole story.
		return ShareSourceLocal
	}
}
