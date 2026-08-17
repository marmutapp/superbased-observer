package govern

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/policyfam/nodegov"
)

// resolveRule is one row of the §3.7 resolution table: a predicate over the
// resolver's inputs and the terminal Effective it produces. The table is
// walked top-down, FIRST MATCH WINS (CLAUDE.md #5 — a data table, not an
// if/else-if ladder), and every row has its own test case.
type resolveRule struct {
	name  string
	match func(in resolveInput) bool
	// result is terminal for every row except the last (which applies the
	// intersection); a nil result means "fall through to application".
	result func(in resolveInput) Effective
}

// resolveInput is the pre-computed input the rows match against.
type resolveInput struct {
	spec      Delivered
	grant     *Grant
	live      LiveIdentity
	now       time.Time
	authority map[string]bool
}

func resolveTable() []resolveRule {
	return []resolveRule{
		{
			// Row 1: no grant. THE dormancy rule — a fully valid, correctly
			// signed body delivered to an ungranted node applies nothing.
			name:  "no grant",
			match: func(in resolveInput) bool { return in.grant == nil },
			result: func(resolveInput) Effective {
				return Effective{State: StateNoGrant}
			},
		},
		{
			// Row 2: the grant's TTL lapsed. This is the offboarding
			// backstop (§5.3): an org that stops authorizing a node stops
			// governing it, without needing the node to be reachable.
			name: "grant expired",
			match: func(in resolveInput) bool {
				return !in.grant.ExpiresAt.IsZero() && in.now.After(in.grant.ExpiresAt)
			},
			result: func(in resolveInput) Effective {
				return Effective{State: StateGrantExpired, ExpiresAt: in.grant.ExpiresAt, OrgName: in.grant.OrgName}
			},
		},
		{
			// Row 3: the grant does not belong to the live enrolment. A
			// raced unenrol / re-enrol wins immediately.
			name: "identity changed",
			match: func(in resolveInput) bool {
				return !in.live.Enrolled ||
					in.live.OrgKey != in.grant.OrgKey ||
					in.live.Generation != in.grant.Generation
			},
			result: func(resolveInput) Effective {
				return Effective{State: StateIdentityChanged}
			},
		},
		{
			// Row 3b (adversarial review A2): the grant was bound to an org
			// policy signing key this node no longer pins. Generalizes the
			// MDM flow's anti-substitution check off the MDM path: without
			// it, a node whose pin was later TOFU-set to a DIFFERENT key
			// would keep honouring authority granted under the old one.
			name: "key pin mismatch",
			match: func(in resolveInput) bool {
				return in.grant.KeyPinSHA256 != "" && in.live.KeyPinSHA256 != in.grant.KeyPinSHA256
			},
			result: func(resolveInput) Effective {
				return Effective{State: StateKeyPinMismatch}
			},
		},
		{
			// Row 5: a live grant, but nothing published (or an accept-path
			// inert verdict with no body to apply).
			name:  "no policy delivered",
			match: func(in resolveInput) bool { return !in.spec.Present },
			result: func(in resolveInput) Effective {
				e := Effective{State: StateNoPolicy, OrgName: in.grant.OrgName, ExpiresAt: in.grant.ExpiresAt}
				return e
			},
		},
	}
}

// Resolve maps a delivered node.governance resource, a grant, and the live
// enrolment identity onto the posture the node actually applies.
//
// It is PURE: same inputs, same output, including Hash. `now` decides grant
// expiry — the caller owns the clock.
func Resolve(spec Delivered, grant *Grant, live LiveIdentity, now time.Time) Effective {
	in := resolveInput{spec: spec, grant: grant, live: live, now: now}
	if grant != nil {
		in.authority = make(map[string]bool, len(grant.Authority))
		for _, a := range grant.Authority {
			in.authority[a] = true
		}
	}
	for _, rule := range resolveTable() {
		if rule.match(in) {
			e := rule.result(in)
			e.normalize(grant)
			return e
		}
	}
	return applyIntersection(in)
}

// applyIntersection is the table's terminal row: a live grant AND a
// delivered body. Each directive class is applied only if the grant carries
// its authority token; otherwise it is DROPPED and recorded, which is what
// forces the caller to report accepted_inert / not_preauthorized.
func applyIntersection(in resolveInput) Effective {
	e := Effective{
		Active:    true,
		State:     StateApplied,
		Version:   in.spec.Version,
		OrgName:   in.grant.OrgName,
		ExpiresAt: in.grant.ExpiresAt,
		Notice: Notice{
			OrgDisplayName: in.spec.Spec.Notice.OrgDisplayName,
			Contact:        in.spec.Spec.Notice.Contact,
			PolicyURL:      in.spec.Spec.Notice.PolicyURL,
		},
	}
	for _, class := range directiveClasses() {
		if !class.present(in.spec.Spec) {
			continue
		}
		switch {
		case in.spec.InertReason != "":
			// The accept path already refused to let this body run (e.g. the
			// node's own preauthorize_enforce gate). Nothing is applied, and
			// the reason travels through unchanged rather than being
			// relabelled.
			e.Dropped = append(e.Dropped, Dropped{Directive: class.name, Reason: in.spec.InertReason})
		case in.authority[class.authority]:
			class.apply(&e, in.spec.Spec)
		default:
			// The org asked for something this node never granted. Drop the
			// whole class and RECORD it — that is what forces the caller to
			// report accepted_inert rather than effective, so a partial
			// application can never masquerade as convergence. The notice is
			// kept either way: the developer still gets told who manages the
			// machine.
			e.Dropped = append(e.Dropped, Dropped{Directive: class.name, Reason: class.dropReason(in)})
		}
	}
	if len(e.Dropped) > 0 {
		e.State = StateInert
	}
	// features → pinned is a COMPILE-TIME alias, so the two maps merge into
	// the ONE map the sidecar materializes. They are merged only AFTER each
	// has passed its own authority gate above, so feature.lock and
	// settings.pin remain independently revocable.
	e.mergeFeaturePins()
	e.normalize(in.grant)
	return e
}

// directiveClass is one row of the authority-intersection table: the
// directive class an org body may carry, the authority token it requires,
// and how it lands on the resolved posture. Table-driven (CLAUDE.md #5) so a
// fifth class cannot be added with a different set of checks.
type directiveClass struct {
	name      string
	authority string
	present   func(nodegov.PolicySpec) bool
	apply     func(*Effective, nodegov.PolicySpec)
	// dropReason lets a class explain a drop more precisely than
	// not_preauthorized. Only `share` uses it today, for the retired
	// capture.raise token.
	dropReason func(resolveInput) string
}

func directiveClasses() []directiveClass {
	notPreauthorized := func(resolveInput) string { return ReasonNotPreauthorized }
	return []directiveClass{
		{
			name: "sections", authority: AuthorityDashboardVisibility,
			// ALWAYS considered present, unlike the three Phase-1b classes.
			// This preserves Phase 1a's semantics exactly: an org that
			// publishes a node.governance resource to a node that granted no
			// dashboard.visibility reports accepted_inert, even when the body
			// carries only notice copy. The notice IS a surface change, so
			// the base class of the family is never "absent".
			present: func(nodegov.PolicySpec) bool { return true },
			apply: func(e *Effective, s nodegov.PolicySpec) {
				e.HiddenSections = append(e.HiddenSections, s.HiddenSections...)
				e.ReadOnlySections = append(e.ReadOnlySections, s.ReadOnlySections...)
				e.HiddenSettings = append(e.HiddenSettings, s.HiddenSettings...)
				e.ReadOnlySettings = append(e.ReadOnlySettings, s.ReadOnlySettings...)
			},
			dropReason: notPreauthorized,
		},
		{
			name: "pinned", authority: AuthoritySettingsPin,
			present: func(s nodegov.PolicySpec) bool { return len(s.Pinned) > 0 },
			apply: func(e *Effective, s nodegov.PolicySpec) {
				e.Pinned = mergeAny(e.Pinned, s.Pinned)
			},
			dropReason: notPreauthorized,
		},
		{
			name: "share", authority: AuthorityCapturePin,
			present: func(s nodegov.PolicySpec) bool { return len(s.Share) > 0 },
			apply: func(e *Effective, s nodegov.PolicySpec) {
				e.Share = mergeAny(e.Share, s.Share)
			},
			dropReason: func(in resolveInput) string {
				// A grant minted by a Phase-1a server can only carry
				// capture.raise, which is retired and grants nothing. Saying
				// so is materially more useful to the developer than
				// "not_preauthorized": the fix is a fresh `observer enroll`,
				// not an admin action.
				if in.authority[AuthorityCaptureRaise] {
					return ReasonAuthorityRetired
				}
				return ReasonNotPreauthorized
			},
		},
		{
			name: "features", authority: AuthorityFeatureLock,
			present: func(s nodegov.PolicySpec) bool { return len(s.Features) > 0 },
			apply: func(e *Effective, s nodegov.PolicySpec) {
				if e.Features == nil {
					e.Features = make(map[string]bool, len(s.Features))
				}
				for id, on := range s.Features {
					e.Features[id] = on
				}
				e.featurePins = mergeAny(e.featurePins, s.FeaturePinned)
			},
			dropReason: notPreauthorized,
		},
	}
}

func mergeAny(dst, src map[string]any) map[string]any {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]any, len(src))
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// mergeFeaturePins folds the authorized feature expansions into Pinned.
// A feature and an explicit pin can never disagree — nodegov.Compile refuses
// a body where they do — so the merge order is irrelevant and no precedence
// rule is needed or invented here.
func (e *Effective) mergeFeaturePins() {
	e.Pinned = mergeAny(e.Pinned, e.featurePins)
	e.featurePins = nil
}

// normalize fills the grant-derived fields every branch shares, guarantees
// non-nil slices (so the JSON surface is [] rather than null on a solo
// node), and computes the content hash of the RESOLVED posture.
func (e *Effective) normalize(grant *Grant) {
	if e.HiddenSections == nil {
		e.HiddenSections = []string{}
	}
	if e.ReadOnlySections == nil {
		e.ReadOnlySections = []string{}
	}
	if e.HiddenSettings == nil {
		e.HiddenSettings = []string{}
	}
	if e.ReadOnlySettings == nil {
		e.ReadOnlySettings = []string{}
	}
	if grant != nil {
		known, unknown := splitAuthority(grant.Authority)
		e.Authority = known
		e.UnknownAuthority = unknown
	}
	e.Hash = e.contentHash()
}

// splitAuthority partitions a grant's tokens into the ones this build
// understands and the ones it does not. An unknown token is never a failure
// (forward compat) and never silently swallowed (it is reported).
func splitAuthority(tokens []string) (known, unknown []string) {
	seenK := map[string]bool{}
	seenU := map[string]bool{}
	for _, t := range tokens {
		switch {
		case KnownAuthority(t):
			if !seenK[t] {
				seenK[t] = true
				known = append(known, t)
			}
		case t != "":
			if !seenU[t] {
				seenU[t] = true
				unknown = append(unknown, t)
			}
		}
	}
	sort.Strings(known)
	sort.Strings(unknown)
	return known, unknown
}

// contentHash is the content address of the RUNNING posture. It covers the
// state, the version and every applied list, so a posture that dropped a
// directive hashes differently from the delivered body — which is precisely
// the fact that stops a partial application reporting as convergence.
func (e Effective) contentHash() string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			_, _ = h.Write([]byte(p))
			_, _ = h.Write([]byte{0x1e})
		}
	}
	write("state", string(e.State))
	write("version", strconv.FormatInt(e.Version, 10))
	for _, pair := range []struct {
		label string
		ids   []string
	}{
		{"hidden", e.HiddenSections},
		{"read_only", e.ReadOnlySections},
		{"settings_hidden", e.HiddenSettings},
		{"settings_read_only", e.ReadOnlySettings},
	} {
		for _, id := range pair.ids {
			write(pair.label, id)
		}
	}
	// The Phase-1b directive classes are part of the RUNNING posture, so a
	// changed pin is a changed hash. Without this, a body that altered only
	// its pinned map would hash like the previous one and the node would
	// report convergence on a posture it had not yet adopted.
	for _, key := range sortedAnyKeys(e.Pinned) {
		write("pinned", key, hashValue(e.Pinned[key]))
	}
	for _, key := range sortedAnyKeys(e.Share) {
		write("share", key, hashValue(e.Share[key]))
	}
	for _, id := range sortedBoolKeys(e.Features) {
		write("feature", id, strconv.FormatBool(e.Features[id]))
	}
	write("notice", e.Notice.OrgDisplayName, e.Notice.Contact, e.Notice.PolicyURL)
	for _, d := range e.Dropped {
		write("dropped", d.Directive, d.Reason)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sortedAnyKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedBoolKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// hashValue renders a pinned/share value deterministically. It must never
// depend on %v's formatting of a slice, so lists are joined explicitly.
func hashValue(v any) string {
	switch tv := v.(type) {
	case bool:
		return strconv.FormatBool(tv)
	case string:
		return tv
	case int64:
		return strconv.FormatInt(tv, 10)
	case []string:
		return strings.Join(tv, "\x1f")
	default:
		return fmt.Sprintf("%v", v)
	}
}
