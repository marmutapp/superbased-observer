package admission

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// BodyV1 is the org-wire v1 body for the admission.input / admission.output
// families (docs/plane-a/unified-policy-resource.md §3, §6): the exact JSON
// shape an org publisher POSTs and an agent later finds inside a fetched
// SignedPolicyResource.Body. It mirrors PolicyInput field-for-field; the
// only difference is JSON tags — PolicyInput is also built directly in Go
// (e.g. from [observability.admission] TOML at the cmd/observer boundary)
// and must not carry a wire opinion.
type BodyV1 struct {
	Mode                   string            `json:"mode"`
	Strict                 bool              `json:"strict,omitempty"`
	Scope                  string            `json:"scope,omitempty"`
	SecretRemoteJudge      string            `json:"secret_remote_judge,omitempty"`
	JudgeChunkBytes        int               `json:"judge_chunk_bytes,omitempty"`
	JudgeChunkOverlapBytes int               `json:"judge_chunk_overlap_bytes,omitempty"`
	Prefilter              PrefilterBodyV1   `json:"prefilter,omitempty"`
	Criteria               []CriterionBodyV1 `json:"criteria,omitempty"`
}

// PrefilterBodyV1 is the wire shape of the deterministic pre-filter layer.
type PrefilterBodyV1 struct {
	Allow           []string `json:"allow,omitempty"`
	Deny            []string `json:"deny,omitempty"`
	MaxMessageBytes int      `json:"max_message_bytes,omitempty"`
}

// CriterionBodyV1 is the wire shape of one uncompiled criterion.
type CriterionBodyV1 struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Name       string   `json:"name,omitempty"`
	Definition string   `json:"definition,omitempty"`
	Topics     []string `json:"topics,omitempty"`
	Decision   string   `json:"decision"`
	Severity   string   `json:"severity,omitempty"`
}

// ToPolicyInput maps the wire body onto the pure engine's input — the same
// shape internal/obs/httpapi/admission.go's policyInputFromDTO builds for the
// dashboard editor, reused here for the org-wire boundary.
func (b BodyV1) ToPolicyInput() PolicyInput {
	in := PolicyInput{
		Mode:                   b.Mode,
		Strict:                 b.Strict,
		Scope:                  b.Scope,
		SecretRemoteJudge:      b.SecretRemoteJudge,
		JudgeChunkBytes:        b.JudgeChunkBytes,
		JudgeChunkOverlapBytes: b.JudgeChunkOverlapBytes,
		Prefilter: PrefilterInput{
			Allow:           b.Prefilter.Allow,
			Deny:            b.Prefilter.Deny,
			MaxMessageBytes: b.Prefilter.MaxMessageBytes,
		},
	}
	for _, c := range b.Criteria {
		in.Criteria = append(in.Criteria, CriterionInput{
			ID:         c.ID,
			Type:       c.Type,
			Name:       c.Name,
			Definition: c.Definition,
			Topics:     c.Topics,
			Decision:   c.Decision,
			Severity:   c.Severity,
		})
	}
	return in
}

// BodyV1FromPolicyInput is the inverse of ToPolicyInput, letting a server GET
// surface or a config-derived body reconstruct the wire shape.
func BodyV1FromPolicyInput(in PolicyInput) BodyV1 {
	b := BodyV1{
		Mode:                   in.Mode,
		Strict:                 in.Strict,
		Scope:                  in.Scope,
		SecretRemoteJudge:      in.SecretRemoteJudge,
		JudgeChunkBytes:        in.JudgeChunkBytes,
		JudgeChunkOverlapBytes: in.JudgeChunkOverlapBytes,
		Prefilter: PrefilterBodyV1{
			Allow:           in.Prefilter.Allow,
			Deny:            in.Prefilter.Deny,
			MaxMessageBytes: in.Prefilter.MaxMessageBytes,
		},
	}
	for _, c := range in.Criteria {
		b.Criteria = append(b.Criteria, CriterionBodyV1{
			ID:         c.ID,
			Type:       c.Type,
			Name:       c.Name,
			Definition: c.Definition,
			Topics:     c.Topics,
			Decision:   c.Decision,
			Severity:   c.Severity,
		})
	}
	return b
}

// DecodeBody strictly decodes raw org-wire JSON bytes into a BodyV1: unknown
// fields are rejected (DisallowUnknownFields), the document must not exceed
// maxBytes, and any byte after the JSON value is rejected — the same
// closed-document discipline as orgcontract.DecodeCapped, reimplemented here
// so this package stays free of an orgcontract import (purity,
// imports_test.go — internal/orgcontract is not on policyfam's forbidden
// list, but policyfam must work standalone for internal/orgclient/orgserver
// callers that don't want the wider orgcontract surface for a single decode).
func DecodeBody(raw []byte, maxBytes int64) (BodyV1, error) {
	if maxBytes <= 0 {
		return BodyV1{}, fmt.Errorf("policyfam/admission.DecodeBody: cap must be positive, got %d", maxBytes)
	}
	if int64(len(raw)) > maxBytes {
		return BodyV1{}, fmt.Errorf("policyfam/admission.DecodeBody: body is %d bytes, exceeds the %d-byte cap", len(raw), maxBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var body BodyV1
	if err := dec.Decode(&body); err != nil {
		return BodyV1{}, fmt.Errorf("policyfam/admission.DecodeBody: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return BodyV1{}, fmt.Errorf("policyfam/admission.DecodeBody: trailing bytes after the document")
	}
	return body, nil
}

// CanonicalJSON re-encodes a decoded BodyV1 deterministically. BodyV1's field
// order is fixed by its struct definition, so the same logical body always
// produces byte-identical JSON regardless of how the original bytes were
// formatted, key-ordered, or spaced. BodyHash (unified-policy-resource.md
// §4.3) is computed over THIS output, never over whatever bytes a publisher
// happened to submit — so two semantically-equal bodies published a
// character apart (extra whitespace, reordered keys) hash identically.
func CanonicalJSON(b BodyV1) ([]byte, error) {
	out, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("policyfam/admission.CanonicalJSON: %w", err)
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
		return PolicySpec{}, nil, fmt.Errorf("policyfam/admission.CompileBody: %w", err)
	}
	return spec, canon, nil
}

// RequiresJudge reports whether spec has any judged criterion (valid_use_case
// or custom) — the runtime capability an enforcement point must advertise
// before a body carrying it can be ACCEPTED (not merely compiled), per the
// closed capability registry (plan §6.6).
func RequiresJudge(spec PolicySpec) bool {
	for _, c := range spec.Criteria {
		if c.Type.Judged() {
			return true
		}
	}
	return false
}

// ValidateRuntimeCaps checks a compiled spec against the enforcement point's
// LIVE capabilities. v1's only capability token this family cares about is
// "a judge is configured": a body with a judged criterion is rejected here
// when the runtime reports hasJudge=false, so the agent's four-gate accept
// (unified-policy-resource.md §7 gate 4) can report capability_mismatch
// instead of silently installing a body it can never fully evaluate.
func ValidateRuntimeCaps(spec PolicySpec, hasJudge bool) error {
	if RequiresJudge(spec) && !hasJudge {
		return errors.New("policyfam/admission.ValidateRuntimeCaps: body requires a judge (a valid_use_case or custom criterion) but the runtime has none configured")
	}
	return nil
}
