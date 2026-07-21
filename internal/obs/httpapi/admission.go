// admission.go serves the obs input-admission surfaces (admission spec §8):
// the SDK/proxy front-door check, the status probe, and the verdict timeline.
// All routes register through the same ExtraRoutes seam as the trajectory
// endpoints (decision D4) — this package never imports the dashboard.
package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/obs"
	"github.com/marmutapp/superbased-observer/internal/obs/admission"
	obsstore "github.com/marmutapp/superbased-observer/internal/obs/store"
)

// PolicyPersistFunc write-through-persists an applied admission policy to the
// node's [observability.admission] config so a daemon restart keeps the
// admin's edit (gap-audit item #11). policyJSON is the raw editor POST body
// (the admissionPolicyDTO wire shape); the seam takes bytes — not an
// internal/obs type — so the wiring point in cmd/observer can implement it
// WITHOUT importing internal/obs (the reverse-import separability boundary,
// tests/invariant/obs_boundary_test.go). A nil func (the default) means
// persistence is unavailable: a persist-requested apply still hot-swaps in
// memory and honestly reports "persisted": false.
type PolicyPersistFunc func(ctx context.Context, policyJSON []byte) error

// SetPolicyPersister injects the opt-in write-through persistence seam used by
// POST /api/obs/admission/policy?persist=1. Called once at wiring time; safe
// to leave unset, in which case persistence is reported unavailable and the
// editor stays in-memory-only (the pre-existing behavior).
func (a *API) SetPolicyPersister(fn PolicyPersistFunc) { a.persistPolicy = fn }

// admissionCheckRequest is the SDK admit() / proxy body.
type admissionCheckRequest struct {
	Text      string `json:"text"`
	Tenant    string `json:"tenant"`
	User      string `json:"user"`
	Session   string `json:"session"`
	TraceID   string `json:"trace_id"`
	RequestID string `json:"request_id"`
}

// handleAdmissionCheck evaluates one incoming request and records the shadow
// verdict. P1 is observe-only: the response always allows (admissionsvc.Check
// clamps to observe), while EnforceDecision previews what enforce would do.
func (a *API) handleAdmissionCheck(w http.ResponseWriter, r *http.Request) {
	if a.admission == nil {
		http.Error(w, "admission not enabled", http.StatusNotFound)
		return
	}
	var req admissionCheckRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	resp := a.admission.Check(r.Context(), obs.AdmissionCheck{
		Text:      req.Text,
		Tenant:    req.Tenant,
		User:      req.User,
		Session:   req.Session,
		TraceID:   req.TraceID,
		RequestID: req.RequestID,
		Persist:   true,
	})
	a.writeJSON(w, resp)
}

// admissionStatusResponse is the /api/obs/admission/status payload.
type admissionStatusResponse struct {
	Enabled       bool           `json:"enabled"`
	Mode          string         `json:"mode"`
	JudgeHosting  string         `json:"judge_hosting"`
	CriteriaCount int            `json:"criteria_count"`
	PolicyHash    string         `json:"policy_hash,omitempty"`
	Decisions24h  map[string]int `json:"decisions_24h"`
	Chain         chainStatus    `json:"chain"`
}

type chainStatus struct {
	Rows int  `json:"rows"`
	OK   bool `json:"ok"`
}

// handleAdmissionStatus reports posture, 24h verdict counts, and a chain
// quick-check (admission spec §8 `admission status`).
func (a *API) handleAdmissionStatus(w http.ResponseWriter, r *http.Request) {
	resp := admissionStatusResponse{Decisions24h: map[string]int{}, Mode: "off"}
	if a.admission == nil {
		a.writeJSON(w, resp)
		return
	}
	resp.Enabled = a.admission.Enabled()
	resp.Mode = a.admission.Mode()
	resp.JudgeHosting = a.admission.JudgeHosting()
	if p, ok := a.admission.Policy(); ok {
		resp.CriteriaCount = len(p.Criteria)
		resp.PolicyHash = p.Hash
	}
	if a.store != nil {
		if counts, err := a.store.AdmissionDecisionCounts(r.Context(), time.Now().Add(-24*time.Hour)); err == nil {
			resp.Decisions24h = counts
		}
		if cr, err := a.store.VerifyAdmissionChain(r.Context()); err == nil {
			resp.Chain = chainStatus{Rows: cr.Rows, OK: cr.OK}
		}
	}
	a.writeJSON(w, resp)
}

// admissionBudgetResponse is the /api/obs/admission/budget payload: the
// per-end-user budget caps + the 24h would-block tally + the top spenders.
type admissionBudgetResponse struct {
	Enabled     bool                    `json:"enabled"`
	FiveHourUSD float64                 `json:"five_hour_usd"`
	WeeklyUSD   float64                 `json:"weekly_usd"`
	MonthlyUSD  float64                 `json:"monthly_usd"`
	Breaches24h int                     `json:"breaches_24h"`
	TopSpenders []obsstore.UserSpendRow `json:"top_spenders"`
}

// handleAdmissionBudget reports the per-end-user budget guardrail state
// (docs/guardrails.md): configured caps, 24h breaches, and top spenders per
// window. Read-only; node-local; metadata only (cost + user, never content).
func (a *API) handleAdmissionBudget(w http.ResponseWriter, r *http.Request) {
	resp := admissionBudgetResponse{TopSpenders: []obsstore.UserSpendRow{}}
	if a.admission != nil {
		resp.Enabled, resp.FiveHourUSD, resp.WeeklyUSD, resp.MonthlyUSD = a.admission.BudgetStatus()
	}
	if a.store != nil {
		now := time.Now().UTC()
		if rows, err := a.store.TopUserSpend(r.Context(), now, 20); err == nil && rows != nil {
			resp.TopSpenders = rows
		}
		if n, err := a.store.CountBudgetBreaches(r.Context(), now.Add(-24*time.Hour)); err == nil {
			resp.Breaches24h = n
		}
	}
	a.writeJSON(w, resp)
}

// --- C-P2: lint-gated policy editor (admission spec §6/§7). The admin edits
// the policy that governs their HOSTED LLM APPLICATION's end-user requests and
// applies it live. The compile/lint stay in internal/obs/admission (the pure
// engine); this handler only maps the wire DTO, gates on a fatal lint, and
// hot-reloads via SetPolicy. NOTE: SetPolicy is IN-MEMORY — an applied policy
// is live until the daemon restarts, when [observability.admission] config is
// reloaded. config.toml (or `observer obs admission setup`) is the persistent
// source; the editor UI says so. ---

// admissionCriterionDTO is one criterion in the editor wire shape.
type admissionCriterionDTO struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Name       string   `json:"name"`
	Definition string   `json:"definition"`
	Topics     []string `json:"topics"`
	Decision   string   `json:"decision"`
	Severity   string   `json:"severity"`
}

// admissionPrefilterDTO is the deterministic pre-filter in the editor wire shape.
type admissionPrefilterDTO struct {
	Allow           []string `json:"allow"`
	Deny            []string `json:"deny"`
	MaxMessageBytes int      `json:"max_message_bytes"`
}

// admissionPolicyDTO is the full editable policy (mirrors admission.PolicyInput
// with snake_case JSON). The editor GETs it, the admin edits it, POSTs it back.
type admissionPolicyDTO struct {
	Mode              string                  `json:"mode"`
	Strict            bool                    `json:"strict"`
	Scope             string                  `json:"scope"`
	SecretRemoteJudge string                  `json:"secret_remote_judge"`
	Prefilter         admissionPrefilterDTO   `json:"prefilter"`
	Criteria          []admissionCriterionDTO `json:"criteria"`
}

// lintIssueDTO is one Lint finding surfaced to the editor.
type lintIssueDTO struct {
	CriterionID string `json:"criterion_id,omitempty"`
	Message     string `json:"message"`
	Fatal       bool   `json:"fatal"`
}

// admissionPolicyGetResponse is GET /api/obs/admission/policy: the current
// live policy in editable form, so the editor can prefill.
type admissionPolicyGetResponse struct {
	Enabled bool               `json:"enabled"`
	Mode    string             `json:"mode"`
	Hash    string             `json:"policy_hash,omitempty"`
	Policy  admissionPolicyDTO `json:"policy"`
}

// admissionPolicyApplyResponse is POST /api/obs/admission/policy: whether the
// policy applied, its new hash, any compile error, and the lint issues (both
// warnings on a successful apply and the fatal issues that blocked one).
//
// Persisted/PersistError report the opt-in write-through result (persist=1).
// Applied is the in-memory hot-swap; Persisted is the durable config.toml
// write. They are INDEPENDENT: a successful hot-swap that then fails to
// persist is Applied=true, Persisted=false with PersistError set — a 200 (the
// swap DID happen), never a 5xx that would imply nothing changed. Persisted is
// always emitted (never omitempty) so the caller can always read true/false;
// with no persist flag it is honestly false (nothing was written to disk).
type admissionPolicyApplyResponse struct {
	Applied      bool           `json:"applied"`
	Persisted    bool           `json:"persisted"`
	PersistError string         `json:"persist_error,omitempty"`
	PolicyHash   string         `json:"policy_hash,omitempty"`
	Error        string         `json:"error,omitempty"`
	Issues       []lintIssueDTO `json:"issues"`
}

// handleAdmissionGetPolicy returns the current live policy in editable form.
func (a *API) handleAdmissionGetPolicy(w http.ResponseWriter, _ *http.Request) {
	if a.admission == nil {
		http.Error(w, "admission not enabled", http.StatusNotFound)
		return
	}
	resp := admissionPolicyGetResponse{Enabled: a.admission.Enabled(), Mode: a.admission.Mode()}
	if p, ok := a.admission.Policy(); ok {
		resp.Hash = p.Hash
		resp.Policy = policyDTOFromSpec(p)
	}
	a.writeJSON(w, resp)
}

// handleAdmissionSetPolicy lints + compiles a posted policy and, only if no
// fatal lint issue and compile succeeds, hot-reloads it via SetPolicy. A fatal
// lint or a compile error is a 422 with the issues (the live policy is
// untouched — and NOTHING is persisted); a good policy is a 200 with the new
// hash + any warnings.
//
// Persistence is OPT-IN via the `persist` query parameter (persist=1|true|yes;
// see wantsPersist). Without it the apply is IN-MEMORY ONLY — today's default,
// unchanged — and Persisted is false. With it, after a successful hot-swap the
// raw posted policy JSON is handed to the injected persister so it can write
// through to the node's [observability.admission] config; a persist failure is
// reported as Applied=true, Persisted=false + persist_error on a 200 (the swap
// already happened), never a 5xx.
func (a *API) handleAdmissionSetPolicy(w http.ResponseWriter, r *http.Request) {
	if a.admission == nil {
		http.Error(w, "admission not enabled", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	var dto admissionPolicyDTO
	if err := json.Unmarshal(body, &dto); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	in := policyInputFromDTO(dto)
	resp := admissionPolicyApplyResponse{Issues: lintIssuesToDTO(admission.Lint(in))}
	if anyFatalIssue(resp.Issues) {
		// Fatal lint: never applied, never persisted.
		a.writeJSONStatus(w, http.StatusUnprocessableEntity, resp)
		return
	}
	spec, err := admission.Compile(in)
	if err != nil {
		// Compile error: never applied, never persisted.
		resp.Error = err.Error()
		a.writeJSONStatus(w, http.StatusUnprocessableEntity, resp)
		return
	}
	a.admission.SetPolicy(r.Context(), spec)
	resp.Applied = true
	resp.PolicyHash = spec.Hash
	if wantsPersist(r) {
		a.persistPolicyJSON(r.Context(), body, &resp)
	}
	a.writeJSON(w, resp)
}

// persistPolicyJSON attempts the opt-in write-through of an already-applied
// policy, folding the outcome into resp. A nil seam (persistence not wired on
// this node) or a write error leaves Persisted=false with an explicit
// persist_error — the in-memory swap has already succeeded, so this is never
// promoted to an HTTP error.
func (a *API) persistPolicyJSON(ctx context.Context, policyJSON []byte, resp *admissionPolicyApplyResponse) {
	if a.persistPolicy == nil {
		resp.PersistError = "persistence not available on this node (policy applied in memory only)"
		return
	}
	if err := a.persistPolicy(ctx, policyJSON); err != nil {
		resp.PersistError = err.Error()
		return
	}
	resp.Persisted = true
}

// wantsPersist reports whether the request opted into write-through
// persistence via ?persist=1 (also accepting true/yes/on, case-insensitive).
// Absent or any other value keeps the default in-memory-only behavior.
func wantsPersist(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("persist"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// policyDTOFromSpec reconstructs the editable wire shape from a compiled spec.
// The pre-filter's compiled regexps recover their source via Regexp.String().
func policyDTOFromSpec(p admission.PolicySpec) admissionPolicyDTO {
	dto := admissionPolicyDTO{
		Mode:              p.Mode.String(),
		Strict:            p.Strict,
		Scope:             p.Scope.String(),
		SecretRemoteJudge: p.SecretRemoteJudge.String(),
		Prefilter: admissionPrefilterDTO{
			Allow:           regexpSources(p.Prefilter.Allow),
			Deny:            regexpSources(p.Prefilter.Deny),
			MaxMessageBytes: p.Prefilter.MaxMessageBytes,
		},
		Criteria: make([]admissionCriterionDTO, 0, len(p.Criteria)),
	}
	for _, c := range p.Criteria {
		dto.Criteria = append(dto.Criteria, admissionCriterionDTO{
			ID:         c.ID,
			Type:       string(c.Type),
			Name:       c.Name,
			Definition: c.Definition,
			Topics:     c.Topics,
			Decision:   c.Decision.String(),
			Severity:   c.Severity.String(),
		})
	}
	return dto
}

// policyInputFromDTO maps the editor wire shape onto the pure engine's input.
func policyInputFromDTO(dto admissionPolicyDTO) admission.PolicyInput {
	in := admission.PolicyInput{
		Mode:              dto.Mode,
		Strict:            dto.Strict,
		Scope:             dto.Scope,
		SecretRemoteJudge: dto.SecretRemoteJudge,
		Prefilter: admission.PrefilterInput{
			Allow:           dto.Prefilter.Allow,
			Deny:            dto.Prefilter.Deny,
			MaxMessageBytes: dto.Prefilter.MaxMessageBytes,
		},
	}
	for _, c := range dto.Criteria {
		in.Criteria = append(in.Criteria, admission.CriterionInput{
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

func regexpSources(res []*regexp.Regexp) []string {
	if len(res) == 0 {
		return nil
	}
	out := make([]string, 0, len(res))
	for _, re := range res {
		out = append(out, re.String())
	}
	return out
}

func lintIssuesToDTO(issues []admission.Issue) []lintIssueDTO {
	out := make([]lintIssueDTO, 0, len(issues))
	for _, is := range issues {
		out = append(out, lintIssueDTO{CriterionID: is.CriterionID, Message: is.Message, Fatal: is.Fatal})
	}
	return out
}

func anyFatalIssue(issues []lintIssueDTO) bool {
	for _, is := range issues {
		if is.Fatal {
			return true
		}
	}
	return false
}

// handleAdmissionVerdicts serves the timeline (admission spec §8 dashboard),
// newest-first, filterable by decision / criterion / window.
func (a *API) handleAdmissionVerdicts(w http.ResponseWriter, r *http.Request) {
	if a.store == nil {
		a.writeJSON(w, []struct{}{})
		return
	}
	rows, err := a.store.ListAdmissionEvents(r.Context(), admissionOptsFromQuery(r.URL.Query()))
	if err != nil {
		a.writeErr(w, err)
		return
	}
	if rows == nil {
		rows = []obsstore.AdmissionEventView{}
	}
	a.writeJSON(w, rows)
}

// admissionOptsFromQuery parses the timeline filters (decision, criterion,
// win=<hours>, limit, offset) into store options.
func admissionOptsFromQuery(q url.Values) obsstore.AdmissionListOptions {
	opts := obsstore.AdmissionListOptions{
		Decision:  q.Get("decision"),
		Criterion: q.Get("criterion"),
	}
	if hrs, err := strconv.Atoi(q.Get("win")); err == nil && hrs > 0 {
		opts.Since = time.Now().Add(-time.Duration(hrs) * time.Hour)
	}
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		opts.Limit = n
	}
	if n, err := strconv.Atoi(q.Get("offset")); err == nil && n > 0 {
		opts.Offset = n
	}
	return opts
}
