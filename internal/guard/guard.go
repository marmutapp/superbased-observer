package guard

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/policy"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// GuardErrorRuleID is the synthetic rule ID stamped on verdicts (and
// audit rows) produced by the Q2 failure wrapper instead of a real
// evaluation — a recovered panic or a degraded policy layer. It is
// not a catalog rule; dashboards and the CLI treat it as a guard
// health signal.
const GuardErrorRuleID = "guard_error"

// maxProjectEngines bounds the per-project engine cache. Projects per
// daemon are typically O(10); the bound only guards against a
// pathological watcher feeding thousands of synthetic roots. On
// overflow new roots evaluate with the base engine (correct, minus
// any project-layer escalations for those roots).
const maxProjectEngines = 64

// Options configures Guard construction. All I/O inputs (home dir,
// known roots) are resolved by the caller — composition stays at the
// cmd layer.
type Options struct {
	// Config is the loaded [guard] section.
	Config config.GuardConfig
	// Home is the daemon user's home directory (native path flavor),
	// used for "~" expansion in policy paths and pattern matching.
	Home string
	// KnownProjectRoots is the daemon's observed-project root set —
	// R-151's cross-project-bleed reference. Snapshot at
	// construction; projects created later join on the next daemon
	// start (documented approximation; engines are cheap but the
	// set's churn doesn't justify a live feed in G3).
	KnownProjectRoots []string
	// ReadFile overrides policy-file reading in tests. Nil means
	// os.ReadFile.
	ReadFile func(path string) ([]byte, error)
	// OnPolicyState, when non-nil, is invoked once per successfully
	// loaded policy layer — including project layers that load
	// LAZILY on first event for their root, which a one-shot
	// PolicyStates() poll at startup would miss. The cmd composition
	// wires it to store.RecordGuardPolicyState (the §14.4
	// policy-change log); guard itself never touches the store.
	// Called outside guard locks; implementations may do I/O but
	// must not call back into the Guard.
	OnPolicyState func(PolicyState)
	// Notifier, when non-nil, receives desktop alerts for verdicts
	// meeting the [guard.alerts] threshold (MaybeAlert). The cmd
	// composition wires guard/notify.Desktop; nil disables alerting
	// (tests, alerts.desktop=false).
	Notifier Notifier
	// OrgKeyPinHash, when non-empty, is the sha256 hex of the org
	// policy public key pinned at enrolment
	// (orgcontract.PublicKeyPinHash; the guard_policy_state key-pin
	// row). The org bundle loader then requires the cached envelope's
	// embedded key to match the pin in addition to the self-contained
	// signature check. The daemon composition (guardwire) reads the
	// pin from the store and sets this; hook processes leave it empty
	// — they still verify the envelope's signature against its
	// embedded key (catching corruption and casual tampering) but
	// skip the pin comparison rather than pay a DB open before the
	// hook reply (§6.4). The wire seam (orgclient fetch) is where
	// untrusted bytes enter and ALWAYS pin-checks before writing the
	// cache, so a hook process only ever reads a file that already
	// passed the full check once.
	OrgKeyPinHash string
}

// Notifier is the alert channel seam (guard spec §3.1: notify owns
// all alerting). Implementations must be best-effort and non-blocking
// in spirit — MaybeAlert is called off hot paths (after replies /
// persists), but a notifier still must not hang its caller.
type Notifier interface {
	Notify(title, body string)
}

// PolicyState describes one loaded policy layer for the §14.4
// policy-change log. The caller (cmd composition) persists these via
// store.RecordGuardPolicyState — guard does not import store.
type PolicyState struct {
	// Layer is "user", "project" or "org".
	Layer string
	// Path is the source file location (the bundle cache path for the
	// org layer).
	Path string
	// Version is the org bundle version for the org layer; empty for
	// local file layers (they have no version concept beyond the
	// content hash).
	Version string
	// ContentHash is the sha256 hex of the policy source bytes — for
	// the org layer that is the bundle TOML itself, not the envelope,
	// so the hash lines up with the fetch-time row the org client
	// records.
	ContentHash string
}

// Guard is the composition root: engines + taint tracker + failure
// wrapper. Safe for concurrent use (immutable engines; mutex-guarded
// caches and trackers).
type Guard struct {
	cfg      config.GuardConfig
	home     string
	roots    []string
	readFile func(string) ([]byte, error)

	// onPolicyState is Options.OnPolicyState (may be nil).
	onPolicyState func(PolicyState)

	// set holds the ENTIRE org-derived engine snapshot behind ONE
	// atomic.Pointer (P0-7 hot-reload): the base engine, the parsed
	// org + user layers, the builtin/org/user layer states + rule
	// categories, and the per-snapshot lazy project-engine cache.
	// Every hot-path read Loads it lock-free; ReloadOrgLayer builds a
	// FRESH engineSet against a re-verified org bundle and Stores it,
	// so an accepted org policy applies to the LIVE decision engine
	// in-process with no daemon restart. In-flight Evaluate calls keep
	// the snapshot they Loaded (a consistent view); the swapped-in set
	// starts with an empty project cache that rebuilds lazily against
	// the new org layer.
	set atomic.Pointer[engineSet]
	// reloadMu serializes ReloadOrgLayer construct+publish so a
	// concurrent reload can never publish an OLDER snapshot over a
	// newer one (the production caller is single-threaded — the mutex
	// is defensive and makes the concurrency test honest).
	reloadMu sync.Mutex
	// orgKeyPinHash + orgBundlePath are IMMUTABLE after New — the
	// re-verification inputs ReloadOrgLayer feeds back through the
	// SAME §14.2 acceptance path (signature + optional key pin) New
	// used, so a reload verifies identically to construction.
	orgKeyPinHash string
	orgBundlePath string

	// mu guards issues only — the engine snapshot (base, layers,
	// project cache, states, categories) lives behind g.set.
	mu     sync.Mutex
	issues []string

	taint *taintTracker

	// egressAllow are the compiled [guard.proxy].egress_allow value
	// patterns (proxyguard.go); invalid patterns degrade to LoadIssues.
	egressAllow []*regexp.Regexp
	// proxyMu guards proxySeen, the proxy seam's per-session
	// record-dedup state (proxyguard.go). Separate from mu: the proxy
	// hot path must never contend with engine-cache loads.
	proxyMu   sync.Mutex
	proxySeen map[string]*proxySeen

	// notifier + alertMin implement [guard.alerts] (MaybeAlert).
	notifier Notifier
	alertMin policy.Severity

	// approvals is the §6.3 grant lookup (SetApprovalLookup); nil =
	// no approvals, every blocking verdict enforces.
	approvals ApprovalLookup

	// mcpPins is the §9.2 pin-status lookup (SetMCPPinLookup); nil =
	// every MCP server marks mcp_unpinned taint (the G3 baseline).
	mcpPins MCPPinLookup
	// watches are the watched-config-path triggers — ONE mechanism
	// with two consumers (the §9.2 MCP re-scan and the §13.2 dialect
	// drift check), keyed by consumer tag. Registered once at
	// composition (SetMCPRescan / SetDialectRescan) before traffic;
	// read on the ingest path (mcp.go maybeConfigRescan).
	watches map[string]configWatch

	// Budget state (§12.1, budget.go): the injected spend lookup, its
	// TTL cache, and the once-per-session flag-record dedup.
	budgetLookup   BudgetLookup
	budgetMu       sync.Mutex
	budgetCache    map[string]budgetEntry
	budgetRecorded map[string]map[string]bool
	// repeats is the A-610 consecutive-identical tracker (§12.2,
	// repeat.go) feeding Event.RepeatCount on the ingest path.
	repeats repeatTracker
}

// New constructs a Guard from the loaded configuration. Construction
// is deliberately tolerant of POLICY-FILE problems (a malformed user
// policy degrades to built-ins with the issue recorded — the daemon
// must never refuse to start over a policy typo; `observer guard
// lint` is the strict checker) but strict about CONFIG problems
// (unknown mode strings error: that's an operator typo in
// config.toml, the same class the config loader rejects).
func New(opts Options) (*Guard, error) {
	mode, err := policy.ParseMode(modeOrDefault(opts.Config.Mode))
	if err != nil {
		return nil, fmt.Errorf("guard.New: %w", err)
	}
	g := &Guard{
		cfg:           opts.Config,
		home:          opts.Home,
		roots:         opts.KnownProjectRoots,
		readFile:      opts.ReadFile,
		onPolicyState: opts.OnPolicyState,
		orgKeyPinHash: opts.OrgKeyPinHash,
		orgBundlePath: OrgBundlePath(opts.Config, opts.Home),
		taint:         newTaintTracker(opts.Config.Taint),
		watches:       make(map[string]configWatch),
	}
	if g.readFile == nil {
		g.readFile = os.ReadFile
	}
	// [guard.alerts].min_severity: parse once; an empty/unknown value
	// falls back to the documented "high" default (the config loader
	// validates the field, so unknown only happens on hand-built
	// Options in tests).
	g.alertMin = policy.SeverityHigh
	if s, err := policy.ParseSeverity(opts.Config.Alerts.MinSeverity); err == nil {
		g.alertMin = s
	}
	if opts.Config.Alerts.Desktop {
		g.notifier = opts.Notifier
	}
	// [guard.proxy].egress_allow: compile once; bad patterns degrade
	// (skipped + recorded) per the policy-file failure posture.
	var allowIssues []string
	g.egressAllow, allowIssues = compileEgressAllow(opts.Config.Proxy.EgressAllow)
	g.issues = append(g.issues, allowIssues...)

	// Layers are accumulated into LOCALS (not g fields) and sealed
	// into the initial engineSet at the end — the snapshot is the one
	// owner of the engine state (P0-7).
	var orgLayer, userLayer *policyFile
	var states []PolicyState

	// Org layer (G13): read + verify + parse the locally cached,
	// signed policy bundle. Absence is normal (not enrolled, no
	// bundle published, or pre-G13 server); every failure degrades to
	// local-only policy with the issue recorded — the §14.2 compat
	// posture mirrors the user-layer failure posture below.
	if g.orgBundlePath != "" {
		pf, st, issue, loaded := g.parseOrgBundle(g.orgBundlePath, opts.OrgKeyPinHash)
		if issue != "" {
			g.issues = append(g.issues, issue)
		}
		if loaded {
			orgLayer = pf
			states = append(states, st)
		}
	}

	// User layer: read + parse the configured policy file. Absence
	// is normal (no user policy); a read/parse failure degrades to
	// built-ins with the issue recorded (doc.go failure posture).
	userPath := UserPolicyPath(opts.Config, opts.Home)
	if userPath != "" {
		raw, err := g.readFile(userPath)
		switch {
		case err == nil:
			pf, perr := parsePolicyFile(raw, layerUser)
			if perr != nil {
				g.issues = append(g.issues, fmt.Sprintf("user policy %s: %v", userPath, perr))
			} else {
				userLayer = pf
				states = append(states, PolicyState{Layer: layerUser, Path: userPath, ContentHash: sha256hex(raw)})
			}
		case os.IsNotExist(err):
			// no user policy — built-ins only
		default:
			g.issues = append(g.issues, fmt.Sprintf("user policy %s: %v", userPath, err))
		}
	}

	base, err := g.buildEngine(mode, orgLayer, userLayer, nil)
	if err != nil {
		// A layer that breaks ENGINE construction (e.g. an override
		// on an unknown rule) degrades the same way a parse failure
		// does: drop the offending layer, keep the innocent one.
		// Isolate the culprit by retrying without org, then without
		// user, then without both — the first combination that builds
		// wins, and every dropped layer records an issue (and its
		// state, so PolicyStates never reports a dropped layer).
		firstErr := err
		built := false
		if orgLayer != nil {
			if b, e := g.buildEngine(mode, nil, userLayer, nil); e == nil {
				g.issues = append(g.issues, fmt.Sprintf("org policy layer dropped: %v", firstErr))
				orgLayer, base, built = nil, b, true
				states = dropLayerState(states, layerOrg)
			}
		}
		if !built && userLayer != nil {
			if b, e := g.buildEngine(mode, orgLayer, nil, nil); e == nil {
				g.issues = append(g.issues, fmt.Sprintf("user policy layer dropped: %v", firstErr))
				userLayer, base, built = nil, b, true
				states = dropLayerState(states, layerUser)
			}
		}
		if !built {
			if orgLayer != nil || userLayer != nil {
				g.issues = append(g.issues, fmt.Sprintf("org+user policy layers dropped: %v", firstErr))
			}
			orgLayer, userLayer, states = nil, nil, nil
			b, e := g.buildEngine(mode, nil, nil, nil)
			if e != nil {
				return nil, fmt.Errorf("guard.New: built-in engine: %w", e)
			}
			base = b
		}
	}

	g.set.Store(newEngineSet(base, orgLayer, userLayer, states, buildRuleCategories(orgLayer, userLayer)))

	// Fire OnPolicyState for the SURVIVING layer states (a dropped
	// layer is never reported as loaded). Project layers load lazily
	// and fire from engineFor.
	if g.onPolicyState != nil {
		for i := range states {
			g.onPolicyState(states[i])
		}
	}
	return g, nil
}

// modeOrDefault maps an empty config mode to the D2 default.
func modeOrDefault(s string) string {
	if strings.TrimSpace(s) == "" {
		return string(policy.ModeObserve)
	}
	return s
}

// buildEngine assembles a policy.Engine from config + the passed org +
// user layers + an optional project layer, applying the §4.6 one-way
// layering merge (org floor on top). The layers are PARAMETERS (not g
// fields) so New, ReloadOrgLayer, and the per-snapshot project-engine
// build all funnel through this one seam against an explicit layer set
// (B3 — no hidden read of mutable guard state).
func (g *Guard) buildEngine(mode policy.Mode, org, user, project *policyFile) (*policy.Engine, error) {
	extra, overrides, mergeIssues := mergeLayers(org, user, project)
	g.recordIssues(mergeIssues)
	// Boundary slices pass through as-is: nil (section absent) lets
	// the engine defaults apply; an explicitly-empty TOML list means
	// "none" — the decoder already produces exactly this distinction.
	return policy.New(policy.Config{
		Mode:              mode,
		Home:              g.home,
		AllowPaths:        g.cfg.Boundary.AllowPaths,
		ProtectedBranches: g.cfg.Boundary.ProtectedBranches,
		KnownProjectRoots: g.roots,
		Disabled:          g.cfg.Rules.Disable,
		ExtraRules:        extra,
		Overrides:         overrides,
		// [guard.budget] (§12.1): thresholds for the B-601..B-604 $ rows;
		// hard upgrades their enforce-mode decision to deny. The
		// [guard.budget.window] utilization thresholds feed the
		// B-610..B-613 limit rows (CategoryLimit — untouched by hard).
		BudgetSessionUSD:    g.cfg.Budget.SessionUSD,
		BudgetDailyUSD:      g.cfg.Budget.DailyUSD,
		BudgetWeeklyUSD:     g.cfg.Budget.WeeklyUSD,
		BudgetMonthlyUSD:    g.cfg.Budget.MonthlyUSD,
		BudgetHard:          g.cfg.Budget.Hard,
		LimitUtil5hWarn:     g.cfg.Budget.Window.Util5hWarn,
		LimitUtil5hDeny:     g.cfg.Budget.Window.Util5hDeny,
		LimitUtilWeeklyWarn: g.cfg.Budget.Window.UtilWeeklyWarn,
		LimitUtilWeeklyDeny: g.cfg.Budget.Window.UtilWeeklyDeny,
		// R-172's shell-arg row runs the typed certain-only detector
		// (Module rule 1: scrub arrives as an injected func — policy
		// imports zero observer packages).
		SecretDetect: scrub.CertainSecretTypes,
	})
}

// recordIssues appends load-time issues under the Guard's lock.
func (g *Guard) recordIssues(issues []string) {
	if len(issues) == 0 {
		return
	}
	g.mu.Lock()
	g.issues = append(g.issues, issues...)
	g.mu.Unlock()
}

// LoadIssues returns the policy-load problems recorded so far (parse
// failures, dropped relaxation attempts). The caller logs them and
// emits a guard_error audit row per issue.
func (g *Guard) LoadIssues() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, len(g.issues))
	copy(out, g.issues)
	return out
}

// PolicyStates returns the loaded policy-layer descriptors for the
// §14.4 policy-change log (persisted by the caller through
// store.RecordGuardPolicyState). Read off the live snapshot: after a
// successful ReloadOrgLayer the org entry carries the just-accepted
// version + hash (the pair guardRunningFromGuard reads).
func (g *Guard) PolicyStates() []PolicyState {
	return g.set.Load().policyStates()
}

// Mode returns the active global mode.
func (g *Guard) Mode() policy.Mode { return g.set.Load().base.Mode() }

// RuleCount returns the base engine's active rule-row count (status
// surfaces).
func (g *Guard) RuleCount() int { return g.set.Load().base.RuleCount() }

// MaybeAlert fires a desktop notification for a verdict that meets
// the [guard.alerts] threshold (desktop enabled at construction +
// severity >= min_severity + an actual rule hit). Called by the
// persistence sites AFTER replies/persists — never on the latency-
// critical half of a hot path. Best-effort by the Notifier contract.
//
// Volume note: the default min_severity "high" keeps this to the
// rare verdicts worth interrupting a human for; if the P0 soak shows
// alert fatigue the threshold (not this mechanism) is the knob.
func (g *Guard) MaybeAlert(v ActionVerdict) {
	if g.notifier == nil || v.Verdict.RuleID == "" {
		return
	}
	if v.Verdict.Severity < g.alertMin && !v.GuardError {
		return
	}
	title := "Observer Guard: " + v.Verdict.RuleID + " " + v.Verdict.Decision.String()
	body := v.Verdict.Reason
	if v.Input.Tool != "" {
		body = v.Input.Tool + ": " + body
	}
	g.notifier.Notify(title, body)
}

// EffectiveRules returns the BASE engine's effective rule rows
// (built-ins + user layer, with overrides applied) for `observer
// guard rules --effective`. Project layers are per-root and lazy;
// the CLI surfaces the base set plus the loaded project states.
func (g *Guard) EffectiveRules() []policy.RuleInfo {
	return g.set.Load().base.RuleInfos()
}

// CategoryFor returns the category of a rule ID for audit-row
// attribution ("" when unknown; GuardErrorRuleID reports "guard" so
// wrapper rows filter as guard-health signals). The CLI's unpaired
// call Loads its own snapshot; the in-package Evaluate+CategoryFor hot
// paths thread the SAME snapshot via categoryWith (the NIT
// same-evaluation-snapshot contract).
func (g *Guard) CategoryFor(ruleID string) string {
	return g.categoryWith(g.set.Load(), ruleID)
}

// Evaluate is the guard-level evaluation seam: it stamps nothing,
// reads nothing — it picks the project-appropriate engine and applies
// the Q2 failure wrapper around the pure evaluation. The returned
// guardErr is non-nil when the wrapper engaged (recovered panic); the
// verdict is then the fail-open allow (or fail-closed deny under
// [guard] strict) carrying GuardErrorRuleID, and the caller must
// surface it as an audit row.
//
// Callers that own cross-event state (the ingest seam, the hook
// handler) populate ev.Taint BEFORE calling — see EvaluateActions for
// the ingest path that does both. The public entry Loads one snapshot;
// in-package callers that also categorize should use evaluateWith +
// categoryWith with a single Loaded snapshot (the NIT contract).
func (g *Guard) Evaluate(ev policy.Event) (verdict policy.Verdict, guardErr error) {
	return g.evaluateWith(g.set.Load(), ev)
}

// evaluateWith evaluates ev against an ALREADY-LOADED snapshot so the
// caller can pair it with categoryWith on the same snapshot (the
// same-evaluation-snapshot contract). The Q2 failure wrapper is
// applied here.
func (g *Guard) evaluateWith(es *engineSet, ev policy.Event) (verdict policy.Verdict, guardErr error) {
	defer func() {
		if r := recover(); r != nil {
			guardErr = fmt.Errorf("guard.Evaluate: recovered: %v", r)
			verdict = g.failureVerdict(guardErr)
		}
	}()
	return es.engineFor(g, ev.ProjectRoot).Evaluate(ev), nil
}

// categoryWith returns the category of ruleID off an ALREADY-LOADED
// snapshot — the in-package pair for evaluateWith so a hot path never
// does a second g.set.Load() between evaluating and categorizing.
func (g *Guard) categoryWith(es *engineSet, ruleID string) string {
	if ruleID == GuardErrorRuleID {
		return "guard"
	}
	return es.categoryFor(ruleID)
}

// failureVerdict builds the Q2 wrapper verdict: fail-open allow by
// default, fail-closed deny under [guard] strict. Severity is high
// either way — a guard internal error is always worth surfacing.
func (g *Guard) failureVerdict(err error) policy.Verdict {
	v := policy.Verdict{
		Decision: policy.DecisionAllow,
		RuleID:   GuardErrorRuleID,
		Severity: policy.SeverityHigh,
		Reason:   "guard internal error (fail-open): " + err.Error(),
		Advice:   "Run `observer guard lint` and check the daemon log; report a reproducer if the panic persists.",
		Source:   policy.SourceBuiltin,
	}
	if g.cfg.Strict {
		v.Decision = policy.DecisionDeny
		v.Reason = "guard internal error (fail-closed, [guard] strict=true): " + err.Error()
	}
	return v
}

// UserPolicyPath resolves the configured [guard.rules] user_policy
// location with ~ expanded against home — the exact resolution New
// applies when loading the layer. Exported so editing surfaces (the
// dashboard policy editor) target the same file the guard loads;
// empty when no user policy is configured.
func UserPolicyPath(cfg config.GuardConfig, home string) string {
	return expandUserPath(cfg.Rules.UserPolicy, home)
}

// ProjectPolicyPath resolves a project root's policy-file location
// ([guard.rules] project_policy, relative to the root) — the exact
// resolution loadProjectEngine applies. Empty when either part is
// unconfigured.
func ProjectPolicyPath(cfg config.GuardConfig, projectRoot string) string {
	if projectRoot == "" || cfg.Rules.ProjectPolicy == "" {
		return ""
	}
	return filepath.Join(projectRoot, filepath.FromSlash(cfg.Rules.ProjectPolicy))
}

// OrgBundlePath resolves the configured [guard.rules] org_bundle
// cache location with ~ expanded against home — the exact resolution
// New applies before loadOrgBundle. Empty when unconfigured.
func OrgBundlePath(cfg config.GuardConfig, home string) string {
	return expandUserPath(cfg.Rules.OrgBundle, home)
}

// expandUserPath expands a leading "~/" against home. Empty input
// stays empty (no user policy configured).
func expandUserPath(p, home string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home == "" {
			return ""
		}
		return filepath.Join(home, p[2:])
	}
	return p
}
