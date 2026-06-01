package dashboard

import (
	"context"
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
// (admin sees all, lead sees only their teams) is enforced HERE, per request,
// against the resolved caller — defence in depth: the rollup queries trust
// their Scope, the handlers do not trust the URL.
type API struct {
	db          *sql.DB
	cache       *rollup.Cache
	adminEmails map[string]bool
	logger      *slog.Logger
	now         func() time.Time
}

// NewAPI constructs the dashboard API over the server DB. adminEmails is the
// org-admin allow-list from config (case-insensitive).
func NewAPI(db *sql.DB, cache *rollup.Cache, adminEmails []string, logger *slog.Logger) *API {
	if logger == nil {
		logger = slog.Default()
	}
	set := make(map[string]bool, len(adminEmails))
	for _, e := range adminEmails {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			set[e] = true
		}
	}
	return &API{
		db: db, cache: cache, adminEmails: set, logger: logger,
		now: func() time.Time { return time.Now().UTC() },
	}
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
	var email string
	err := a.db.QueryRowContext(ctx, `SELECT email FROM org_members WHERE user_id = ?`, userID).Scan(&email)
	if errors.Is(err, sql.ErrNoRows) {
		return rollup.Scope{}, nil
	}
	if err != nil {
		return rollup.Scope{}, err
	}
	if a.adminEmails[strings.ToLower(email)] {
		return rollup.Scope{Admin: true}, nil
	}
	rows, err := a.db.QueryContext(ctx,
		`SELECT team_id FROM org_team_members WHERE user_id = ? AND role = 'lead' ORDER BY team_id`, userID)
	if err != nil {
		return rollup.Scope{}, err
	}
	defer rows.Close()
	var led []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return rollup.Scope{}, err
		}
		led = append(led, t)
	}
	return rollup.Scope{TeamIDs: led}, rows.Err()
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
