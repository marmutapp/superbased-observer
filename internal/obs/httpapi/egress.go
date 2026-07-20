package httpapi

import (
	"net/http"
	"sort"
	"time"

	"github.com/marmutapp/superbased-observer/internal/obs/egress"
	obsstore "github.com/marmutapp/superbased-observer/internal/obs/store"
)

// egress.go serves the read-only Plane-A egress-routing surface (G22): the
// installed policy posture + the obs_egress_decisions audit timeline with the
// realized outcome the proxy reported back. Both endpoints render VERBATIM
// store/policy values — no re-derivation, no smoothing. The data is NODE-LOCAL
// (design §8: no org tier), so this node surface — like the `observer obs
// egress` CLI — is the only place the audit log is viewable at all.

// egressTargetView is one typed [[observability.egress.targets]] entry from
// the INSTALLED (compiled) policy. The URL is operator-authored config served
// back to the operator's own loopback dashboard — not content.
type egressTargetView struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Shape string `json:"shape"`
}

// egressRuleView is one compiled rule, summarized to its primary action.
type egressRuleView struct {
	Name          string `json:"name"`
	Action        string `json:"action"`
	Target        string `json:"target,omitempty"` // upstream id / model / effort per action
	ReasonCode    string `json:"reason_code"`
	OnUnavailable string `json:"on_unavailable"`
	// Pinned mirrors the evaluator's MustUseTarget derivation: a
	// route_to_upstream rule with on_unavailable=deny is proxy-pinned
	// (fails CLOSED when the target is unavailable at runtime).
	Pinned bool `json:"pinned"`
}

// egressChainView is the hash-chain verify result for the audit log.
type egressChainView struct {
	Rows   int    `json:"rows"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// egressStatusResponse is GET /api/obs/egress/status: the installed policy
// posture. enabled=false + mode "off" when no egress policy is installed
// (egress disabled, compile-failed, or admission itself not wired — egress
// composes on the admission verdict).
type egressStatusResponse struct {
	Enabled           bool               `json:"enabled"`
	Mode              string             `json:"mode"`
	PolicyHash        string             `json:"policy_hash,omitempty"`
	Rules             []egressRuleView   `json:"rules"`
	Targets           []egressTargetView `json:"targets"`
	DecisionsByAction map[string]int     `json:"decisions_by_action"`
	Decisions24h      int                `json:"decisions_24h"`
	Chain             egressChainView    `json:"chain"`
}

// handleEgressStatus reports the installed egress policy (mode / rules /
// typed targets / policy hash), the by-action decision counts, and the
// tamper-evidence chain verify — the node-dashboard peer of `observer obs
// egress`. Read-only.
func (a *API) handleEgressStatus(w http.ResponseWriter, r *http.Request) {
	resp := egressStatusResponse{
		Mode:              egress.ModeOff,
		Rules:             []egressRuleView{},
		Targets:           []egressTargetView{},
		DecisionsByAction: map[string]int{},
		Chain:             egressChainView{OK: true},
	}
	if a.admission != nil {
		if spec, ok := a.admission.EgressPolicy(); ok {
			resp.Mode = spec.Mode
			resp.Enabled = spec.Mode != egress.ModeOff
			resp.PolicyHash = spec.Hash
			for _, cr := range spec.Rules {
				resp.Rules = append(resp.Rules, ruleView(cr))
			}
			for _, t := range spec.Targets {
				resp.Targets = append(resp.Targets, egressTargetView{ID: t.ID, URL: t.URL, Shape: string(t.Shape)})
			}
			sort.Slice(resp.Targets, func(i, j int) bool { return resp.Targets[i].ID < resp.Targets[j].ID })
		}
	}
	if a.store != nil {
		if counts, err := a.store.EgressActionCounts(r.Context(), time.Time{}); err == nil && counts != nil {
			resp.DecisionsByAction = counts
		}
		if counts24, err := a.store.EgressActionCounts(r.Context(), time.Now().Add(-24*time.Hour)); err == nil {
			for _, n := range counts24 {
				resp.Decisions24h += n
			}
		}
		if cr, err := a.store.VerifyEgressChain(r.Context()); err == nil {
			resp.Chain = egressChainView{Rows: cr.Rows, OK: cr.OK, Detail: cr.Detail}
		}
	}
	a.writeJSON(w, resp)
}

// ruleView summarizes one compiled rule to its primary action + fail posture.
func ruleView(cr egress.CompiledRule) egressRuleView {
	v := egressRuleView{
		Name:          cr.Name,
		Action:        string(cr.Action),
		ReasonCode:    string(cr.ReasonCode),
		OnUnavailable: cr.OnUnavailable,
	}
	switch cr.Action {
	case egress.ActionRouteUpstream:
		v.Target = cr.UpstreamID
		v.Pinned = cr.OnUnavailable == egress.OnUnavailableDeny
	case egress.ActionRouteModel:
		v.Target = cr.Model
	case egress.ActionSetEffort:
		v.Target = string(cr.Effort)
	}
	return v
}

// egressDecisionsResponse is GET /api/obs/egress/decisions: the audit
// timeline, newest first, VERBATIM store rows (decision half + the realized
// outcome the proxy reported back). decisions is never null.
type egressDecisionsResponse struct {
	Decisions []obsstore.EgressDecisionView `json:"decisions"`
}

// handleEgressDecisions lists recorded egress decisions (newest first,
// ?limit= capped by the store). Raw request text is never on these rows —
// only operator-config values, enums, and hashes (+ the end-user id, which is
// node-local by design).
func (a *API) handleEgressDecisions(w http.ResponseWriter, r *http.Request) {
	resp := egressDecisionsResponse{Decisions: []obsstore.EgressDecisionView{}}
	if a.store != nil {
		rows, err := a.store.ListEgressDecisions(r.Context(), intParam(r, "limit", 100))
		if err != nil {
			a.writeErr(w, err)
			return
		}
		if rows != nil {
			resp.Decisions = rows
		}
	}
	a.writeJSON(w, resp)
}
