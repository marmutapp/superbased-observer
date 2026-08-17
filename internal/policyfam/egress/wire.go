package egress

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// BodyV1 is the org-wire v1 body for the egress.routing_guardrail family
// (docs/plane-a/unified-policy-resource.md §3, §6): the exact JSON shape an
// org publisher POSTs and an agent later finds inside a fetched
// SignedPolicyResource.Body.
//
// Unlike RuleInput (which flattens a rule's action onto itself), BodyV1
// NESTS a rule's action fields under "action" — mirroring
// internal/obs/httpapi/egress_editor.go's dashboard editor DTO, the
// established client-facing convention for this family. ToPolicyInput maps
// the nested shape onto the flat RuleInput the engine compiles, at this one
// boundary (Phase F item 5).
type BodyV1 struct {
	Mode            string            `json:"mode"`
	CooldownSeconds int               `json:"cooldown_seconds,omitempty"`
	Targets         []TargetBodyV1    `json:"targets,omitempty"`
	Rules           []RuleBodyV1      `json:"rules,omitempty"`
	Cohorts         map[string]string `json:"cohorts,omitempty"`
}

// TargetBodyV1 is one typed upstream target on the wire.
type TargetBodyV1 struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Shape string `json:"shape"`
}

// WhenBodyV1 is one matcher set on the wire. BudgetBandAtLeast is a pointer
// so 0.0 (a real "any burn" threshold) is distinguishable from unset,
// matching WhenInput.BudgetBandSet.
type WhenBodyV1 struct {
	VerdictAtLeast    string   `json:"verdict_at_least,omitempty"`
	Criterion         string   `json:"criterion,omitempty"`
	SeverityAtLeast   string   `json:"severity_at_least,omitempty"`
	ContentClass      string   `json:"content_class,omitempty"`
	ModelGlob         string   `json:"model_glob,omitempty"`
	Provider          string   `json:"provider,omitempty"`
	User              string   `json:"user,omitempty"`
	UserCohort        string   `json:"user_cohort,omitempty"`
	BudgetBandAtLeast *float64 `json:"budget_band_at_least,omitempty"`
	MinPromptTokens   int      `json:"min_prompt_tokens,omitempty"`
}

// ActionBodyV1 is the nested action half of a rule — exactly one primary
// must be set (lint-enforced by the engine).
type ActionBodyV1 struct {
	RouteToUpstream string `json:"route_to_upstream,omitempty"`
	RouteToModel    string `json:"route_to_model,omitempty"`
	SetEffort       string `json:"set_effort,omitempty"`
	Deny            bool   `json:"deny,omitempty"`
	NoRoute         bool   `json:"no_route,omitempty"`
}

// RuleBodyV1 is one first-match-wins rule on the wire.
type RuleBodyV1 struct {
	Name          string       `json:"name"`
	When          WhenBodyV1   `json:"when,omitempty"`
	Action        ActionBodyV1 `json:"action"`
	OnUnavailable string       `json:"on_unavailable,omitempty"`
	Reason        string       `json:"reason,omitempty"`
	ReasonCode    string       `json:"reason_code,omitempty"`
}

// ToPolicyInput maps the wire body's nested action shape onto the pure
// engine's flat RuleInput — the same mapping
// internal/obs/httpapi/egress_editor.go's egressPolicyInputFromDTO performs
// for the dashboard editor, reused here for the org-wire boundary.
func (b BodyV1) ToPolicyInput() PolicyInput {
	in := PolicyInput{
		Mode:            b.Mode,
		CooldownSeconds: b.CooldownSeconds,
		Cohorts:         b.Cohorts,
	}
	for _, t := range b.Targets {
		in.Targets = append(in.Targets, TargetInput{ID: t.ID, URL: t.URL, Shape: t.Shape})
	}
	for _, r := range b.Rules {
		ri := RuleInput{
			Name:            r.Name,
			RouteToUpstream: r.Action.RouteToUpstream,
			RouteToModel:    r.Action.RouteToModel,
			SetEffort:       r.Action.SetEffort,
			Deny:            r.Action.Deny,
			NoRoute:         r.Action.NoRoute,
			Reason:          r.Reason,
			ReasonCode:      r.ReasonCode,
			OnUnavailable:   r.OnUnavailable,
			When: WhenInput{
				VerdictAtLeast:  r.When.VerdictAtLeast,
				Criterion:       r.When.Criterion,
				SeverityAtLeast: r.When.SeverityAtLeast,
				ContentClass:    r.When.ContentClass,
				ModelGlob:       r.When.ModelGlob,
				Provider:        r.When.Provider,
				User:            r.When.User,
				UserCohort:      r.When.UserCohort,
				MinPromptTokens: r.When.MinPromptTokens,
			},
		}
		if r.When.BudgetBandAtLeast != nil {
			ri.When.BudgetBandAtLeast = *r.When.BudgetBandAtLeast
			ri.When.BudgetBandSet = true
		}
		in.Rules = append(in.Rules, ri)
	}
	return in
}

// BodyV1FromPolicyInput is the inverse of ToPolicyInput, letting a server GET
// surface reconstruct the wire (nested-action) shape from the flat engine
// input.
func BodyV1FromPolicyInput(in PolicyInput) BodyV1 {
	b := BodyV1{Mode: in.Mode, CooldownSeconds: in.CooldownSeconds, Cohorts: in.Cohorts}
	for _, t := range in.Targets {
		b.Targets = append(b.Targets, TargetBodyV1{ID: t.ID, URL: t.URL, Shape: t.Shape})
	}
	for _, r := range in.Rules {
		rb := RuleBodyV1{
			Name:          r.Name,
			OnUnavailable: r.OnUnavailable,
			Reason:        r.Reason,
			ReasonCode:    r.ReasonCode,
			Action: ActionBodyV1{
				RouteToUpstream: r.RouteToUpstream,
				RouteToModel:    r.RouteToModel,
				SetEffort:       r.SetEffort,
				Deny:            r.Deny,
				NoRoute:         r.NoRoute,
			},
			When: WhenBodyV1{
				VerdictAtLeast:  r.When.VerdictAtLeast,
				Criterion:       r.When.Criterion,
				SeverityAtLeast: r.When.SeverityAtLeast,
				ContentClass:    r.When.ContentClass,
				ModelGlob:       r.When.ModelGlob,
				Provider:        r.When.Provider,
				User:            r.When.User,
				UserCohort:      r.When.UserCohort,
				MinPromptTokens: r.When.MinPromptTokens,
			},
		}
		if r.When.BudgetBandSet {
			band := r.When.BudgetBandAtLeast
			rb.When.BudgetBandAtLeast = &band
		}
		b.Rules = append(b.Rules, rb)
	}
	return b
}

// DecodeBody strictly decodes raw org-wire JSON bytes into a BodyV1: unknown
// fields are rejected (DisallowUnknownFields), the document must not exceed
// maxBytes, and any byte after the JSON value is rejected. Mirrors
// policyfam/admission.DecodeBody's discipline (see its doc comment).
func DecodeBody(raw []byte, maxBytes int64) (BodyV1, error) {
	if maxBytes <= 0 {
		return BodyV1{}, fmt.Errorf("policyfam/egress.DecodeBody: cap must be positive, got %d", maxBytes)
	}
	if int64(len(raw)) > maxBytes {
		return BodyV1{}, fmt.Errorf("policyfam/egress.DecodeBody: body is %d bytes, exceeds the %d-byte cap", len(raw), maxBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var body BodyV1
	if err := dec.Decode(&body); err != nil {
		return BodyV1{}, fmt.Errorf("policyfam/egress.DecodeBody: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return BodyV1{}, fmt.Errorf("policyfam/egress.DecodeBody: trailing bytes after the document")
	}
	return body, nil
}

// CanonicalJSON re-encodes a decoded BodyV1 deterministically (see
// policyfam/admission.CanonicalJSON's doc comment for why this, not the raw
// submitted bytes, is what BodyHash is computed over).
func CanonicalJSON(b BodyV1) ([]byte, error) {
	out, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("policyfam/egress.CanonicalJSON: %w", err)
	}
	return out, nil
}

// CompileBody decodes and canonicalizes raw org-wire JSON, then compiles it
// into a ready PolicySpec, returning the canonical JSON bytes the caller
// should sign/hash as the resource's Body.
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
		return PolicySpec{}, nil, fmt.Errorf("policyfam/egress.CompileBody: %w", err)
	}
	return spec, canon, nil
}
