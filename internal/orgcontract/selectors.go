package orgcontract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Policy-resource targeting selectors (P0-10 Phase B; design of record
// docs/plans/policy-targeting-rollback-design-2026-08-13.md §2). The
// vocabulary is CLOSED — workspace | environment | service — and lives here,
// in the shared contract package, so the org server (which normalizes and
// SIGNS the predicate) and the agent (which re-derives the canonical form to
// keep SignedPolicyResource.SelectorsJSON a grammar-constrained field) can
// never drift on what "canonical" means. Drift between the two sides would
// be a signature-verification break or, worse, a silently-widened envelope.

// MaxPolicyResourceSelectorsBytes bounds SignedPolicyResource.SelectorsJSON.
// The three-key closed vocabulary cannot legitimately need more than a
// kilobyte; a larger value is either a publisher bug or an attempt to smuggle
// payload through a field the closed-envelope gate treats as grammar-checked
// rather than semantically interpreted.
const MaxPolicyResourceSelectorsBytes = 1 << 10 // 1 KiB

// Selectors is the targeting predicate carried (pre-serialized) by
// SignedPolicyResource.SelectorsJSON. An absent/empty field matches every
// value of that attribute; a set field requires an exact string match. The
// all-empty predicate ("{}") matches every node.
type Selectors struct {
	Workspace   string `json:"workspace,omitempty"`
	Environment string `json:"environment,omitempty"`
	Service     string `json:"service,omitempty"`
}

// IsEmpty reports whether the predicate targets everyone (no key set).
func (s Selectors) IsEmpty() bool {
	return s.Workspace == "" && s.Environment == "" && s.Service == ""
}

// ParseSelectors strictly decodes a selectors JSON document. Empty input and
// "{}" both decode to the empty (match-all) predicate. Decoding is strict —
// an unknown key, a non-string value, or trailing content is an error, never
// a silently-ignored field: the agent's closed-envelope gate depends on this
// grammar being total.
func ParseSelectors(raw string) (Selectors, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "{}" {
		return Selectors{}, nil
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var s Selectors
	if err := dec.Decode(&s); err != nil {
		return Selectors{}, fmt.Errorf("orgcontract.ParseSelectors: %w", err)
	}
	if dec.More() {
		return Selectors{}, fmt.Errorf("orgcontract.ParseSelectors: trailing content after the selectors object")
	}
	s.Workspace = strings.TrimSpace(s.Workspace)
	s.Environment = strings.TrimSpace(s.Environment)
	s.Service = strings.TrimSpace(s.Service)
	return s, nil
}

// CanonicalSelectorsJSON renders a predicate in the ONE canonical encoding
// the signing message binds: compact, keys sorted, empty values omitted,
// "{}" when nothing is targeted.
func CanonicalSelectorsJSON(s Selectors) string {
	pairs := map[string]string{}
	if s.Workspace != "" {
		pairs["workspace"] = s.Workspace
	}
	if s.Environment != "" {
		pairs["environment"] = s.Environment
	}
	if s.Service != "" {
		pairs["service"] = s.Service
	}
	if len(pairs) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(pairs[k])
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.String()
}

// NormalizeSelectorsJSON validates and canonicalizes a selectors JSON
// document for the signing message: strict decode, trimmed values, sorted
// keys, "{}" when empty. It is TOLERANT of a non-canonical input (padded
// whitespace, unsorted keys, explicit empty values) — canonicalization is
// exactly its job. The strictness that matters to a verifying agent comes
// from comparing the input to this output byte for byte; see
// ValidateCanonicalSelectorsJSON.
func NormalizeSelectorsJSON(raw string) (string, error) {
	s, err := ParseSelectors(raw)
	if err != nil {
		return "", fmt.Errorf("orgcontract.NormalizeSelectorsJSON: %w", err)
	}
	return CanonicalSelectorsJSON(s), nil
}

// ValidateCanonicalSelectorsJSON is the AGENT-SIDE closed-envelope gate for
// SignedPolicyResource.SelectorsJSON (design §2 step 1): the field must be
// within MaxPolicyResourceSelectorsBytes and BYTE-IDENTICAL to its own
// canonical form. The targeting field opens; its grammar does not. A
// semantically-equal but non-canonical spelling (unsorted keys, padded
// whitespace, an unknown key, an explicitly-empty value) is a violation, not
// a normalization opportunity — the agent must never interpret a form the
// signer did not produce.
func ValidateCanonicalSelectorsJSON(raw string) (Selectors, error) {
	if len(raw) > MaxPolicyResourceSelectorsBytes {
		return Selectors{}, fmt.Errorf("orgcontract.ValidateCanonicalSelectorsJSON: selectors_json is %d bytes, over the %d-byte maximum",
			len(raw), MaxPolicyResourceSelectorsBytes)
	}
	s, err := ParseSelectors(raw)
	if err != nil {
		return Selectors{}, fmt.Errorf("orgcontract.ValidateCanonicalSelectorsJSON: %w", err)
	}
	if canonical := CanonicalSelectorsJSON(s); canonical != raw {
		return Selectors{}, fmt.Errorf("orgcontract.ValidateCanonicalSelectorsJSON: selectors_json %q is not canonical (want %q)", raw, canonical)
	}
	return s, nil
}

// SelectorKey names one attribute of the closed selector vocabulary. Returned
// (sorted) by CorroborateSelectors so a caller can log/report exactly which
// attributes disagreed without re-deriving the comparison.
const (
	SelectorKeyWorkspace   = "workspace"
	SelectorKeyEnvironment = "environment"
	SelectorKeyService     = "service"
)

// CorroborateSelectors compares a signed targeting predicate against a node's
// LOCALLY-CONFIGURED attributes (design §2 step 2 — "soft-strict"). It is
// three-valued per key:
//
//   - the predicate does not set the key → nothing to corroborate;
//   - the predicate sets it and the node configured a DIFFERENT value →
//     the key lands in mismatched (the caller rejects, keeping prior LKG);
//   - the predicate sets it and the node configured NOTHING → the key lands
//     in uncorroborated (the caller accepts and logs).
//
// Both slices come back sorted for stable logging. This check can only ever
// NARROW what a node installs — the server already chose which resource to
// serve, from attributes bound to the verified identity — so it cannot be
// used to acquire policy. It catches a mis-targeted publish or a mis-
// delivered envelope, and a node with no attributes configured at all
// accepts everything (no fleet-wide breakage on upgrade).
func CorroborateSelectors(sel, nodeAttrs Selectors) (mismatched, uncorroborated []string) {
	pairs := []struct {
		key      string
		selector string
		attr     string
	}{
		{SelectorKeyEnvironment, sel.Environment, nodeAttrs.Environment},
		{SelectorKeyService, sel.Service, nodeAttrs.Service},
		{SelectorKeyWorkspace, sel.Workspace, nodeAttrs.Workspace},
	}
	for _, p := range pairs {
		switch {
		case p.selector == "":
			// Match-all for this attribute.
		case p.attr == "":
			uncorroborated = append(uncorroborated, p.key)
		case p.attr != p.selector:
			mismatched = append(mismatched, p.key)
		}
	}
	return mismatched, uncorroborated
}
