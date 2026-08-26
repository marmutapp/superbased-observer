package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/govern"
	"github.com/marmutapp/superbased-observer/internal/policyfam/nodegov"
)

// Admin-controlled Plane B, the node-dashboard enforcement point
// (docs/plans/admin-controlled-plane-b-spec-2026-08-15.md §3.5, Phase 1a).
//
// TWO layers, always, and the API one is the load-bearing one:
//
//  1. API REFUSAL (this file). A request for a route belonging to a hidden
//     section is 404; a MUTATING request for a route in a read-only section
//     is 409. A frontend-only hide is theatre — the threat model assumes the
//     developer can open devtools.
//  2. UI HIDING (the SPA, via GET /api/governance).
//
// The middleware is installed on BOTH branches of guardedHandler — the
// loopback listener and the remotely-exposed one. The adversarial review
// found the natural-looking home for it (beside capMap, inside remoteAuthz)
// is REMOTE-ONLY: Server.Handler() discards capMap and remoteAuthz's own
// comment records that "the direct owner-trusted loopback listener never
// runs remoteAuthz". Installing it there would have put governance
// everywhere EXCEPT the normal developer node dashboard.

// Section is the node-dashboard nav section a route belongs to — the unit an
// organization's governance body hides or locks. Its values are exactly the
// ids in web/src/lib/nav.ts, which internal/policyfam/nodegov pins against
// that file.
//
// SectionNone means "not part of any single page": status probes, the SPA
// shell, cross-cutting reads, and the governance endpoint itself. An
// unmapped/None route is NEVER hidden (the adversarial review's D-D
// default): fail-open here leaks at most one endpoint of a hidden page,
// while fail-closed would 404 every new route the moment any section is
// hidden.
type Section string

// The closed section vocabulary. Kept as a flat list rather than derived
// from nodegov's slice so the constants are usable at the registration site;
// TestDashboardSectionsMatchNodegovVocabulary pins the two together. The
// UNHIDEABLE floor, by contrast, is read straight from nodegov below —
// there must be exactly one definition of "cannot be hidden".
const (
	SectionNone        Section = ""
	SectionOverview    Section = "overview"
	SectionLive        Section = "live"
	SectionSessions    Section = "sessions"
	SectionActions     Section = "actions"
	SectionSecurity    Section = "security"
	SectionEgress      Section = "egress"
	SectionSearch      Section = "search"
	SectionCost        Section = "cost"
	SectionAnalysis    Section = "analysis"
	SectionTools       Section = "tools"
	SectionCompression Section = "compression"
	SectionCache       Section = "cache"
	SectionSuggestions Section = "suggestions"
	SectionRouting     Section = "routing"
	SectionBenchmarks  Section = "benchmarks"
	SectionDiscovery   Section = "discovery"
	SectionPatterns    Section = "patterns"
	SectionPolicies    Section = "policies"
	SectionPrivacy     Section = "privacy"
	SectionTerminals   Section = "terminals"
	SectionRemote      Section = "remote"
	SectionSettings    Section = "settings"
)

// AllSections is the closed set, for the coverage test and for callers that
// need to validate a section id.
var AllSections = []Section{
	SectionOverview, SectionLive, SectionSessions, SectionActions, SectionSecurity,
	SectionEgress, SectionSearch, SectionCost, SectionAnalysis, SectionTools,
	SectionCompression, SectionCache, SectionSuggestions, SectionRouting,
	SectionBenchmarks, SectionDiscovery, SectionPatterns, SectionPolicies,
	SectionPrivacy, SectionTerminals, SectionRemote, SectionSettings,
}

// GovernanceProvider resolves the node's live governance posture. It is the
// ONE seam this package has onto the feature: nil (the default, and every
// solo node) means the guard is not installed at all and the handler chain
// is byte-identical to an ungoverned build.
type GovernanceProvider func(ctx context.Context) govern.Effective

// configSectionPrefix is the one route whose SUB-resource is itself a
// governed unit: /api/config/section/<settings-section-id> is how the SPA
// writes one Settings sub-section. Honouring settings_read_only /
// settings_hidden at the API therefore needs this single explicit rule —
// without it those two directive classes would be pure UI theatre.
const configSectionPrefix = "/api/config/section/"

// governanceGuard wraps next with the §3.5 API-refusal layer. mux is used
// only to resolve the matched route pattern (reusing ServeMux's own
// matching, exactly as remoteAuthz does); sections maps that pattern to its
// nav section.
func (s *Server) governanceGuard(mux *http.ServeMux, sections map[string]Section, next http.Handler) http.Handler {
	if s.opts.Governance == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		eff := s.opts.Governance(r.Context())
		// Short-circuit BEFORE any route resolution when this node is not
		// governed, so an ungranted node's request path never changes shape.
		if !eff.Active {
			next.ServeHTTP(w, r)
			return
		}
		_, pattern := mux.Handler(r)
		if sec := sections[pattern]; sec != SectionNone && !nodegov.IsUnhideableNavSection(string(sec)) {
			if eff.IsNavSectionHidden(string(sec)) {
				writeGovernanceRefusal(w, http.StatusNotFound, "governance_hidden", string(sec), eff,
					"This page is managed by your organization and is not available on this machine.")
				return
			}
			if eff.IsNavSectionReadOnly(string(sec)) && isUnsafeMethod(strings.ToUpper(r.Method)) {
				writeGovernanceRefusal(w, http.StatusConflict, "governance_read_only", string(sec), eff,
					"This setting is pinned by your organization and cannot be changed here.")
				return
			}
		}
		// T8's structural floor, enforced a SECOND time here. The family
		// compiler already refuses a body naming one of these, on both the
		// publish and the accept path — but this guard also runs against
		// postures that did not come through today's compiler (an LKG cache
		// written by an older build, a hand-edited DB), and the one rule
		// that must never fail is the one that keeps a developer able to see
		// what their employer configured.
		if id, ok := configSectionID(r.URL.Path); ok && !nodegov.IsUnhideableSettingsSection(id) {
			if eff.IsSettingsSectionHidden(id) {
				writeGovernanceRefusal(w, http.StatusNotFound, "governance_hidden", id, eff,
					"This page is managed by your organization and is not available on this machine.")
				return
			}
			if eff.IsSettingsSectionReadOnly(id) && isUnsafeMethod(strings.ToUpper(r.Method)) {
				writeGovernanceRefusal(w, http.StatusConflict, "governance_read_only", id, eff,
					"This setting is pinned by your organization and cannot be changed here.")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// configSectionID extracts the settings sub-section id from a
// /api/config/section/<id> path.
func configSectionID(path string) (string, bool) {
	if !strings.HasPrefix(path, configSectionPrefix) {
		return "", false
	}
	id := strings.TrimPrefix(path, configSectionPrefix)
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

// governanceRefusal is the refusal body. It NAMES the cause — the honest
// -disabled-copy rule applied to an HTTP response: a developer inspecting a
// 404 must be able to tell "your organization hid this" from "this endpoint
// does not exist".
type governanceRefusal struct {
	Error   string `json:"error"`
	Section string `json:"section"`
	Family  string `json:"family"`
	Version int64  `json:"version"`
	OrgName string `json:"org_name,omitempty"`
	Message string `json:"message"`
}

func writeGovernanceRefusal(w http.ResponseWriter, status int, code, section string, eff govern.Effective, msg string) {
	orgName := eff.Notice.OrgDisplayName
	if orgName == "" {
		orgName = eff.OrgName
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(governanceRefusal{
		Error:   code,
		Section: section,
		Family:  "node.governance",
		Version: eff.Version,
		OrgName: orgName,
		Message: msg,
	})
}

// handleGovernance serves GET /api/governance: the node's resolved
// governance posture, which the SPA reads to filter its nav and render the
// managed notice.
//
// It is registered UNCONDITIONALLY and returns the DORMANT posture
// (active:false, empty lists) on any node without a grant — including every
// solo node — so the SPA needs no capability probing and the solo behaviour
// is a value, not a missing route.
func (s *Server) handleGovernance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	var eff govern.Effective
	if s.opts.Governance != nil {
		eff = s.opts.Governance(r.Context())
	} else {
		// No provider wired: resolve the zero inputs so the shape is
		// identical to a governed build's ungranted answer.
		eff = govern.Resolve(govern.Delivered{}, nil, govern.LiveIdentity{}, time.Now())
	}
	writeJSON(w, governanceResponse{Effective: eff, Share: s.resolveShareBlock(eff)})
}

// governanceResponse is the wire shape of GET /api/governance: the resolved
// posture, with the `share` block REPLACED by the resolved per-key rows the
// Privacy page renders.
//
// The embedded Effective carries its own `share` field (the org's directives
// as delivered, map[string]any). This outer field shadows it — encoding/json
// resolves a tag collision in favour of the shallower field — which is the
// point: serializing Effective verbatim emitted bare booleans
// ({"share":{"cache_detail":true}}) where the SPA expects
// {effective, local, source} objects, so every consumer read `.source` off a
// boolean. That produced "not shared" for a tier the org had just RAISED and
// an undefined label lookup, on the one card whose entire job is telling a
// developer what their employer configured — and it fired only once an org
// actually published a share directive, i.e. exactly when it mattered.
type governanceResponse struct {
	govern.Effective
	Share map[string]governanceShareKey `json:"share,omitempty"`
}

// governanceShareKey is one row of the `share` block: what is in force, what
// this machine's own config asks for, and which of the two decided it.
//
// Local is `any` and may be null: it is the node's own setting for the key,
// and a key this build has no local counterpart for (see shareLocalTable) has
// no honest value to report there.
type governanceShareKey struct {
	Effective any                `json:"effective"`
	Local     any                `json:"local"`
	Source    govern.ShareSource `json:"source"`
	// PolicyVersion is the delivered body's version, so the Source column can
	// name the policy that decided the row. Omitted on a dormant posture.
	PolicyVersion int64 `json:"policy_version,omitempty"`
}

// shareLocalTable maps each org-directable share key (the closed
// nodegov.ShareKeys vocabulary) onto this node's own [org_client.share]
// setting. Table-driven rather than a switch so the coverage test can walk it
// against nodegov.ShareKeys and fail the moment the vocabulary grows a key
// this surface would silently mis-report.
//
// Values are `any` because the vocabulary is mixed-kind: bool for every tier,
// []string for target_action_allowlist.
var shareLocalTable = map[string]func(config.OrgClientShareConfig) any{
	"full_content":            func(c config.OrgClientShareConfig) any { return c.FullContent },
	"full_tool_bodies":        func(c config.OrgClientShareConfig) any { return c.FullToolBodies },
	"routing_summary":         func(c config.OrgClientShareConfig) any { return c.RoutingSummary },
	"cache_detail":            func(c config.OrgClientShareConfig) any { return c.CacheDetail },
	"routing_detail":          func(c config.OrgClientShareConfig) any { return c.RoutingDetail },
	"limit_gauge":             func(c config.OrgClientShareConfig) any { return c.LimitGauge },
	"codeintel_detail":        func(c config.OrgClientShareConfig) any { return c.CodeintelDetail },
	"process_detail":          func(c config.OrgClientShareConfig) any { return c.ProcessDetail },
	"terminal_detail":         func(c config.OrgClientShareConfig) any { return c.TerminalDetail },
	"policy_state":            func(c config.OrgClientShareConfig) any { return c.PolicyState },
	"target_action_allowlist": func(c config.OrgClientShareConfig) any { return c.TargetActionAllowlist },
	"obs.summary":             func(c config.OrgClientShareConfig) any { return c.Obs.Summary },
	"obs.traces":              func(c config.OrgClientShareConfig) any { return c.Obs.Traces },
	"obs.content":             func(c config.OrgClientShareConfig) any { return c.Obs.Content },
	"obs.eval_summary":        func(c config.OrgClientShareConfig) any { return c.Obs.EvalSummary },
	"obs.admission":           func(c config.OrgClientShareConfig) any { return c.Obs.Admission },
	"obs.eval_items":          func(c config.OrgClientShareConfig) any { return c.Obs.EvalItems },
}

// resolveShareBlock resolves the org's delivered share directives against this
// node's own [org_client.share] settings, one row per key the org PUBLISHED —
// and only those, so the block's key set is unchanged and a key the org said
// nothing about keeps resolving to "you" in the SPA without a row.
//
// The merge is govern's, not this package's: MergeBool/LowerList are the same
// functions the push seam applies, so the "In force" column cannot drift from
// what actually ships. Reading the node's own config here (the same
// loadConfigForDashboard the Settings page uses) rather than taking an
// injected ShareOptions keeps the seam count at zero — /api/governance is
// fetched once per SPA mount, not on a hot path.
func (s *Server) resolveShareBlock(eff govern.Effective) map[string]governanceShareKey {
	if len(eff.Share) == 0 {
		return nil
	}
	var local config.OrgClientShareConfig
	if cfg, err := loadConfigForDashboard(s.opts.ConfigPath); err == nil {
		local = cfg.OrgClient.Share
	} else if s.opts.Logger != nil {
		// A config this process cannot parse is a real condition, not a
		// reason to drop the block: the org's directives are still in force
		// and the developer still gets to see them. The rows degrade to the
		// zero-value floor for `local`, which is what the push seam would
		// also fall back to.
		s.opts.Logger.Warn("governance: could not read local share settings", "error", err)
	}
	out := make(map[string]governanceShareKey, len(eff.Share))
	for key, directive := range eff.Share {
		row := governanceShareKey{PolicyVersion: eff.Version}
		fn, known := shareLocalTable[key]
		switch {
		case !known:
			// Unreachable while TestShareLocalTableCoversNodegovVocabulary
			// passes — nodegov.Compile refuses any share key outside its own
			// table on both the publish and the accept path. Kept so a drift
			// shows the key with the org named as its only source, rather
			// than dropping it (an invisible directive is the worst outcome
			// on a transparency surface).
			row.Effective, row.Local, row.Source = directive, nil, govern.ShareSourceOrg
		case isListDirective(directive):
			lv, _ := fn(local).([]string)
			ev := eff.LowerList(key, lv)
			// Normalize nil to [] on the wire only. govern preserves the
			// nil/empty distinction because the ALGEBRA needs it; the SPA
			// types both halves as string[] and renders either as "none", so
			// emitting null here would buy nothing and break the type.
			row.Effective, row.Local = orEmpty(ev), orEmpty(lv)
			row.Source = sourceForList(lv, ev)
		default:
			lv, _ := fn(local).(bool)
			// Gated (W-8), not the plain MergeBool/SourceForBool: this card
			// is the transparency surface, so it must report the exact
			// per-tier answer (ExtractionAuthorized) the push seam itself
			// gates on, not MergeBool's documented conservative
			// over-report. See internal/govern/sharetiers.go.
			row.Effective = eff.MergeBoolGated(key, lv)
			row.Local = lv
			row.Source = eff.SourceForBoolGated(key, lv)
		}
		out[key] = row
	}
	return out
}

// isListDirective reports whether an org directive is the list-kind
// (target_action_allowlist). Branching on the DIRECTIVE's shape rather than on
// the key name keeps the kind knowledge in one place — nodegov's compiler,
// which normalized it.
func isListDirective(v any) bool {
	_, ok := v.([]string)
	return ok
}

// sourceForList attributes a list-valued key. The list algebra is
// intersection-only in BOTH tenancies (there is no RaiseList), so the org can
// only ever have narrowed the node's own list or left it alone.
func sourceForList(local, effective []string) govern.ShareSource {
	if len(effective) < len(local) {
		return govern.ShareSourceOrg
	}
	return govern.ShareSourceBoth
}

// orEmpty renders a nil string slice as [] rather than null.
func orEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
