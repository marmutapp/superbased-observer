package providers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// BodyV1 is the org-wire v1 body for the gateway.providers family
// (docs/plans/gateway-config-plane-spec-2026-08-15.md Phase 3): the exact
// JSON shape an org publisher POSTs and an agent later finds inside a
// fetched SignedPolicyResource.Body:
//
//	{"upstreams":{"<lane-id>":{"base_url":"https://..."}},"auto_default_lane":"<lane-id>"}
type BodyV1 struct {
	Upstreams       map[string]UpstreamBodyV1 `json:"upstreams"`
	AutoDefaultLane string                    `json:"auto_default_lane,omitempty"`
}

// UpstreamBodyV1 is the wire shape of one lane's upstream definition. It is
// a one-field object (rather than a bare string) so the wire shape can grow
// per-lane fields later (e.g. headers, timeouts) without a breaking change.
type UpstreamBodyV1 struct {
	BaseURL string `json:"base_url"`
}

// ToPolicyInput maps the wire body onto the pure engine's input.
func (b BodyV1) ToPolicyInput() PolicyInput {
	in := PolicyInput{
		AutoDefaultLane: b.AutoDefaultLane,
	}
	if len(b.Upstreams) > 0 {
		in.Upstreams = make(map[string]string, len(b.Upstreams))
		for id, u := range b.Upstreams {
			in.Upstreams[id] = u.BaseURL
		}
	}
	return in
}

// BodyV1FromPolicyInput is the inverse of ToPolicyInput, letting a server
// GET surface or a config-derived body reconstruct the wire shape.
func BodyV1FromPolicyInput(in PolicyInput) BodyV1 {
	b := BodyV1{AutoDefaultLane: in.AutoDefaultLane}
	if len(in.Upstreams) > 0 {
		b.Upstreams = make(map[string]UpstreamBodyV1, len(in.Upstreams))
		for id, base := range in.Upstreams {
			b.Upstreams[id] = UpstreamBodyV1{BaseURL: base}
		}
	}
	return b
}

// DecodeBody strictly decodes raw org-wire JSON bytes into a BodyV1: unknown
// fields are rejected (DisallowUnknownFields), the document must not exceed
// maxBytes, and any byte after the JSON value is rejected — the same
// closed-document discipline as policyfam/admission.DecodeBody, reimplemented
// here so this package stays free of an orgcontract import and can compile a
// body standalone for internal/orgserver/internal/orgclient callers.
func DecodeBody(raw []byte, maxBytes int64) (BodyV1, error) {
	if maxBytes <= 0 {
		return BodyV1{}, fmt.Errorf("policyfam/providers.DecodeBody: cap must be positive, got %d", maxBytes)
	}
	if int64(len(raw)) > maxBytes {
		return BodyV1{}, fmt.Errorf("policyfam/providers.DecodeBody: body is %d bytes, exceeds the %d-byte cap", len(raw), maxBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var body BodyV1
	if err := dec.Decode(&body); err != nil {
		return BodyV1{}, fmt.Errorf("policyfam/providers.DecodeBody: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return BodyV1{}, fmt.Errorf("policyfam/providers.DecodeBody: trailing bytes after the document")
	}
	return body, nil
}

// CanonicalJSON re-encodes a decoded BodyV1 deterministically. encoding/json
// marshals map keys in sorted order, so the lane table's key order never
// depends on how the original bytes were formatted; BodyHash is computed
// over exactly this output, never over whatever bytes a publisher happened
// to submit — so two semantically-equal bodies published a character apart
// (extra whitespace, reordered keys) hash identically.
func CanonicalJSON(b BodyV1) ([]byte, error) {
	out, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("policyfam/providers.CanonicalJSON: %w", err)
	}
	return out, nil
}

// CompileBody decodes and canonicalizes raw org-wire JSON, then compiles it
// into a ready PolicySpec. It returns the canonical JSON bytes the caller
// should sign/hash as the resource's Body — so BodyHash is always computed
// over exactly the bytes DecodeBody would reproduce from that Body, never
// over the publisher's raw submission.
func CompileBody(raw []byte, maxBytes int64) (spec PolicySpec, canonicalBody []byte, err error) {
	body, err := DecodeBody(raw, maxBytes)
	if err != nil {
		return PolicySpec{}, nil, err
	}
	canon, err := CanonicalJSON(body)
	if err != nil {
		return PolicySpec{}, nil, err
	}
	spec, err = Compile(body.ToPolicyInput())
	if err != nil {
		return PolicySpec{}, nil, fmt.Errorf("policyfam/providers.CompileBody: %w", err)
	}
	return spec, canon, nil
}
