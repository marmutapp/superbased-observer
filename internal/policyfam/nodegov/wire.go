package nodegov

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxSchema is the highest body schema this agent generation understands.
// A body declaring a HIGHER schema is rejected (decode_failed at the agent,
// lint failure at publish) rather than partially applied: the vocabularies
// are versioned BY the schema, so a newer schema may legitimately name ids
// this build has never heard of, and silently dropping them would make the
// admin console lie about what a node hid.
//
// 1 → 2 in Phase 1b: `pinned`, `share` and `features` are new top-level body
// keys, and DecodeBody runs DisallowUnknownFields, so a Phase-1a agent would
// reject a body carrying them REGARDLESS of what we do. Bumping the schema
// makes that rejection legible (`decode_failed`, keep the previous LKG)
// rather than mysterious. A 1b agent accepts schemas 1 and 2.
const MaxSchema = 2

// Body is the org-wire body for the node.governance family
// (docs/plans/admin-controlled-plane-b-spec-2026-08-15.md §3.2 for schema 1;
// the Phase-1b mini-spec §1.9/§2/§3 for schema 2):
//
//	{"schema":2,
//	 "sections":{"hidden":["benchmarks"],"read_only":["policies"],
//	             "settings_hidden":["process"],"settings_read_only":["guard"]},
//	 "pinned":{"guard.enabled":true},
//	 "share":{"full_content":false},
//	 "features":{"guard":true},
//	 "notice":{"org_display_name":"Acme","contact":"it@acme.example"}}
//
// Schema 2 is ADDITIVE over schema 1: `sections` and `notice` are
// byte-identical, and a schema-1 body naming any of the three new keys is a
// hard error (see DecodeBody), so the schema number always describes what
// the body actually contains.
type Body struct {
	Schema   int        `json:"schema"`
	Sections SectionsV1 `json:"sections,omitempty"`
	Notice   *NoticeV1  `json:"notice,omitempty"`
	// Pinned is the settings.pin directive class: dotted config path →
	// typed value, drawn from PinnableKeys. Schema 2+.
	Pinned map[string]any `json:"pinned,omitempty"`
	// Share is the capture.pin directive class: [org_client.share] key →
	// value, drawn from ShareKeys, applied LOWERING-ONLY (§2.1). Schema 2+.
	Share map[string]any `json:"share,omitempty"`
	// Features is the feature.lock directive class: feature id → bool,
	// expanded at compile time into pinned entries (§3). Schema 2+.
	Features map[string]bool `json:"features,omitempty"`
}

// BodyV1 is the Phase-1a name for the same struct, kept so existing
// references keep compiling.
//
// Deprecated: use Body.
type BodyV1 = Body

// SectionsV1 is the dashboard.visibility directive class.
type SectionsV1 struct {
	Hidden           []string `json:"hidden,omitempty"`
	ReadOnly         []string `json:"read_only,omitempty"`
	SettingsHidden   []string `json:"settings_hidden,omitempty"`
	SettingsReadOnly []string `json:"settings_read_only,omitempty"`
}

// NoticeV1 is the wire shape of the honesty copy.
type NoticeV1 struct {
	OrgDisplayName string `json:"org_display_name,omitempty"`
	Contact        string `json:"contact,omitempty"`
	PolicyURL      string `json:"policy_url,omitempty"`
}

// ToPolicyInput maps the wire body onto the pure engine's input.
func (b Body) ToPolicyInput() PolicyInput {
	in := PolicyInput{
		HiddenSections:   b.Sections.Hidden,
		ReadOnlySections: b.Sections.ReadOnly,
		HiddenSettings:   b.Sections.SettingsHidden,
		ReadOnlySettings: b.Sections.SettingsReadOnly,
		Pinned:           b.Pinned,
		Share:            b.Share,
		Features:         b.Features,
	}
	if b.Notice != nil {
		in.Notice = Notice{
			OrgDisplayName: b.Notice.OrgDisplayName,
			Contact:        b.Notice.Contact,
			PolicyURL:      b.Notice.PolicyURL,
		}
	}
	return in
}

// BodyFromPolicySpec is the inverse, letting a server GET surface or a
// round-trip test reconstruct the wire shape from a compiled spec.
//
// Note that the FEATURE block does not round-trip as a feature block: a
// feature is a compile-time alias, so a compiled spec carries only the pins
// it expanded to. The canonical form of `{"features":{"guard":true}}` is
// therefore `{"pinned":{"guard.enabled":true}}`, which is exactly the
// property that stops a feature lock drifting from the pin implementing it.
func BodyFromPolicySpec(s PolicySpec) Body {
	b := Body{
		Schema: MaxSchema,
		Sections: SectionsV1{
			Hidden:           s.HiddenSections,
			ReadOnly:         s.ReadOnlySections,
			SettingsHidden:   s.HiddenSettings,
			SettingsReadOnly: s.ReadOnlySettings,
		},
		Pinned: s.Pinned,
		Share:  s.Share,
	}
	if len(b.Pinned) == 0 {
		b.Pinned = nil
	}
	if len(b.Share) == 0 {
		b.Share = nil
	}
	if s.Notice != (Notice{}) {
		b.Notice = &NoticeV1{
			OrgDisplayName: s.Notice.OrgDisplayName,
			Contact:        s.Notice.Contact,
			PolicyURL:      s.Notice.PolicyURL,
		}
	}
	return b
}

// BodyV1FromPolicySpec is the Phase-1a name.
//
// Deprecated: use BodyFromPolicySpec.
func BodyV1FromPolicySpec(s PolicySpec) Body { return BodyFromPolicySpec(s) }

// DecodeBody strictly decodes raw org-wire JSON bytes into a BodyV1: unknown
// fields are rejected (DisallowUnknownFields), the document must not exceed
// maxBytes, any byte after the JSON value is rejected, and the schema gate
// runs here so every caller (publish lint, agent accept, LKG replay) applies
// it identically.
func DecodeBody(raw []byte, maxBytes int64) (Body, error) {
	if maxBytes <= 0 {
		return Body{}, fmt.Errorf("policyfam/nodegov.DecodeBody: cap must be positive, got %d", maxBytes)
	}
	if int64(len(raw)) > maxBytes {
		return Body{}, fmt.Errorf("policyfam/nodegov.DecodeBody: body is %d bytes, exceeds the %d-byte cap", len(raw), maxBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var body Body
	if err := dec.Decode(&body); err != nil {
		return Body{}, fmt.Errorf("policyfam/nodegov.DecodeBody: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Body{}, fmt.Errorf("policyfam/nodegov.DecodeBody: trailing bytes after the document")
	}
	if body.Schema < 1 {
		return Body{}, fmt.Errorf("policyfam/nodegov.DecodeBody: schema is required and must be >= 1")
	}
	if body.Schema > MaxSchema {
		return Body{}, fmt.Errorf("policyfam/nodegov.DecodeBody: body declares schema %d, this build understands up to %d", body.Schema, MaxSchema)
	}
	// Schema 1 predates the three Phase-1b directive classes. A schema-1
	// body naming one is a HARD error rather than a silent drop, so the
	// schema number always describes what the body actually contains and an
	// admin can never believe a pin took effect on a body that declared
	// itself older than the feature.
	if body.Schema < 2 {
		for name, present := range map[string]bool{
			"pinned":   len(body.Pinned) > 0,
			"share":    len(body.Share) > 0,
			"features": len(body.Features) > 0,
		} {
			if present {
				return Body{}, fmt.Errorf("policyfam/nodegov.DecodeBody: %q requires schema 2, but the body declares schema %d", name, body.Schema)
			}
		}
	}
	return body, nil
}

// CanonicalJSON re-encodes a decoded BodyV1 deterministically. BodyHash is
// computed over exactly this output, never over whatever bytes a publisher
// happened to submit, so two semantically-equal bodies hash identically.
//
// It canonicalizes through Compile so the sorted/deduplicated list order is
// the one that lands on the wire — a body listing ["remote","benchmarks"]
// and one listing ["benchmarks","remote"] are the same resource.
func CanonicalJSON(b Body) ([]byte, error) {
	spec, err := Compile(b.ToPolicyInput())
	if err != nil {
		return nil, err
	}
	canon := BodyFromPolicySpec(spec)
	canon.Schema = b.Schema
	out, err := json.Marshal(canon)
	if err != nil {
		return nil, fmt.Errorf("policyfam/nodegov.CanonicalJSON: %w", err)
	}
	return out, nil
}

// CompileBody decodes and canonicalizes raw org-wire JSON, then compiles it
// into a ready PolicySpec, returning the canonical bytes the caller should
// sign/hash as the resource's Body.
func CompileBody(raw []byte, maxBytes int64) (spec PolicySpec, canonicalBody []byte, err error) {
	body, err := DecodeBody(raw, maxBytes)
	if err != nil {
		return PolicySpec{}, nil, err
	}
	spec, err = Compile(body.ToPolicyInput())
	if err != nil {
		return PolicySpec{}, nil, fmt.Errorf("policyfam/nodegov.CompileBody: %w", err)
	}
	canon, err := CanonicalJSON(body)
	if err != nil {
		return PolicySpec{}, nil, err
	}
	return spec, canon, nil
}
