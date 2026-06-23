package dashboard

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/guard"
	"github.com/marmutapp/superbased-observer/internal/orgserver/api"
	"github.com/marmutapp/superbased-observer/internal/orgserver/auth"
	gen "github.com/marmutapp/superbased-observer/internal/orgserver/dashboard/gen"
	"github.com/marmutapp/superbased-observer/internal/orgserver/rollup"
)

// Guard-layer dashboard endpoints (guard spec §14.3 rollups + §14.5 RBAC,
// G14). Role model, layered on the existing SAML session + admin_emails +
// team-lead scoping:
//
//   - guard READS (/api/org/guard/{overview,rules,teams,agents}): an org
//     admin, policy_admin, or security_viewer reads org-wide; a team lead
//     reads their teams' slice; a plain member gets an empty scope (zero
//     rows, not 403 — the cost-endpoint convention).
//   - policy READS (bundle history/content): admin, policy_admin or
//     security_viewer only — 403 otherwise. The bundle is org-wide policy,
//     not team data, so lead scoping cannot apply.
//   - policy WRITES (lint is a dry-run write surface, publish mutates):
//     admin or policy_admin only. Publish goes through the SAME
//     api.PublishPolicyBundle gate as the G13 CLI and is audit-logged.

// guardAuthority bundles what the guard handlers need about a caller.
type guardAuthority struct {
	userID      string
	email       string
	admin       bool
	policyAdmin bool
	secViewer   bool
	scope       rollup.Scope // guard-read scope
}

// orgWideGuardReader reports whether the caller reads guard data org-wide.
func (g guardAuthority) orgWideGuardReader() bool {
	return g.admin || g.policyAdmin || g.secViewer
}

// canPublishPolicy reports whether the caller may lint/publish bundles.
func (g guardAuthority) canPublishPolicy() bool {
	return g.admin || g.policyAdmin
}

// guardCaller resolves the SAML-authenticated user into its guard authority.
// On any failure it writes the response and returns ok=false.
func (a *API) guardCaller(w http.ResponseWriter, r *http.Request) (guardAuthority, bool) {
	id, present := auth.UserIDFromContext(r.Context())
	if !present {
		auth.WriteError(w, http.StatusUnauthorized, "unauthorized", "login required")
		return guardAuthority{}, false
	}
	ga, err := a.resolveGuardAuthority(r.Context(), id)
	if err != nil {
		a.fail(w, "resolve guard authority", err)
		return guardAuthority{}, false
	}
	return ga, true
}

// resolveGuardAuthority computes the caller's roles from the config email
// lists and their guard-read scope: org-wide for any of the three roles,
// led teams otherwise (the lead sees the same slice of guard data as of
// cost data), empty for a plain member.
func (a *API) resolveGuardAuthority(ctx context.Context, userID string) (guardAuthority, error) {
	ga := guardAuthority{userID: userID}
	email, found, err := a.memberEmail(ctx, userID)
	if err != nil || !found {
		return ga, err
	}
	ga.email = email
	lower := strings.ToLower(email)
	ga.admin = a.adminEmails[lower]
	ga.policyAdmin = a.policyAdmins[lower]
	ga.secViewer = a.secViewers[lower]
	if ga.orgWideGuardReader() {
		ga.scope = rollup.Scope{Admin: true}
		return ga, nil
	}
	led, err := a.leadTeams(ctx, userID)
	if err != nil {
		return guardAuthority{}, err
	}
	ga.scope = rollup.Scope{TeamIDs: led}
	return ga, nil
}

// --- Guard rollup reads ------------------------------------------------------

// OrgGuardOverview implements GET /api/org/guard/overview.
func (a *API) OrgGuardOverview(w http.ResponseWriter, r *http.Request, params gen.OrgGuardOverviewParams) {
	ga, ok := a.guardCaller(w, r)
	if !ok {
		return
	}
	win := windowOf(params.Days)
	res, err := rollup.Cached(a.cache, rollup.CacheKey("guard-overview", ga.scope, win), rollup.TTLGuard,
		func() (rollup.GuardOverviewResult, error) {
			return rollup.GuardOverview(r.Context(), a.db, win, ga.scope, a.now())
		})
	if err != nil {
		a.fail(w, "guard overview", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgGuardRules implements GET /api/org/guard/rules.
func (a *API) OrgGuardRules(w http.ResponseWriter, r *http.Request, params gen.OrgGuardRulesParams) {
	ga, ok := a.guardCaller(w, r)
	if !ok {
		return
	}
	win := windowOf(params.Days)
	res, err := rollup.Cached(a.cache, rollup.CacheKey("guard-rules", ga.scope, win), rollup.TTLGuard,
		func() (rollup.GuardRulesResult, error) {
			return rollup.GuardRules(r.Context(), a.db, win, ga.scope, a.now())
		})
	if err != nil {
		a.fail(w, "guard rules", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgGuardTeams implements GET /api/org/guard/teams.
func (a *API) OrgGuardTeams(w http.ResponseWriter, r *http.Request, params gen.OrgGuardTeamsParams) {
	ga, ok := a.guardCaller(w, r)
	if !ok {
		return
	}
	win := windowOf(params.Days)
	res, err := rollup.Cached(a.cache, rollup.CacheKey("guard-teams", ga.scope, win), rollup.TTLGuard,
		func() (rollup.GuardTeamsResult, error) {
			return rollup.GuardTeams(r.Context(), a.db, win, ga.scope, a.now())
		})
	if err != nil {
		a.fail(w, "guard teams", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgGuardAgents implements GET /api/org/guard/agents — the per-agent
// chain-continuity report. Per-developer rows are a privacy-sensitive
// disclosure, so the audit row is written BEFORE the data is fetched (the
// OrgTeamDevelopers rule) and the result is deliberately uncached.
func (a *API) OrgGuardAgents(w http.ResponseWriter, r *http.Request) {
	ga, ok := a.guardCaller(w, r)
	if !ok {
		return
	}
	if err := rollup.WriteAudit(r.Context(), a.db, ga.userID, rollup.ActionViewGuardAgents, "", "", sourceIP(r), a.now()); err != nil {
		a.fail(w, "audit guard agents", err)
		return
	}
	res, err := rollup.GuardAgents(r.Context(), a.db, ga.scope)
	if err != nil {
		a.fail(w, "guard agents", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// --- Policy bundle reads -----------------------------------------------------

// requireGuardPolicyReader gates the bundle read endpoints.
func (a *API) requireGuardPolicyReader(w http.ResponseWriter, r *http.Request) (guardAuthority, bool) {
	ga, ok := a.guardCaller(w, r)
	if !ok {
		return guardAuthority{}, false
	}
	if !ga.orgWideGuardReader() {
		auth.WriteError(w, http.StatusForbidden, "forbidden", "guard policy access requires an admin, policy_admin or security_viewer role")
		return guardAuthority{}, false
	}
	return ga, true
}

// requirePolicyAdmin gates the authoring endpoints (lint, publish).
func (a *API) requirePolicyAdmin(w http.ResponseWriter, r *http.Request) (guardAuthority, bool) {
	ga, ok := a.guardCaller(w, r)
	if !ok {
		return guardAuthority{}, false
	}
	if !ga.canPublishPolicy() {
		auth.WriteError(w, http.StatusForbidden, "forbidden", "policy authoring requires an admin or policy_admin role")
		return guardAuthority{}, false
	}
	return ga, true
}

// OrgGuardPolicyBundles implements GET /api/org/guard/policy/bundles.
func (a *API) OrgGuardPolicyBundles(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireGuardPolicyReader(w, r); !ok {
		return
	}
	metas, err := api.ListPolicyBundles(r.Context(), a.db)
	if err != nil {
		a.fail(w, "policy bundles", err)
		return
	}
	res := rollup.GuardPolicyBundlesResult{
		SigningConfigured: a.policySigner != nil,
		Bundles:           []rollup.GuardPolicyBundleInfo{},
	}
	for _, m := range metas { // newest first per ListPolicyBundles
		res.Bundles = append(res.Bundles, rollup.GuardPolicyBundleInfo{
			Version: m.Version, SignedAt: m.SignedAt, CreatedBy: m.CreatedBy,
			Description: m.Description, TOMLBytes: m.TOMLBytes,
		})
	}
	if len(res.Bundles) > 0 {
		res.ActiveVersion = res.Bundles[0].Version
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgGuardPolicyBundleDetail implements GET /api/org/guard/policy/bundles/{version}.
func (a *API) OrgGuardPolicyBundleDetail(w http.ResponseWriter, r *http.Request, version int64) {
	if _, ok := a.requireGuardPolicyReader(w, r); !ok {
		return
	}
	b, err := api.PolicyBundleByVersion(r.Context(), a.db, version)
	if errors.Is(err, api.ErrNoPolicyBundle) {
		auth.WriteError(w, http.StatusNotFound, "not_found", "no such bundle version")
		return
	}
	if err != nil {
		a.fail(w, "policy bundle detail", err)
		return
	}
	writeJSON(w, http.StatusOK, rollup.GuardPolicyBundleDetail{
		Version: b.Version, BundleTOML: b.BundleTOML, SignedAt: b.SignedAt, Description: b.Description,
	})
}

// --- Policy authoring --------------------------------------------------------

// dryRunWindow is the trailing window the lint endpoint's §14.2 dry-run
// statistics are computed over (the default rollup window).
var dryRunWindow = rollup.Window{Days: rollup.DefaultWindowDays}

// OrgGuardPolicyLint implements POST /api/org/guard/policy/lint: the exact
// guard.Lint("org") refusal the publish gate runs, plus dry-run stats for
// the draft's referenced rule ids. Nothing is written.
func (a *API) OrgGuardPolicyLint(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requirePolicyAdmin(w, r); !ok {
		return
	}
	var in gen.GuardPolicyLintInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.BundleToml == "" {
		auth.WriteError(w, http.StatusBadRequest, "bad_request", "bundle_toml is required")
		return
	}

	res := rollup.GuardPolicyLintResult{
		Problems:   []string{},
		WindowDays: dryRunWindow.Days,
		DryRun:     []rollup.GuardRuleDryRun{},
	}
	if problems := guard.Lint([]byte(in.BundleToml), "org"); len(problems) > 0 {
		res.Problems = problems
	}
	res.OK = len(res.Problems) == 0

	// Dry-run stats are best-effort decoration on the lint verdict: a
	// structurally unparseable draft already failed Lint above, so a refs
	// error adds nothing.
	if overrides, declared, err := guard.PolicyRuleRefs([]byte(in.BundleToml)); err == nil {
		hits, err := rollup.GuardRuleHitsForIDs(r.Context(), a.db, dryRunWindow, overrides, a.now())
		if err != nil {
			a.fail(w, "policy dry-run", err)
			return
		}
		seen := map[string]bool{}
		for _, id := range overrides {
			if seen[id] {
				continue
			}
			seen[id] = true
			d := hits[id] // zero value when the window has no hits
			d.RuleID, d.Computable = id, true
			res.DryRun = append(res.DryRun, d)
		}
		for _, id := range declared {
			if seen[id] {
				continue
			}
			seen[id] = true
			res.DryRun = append(res.DryRun, rollup.GuardRuleDryRun{RuleID: id, Computable: false})
		}
	}
	writeJSON(w, http.StatusOK, res)
}

// OrgGuardPolicyPublish implements POST /api/org/guard/policy/publish. It is
// the dashboard face of the ONE authoring gate: api.PublishPolicyBundle
// (lint + sign + insert in one transaction), exactly what the G13 CLI calls.
// The signing key is loaded per request via the injected PolicySigner and
// dropped immediately; 409 when the channel is off. The publish is recorded
// in audit_log with the assigned version.
func (a *API) OrgGuardPolicyPublish(w http.ResponseWriter, r *http.Request) {
	ga, ok := a.requirePolicyAdmin(w, r)
	if !ok {
		return
	}
	var in gen.GuardPolicyPublishInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.BundleToml == "" {
		auth.WriteError(w, http.StatusBadRequest, "bad_request", "bundle_toml is required")
		return
	}
	if a.policySigner == nil {
		auth.WriteError(w, http.StatusConflict, "policy_channel_off",
			"no policy signing key configured ([policy].signing_key_path) — publish via the dashboard is disabled")
		return
	}
	priv, err := a.policySigner()
	if err != nil {
		a.fail(w, "load policy signing key", err)
		return
	}
	desc := ""
	if in.Description != nil {
		desc = *in.Description
	}
	version, err := api.PublishPolicyBundle(r.Context(), a.db, priv, in.BundleToml, ga.email, desc)
	if errors.Is(err, api.ErrBundleInvalid) {
		auth.WriteError(w, http.StatusBadRequest, "bundle_invalid", err.Error())
		return
	}
	if err != nil {
		a.fail(w, "publish policy bundle", err)
		return
	}
	if err := rollup.WriteAudit(r.Context(), a.db, ga.userID, rollup.ActionPublishBundle,
		"", "v"+strconv.FormatInt(version, 10), sourceIP(r), a.now()); err != nil {
		a.logger.Error("publish bundle: audit", "err", err)
	}
	a.cache.Invalidate()
	writeJSON(w, http.StatusCreated, rollup.GuardPolicyPublishResult{Version: version})
}
