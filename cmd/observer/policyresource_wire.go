package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/orgclient"
	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	"github.com/marmutapp/superbased-observer/internal/policyfam"
	"github.com/marmutapp/superbased-observer/internal/policyfam/admission"
	"github.com/marmutapp/superbased-observer/internal/policyfam/providers"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// Plane-A P0-5 Phase W: v1 unified-policy-resource orchestration at the
// cmd/observer layer (plan docs/plans/plane-a-p0-5-unified-policy-resource-v1-plan.md
// §6.5/§6.6/§6.9/§6.10, §8 items 4-5). This file owns:
//
//   - policyResourceCacheDir / policyResourceOptionsFor: the shared knobs
//     both the LKG loader below and the steady-state poller
//     (runPolicyResourcePoller) build orgclient.PolicyResourceOptions from.
//   - loadPolicyResourceLKG: the §6.5 recovery matrix, run ONCE,
//     SYNCHRONOUSLY, before start.go binds the proxy listener.
//   - runPolicyResourcePoller: the §6.6/§6.9 steady-state poll-and-apply
//     loop, started only AFTER the listener binds.
//
// Neither function imports internal/obs — they speak to the shared
// admission service ONLY through *obsAdmissionHandle's PublishOrg*/
// ClearOrg* methods (obs_wire.go), preserving the reverse-import boundary
// (tests/invariant/obs_boundary_test.go: only obs_wire.go may import
// internal/obs).

// policyResourceCacheDir resolves the base directory for the §6.2
// generation-scoped policy-resource cache tree
// (<dir>/<org_key>/<generation>/<family>.json), mirroring
// policyStateSeqPath's convention (policystate_wire.go): beside the guard
// org-bundle cache when one is configured, else beside the observer DB.
func policyResourceCacheDir(cfg config.Config) string {
	if p := orgBundleCachePath(cfg); p != "" {
		return filepath.Join(filepath.Dir(p), "policy-resource")
	}
	return filepath.Join(filepath.Dir(cfg.Observer.DBPath), "policy-resource")
}

// policyResourceOptionsFor builds the orgclient.PolicyResourceOptions both
// the LKG loader and the poller share: the cache dir and the operator's
// accept/preauthorize lists (plan §6.4). LiveCapabilities is left empty —
// v1 wires no judge (or other) capability onto this rail (plan §6.8: the
// org-only service permits no remote judge), so any body declaring a
// nonempty RequiredCapabilities fails closed with capability_mismatch,
// exactly the plan's "fail-closed default before Phase W wires a real
// capability source" posture for LiveCapabilities.
// NodeAttrs carries the operator's locally-configured targeting attributes
// (P0-10 Phase B) so both the live accept path and the LKG path corroborate
// a signed selector predicate against the SAME attributes — an LKG install
// must never resurrect an envelope the live path would now reject.
func policyResourceOptionsFor(cfg config.Config) orgclient.PolicyResourceOptions {
	return orgclient.PolicyResourceOptions{
		CacheDir:            policyResourceCacheDir(cfg),
		AcceptFamilies:      cfg.OrgClient.Policy.AcceptFamilies,
		PreauthorizeEnforce: cfg.OrgClient.Policy.PreauthorizeEnforce,
		NodeAttrs:           policyResourceNodeAttrs(cfg),
	}
}

// policyResourceNodeAttrs maps [org_client.policy]'s node attribute knobs
// onto the shared contract's Selectors shape. Corroboration only — these are
// never presented to the server as an authorization claim (the server binds
// attributes to the verified identity; design §2).
func policyResourceNodeAttrs(cfg config.Config) orgcontract.Selectors {
	return orgcontract.Selectors{
		Workspace:   cfg.OrgClient.Policy.NodeWorkspace,
		Environment: cfg.OrgClient.Policy.NodeEnvironment,
		Service:     cfg.OrgClient.Policy.NodeService,
	}
}

// gatewayProvidersHandle is the SEPARATE, admission-independent install
// seam for the gateway.providers family (Phase 3,
// docs/plans/gateway-config-plane-spec-2026-08-15.md). Applying a remote
// lane table mutates internal/proxy.Proxy's live routing table directly —
// there is no obs/admission service involved at all, unlike
// admission.input and egress.routing_guardrail, which install onto the
// shared obs.AdmissionService's Org layer via *obsAdmissionHandle.
//
// A nil *gatewayProvidersHandle (or a nil apply/clear field) means this
// node has nowhere to apply a lane table — Apply/Clear are then no-ops, so
// publishPolicyResourceResult/clearOrgLayer never need to special-case "not
// wired on this build" beyond calling through this handle, exactly
// mirroring *obsAdmissionHandle's own nil-safe methods.
// It is ALSO the truth source for the P0-6 gateway.providers effective-state
// row (docs/plans/policy-state-v2-gateway-providers-spec-2026-08-15.md §2.2):
// the org-rail IDENTITY (accepted version + signed BodyHash + inert reason)
// exists nowhere else in the process, because internal/proxy neither knows
// nor should know it. The LOCAL half of the report is read LIVE from
// liveLanes instead, so the common case is a genuine observation of the
// running table rather than a mirror of what we believe we installed.
//
// INVARIANT: this handle is the ONLY caller of Proxy.SetLaneTable outside
// tests (Proxy.SetUpstreams has no non-test caller at all). A second mutator
// would desynchronise the org-rail half of the report and MUST either route
// through this handle or extend it.
type gatewayProvidersHandle struct {
	// apply installs a fresh lane table (upstreams + optional default
	// lane) onto the live proxy, all-or-nothing
	// (internal/proxy.Proxy.SetLaneTable's contract). A non-nil error
	// means the CURRENT lane table is left UNCHANGED — the caller only
	// logs it, it never crashes.
	apply func(upstreams map[string]string, autoDefaultLane string) error
	// clear reverts the proxy to its bootstrap ([proxy] config.toml) lane
	// table — a withdrawn org policy restores the operator's own
	// configured lanes, never an empty table.
	clear func()
	// liveLanes reads the CURRENTLY live lane table (Proxy.LaneTable). Only
	// the P0-6 local-row hash uses it; nil means "no live read available on
	// this node", which reports as no_policy.
	liveLanes func() (map[string]string, string)

	mu    sync.Mutex
	state gatewayProvidersState
}

// gatewayProvidersState is the org-rail half of the gateway.providers
// effective-state truth: what the org rail last INSTALLED (or failed to
// install) through this handle. The local half is never stored here — it is
// read live from liveLanes at report time.
type gatewayProvidersState struct {
	// hasOrgRail is true once an org-published lane table is installed
	// (applied or accepted-inert); false after a clear.
	hasOrgRail bool
	// version / bodyHash are the accepted org resource's identity. bodyHash
	// is the SIGNED BodyHash, never the compiled Spec.Hash — same org-rail
	// wire rule as admitter/egress.
	version  int64
	bodyHash string
	// inertReason is the §6.4 preauthorization (or other) inert reason on an
	// accepted-but-not-applied table. Empty when the table is routing.
	inertReason string
	// applyFailedVersion / applyRejectCode record a delivered version the
	// live proxy REFUSED (SetLaneTable returned an error, so the previous
	// table is still routing). Cleared by the next successful apply or
	// clear.
	applyFailedVersion int64
	applyRejectCode    string
}

// newGatewayProvidersHandle builds the daemon-lifetime install seam. All
// three closures may be nil (a node with no proxy lane wiring); every method
// is nil-safe in that case, and the P0-6 row then reports no_policy.
func newGatewayProvidersHandle(
	apply func(upstreams map[string]string, autoDefaultLane string) error,
	clear func(),
	liveLanes func() (map[string]string, string),
) *gatewayProvidersHandle {
	return &gatewayProvidersHandle{apply: apply, clear: clear, liveLanes: liveLanes}
}

// Apply installs upstreams/autoDefaultLane through the handle. Nil-safe: a
// nil handle or a nil apply field is a no-op returning nil (nothing to
// apply on this node, not a failure).
func (h *gatewayProvidersHandle) Apply(upstreams map[string]string, autoDefaultLane string) error {
	if h == nil || h.apply == nil {
		return nil
	}
	return h.apply(upstreams, autoDefaultLane)
}

// Clear reverts to the bootstrap lane table through the handle and forgets
// the org-rail state, so the next report falls back to the node's own live
// lanes (local_effective) or no_policy. Nil-safe.
func (h *gatewayProvidersHandle) Clear() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.state = gatewayProvidersState{}
	h.mu.Unlock()
	if h.clear == nil {
		return
	}
	h.clear()
}

// recordApplied stamps a successfully-applied org lane table.
func (h *gatewayProvidersHandle) recordApplied(version int64, bodyHash string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state = gatewayProvidersState{hasOrgRail: true, version: version, bodyHash: bodyHash}
}

// recordInert stamps an org lane table that was ACCEPTED but deliberately
// NOT applied (the §6.4 preauthorize_enforce gate). The proxy keeps routing
// through whatever it was routing through before.
func (h *gatewayProvidersHandle) recordInert(version int64, bodyHash, reason string) {
	if h == nil {
		return
	}
	if reason == "" {
		reason = "not_preauthorized"
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state = gatewayProvidersState{hasOrgRail: true, version: version, bodyHash: bodyHash, inertReason: reason}
}

// recordApplyFailure stamps a delivered version the live proxy refused. The
// PREVIOUS org-rail identity is preserved (SetLaneTable is all-or-nothing,
// so whatever was routing still is), and the failure rides alongside it as a
// delivered_unaccepted/capability_mismatch observation.
func (h *gatewayProvidersHandle) recordApplyFailure(version int64) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.applyFailedVersion = version
	h.state.applyRejectCode = orgcontract.ReasonCapabilityMismatch
}

// gatewayFacts is the plain snapshot the P0-6 gateway point reader consumes:
// the org-rail record plus a LIVE read of the local lane table.
type gatewayFacts struct {
	HasOrgRail  bool
	Version     int64
	BodyHash    string
	InertReason string
	// LocalHash is the content address of the live lane table, computed with
	// the SAME algorithm the family's compiler uses. Empty when no lanes are
	// live (the local_effective vs no_policy discriminator, mirroring
	// admitter/egress's hash-presence rule).
	LocalHash string
	// ApplyFailedVersion / ApplyRejectCode surface a delivered table the
	// proxy refused.
	ApplyFailedVersion int64
	ApplyRejectCode    string
}

// Facts snapshots the handle for the P0-6 reader. Nil-safe: a nil handle is
// a node with no gateway lane wiring, which reports none/no_policy.
func (h *gatewayProvidersHandle) Facts() gatewayFacts {
	if h == nil {
		return gatewayFacts{}
	}
	h.mu.Lock()
	st := h.state
	h.mu.Unlock()
	f := gatewayFacts{
		HasOrgRail:         st.hasOrgRail,
		Version:            st.version,
		BodyHash:           st.bodyHash,
		InertReason:        st.inertReason,
		ApplyFailedVersion: st.applyFailedVersion,
		ApplyRejectCode:    st.applyRejectCode,
	}
	if h.liveLanes != nil {
		if lanes, autoDefault := h.liveLanes(); len(lanes) > 0 {
			f.LocalHash = providers.HashLaneTable(lanes, autoDefault)
		}
	}
	return f
}

// applyGatewayProviders downcasts a compiled gateway.providers spec and
// applies it through gw, recording the outcome for the P0-6 reporter. A type
// mismatch — defensive; every real call site only ever passes what
// CompileFamilyBody compiled for family gateway.providers — or a nil handle
// is a silent no-op, mirroring PublishOrgAdmission/PublishOrgEgress's own
// defensive posture. An apply error is logged at Warn and never propagated:
// a bad lane table must never crash the daemon, and the previous lane table
// stays live (SetLaneTable's all-or-nothing contract).
//
// enforceAllowed is the plan §6.4 preauthorization verdict and is HONOURED
// here: an inert body is recorded but NOT applied. policyfam's
// SpecRequestsEnforceMode returns true unconditionally for this family
// precisely because "applying a remote lane table always mutates live proxy
// routing the moment it is accepted" — so a node that has not listed
// gateway.providers in [org_client.policy].preauthorize_enforce must keep
// routing through its own lanes. Before the v2 policy-state work this
// verdict was dropped on the floor and every accepted body was applied,
// which both bypassed the operator's consent gate and left no truthful
// effective-state row to report
// (docs/plans/policy-state-v2-gateway-providers-spec-2026-08-15.md §2.3).
func applyGatewayProviders(gw *gatewayProvidersHandle, res orgclient.PolicyResourceResult, logger *slog.Logger) {
	p, ok := res.Spec.(providers.PolicySpec)
	if !ok {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	if !res.EnforceAllowed {
		gw.recordInert(res.Version, res.BodyHash, res.InertReason)
		logger.Info("policy resource: gateway.providers accepted but INERT — the node's own lanes keep routing",
			"version", res.Version, "reason", res.InertReason)
		return
	}
	if err := gw.Apply(p.UpstreamsAsStringMap(), p.AutoDefaultLane); err != nil {
		gw.recordApplyFailure(res.Version)
		logger.Warn("policy resource: applying gateway.providers lane table failed", "err", err, "version", res.Version)
		return
	}
	gw.recordApplied(res.Version, res.BodyHash)
}

// publishPolicyResourceResult dispatches one family's fetch/LKG outcome
// onto its install seam — the ONE place both the LKG loader and the poller
// call, so the two callers can never diverge on how a PolicyResourceResult
// maps onto Publish/Apply/ClearOrg/Clear. admission.input and
// egress.routing_guardrail install through *obsAdmissionHandle (nil-safe);
// gateway.providers installs through the separate *gatewayProvidersHandle
// (also nil-safe) since it has no obs/admission counterpart at all.
func publishPolicyResourceResult(handle *obsAdmissionHandle, gw *gatewayProvidersHandle, ngov *nodeGovernanceHandle, family string, res orgclient.PolicyResourceResult, logger *slog.Logger) {
	switch res.Status {
	case orgclient.PRApplied, orgclient.PRAppliedInert:
		// Plan §6.10 / GWF-SF1: re-check live enrolment identity immediately
		// before Publish/Apply so a fetch paused after CAS commit cannot
		// install under a since-superseded generation. handle.OrgIdentityMatches
		// is nil-safe (returns false on a nil handle) — this is the SAME
		// identity gate for all three families, including gateway.providers,
		// even though only admission/egress publish onto *handle's own Org
		// layer.
		if !handle.OrgIdentityMatches(res.OrgKey, res.Generation) {
			return
		}
		switch family {
		case policyfam.FamilyAdmissionInput:
			handle.PublishOrgAdmission(res.OrgKey, res.Generation, res.Version, res.BodyHash, res.InertReason, res.EnforceAllowed, res.Spec)
		case policyfam.FamilyEgressGuardrail:
			handle.PublishOrgEgress(res.OrgKey, res.Generation, res.Version, res.BodyHash, res.InertReason, res.EnforceAllowed, res.Spec)
		case policyfam.FamilyGatewayProviders:
			applyGatewayProviders(gw, res, logger)
		case policyfam.FamilyNodeGovernance:
			// Admin-controlled Plane B: recording IS applying (the dashboard
			// route guard and the SPA both read the resolved posture), and
			// the GRANT — not this call — decides whether anything actually
			// takes effect.
			ngov.Apply(res)
		}
	case orgclient.PRNone:
		// Withdrawn (404): clear any previously-installed Org layer.
		clearOrgLayer(handle, gw, ngov, family)
	case orgclient.PRRejected:
		// Plan §4.4: gate rejection keeps prior LKG — EXCEPT an identity
		// change (unenrol/re-enrol), which must ClearOrg (§6.9).
		if res.RejectCode == orgclient.PRRejectIdentityChanged {
			clearOrgLayer(handle, gw, ngov, family)
		}
		// PRUnchanged / PRDeliveredUnaccepted: no Org-layer mutation.
	}
}

// clearOrgLayer clears the install seam for one family — the plan §6.9
// ErrNotEnrolled / generation-mismatch / identity-changed outcome path.
// handle.ClearOrgAdmission/ClearOrgEgress and gw.Clear are all nil-safe, so
// this never needs its own top-level nil guard: a family whose seam isn't
// wired on this node simply no-ops.
func clearOrgLayer(handle *obsAdmissionHandle, gw *gatewayProvidersHandle, ngov *nodeGovernanceHandle, family string) {
	switch family {
	case policyfam.FamilyAdmissionInput:
		handle.ClearOrgAdmission()
	case policyfam.FamilyEgressGuardrail:
		handle.ClearOrgEgress()
	case policyfam.FamilyGatewayProviders:
		gw.Clear()
	case policyfam.FamilyNodeGovernance:
		ngov.Clear()
	}
}

// --- §6.6/§6.9 steady-state poller (item 5) --------------------------------

// policyResourceOutcomeSink records one family's classified fetch outcome
// into the P0-6 policy-state reporter (admitter/egress last-fetch slots).
// Nil is fine — the poller still applies/clears Org layers when the
// [org_client.share].policy_state reporter is off.
type policyResourceOutcomeSink func(family string, o orgclient.PolicyResourceFetchOutcome)

// runPolicyResourcePoller runs the plan §6.6/§6.9 steady-state poll loop for
// every v1 family, applying every outcome onto its install seam through the
// SAME publishPolicyResourceResult/clearOrgLayer dispatch the LKG loader
// (below) uses. Apply happens BEFORE the optional outcome sink poke (plan
// §6.6) so a reporter snapshot never races ahead of the live Org layer.
// Never propagates an error (P1, matching every sibling org poll loop in
// start.go) — a stuck/failing poll must never cancel the proxy, watcher, or
// dashboard. gw may be nil (gateway.providers not appliable on this node,
// e.g. the no_obs build or a node with no proxy wired) — every downstream
// call is nil-safe.
func runPolicyResourcePoller(ctx context.Context, cfg config.Config, oc *orgclient.Client, handle *obsAdmissionHandle, gw *gatewayProvidersHandle, ngov *nodeGovernanceHandle, sink policyResourceOutcomeSink, logger *slog.Logger) {
	if oc == nil || (handle == nil && gw == nil && ngov == nil) {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	opts := policyResourceOptionsFor(cfg)
	logger.Debug("policy resource poller: starting", "families", policyfam.SupportedFamilies)
	_ = oc.PolicyResourcePollLoop(ctx, opts, func(pr orgclient.PolicyResourcePollResult) {
		switch {
		case errors.Is(pr.Err, orgclient.ErrNotEnrolled):
			// Plan §6.9 + Codex SF4: clear Org layer AND let Classify emit
			// Cleared so the outcome sink wipes stale reject/unreachable slots.
			clearOrgLayer(handle, gw, ngov, pr.Family)
		case pr.Err != nil:
			// Transport/auth/indeterminate failure: leave whatever is
			// currently installed alone — a transient unreachability
			// must not tear down a still-valid Org layer. The per-Check
			// identity recheck (activeEnrolmentIdentity) is what fences
			// a layer that has ACTUALLY gone stale; this loop only acts
			// on a decisive fetch outcome. PolicyResourcePollLoop already
			// logs the failure.
		default:
			publishPolicyResourceResult(handle, gw, ngov, pr.Family, pr.Result, logger)
		}
		// Apply-then-poke: classify AFTER the Org-layer mutation so the
		// reporter's next snapshot observes the post-apply state.
		if sink != nil {
			if o, ok := orgclient.ClassifyPolicyResourceFetch(pr.Result, pr.Err); ok {
				sink(pr.Family, o)
			}
		}
	})
}

// --- §6.5 LKG-before-listener (item 4) -------------------------------------

// verifiedCachedResource is one family's on-disk cache envelope, verified
// and compiled (loadPolicyResourceLKG's recovery matrix input).
type verifiedCachedResource struct {
	resource orgcontract.SignedPolicyResource
	spec     any
	digest   string
}

// verifyCachedPolicyResource reads, signature-verifies (against the pinned
// key, when one has ever been recorded), and compiles the on-disk cache
// envelope at path for one family. Any failure — missing file, bad JSON, a
// family mismatch, a bad signature, an unpinned/mismatched key, or a
// compile error — is reported as a single error so the caller's §6.5 "no
// verified cache" branch fires uniformly regardless of which gate failed: a
// corrupt or tampered local cache must never be trusted just because one
// early gate happened to pass.
// nodeAttrs are the CURRENT locally-configured targeting attributes: a
// cached envelope whose signed selectors contradict them is treated exactly
// like an unverifiable cache (design §2 "LKG" — the node's attributes
// changed, so the cached policy no longer applies to it), which sends the
// caller down matrix row 1: blank the ETag and refetch.
func verifyCachedPolicyResource(ctx context.Context, st *store.Store, orgURL, family, path string, nodeAttrs orgcontract.Selectors) (verifiedCachedResource, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return verifiedCachedResource{}, fmt.Errorf("read cache: %w", err)
	}
	var resource orgcontract.SignedPolicyResource
	if err := json.Unmarshal(raw, &resource); err != nil {
		return verifiedCachedResource{}, fmt.Errorf("decode cache: %w", err)
	}
	if resource.Family != family {
		return verifiedCachedResource{}, fmt.Errorf("cache family mismatch: got %q want %q", resource.Family, family)
	}
	// Plan §4.4 closed-envelope on every LKG path (same gates as live accept).
	if resource.ID != "default" || resource.Version <= 0 || resource.CompilerVersion != "v1" {
		return verifiedCachedResource{}, fmt.Errorf("cache closed-envelope violation: id=%q version=%d compiler=%q",
			resource.ID, resource.Version, resource.CompilerVersion)
	}
	// P0-10 Phase B: the same canonical-form gate the live accept path runs
	// (orgcontract.ValidateCanonicalSelectorsJSON), then the same targeting
	// corroboration — against CURRENT config, not whatever was configured
	// when the envelope was cached.
	selectors, selErr := orgcontract.ValidateCanonicalSelectorsJSON(resource.SelectorsJSON)
	if selErr != nil {
		return verifiedCachedResource{}, fmt.Errorf("cache closed-envelope violation: %w", selErr)
	}
	if mismatched, _ := orgcontract.CorroborateSelectors(selectors, nodeAttrs); len(mismatched) > 0 {
		return verifiedCachedResource{}, fmt.Errorf("cached envelope's selectors %s contradict this node's configured attributes on %s",
			resource.SelectorsJSON, strings.Join(mismatched, ","))
	}
	pub, err := orgcontract.VerifyPolicyResource(resource)
	if err != nil {
		return verifiedCachedResource{}, fmt.Errorf("verify signature: %w", err)
	}
	pin, perr := loadPolicyResourceKeyPin(ctx, st, orgURL)
	if perr != nil {
		return verifiedCachedResource{}, fmt.Errorf("load key pin: %w", perr)
	}
	// Codex SF2 / plan §4.4: LKG fail-closed without a recorded pin — never
	// trust a cache under TOFU-absent identity (live fetch may still TOFU).
	if pin == "" {
		return verifiedCachedResource{}, errors.New("no org policy signing key pin recorded — refuse LKG")
	}
	if pin != orgcontract.PublicKeyPinHash(pub) {
		return verifiedCachedResource{}, errors.New("cached envelope's signing key does not match the pinned key")
	}
	spec, _, cerr := policyfam.CompileFamilyBody(family, []byte(resource.Body), orgclient.DefaultMaxPolicyResourceBodyBytes)
	if cerr != nil {
		return verifiedCachedResource{}, fmt.Errorf("compile body: %w", cerr)
	}
	digest, derr := orgcontract.PolicyResourceMessageDigest(resource)
	if derr != nil {
		return verifiedCachedResource{}, fmt.Errorf("digest: %w", derr)
	}
	return verifiedCachedResource{resource: resource, spec: spec, digest: digest}, nil
}

// loadPolicyResourceKeyPin mirrors orgclient's private loadKeyPin (same
// store row: layer="org", path=orgclient.PolicyKeyPinPath(orgURL)) — a
// second, cmd-side reader is used rather than exporting orgclient's
// internal method, since this file already needs its own read-only access
// to the pin row for the LKG cache-verify path and orgclient's copy is
// wired through its unexported *Client receiver.
func loadPolicyResourceKeyPin(ctx context.Context, st *store.Store, orgURL string) (string, error) {
	states, err := st.LatestGuardPolicyStates(ctx)
	if err != nil {
		return "", err
	}
	pinPath := orgclient.PolicyKeyPinPath(orgURL)
	for _, s := range states {
		if s.Layer == "org" && s.Path == pinPath {
			return s.ContentHash, nil
		}
	}
	return "", nil
}

// policyResourceEnforceGate re-derives EnforceAllowed/InertReason (plan
// §6.4) for a cache-installed envelope the same way
// orgclient.acceptPolicyResource does for a freshly-fetched one — the
// durable state row itself carries no EnforceAllowed/InertReason column
// (only floor/last version + hashes), so the LKG install path recomputes
// it from the CURRENT [org_client.policy].preauthorize_enforce config
// every time, exactly like a live fetch would.
func policyResourceEnforceGate(family string, spec any, preauthorizeEnforce []string) (enforceAllowed bool, inertReason string) {
	if policyfam.SpecRequestsEnforceMode(family, spec) && !containsString(family, preauthorizeEnforce) {
		return false, "not_preauthorized"
	}
	return true, ""
}

func containsString(s string, set []string) bool {
	for _, v := range set {
		if v == s {
			return true
		}
	}
	return false
}

// capabilitiesSubsetLKG mirrors orgclient's RequiredCapabilities ⊆ live
// check for the LKG install path (same Gate-5 posture as live accept).
func capabilitiesSubsetLKG(required, live []string) bool {
	if len(required) == 0 {
		return true
	}
	liveSet := make(map[string]struct{}, len(live))
	for _, c := range live {
		liveSet[c] = struct{}{}
	}
	for _, c := range required {
		if _, ok := liveSet[c]; !ok {
			return false
		}
	}
	return true
}

// installPolicyResourceLKG installs an already-verified cache envelope
// (matrix rows 3/4) as the family's Org layer, after re-applying the same
// accept_families / runtime-capability gates live accept uses (plan §4.4 /
// §6.6 — LKG must not resurrect a withdrawn or unrealizable envelope).
func installPolicyResourceLKG(family, orgKey string, generation int64, opts orgclient.PolicyResourceOptions, cached verifiedCachedResource, handle *obsAdmissionHandle, gw *gatewayProvidersHandle, ngov *nodeGovernanceHandle, logger *slog.Logger) {
	if !containsString(family, opts.AcceptFamilies) {
		logger.Info("policy resource LKG: family not in accept_families — leaving uninstalled", "family", family)
		return
	}
	if adm, ok := cached.spec.(admission.PolicySpec); ok {
		if verr := admission.ValidateRuntimeCaps(adm, containsString("judge", opts.LiveCapabilities)); verr != nil {
			logger.Info("policy resource LKG: body-derived capability mismatch — leaving uninstalled",
				"family", family, "err", verr)
			return
		}
	}
	if len(cached.resource.RequiredCapabilities) > 0 && !capabilitiesSubsetLKG(cached.resource.RequiredCapabilities, opts.LiveCapabilities) {
		logger.Info("policy resource LKG: required capabilities not live — leaving uninstalled", "family", family)
		return
	}
	enforceAllowed, inertReason := policyResourceEnforceGate(family, cached.spec, opts.PreauthorizeEnforce)
	status := orgclient.PRApplied
	if !enforceAllowed {
		status = orgclient.PRAppliedInert
	}
	publishPolicyResourceResult(handle, gw, ngov, family, orgclient.PolicyResourceResult{
		Status: status, Version: cached.resource.Version, EnforceAllowed: enforceAllowed,
		InertReason: inertReason, Family: family, OrgKey: orgKey, Generation: generation,
		BodyHash: cached.resource.BodyHash, Spec: cached.spec,
	}, logger)
	logger.Info("policy resource LKG: installed verified cache", "family", family,
		"version", cached.resource.Version, "enforce_allowed", enforceAllowed)
}

// repairAndInstallPolicyResourceLKG runs matrix row 4 (or the no-durable-
// floor case, which is equivalent to a floor of 0): a verified cache ahead
// of the durable floor/state — the exact window between the §6.3 cache
// fsync+rename and the DB CAS commit, e.g. a crash between the two — is
// repaired by re-running the CAS-fenced commit against the verified
// envelope's own version/hash/digest, then installed. Re-validates identity
// and the floor INSIDE the fence (a concurrent writer may have already
// caught up), matching store.WithPolicyResourceFence's own contract.
func repairAndInstallPolicyResourceLKG(ctx context.Context, st *store.Store, orgKey string, generation int64, family string, opts orgclient.PolicyResourceOptions, cached verifiedCachedResource, handle *obsAdmissionHandle, gw *gatewayProvidersHandle, ngov *nodeGovernanceHandle, logger *slog.Logger) {
	commit := store.PolicyResourceCommit{
		Generation: generation, FloorVersion: cached.resource.Version, LastVersion: cached.resource.Version,
		BodyHash: cached.resource.BodyHash, MsgDigest: cached.digest,
	}
	repaired := false
	fenceErr := st.WithPolicyResourceFence(ctx, orgKey, family, func(_ context.Context, fence store.PolicyResourceFence) (*store.PolicyResourceCommit, error) {
		if fence.Tombstoned || fence.Generation != generation {
			return nil, nil // identity changed concurrently — abort, install nothing
		}
		if fence.HasState && cached.resource.Version <= fence.FloorVersion {
			return nil, nil // a concurrent writer already caught up — nothing to repair
		}
		repaired = true
		return &commit, nil
	})
	if fenceErr != nil {
		logger.Warn("policy resource LKG: DB repair failed — leaving family uninstalled", "family", family, "err", fenceErr)
		return
	}
	if !repaired {
		// (nil, nil) abort inside the fence: identity/floor moved — do NOT
		// install the stale cache (Codex B7 / plan §6.5).
		logger.Info("policy resource LKG: repair aborted (identity/floor raced) — leaving family uninstalled", "family", family)
		return
	}
	installPolicyResourceLKG(family, orgKey, generation, opts, cached, handle, gw, ngov, logger)
}

// refetchPolicyResourceLKG runs matrix rows 1/2: the on-disk cache is
// either unverifiable or behind/inconsistent with the durable floor, so it
// must never be installed as-is. The saved ETag is blanked first so the
// refetch can never short-circuit on a 304 (the whole point of this branch
// is that the local cache/DB pair is not trustworthy — a fresh signed body
// must be re-verified from scratch). A refetch failure leaves the family
// with no Org layer (Local/nil), reporting none/no_policy — never
// pending_restart.
func refetchPolicyResourceLKG(ctx context.Context, oc *orgclient.Client, st *store.Store, opts orgclient.PolicyResourceOptions, orgKey string, generation int64, family string, handle *obsAdmissionHandle, gw *gatewayProvidersHandle, ngov *nodeGovernanceHandle, logger *slog.Logger) {
	if err := st.SavePolicyResourceETag(ctx, orgKey, generation, family, ""); err != nil {
		logger.Warn("policy resource LKG: etag clear failed (refetch may still hit a stale 304)", "family", family, "err", err)
	}
	res, err := oc.FetchAndAcceptPolicyResource(ctx, family, opts)
	if err != nil {
		logger.Warn("policy resource LKG: refetch failed — booting with no Org layer for this family", "family", family, "err", err)
		clearOrgLayer(handle, gw, ngov, family)
		return
	}
	publishPolicyResourceResult(handle, gw, ngov, family, res, logger)
	logger.Info("policy resource LKG: refetch outcome", "family", family, "status", res.Status)
}

// loadPolicyResourceLKGFamily runs the plan §6.5 recovery matrix for ONE
// family: verify the on-disk cache, compare it against the durable
// (floor_version, msg_digest) state row, and either install it as-is,
// repair the DB and install, or force a refetch — never installing a
// cryptographically valid cache that sits below the durable replay floor.
func loadPolicyResourceLKGFamily(ctx context.Context, oc *orgclient.Client, st *store.Store, opts orgclient.PolicyResourceOptions, orgURL, orgKey string, generation int64, family string, handle *obsAdmissionHandle, gw *gatewayProvidersHandle, ngov *nodeGovernanceHandle, logger *slog.Logger) {
	cachePath := filepath.Join(opts.CacheDir, orgKey, strconv.FormatInt(generation, 10), family+".json")
	cached, cacheErr := verifyCachedPolicyResource(ctx, st, orgURL, family, cachePath, opts.NodeAttrs)

	stateRow, hasState, err := st.LoadPolicyResourceState(ctx, orgKey, family)
	if err != nil {
		logger.Warn("policy resource LKG: state read failed — leaving family uninstalled", "family", family, "err", err)
		return
	}

	switch {
	case cacheErr != nil:
		// Row 1: no verified cache — force a refetch.
		refetchPolicyResourceLKG(ctx, oc, st, opts, orgKey, generation, family, handle, gw, ngov, logger)
	case !hasState:
		// No durable floor at all (implicitly 0): a verified cache with
		// version > 0 is ahead of it — same repair-then-install path as
		// row 4 below.
		repairAndInstallPolicyResourceLKG(ctx, st, orgKey, generation, family, opts, cached, handle, gw, ngov, logger)
	case cached.resource.Version < stateRow.FloorVersion,
		cached.resource.Version == stateRow.FloorVersion && cached.digest != stateRow.MsgDigest:
		// Row 2: cache behind the durable floor, or an equal-version
		// digest mismatch (replay/corruption) — never install; refetch.
		refetchPolicyResourceLKG(ctx, oc, st, opts, orgKey, generation, family, handle, gw, ngov, logger)
	case cached.resource.Version == stateRow.FloorVersion:
		// Row 3: cache matches the durable floor exactly — install as-is,
		// but only when the state row's generation still matches the live
		// enrolment generation and the family remains accepted (plan §4.4 /
		// §6.6 — LKG must not resurrect a withdrawn accept-list entry).
		if stateRow.Generation != generation {
			logger.Warn("policy resource LKG: state generation mismatch — leaving family uninstalled", "family", family,
				"state_gen", stateRow.Generation, "live_gen", generation)
			return
		}
		installPolicyResourceLKG(family, orgKey, generation, opts, cached, handle, gw, ngov, logger)
	default: // cached.resource.Version > stateRow.FloorVersion
		// Row 4: cache ahead of the durable floor — repair, then install.
		repairAndInstallPolicyResourceLKG(ctx, st, orgKey, generation, family, opts, cached, handle, gw, ngov, logger)
	}
}

// loadPolicyResourceLKG runs the plan §6.5 recovery matrix for every v1
// family, synchronously, exactly once — start.go calls this BEFORE
// launching the proxy listener's goroutine, so a verified Org layer (or an
// honest none/no_policy) is in place before the first proxied request can
// ever be evaluated by Check(). Never returns an error (P1 — a wholly
// offline node, or one with no policy-resource history at all, must still
// boot cleanly): every failure downgrades to "leave this family's install
// seam uninstalled" plus a log line. gw may be nil (gateway.providers not
// appliable on this node) — the loop still runs for admission/egress in
// that case, and vice versa: a nil handle with a non-nil gw still lets
// gateway.providers load.
func loadPolicyResourceLKG(ctx context.Context, cfg config.Config, oc *orgclient.Client, st *store.Store, handle *obsAdmissionHandle, gw *gatewayProvidersHandle, ngov *nodeGovernanceHandle, logger *slog.Logger) {
	if oc == nil || (handle == nil && gw == nil && ngov == nil) {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	enr, err := st.LoadEnrolment(ctx)
	if err != nil || enr == nil {
		return // not enrolled — nothing to load, every family stays Local/nil
	}
	orgKey := orgclient.OrgKey(enr.OrgServerURL, enr.OrgID)
	genRow, ok, err := st.LoadEnrolmentGeneration(ctx, orgKey)
	if err != nil || !ok || genRow.Tombstoned {
		return // no live generation for this identity — nothing to load
	}
	opts := policyResourceOptionsFor(cfg)
	for _, family := range policyfam.SupportedFamilies {
		loadPolicyResourceLKGFamily(ctx, oc, st, opts, enr.OrgServerURL, orgKey, genRow.Generation, family, handle, gw, ngov, logger)
	}
}
