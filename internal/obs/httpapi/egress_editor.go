// egress_editor.go adds the WRITE half of the Plane-A egress-routing surface:
// a lint-gated policy editor symmetric to the admission editor (admission.go).
// The admin authors typed targets + first-match routing rules in the dashboard
// and applies them live; the compile/lint stay in the pure internal/obs/egress
// engine, this handler only maps the wire DTO, gates on a fatal lint, and
// hot-swaps via SetEgressPolicy.
//
// IMPORTANT (honest semantics): SetEgressPolicy hot-swaps THIS service
// instance's egress policy in memory. Egress only takes real effect on the
// PROXY request path (only the proxy can reroute an upstream), and the proxy
// holds a SEPARATE AdmissionService instance — so a live apply here updates the
// egress STATUS/preview immediately but the proxy enforces the new routing only
// after a persist (?persist=1) + daemon restart. The editor UI says so.
package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"sort"

	"github.com/marmutapp/superbased-observer/internal/obs/egress"
)

// SetEgressPersister injects the opt-in write-through persistence seam used by
// POST /api/obs/egress/policy?persist=1. Symmetric to SetPolicyPersister; safe
// to leave unset (persistence reported unavailable, editor stays in-memory).
func (a *API) SetEgressPersister(fn PolicyPersistFunc) { a.persistEgress = fn }

// egressWhenDTO is one matcher set in the editor wire shape. budget_band_at_least
// is a pointer so 0.0 (a real "any burn" threshold) is distinguishable from
// unset, matching egress.WhenInput.BudgetBandSet.
type egressWhenDTO struct {
	VerdictAtLeast    string   `json:"verdict_at_least"`
	Criterion         string   `json:"criterion"`
	SeverityAtLeast   string   `json:"severity_at_least"`
	ContentClass      string   `json:"content_class"`
	ModelGlob         string   `json:"model_glob"`
	Provider          string   `json:"provider"`
	User              string   `json:"user"`
	UserCohort        string   `json:"user_cohort"`
	BudgetBandAtLeast *float64 `json:"budget_band_at_least"`
	MinPromptTokens   int      `json:"min_prompt_tokens"`
}

// egressActionDTO is the action half of a rule — exactly one primary must be set
// (lint-enforced by the engine).
type egressActionDTO struct {
	RouteToUpstream string `json:"route_to_upstream"`
	RouteToModel    string `json:"route_to_model"`
	SetEffort       string `json:"set_effort"`
	Deny            bool   `json:"deny"`
	NoRoute         bool   `json:"no_route"`
}

// egressRuleDTO is one first-match-wins rule in the editor wire shape.
type egressRuleDTO struct {
	Name          string          `json:"name"`
	When          egressWhenDTO   `json:"when"`
	Action        egressActionDTO `json:"action"`
	OnUnavailable string          `json:"on_unavailable"`
	Reason        string          `json:"reason"`
	ReasonCode    string          `json:"reason_code"`
}

// egressTargetDTO is one typed upstream target.
type egressTargetDTO struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Shape string `json:"shape"`
}

// egressPolicyDTO is the full editable egress policy (mirrors egress.PolicyInput
// with snake_case JSON). The editor GETs it, the admin edits it, POSTs it back.
type egressPolicyDTO struct {
	Mode            string            `json:"mode"`
	CooldownSeconds int               `json:"cooldown_seconds"`
	Targets         []egressTargetDTO `json:"targets"`
	Rules           []egressRuleDTO   `json:"rules"`
	Cohorts         map[string]string `json:"cohorts"`
}

// egressLintIssueDTO is one egress Lint finding surfaced to the editor.
type egressLintIssueDTO struct {
	RuleName string `json:"rule_name,omitempty"`
	Message  string `json:"message"`
	Fatal    bool   `json:"fatal"`
}

// egressPolicyGetResponse is GET /api/obs/egress/policy: the current live egress
// policy in editable form so the editor can prefill.
type egressPolicyGetResponse struct {
	Enabled bool            `json:"enabled"`
	Mode    string          `json:"mode"`
	Hash    string          `json:"policy_hash,omitempty"`
	Policy  egressPolicyDTO `json:"policy"`
}

// egressPolicyApplyResponse mirrors admissionPolicyApplyResponse: applied is the
// in-memory hot-swap, persisted the durable config write, reported independently.
type egressPolicyApplyResponse struct {
	Applied      bool                 `json:"applied"`
	Persisted    bool                 `json:"persisted"`
	PersistError string               `json:"persist_error,omitempty"`
	PolicyHash   string               `json:"policy_hash,omitempty"`
	Error        string               `json:"error,omitempty"`
	Issues       []egressLintIssueDTO `json:"issues"`
}

// handleEgressGetPolicy returns the current live egress policy in editable form.
func (a *API) handleEgressGetPolicy(w http.ResponseWriter, _ *http.Request) {
	if a.admission == nil {
		http.Error(w, "admission not enabled", http.StatusNotFound)
		return
	}
	resp := egressPolicyGetResponse{Mode: egress.ModeOff, Policy: egressPolicyDTO{
		Targets: []egressTargetDTO{}, Rules: []egressRuleDTO{}, Cohorts: map[string]string{},
	}}
	if spec, ok := a.admission.EgressPolicy(); ok {
		resp.Mode = spec.Mode
		resp.Enabled = spec.Mode != egress.ModeOff
		resp.Hash = spec.Hash
		resp.Policy = egressPolicyDTOFromSpec(spec)
	}
	a.writeJSON(w, resp)
}

// handleEgressSetPolicy lints + compiles a posted egress policy and, only if no
// fatal lint issue and compile succeeds, hot-swaps it via SetEgressPolicy. A
// fatal lint or compile error is a 422 with the issues (the live policy is
// untouched — and nothing is persisted); a good policy is a 200 with the new
// hash + any warnings. Persistence is opt-in via ?persist=1 (see wantsPersist).
func (a *API) handleEgressSetPolicy(w http.ResponseWriter, r *http.Request) {
	if a.admission == nil {
		http.Error(w, "admission not enabled", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	var dto egressPolicyDTO
	if err := json.Unmarshal(body, &dto); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	in := egressPolicyInputFromDTO(dto)
	resp := egressPolicyApplyResponse{Issues: egressIssuesToDTO(egress.Lint(in))}
	if egress.HasFatal(egress.Lint(in)) {
		a.writeJSONStatus(w, http.StatusUnprocessableEntity, resp)
		return
	}
	spec, err := egress.Compile(in)
	if err != nil {
		resp.Error = err.Error()
		a.writeJSONStatus(w, http.StatusUnprocessableEntity, resp)
		return
	}
	a.admission.SetEgressPolicy(spec)
	resp.Applied = true
	resp.PolicyHash = spec.Hash
	if wantsPersist(r) {
		a.persistEgressJSON(r.Context(), body, &resp)
	}
	a.writeJSON(w, resp)
}

// persistEgressJSON attempts the opt-in write-through of an already-applied
// egress policy, folding the outcome into resp (never promoted to an HTTP error
// — the in-memory swap already succeeded).
func (a *API) persistEgressJSON(ctx context.Context, policyJSON []byte, resp *egressPolicyApplyResponse) {
	if a.persistEgress == nil {
		resp.PersistError = "persistence not available on this node (policy applied in memory only)"
		return
	}
	if err := a.persistEgress(ctx, policyJSON); err != nil {
		resp.PersistError = err.Error()
		return
	}
	resp.Persisted = true
}

// egressPolicyDTOFromSpec reconstructs the editable wire shape from a compiled
// spec so the editor can prefill the current live policy.
func egressPolicyDTOFromSpec(spec egress.PolicySpec) egressPolicyDTO {
	dto := egressPolicyDTO{
		Mode:            spec.Mode,
		CooldownSeconds: spec.CooldownSeconds,
		Targets:         make([]egressTargetDTO, 0, len(spec.Targets)),
		Rules:           make([]egressRuleDTO, 0, len(spec.Rules)),
		Cohorts:         map[string]string{},
	}
	for _, t := range spec.Targets {
		dto.Targets = append(dto.Targets, egressTargetDTO{ID: t.ID, URL: t.URL, Shape: string(t.Shape)})
	}
	sort.Slice(dto.Targets, func(i, j int) bool { return dto.Targets[i].ID < dto.Targets[j].ID })
	maps.Copy(dto.Cohorts, spec.Cohorts)
	for _, cr := range spec.Rules {
		dto.Rules = append(dto.Rules, egressRuleDTO{
			Name:          cr.Name,
			When:          egressWhenDTOFromCompiled(cr.When),
			Action:        egressActionDTOFromCompiled(cr),
			OnUnavailable: cr.OnUnavailable,
			Reason:        cr.Reason,
			ReasonCode:    string(cr.ReasonCode),
		})
	}
	return dto
}

func egressWhenDTOFromCompiled(w egress.CompiledWhen) egressWhenDTO {
	dto := egressWhenDTO{
		VerdictAtLeast:  w.VerdictAtLeast,
		Criterion:       w.Criterion,
		SeverityAtLeast: w.SeverityAtLeast,
		ContentClass:    w.ContentClass,
		ModelGlob:       w.ModelGlob,
		Provider:        w.Provider,
		User:            w.User,
		UserCohort:      w.UserCohort,
		MinPromptTokens: w.MinPromptTokens,
	}
	if w.BudgetBandSet {
		band := w.BudgetBandAtLeast
		dto.BudgetBandAtLeast = &band
	}
	return dto
}

func egressActionDTOFromCompiled(cr egress.CompiledRule) egressActionDTO {
	switch cr.Action {
	case egress.ActionRouteUpstream:
		return egressActionDTO{RouteToUpstream: cr.UpstreamID}
	case egress.ActionRouteModel:
		return egressActionDTO{RouteToModel: cr.Model}
	case egress.ActionSetEffort:
		return egressActionDTO{SetEffort: string(cr.Effort)}
	case egress.ActionDeny:
		return egressActionDTO{Deny: true}
	default:
		return egressActionDTO{NoRoute: true}
	}
}

// egressPolicyInputFromDTO maps the editor wire shape onto the pure engine input.
func egressPolicyInputFromDTO(dto egressPolicyDTO) egress.PolicyInput {
	in := egress.PolicyInput{
		Mode:            dto.Mode,
		CooldownSeconds: dto.CooldownSeconds,
		Cohorts:         dto.Cohorts,
	}
	for _, t := range dto.Targets {
		in.Targets = append(in.Targets, egress.TargetInput{ID: t.ID, URL: t.URL, Shape: t.Shape})
	}
	for _, r := range dto.Rules {
		ri := egress.RuleInput{
			Name:            r.Name,
			RouteToUpstream: r.Action.RouteToUpstream,
			RouteToModel:    r.Action.RouteToModel,
			SetEffort:       r.Action.SetEffort,
			Deny:            r.Action.Deny,
			NoRoute:         r.Action.NoRoute,
			Reason:          r.Reason,
			ReasonCode:      r.ReasonCode,
			OnUnavailable:   r.OnUnavailable,
			When: egress.WhenInput{
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

func egressIssuesToDTO(issues []egress.Issue) []egressLintIssueDTO {
	out := make([]egressLintIssueDTO, 0, len(issues))
	for _, is := range issues {
		out = append(out, egressLintIssueDTO{RuleName: is.RuleName, Message: is.Message, Fatal: is.Fatal})
	}
	return out
}
