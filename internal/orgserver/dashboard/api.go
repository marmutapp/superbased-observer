package dashboard

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgserver/auth"
	gen "github.com/marmutapp/superbased-observer/internal/orgserver/dashboard/gen"
	"github.com/marmutapp/superbased-observer/internal/orgserver/obsalert"
	"github.com/marmutapp/superbased-observer/internal/orgserver/rollup"
)

// API implements the generated dashboard ServerInterface (the /api/org/* data
// endpoints). The compile-time assertion is the routing-conformance gate: any
// drift between the OpenAPI spec and these handlers fails the build.
var _ gen.ServerInterface = (*API)(nil)

// maxBodyBytes caps request bodies for the small JSON inputs these endpoints
// accept.
const maxBodyBytes = 1 << 20

// API serves the org dashboard data endpoints. Authentication (a valid SAML
// session) is enforced by middleware before any method runs; role scoping
// (admin sees all, lead sees only their teams, plus the §14.5 guard roles —
// see Options) is enforced HERE, per request, against the resolved caller —
// defence in depth: the rollup queries trust their Scope, the handlers do
// not trust the URL.
type API struct {
	db           *sql.DB
	cache        *rollup.Cache
	adminEmails  map[string]bool
	policyAdmins map[string]bool
	secViewers   map[string]bool
	policySigner PolicySigner
	logger       *slog.Logger
	now          func() time.Time
}

// PolicySigner loads the org policy signing key for one dashboard publish.
// nil means the policy channel is off ([policy].signing_key_path unset):
// publish returns 409 and the authoring panel renders read-only. It is
// called PER PUBLISH and the returned key is dropped immediately — no
// private-key material is retained between requests (the G14 key-exposure
// design call; see config.PolicyConfig).
type PolicySigner func() (ed25519.PrivateKey, error)

// Options configures NewAPI. The three email lists are case-insensitive
// allow-lists from [dashboard] config: AdminEmails is the existing org-admin
// bootstrap; PolicyAdminEmails and SecurityViewerEmails are the §14.5 guard
// roles (an admin implicitly holds both — see config.DashboardConfig).
type Options struct {
	AdminEmails          []string
	PolicyAdminEmails    []string
	SecurityViewerEmails []string
	PolicySigner         PolicySigner
}

// NewAPI constructs the dashboard API over the server DB.
func NewAPI(db *sql.DB, cache *rollup.Cache, opts Options, logger *slog.Logger) *API {
	if logger == nil {
		logger = slog.Default()
	}
	return &API{
		db: db, cache: cache,
		adminEmails:  emailSet(opts.AdminEmails),
		policyAdmins: emailSet(opts.PolicyAdminEmails),
		secViewers:   emailSet(opts.SecurityViewerEmails),
		policySigner: opts.PolicySigner,
		logger:       logger,
		now:          func() time.Time { return time.Now().UTC() },
	}
}

// emailSet normalises an email allow-list into a lower-cased lookup set.
func emailSet(emails []string) map[string]bool {
	set := make(map[string]bool, len(emails))
	for _, e := range emails {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			set[e] = true
		}
	}
	return set
}

// caller resolves the SAML-authenticated user id and its rollup Scope. On any
// failure it writes the response and returns ok=false, so handlers can
// `if !ok { return }`.
func (a *API) caller(w http.ResponseWriter, r *http.Request) (userID string, scope rollup.Scope, ok bool) {
	id, present := auth.UserIDFromContext(r.Context())
	if !present {
		auth.WriteError(w, http.StatusUnauthorized, "unauthorized", "login required")
		return "", rollup.Scope{}, false
	}
	sc, err := a.resolveScope(r.Context(), id)
	if err != nil {
		a.fail(w, "resolve scope", err)
		return "", rollup.Scope{}, false
	}
	return id, sc, true
}

// resolveScope maps a user to their authority: an admin (email in the config
// allow-list) sees the whole org; otherwise the caller is scoped to the teams
// they lead (possibly none).
func (a *API) resolveScope(ctx context.Context, userID string) (rollup.Scope, error) {
	email, found, err := a.memberEmail(ctx, userID)
	if err != nil || !found {
		return rollup.Scope{}, err
	}
	if a.adminEmails[strings.ToLower(email)] {
		return rollup.Scope{Admin: true}, nil
	}
	led, err := a.leadTeams(ctx, userID)
	if err != nil {
		return rollup.Scope{}, err
	}
	return rollup.Scope{TeamIDs: led}, nil
}

// memberEmail resolves a user id to its SCIM-provisioned email. found is
// false for an unknown user (a valid session whose user was deprovisioned).
func (a *API) memberEmail(ctx context.Context, userID string) (email string, found bool, err error) {
	err = a.db.QueryRowContext(ctx, `SELECT email FROM org_members WHERE user_id = ?`, userID).Scan(&email)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return email, true, nil
}

// leadTeams returns the team ids the user leads (possibly none).
func (a *API) leadTeams(ctx context.Context, userID string) ([]string, error) {
	rows, err := a.db.QueryContext(ctx,
		`SELECT team_id FROM org_team_members WHERE user_id = ? AND role = 'lead' ORDER BY team_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var led []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		led = append(led, t)
	}
	return led, rows.Err()
}

// canSeeTeam reports whether scope may view team id (admin → any; lead → only
// teams they lead).
func canSeeTeam(scope rollup.Scope, id string) bool {
	return scope.Admin || slices.Contains(scope.TeamIDs, id)
}

// --- Rollup reads ----------------------------------------------------------

// OrgOverview implements GET /api/org/overview.
func (a *API) OrgOverview(w http.ResponseWriter, r *http.Request, params gen.OrgOverviewParams) {
	_, scope, ok := a.caller(w, r)
	if !ok {
		return
	}
	win := windowOf(params.Days)
	res, err := rollup.Cached(a.cache, rollup.CacheKey("overview", scope, win), rollup.TTLOverview,
		func() (rollup.OverviewResult, error) { return rollup.Overview(r.Context(), a.db, win, scope, a.now()) })
	if err != nil {
		a.fail(w, "overview", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgTeams implements GET /api/org/teams.
func (a *API) OrgTeams(w http.ResponseWriter, r *http.Request, params gen.OrgTeamsParams) {
	_, scope, ok := a.caller(w, r)
	if !ok {
		return
	}
	win := windowOf(params.Days)
	res, err := rollup.Cached(a.cache, rollup.CacheKey("teams", scope, win), rollup.TTLTeam,
		func() (rollup.TeamsResult, error) { return rollup.Teams(r.Context(), a.db, win, scope, a.now()) })
	if err != nil {
		a.fail(w, "teams", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgTeamDetail implements GET /api/org/teams/{id}.
func (a *API) OrgTeamDetail(w http.ResponseWriter, r *http.Request, id string, params gen.OrgTeamDetailParams) {
	_, scope, ok := a.caller(w, r)
	if !ok {
		return
	}
	if !canSeeTeam(scope, id) {
		auth.WriteError(w, http.StatusForbidden, "forbidden", "not authorised for this team")
		return
	}
	win := windowOf(params.Days)
	res, found, err := teamDetailCached(a, r.Context(), win, id)
	if err != nil {
		a.fail(w, "team detail", err)
		return
	}
	if !found {
		auth.WriteError(w, http.StatusNotFound, "not_found", "no such team")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgTeamDevelopers implements GET /api/org/teams/{id}/developers — the audited
// per-developer drill-down. The audit row is written BEFORE the data is
// fetched, so the disclosure can never precede its record.
func (a *API) OrgTeamDevelopers(w http.ResponseWriter, r *http.Request, id string, params gen.OrgTeamDevelopersParams) {
	userID, scope, ok := a.caller(w, r)
	if !ok {
		return
	}
	if !canSeeTeam(scope, id) {
		auth.WriteError(w, http.StatusForbidden, "forbidden", "not authorised for this team")
		return
	}
	// Audit FIRST. If the audit write fails, refuse the disclosure.
	if err := rollup.WriteAudit(r.Context(), a.db, userID, rollup.ActionViewDevelopers, id, "", sourceIP(r), a.now()); err != nil {
		a.fail(w, "audit developers", err)
		return
	}
	res, found, err := rollup.Developers(r.Context(), a.db, windowOf(params.Days), id, a.now())
	if err != nil {
		a.fail(w, "developers", err)
		return
	}
	if !found {
		auth.WriteError(w, http.StatusNotFound, "not_found", "no such team")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgPeople implements GET /api/org/people — the AUDITED org-wide
// per-developer leaderboard. Like OrgTeamDevelopers, the audit row is written
// BEFORE the data is fetched, so the disclosure can never precede its record.
// The caller's user id doubles as the self-scope for a plain member.
func (a *API) OrgPeople(w http.ResponseWriter, r *http.Request, params gen.OrgPeopleParams) {
	userID, scope, ok := a.caller(w, r)
	if !ok {
		return
	}
	if err := rollup.WriteAudit(r.Context(), a.db, userID, rollup.ActionViewPeople, "", "", sourceIP(r), a.now()); err != nil {
		a.fail(w, "audit people", err)
		return
	}
	if params.Tool != nil {
		scope.Filters.Tool = *params.Tool
	}
	res, err := rollup.People(r.Context(), a.db, windowOf(params.Days), scope, userID, a.now())
	if err != nil {
		a.fail(w, "people", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgTools implements GET /api/org/tools.
func (a *API) OrgTools(w http.ResponseWriter, r *http.Request, params gen.OrgToolsParams) {
	_, scope, ok := a.caller(w, r)
	if !ok {
		return
	}
	win := windowOf(params.Days)
	res, err := rollup.Cached(a.cache, rollup.CacheKey("tools", scope, win), rollup.TTLOverview,
		func() (rollup.ToolsResult, error) { return rollup.Tools(r.Context(), a.db, win, scope, a.now()) })
	if err != nil {
		a.fail(w, "tools", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgModels implements GET /api/org/models.
func (a *API) OrgModels(w http.ResponseWriter, r *http.Request, params gen.OrgModelsParams) {
	_, scope, ok := a.caller(w, r)
	if !ok {
		return
	}
	win := windowOf(params.Days)
	res, err := rollup.Cached(a.cache, rollup.CacheKey("models", scope, win), rollup.TTLOverview,
		func() (rollup.ModelsResult, error) { return rollup.Models(r.Context(), a.db, win, scope, a.now()) })
	if err != nil {
		a.fail(w, "models", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgActivity implements GET /api/org/activity.
func (a *API) OrgActivity(w http.ResponseWriter, r *http.Request, params gen.OrgActivityParams) {
	_, scope, ok := a.caller(w, r)
	if !ok {
		return
	}
	if params.Tool != nil {
		scope.Filters.Tool = *params.Tool
	}
	win := windowOf(params.Days)
	res, err := rollup.Cached(a.cache, rollup.CacheKey("activity", scope, win), rollup.TTLOverview,
		func() (rollup.ActivityResult, error) { return rollup.Activity(r.Context(), a.db, win, scope, a.now()) })
	if err != nil {
		a.fail(w, "activity", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgTelemetry implements GET /api/org/telemetry — the native-console vendor
// analytics surface. Admin-only: the analytics tables are org-aggregate
// admin-keyed data that never enters the agent push wire, so a team lead has
// no scope to apply and gets 403.
func (a *API) OrgTelemetry(w http.ResponseWriter, r *http.Request, params gen.OrgTelemetryParams) {
	if _, ok := a.requireAdmin(w, r); !ok {
		return
	}
	win := windowOf(params.Days)
	res, err := rollup.Cached(a.cache, rollup.CacheKey("telemetry", rollup.Scope{Admin: true}, win), rollup.TTLOverview,
		func() (rollup.TelemetryResult, error) { return rollup.Telemetry(r.Context(), a.db, win, a.now()) })
	if err != nil {
		a.fail(w, "telemetry", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgRouting implements GET /api/org/routing — the model-routing org rollup
// (§R19). Admin-only: routing_summaries is an org-aggregate surface that never
// enters per-developer scope, so a team lead gets 403 (mirrors OrgTelemetry).
func (a *API) OrgRouting(w http.ResponseWriter, r *http.Request, params gen.OrgRoutingParams) {
	if _, ok := a.requireAdmin(w, r); !ok {
		return
	}
	win := windowOf(params.Days)
	res, err := rollup.Cached(a.cache, rollup.CacheKey("routing", rollup.Scope{Admin: true}, win), rollup.TTLOverview,
		func() (rollup.RoutingResult, error) { return rollup.Routing(r.Context(), a.db, win, a.now()) })
	if err != nil {
		a.fail(w, "routing", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgObsAnalytics implements GET /api/org/obs/analytics — the org-tier
// observability analytics rollup (obs-org-tier T1). Admin-only: obs_summaries
// is an org-aggregate, content-free surface that never enters per-developer
// scope (mirrors OrgRouting / OrgTelemetry).
func (a *API) OrgObsAnalytics(w http.ResponseWriter, r *http.Request, params gen.OrgObsAnalyticsParams) {
	if _, ok := a.requireAdmin(w, r); !ok {
		return
	}
	win := windowOf(params.Days)
	res, err := rollup.Cached(a.cache, rollup.CacheKey("obs_analytics", rollup.Scope{Admin: true}, win), rollup.TTLOverview,
		func() (rollup.ObsAnalyticsResult, error) { return rollup.ObsAnalytics(r.Context(), a.db, win, a.now()) })
	if err != nil {
		a.fail(w, "obs analytics", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgObsEvals implements GET /api/org/obs/evals — the org eval-health rollup
// (obs-org-tier T4). Admin-only: eval summaries are an org-aggregate,
// content-free surface (mirrors OrgRouting).
func (a *API) OrgObsEvals(w http.ResponseWriter, r *http.Request, params gen.OrgObsEvalsParams) {
	if _, ok := a.requireAdmin(w, r); !ok {
		return
	}
	win := windowOf(params.Days)
	res, err := rollup.Cached(a.cache, rollup.CacheKey("obs_evals", rollup.Scope{Admin: true}, win), rollup.TTLOverview,
		func() (rollup.ObsEvalsResult, error) { return rollup.ObsEvals(r.Context(), a.db, win, a.now()) })
	if err != nil {
		a.fail(w, "obs evals", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// orgID returns this single-org server's org id (the org table is a singleton).
// Best-effort: returns "" on error, which simply scopes the obs-alert queries to
// no rows rather than erroring the request.
func (a *API) orgID() string {
	var id string
	_ = a.db.QueryRow(`SELECT org_id FROM org LIMIT 1`).Scan(&id)
	return id
}

// OrgObsAlerts implements GET /api/org/obs/alerts — the obs alert rules + recent
// fires (obs-org-tier OP6b). Admin-only.
func (a *API) OrgObsAlerts(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireAdmin(w, r); !ok {
		return
	}
	res, err := obsalert.LoadAlertRules(r.Context(), a.db, a.orgID())
	if err != nil {
		a.fail(w, "obs alerts", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgCreateObsAlert implements POST /api/org/obs/alerts — create a rule (admin).
func (a *API) OrgCreateObsAlert(w http.ResponseWriter, r *http.Request) {
	userID, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}
	var body gen.OrgCreateObsAlertJSONBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		auth.WriteError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if !obsalert.ValidMetric(body.Metric) {
		auth.WriteError(w, http.StatusBadRequest, "bad_request", "metric must be error_rate, cost_usd or latency_p95_ms")
		return
	}
	in := obsalert.NewRuleInput{
		Metric:    body.Metric,
		Threshold: float64(body.Threshold),
	}
	if body.Name != nil {
		in.Name = *body.Name
	}
	if body.Comparator != nil {
		in.Comparator = *body.Comparator
	}
	if body.WindowDays != nil {
		in.WindowDays = *body.WindowDays
	}
	if body.WebhookUrl != nil {
		in.WebhookURL = *body.WebhookUrl
	}
	if body.CooldownMinutes != nil {
		in.CooldownMinutes = *body.CooldownMinutes
	}
	id, err := obsalert.CreateAlertRule(r.Context(), a.db, a.orgID(), userID, in, a.now())
	if err != nil {
		auth.WriteError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// OrgDeleteObsAlert implements DELETE /api/org/obs/alert/{id} (admin).
func (a *API) OrgDeleteObsAlert(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := a.requireAdmin(w, r); !ok {
		return
	}
	found, err := obsalert.DeleteAlertRule(r.Context(), a.db, a.orgID(), id)
	if err != nil {
		a.fail(w, "delete obs alert", err)
		return
	}
	if !found {
		auth.WriteError(w, http.StatusNotFound, "not_found", "no such rule")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// OrgObsCost implements GET /api/org/obs/cost — the org observability cost
// attribution (obs-org-tier OP6). Admin-only; content-free aggregate.
func (a *API) OrgObsCost(w http.ResponseWriter, r *http.Request, params gen.OrgObsCostParams) {
	if _, ok := a.requireAdmin(w, r); !ok {
		return
	}
	win := windowOf(params.Days)
	res, err := rollup.Cached(a.cache, rollup.CacheKey("obs_cost", rollup.Scope{Admin: true}, win), rollup.TTLOverview,
		func() (rollup.ObsCostResult, error) { return rollup.ObsCost(r.Context(), a.db, win, a.now()) })
	if err != nil {
		a.fail(w, "obs cost", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgObsTrajectories implements GET /api/org/obs/trajectories — the org
// trajectory explorer list (obs-org-tier T2). RBAC-scoped (admin sees the org,
// a lead sees their team's + own), via caller() like OrgSessions. The list is
// content-free structure (names/kinds/durations/cost), not a People disclosure,
// so no audit row (mirrors the metadata session list's posture below).
func (a *API) OrgObsTrajectories(w http.ResponseWriter, r *http.Request, params gen.OrgObsTrajectoriesParams) {
	userID, scope, ok := a.caller(w, r)
	if !ok {
		return
	}
	res, err := rollup.ObsTrajectories(r.Context(), a.db, windowOf(params.Days), scope, userID, 100, a.now())
	if err != nil {
		a.fail(w, "obs trajectories", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgObsTrace implements GET /api/org/obs/trace/{id} — one trace's span tree +
// the proxy-exact wedge. RBAC-scoped exactly like the list; out-of-scope or
// unknown trace ≡ 404.
func (a *API) OrgObsTrace(w http.ResponseWriter, r *http.Request, id string) {
	userID, scope, ok := a.caller(w, r)
	if !ok {
		return
	}
	res, found, err := rollup.ObsTraceDetail(r.Context(), a.db, id, scope, userID, a.now())
	if err != nil {
		a.fail(w, "obs trace", err)
		return
	}
	if !found {
		auth.WriteError(w, http.StatusNotFound, "not_found", "no such trace")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgObsTraceContent implements GET /api/org/obs/trace/{id}/content — the
// AUDITED span-content viewer (obs-org-tier T3). Reading the raw bodies is a
// deeper disclosure than the metadata tree, so it writes a DISTINCT
// view_span_content audit row BEFORE the data (mirrors OrgSessionMessages). If
// the audit write fails, the disclosure is refused.
func (a *API) OrgObsTraceContent(w http.ResponseWriter, r *http.Request, id string) {
	userID, scope, ok := a.caller(w, r)
	if !ok {
		return
	}
	if err := rollup.WriteAudit(r.Context(), a.db, userID, rollup.ActionViewSpanContent, "", id, sourceIP(r), a.now()); err != nil {
		a.fail(w, "audit span content", err)
		return
	}
	res, found, err := rollup.ObsTraceContent(r.Context(), a.db, id, scope, userID)
	if err != nil {
		a.fail(w, "obs trace content", err)
		return
	}
	if !found {
		auth.WriteError(w, http.StatusNotFound, "not_found", "no such trace")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgSessions implements GET /api/org/sessions — the paginated, scoped, AUDITED
// session list. Each row names a developer (the People disclosure class), so the
// handler writes a view_org_sessions audit row BEFORE returning the data.
func (a *API) OrgSessions(w http.ResponseWriter, r *http.Request, params gen.OrgSessionsParams) {
	userID, scope, ok := a.caller(w, r)
	if !ok {
		return
	}
	// Audit FIRST. If the audit write fails, refuse the disclosure.
	if err := rollup.WriteAudit(r.Context(), a.db, userID, rollup.ActionViewSessions, "", "", sourceIP(r), a.now()); err != nil {
		a.fail(w, "audit sessions", err)
		return
	}
	limit, offset := 50, 0
	if params.Limit != nil {
		limit = *params.Limit
	}
	if params.Offset != nil {
		offset = *params.Offset
	}
	var f rollup.SessionFilters
	if params.Tool != nil {
		f.Tool = *params.Tool
	}
	if params.Model != nil {
		f.Model = *params.Model
	}
	res, err := rollup.Sessions(r.Context(), a.db, windowOf(params.Days), scope, userID, f, limit, offset, a.now())
	if err != nil {
		a.fail(w, "sessions", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgSessionDetail implements GET /api/org/sessions/{id} — one session's
// content-free rollup. AUDITED (session id in target_detail). An out-of-scope
// or unknown id is a 404, which doubles as the out-of-scope response so the
// existence of another team's session is not disclosed.
func (a *API) OrgSessionDetail(w http.ResponseWriter, r *http.Request, id string) {
	userID, scope, ok := a.caller(w, r)
	if !ok {
		return
	}
	if err := rollup.WriteAudit(r.Context(), a.db, userID, rollup.ActionViewSessions, "", id, sourceIP(r), a.now()); err != nil {
		a.fail(w, "audit session detail", err)
		return
	}
	res, found, err := rollup.SessionDetail(r.Context(), a.db, id, scope, userID, a.now())
	if err != nil {
		a.fail(w, "session detail", err)
		return
	}
	if !found {
		auth.WriteError(w, http.StatusNotFound, "not_found", "no such session")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgSessionMessages implements GET /api/org/sessions/{id}/messages — the
// captured native-OTel content bodies for one session. This is a DEEPER
// disclosure than the metadata detail (it returns actual prose), so the handler
// writes a distinct view_session_messages audit row BEFORE returning. Scoped
// like the detail; an out-of-scope or unknown id is a 404 (so existence is not
// disclosed). Not cached (it is per-request and sensitive).
func (a *API) OrgSessionMessages(w http.ResponseWriter, r *http.Request, id string) {
	userID, scope, ok := a.caller(w, r)
	if !ok {
		return
	}
	if err := rollup.WriteAudit(r.Context(), a.db, userID, rollup.ActionViewSessionMessages, "", id, sourceIP(r), a.now()); err != nil {
		a.fail(w, "audit session messages", err)
		return
	}
	res, found, err := rollup.SessionMessages(r.Context(), a.db, id, scope, userID, a.now())
	if err != nil {
		a.fail(w, "session messages", err)
		return
	}
	if !found {
		auth.WriteError(w, http.StatusNotFound, "not_found", "no such session")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgLive implements GET /api/org/live — the "who's working now" presence
// surface. AUDITED: each entry names a developer, so the handler writes a
// view_org_sessions audit row before returning. Not cached (it must be live).
func (a *API) OrgLive(w http.ResponseWriter, r *http.Request) {
	userID, scope, ok := a.caller(w, r)
	if !ok {
		return
	}
	if err := rollup.WriteAudit(r.Context(), a.db, userID, rollup.ActionViewSessions, "", "live", sourceIP(r), a.now()); err != nil {
		a.fail(w, "audit live", err)
		return
	}
	res, err := rollup.Live(r.Context(), a.db, scope, userID, a.now())
	if err != nil {
		a.fail(w, "live", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgMovers implements GET /api/org/movers — period-over-period spend movement
// by dimension. Aggregate (no per-developer identity), so not audited.
func (a *API) OrgMovers(w http.ResponseWriter, r *http.Request, params gen.OrgMoversParams) {
	_, scope, ok := a.caller(w, r)
	if !ok {
		return
	}
	dim := "model"
	if params.Dim != nil {
		dim = string(*params.Dim)
	}
	win := windowOf(params.Days)
	res, err := rollup.Cached(a.cache, rollup.CacheKey("movers", scope, win, dim), rollup.TTLOverview,
		func() (rollup.MoversResult, error) { return rollup.Movers(r.Context(), a.db, win, scope, dim, a.now()) })
	if err != nil {
		a.fail(w, "movers", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgReport implements GET /api/org/report — the monthly cost statement.
// Aggregate, not audited.
func (a *API) OrgReport(w http.ResponseWriter, r *http.Request, params gen.OrgReportParams) {
	_, scope, ok := a.caller(w, r)
	if !ok {
		return
	}
	month := ""
	if params.Month != nil {
		month = *params.Month
	}
	res, err := rollup.Cached(a.cache, rollup.CacheKey("report", scope, rollup.Window{}, month), rollup.TTLProject,
		func() (rollup.ReportResult, error) { return rollup.Report(r.Context(), a.db, scope, month, a.now()) })
	if err != nil {
		a.fail(w, "report", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgSuggestions implements GET /api/org/suggestions — the org advisor.
// Aggregate, not audited.
func (a *API) OrgSuggestions(w http.ResponseWriter, r *http.Request, params gen.OrgSuggestionsParams) {
	_, scope, ok := a.caller(w, r)
	if !ok {
		return
	}
	win := windowOf(params.Days)
	res, err := rollup.Cached(a.cache, rollup.CacheKey("suggestions", scope, win), rollup.TTLOverview,
		func() (rollup.SuggestionsResult, error) {
			return rollup.Suggestions(r.Context(), a.db, win, scope, a.now())
		})
	if err != nil {
		a.fail(w, "suggestions", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgProjects implements GET /api/org/projects.
func (a *API) OrgProjects(w http.ResponseWriter, r *http.Request, params gen.OrgProjectsParams) {
	_, scope, ok := a.caller(w, r)
	if !ok {
		return
	}
	win := windowOf(params.Days)
	res, err := rollup.Cached(a.cache, rollup.CacheKey("projects", scope, win), rollup.TTLProject,
		func() (rollup.ProjectsResult, error) { return rollup.Projects(r.Context(), a.db, win, scope, a.now()) })
	if err != nil {
		a.fail(w, "projects", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgProjectDetail implements GET /api/org/projects/{id}. Scoping is structural:
// projectRootByID only resolves projects the caller's scope touched, so an
// out-of-scope (or unknown) id is a 404.
func (a *API) OrgProjectDetail(w http.ResponseWriter, r *http.Request, id string, params gen.OrgProjectDetailParams) {
	_, scope, ok := a.caller(w, r)
	if !ok {
		return
	}
	res, found, err := rollup.ProjectDetail(r.Context(), a.db, windowOf(params.Days), id, scope, a.now())
	if err != nil {
		a.fail(w, "project detail", err)
		return
	}
	if !found {
		auth.WriteError(w, http.StatusNotFound, "not_found", "no such project")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgAudit implements GET /api/org/audit.
func (a *API) OrgAudit(w http.ResponseWriter, r *http.Request, params gen.OrgAuditParams) {
	_, scope, ok := a.caller(w, r)
	if !ok {
		return
	}
	limit, offset := 100, 0
	if params.Limit != nil {
		limit = *params.Limit
	}
	if params.Offset != nil {
		offset = *params.Offset
	}
	res, err := rollup.Audit(r.Context(), a.db, scope, limit, offset)
	if err != nil {
		a.fail(w, "audit", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgLogDrillDown implements POST /api/org/audit/log-drill-down. The UI calls
// it the instant the user clicks "Show developer breakdown", before fetching
// the per-developer data.
func (a *API) OrgLogDrillDown(w http.ResponseWriter, r *http.Request) {
	userID, scope, ok := a.caller(w, r)
	if !ok {
		return
	}
	var in gen.DrillDownLogInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.TeamId == "" {
		auth.WriteError(w, http.StatusBadRequest, "bad_request", "team_id is required")
		return
	}
	if !canSeeTeam(scope, in.TeamId) {
		auth.WriteError(w, http.StatusForbidden, "forbidden", "not authorised for this team")
		return
	}
	if err := rollup.WriteAudit(r.Context(), a.db, userID, rollup.ActionDrillDown, in.TeamId, "", sourceIP(r), a.now()); err != nil {
		a.fail(w, "log drill-down", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Members (admin) -------------------------------------------------------

// OrgMembers implements GET /api/org/members — the active SCIM-provisioned
// org members projected for the admin Invite dropdown. Admin-only: a non-admin
// caller gets 403, because the list is the org-wide user catalogue and a team
// lead has no reason to enumerate it.
func (a *API) OrgMembers(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireAdmin(w, r); !ok {
		return
	}
	res, err := rollup.ListActiveMembers(r.Context(), a.db)
	if err != nil {
		a.fail(w, "members list", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// --- Budgets (admin) -------------------------------------------------------

// OrgBudgetList implements GET /api/org/budgets.
func (a *API) OrgBudgetList(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireAdmin(w, r); !ok {
		return
	}
	res, err := rollup.Budgets(r.Context(), a.db, a.now())
	if err != nil {
		a.fail(w, "budget list", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgBudgetCreate implements POST /api/org/budgets.
func (a *API) OrgBudgetCreate(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireAdmin(w, r); !ok {
		return
	}
	var in gen.BudgetInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if !validBudget(w, in) {
		return
	}
	id, err := a.createBudget(r.Context(), in)
	if errors.Is(err, ErrBudgetExists) {
		auth.WriteError(w, http.StatusConflict, "conflict", "a budget already exists for this scope")
		return
	}
	if err != nil {
		a.fail(w, "budget create", err)
		return
	}
	a.cache.Invalidate()
	a.respondBudget(w, r, id, http.StatusCreated)
}

// OrgBudgetUpdate implements PUT /api/org/budgets/{id}.
func (a *API) OrgBudgetUpdate(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := a.requireAdmin(w, r); !ok {
		return
	}
	var in gen.BudgetInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if !validBudget(w, in) {
		return
	}
	found, err := a.updateBudget(r.Context(), id, in)
	if err != nil {
		a.fail(w, "budget update", err)
		return
	}
	if !found {
		auth.WriteError(w, http.StatusNotFound, "not_found", "no such budget")
		return
	}
	a.cache.Invalidate()
	a.respondBudget(w, r, id, http.StatusOK)
}

// OrgBudgetDelete implements DELETE /api/org/budgets/{id}.
func (a *API) OrgBudgetDelete(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := a.requireAdmin(w, r); !ok {
		return
	}
	found, err := a.deleteBudget(r.Context(), id)
	if err != nil {
		a.fail(w, "budget delete", err)
		return
	}
	if !found {
		auth.WriteError(w, http.StatusNotFound, "not_found", "no such budget")
		return
	}
	a.cache.Invalidate()
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) respondBudget(w http.ResponseWriter, r *http.Request, id string, status int) {
	b, found, err := a.budgetStatusByID(r.Context(), id)
	if err != nil {
		a.fail(w, "budget reload", err)
		return
	}
	if !found {
		auth.WriteError(w, http.StatusNotFound, "not_found", "no such budget")
		return
	}
	writeJSON(w, status, b)
}

// --- Admin: bearers, revoke, team role -------------------------------------

// OrgListBearers implements GET /api/org/admin/bearers.
func (a *API) OrgListBearers(w http.ResponseWriter, r *http.Request, params gen.OrgListBearersParams) {
	if _, ok := a.requireAdmin(w, r); !ok {
		return
	}
	if params.UserId == "" {
		auth.WriteError(w, http.StatusBadRequest, "bad_request", "user_id is required")
		return
	}
	res, err := a.listBearers(r.Context(), params.UserId)
	if err != nil {
		a.fail(w, "list bearers", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgRevokeBearer implements POST /api/org/admin/revoke.
func (a *API) OrgRevokeBearer(w http.ResponseWriter, r *http.Request) {
	userID, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}
	var in gen.RevokeBearerInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Jti == "" {
		auth.WriteError(w, http.StatusBadRequest, "bad_request", "jti is required")
		return
	}
	if err := a.revokeBearer(r.Context(), in.Jti); err != nil {
		a.fail(w, "revoke", err)
		return
	}
	if err := rollup.WriteAudit(r.Context(), a.db, userID, rollup.ActionRevokeBearer, "", in.Jti, sourceIP(r), a.now()); err != nil {
		a.logger.Error("revoke: audit", "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// OrgSetTeamRole implements POST /api/org/admin/team-role.
func (a *API) OrgSetTeamRole(w http.ResponseWriter, r *http.Request) {
	userID, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}
	var in gen.TeamRoleInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.TeamId == "" || in.UserId == "" || (in.Role != "member" && in.Role != "lead") {
		auth.WriteError(w, http.StatusBadRequest, "bad_request", "team_id, user_id and role(member|lead) are required")
		return
	}
	found, err := a.setTeamRole(r.Context(), in.TeamId, in.UserId, string(in.Role))
	if err != nil {
		a.fail(w, "set role", err)
		return
	}
	if !found {
		auth.WriteError(w, http.StatusNotFound, "not_found", "no such team membership")
		return
	}
	if err := rollup.WriteAudit(r.Context(), a.db, userID, rollup.ActionSetTeamRole, in.TeamId, in.UserId+":"+string(in.Role), sourceIP(r), a.now()); err != nil {
		a.logger.Error("set role: audit", "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ---------------------------------------------------------------

// requireAdmin resolves the caller and enforces org-admin authority.
func (a *API) requireAdmin(w http.ResponseWriter, r *http.Request) (string, bool) {
	userID, scope, ok := a.caller(w, r)
	if !ok {
		return "", false
	}
	if !scope.Admin {
		auth.WriteError(w, http.StatusForbidden, "forbidden", "admin only")
		return "", false
	}
	return userID, true
}

// teamDetailCached wraps rollup.TeamDetail in the cache while preserving the
// (result, found) tuple — only a found result is cached.
func teamDetailCached(a *API, ctx context.Context, win rollup.Window, id string) (rollup.TeamDetailResult, bool, error) {
	type cached struct {
		res   rollup.TeamDetailResult
		found bool
	}
	v, err := rollup.Cached(a.cache, rollup.CacheKey("team", rollup.Scope{Admin: true}, win, id), rollup.TTLTeam,
		func() (cached, error) {
			res, found, err := rollup.TeamDetail(ctx, a.db, win, id, a.now())
			return cached{res, found}, err
		})
	return v.res, v.found, err
}

func windowOf(days *gen.WindowDays) rollup.Window {
	if days != nil {
		return rollup.Window{Days: *days}
	}
	return rollup.Window{}
}

func validBudget(w http.ResponseWriter, in gen.BudgetInput) bool {
	if in.Scope != "team" && in.Scope != "project" {
		auth.WriteError(w, http.StatusBadRequest, "bad_request", "scope must be team or project")
		return false
	}
	if in.ScopeId == "" {
		auth.WriteError(w, http.StatusBadRequest, "bad_request", "scope_id is required")
		return false
	}
	if in.MonthlyUsdCap <= 0 {
		auth.WriteError(w, http.StatusBadRequest, "bad_request", "monthly_usd_cap must be > 0")
		return false
	}
	if in.AlertWebhookUrl != nil && *in.AlertWebhookUrl != "" {
		if u, err := url.Parse(*in.AlertWebhookUrl); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			auth.WriteError(w, http.StatusBadRequest, "bad_request", "alert_webhook_url must be an http(s) URL")
			return false
		}
	}
	if in.AlertThresholds != nil {
		for _, t := range *in.AlertThresholds {
			if t <= 0 || t > 2 {
				auth.WriteError(w, http.StatusBadRequest, "bad_request", "alert_thresholds must be in (0, 2]")
				return false
			}
		}
	}
	return true
}

// decodeJSON decodes the request body into v, writing a 400 and returning false
// on failure.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(v); err != nil {
		auth.WriteError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// fail logs the internal error and writes a generic 500 (no internals leak to
// the client).
func (a *API) fail(w http.ResponseWriter, what string, err error) {
	a.logger.Error("dashboard api: "+what, "err", err)
	auth.WriteError(w, http.StatusInternalServerError, "internal", "request failed")
}

// sourceIP returns the first X-Forwarded-For hop if present, else the request's
// remote address (host only).
func sourceIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, found := strings.Cut(xff, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
