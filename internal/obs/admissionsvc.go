package obs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmutapp/superbased-observer/internal/obs/admission"
	"github.com/marmutapp/superbased-observer/internal/obs/egress"
	obsstore "github.com/marmutapp/superbased-observer/internal/obs/store"
)

// admissionsvc.go is the boundary layer for the pure internal/obs/admission
// engine (admission spec §2): it holds the compiled policy (atomic hot-reload),
// the verdict cache, the injected judge, and the store, and turns an
// admission.Result into an SDK/proxy response + an obs_admission_events row. The
// pure package's types don't leak past here.
//
// The config→admission.PolicySpec translation lives at the wiring point
// (cmd/observer/obs_wire.go), NOT here — so this package stays free of
// internal/config, honoring the obs boundary.

// AdmissionJudge is the host judge binding for admission, bound at obs_wire to
// the same chatCompletionsJudge that serves eval (§5 — a second consumer, not
// a new host interface). It is admission's OWN interface (defined in the pure
// package) so admission never imports eval.
type AdmissionJudge = admission.JudgeClient

// AdmissionOptions configures a service at construction (from the wiring point).
type AdmissionOptions struct {
	// Hosting is the judge-hosting label recorded on each verdict
	// (local | provider | aggregator | private | off).
	Hosting string
	// JudgeTimeout bounds one judge call. 0 → a safe default (1500ms).
	JudgeTimeout time.Duration
	// CacheTTL is the verdict-cache lifetime. 0 → 15 minutes.
	CacheTTL time.Duration
	// SecretDetect is the injected pattern-certain secret detector for the
	// §3 layer-3 gate (scrub.CertainSecretTypes at the wiring point). Nil
	// leaves the gate inert — this package imports no scrub.
	SecretDetect func(string) []string
	// BudgetEnabled + BudgetWeeklyUSD + BudgetMonthlyUSD configure the
	// per-end-user spend gate (docs/guardrails.md; org-budget plan §1). The
	// budgeted subject is an end-user of the org-hosted app (obs_traces.user);
	// a breach yields a Deny verdict folded stricter-wins into the response.
	// Zero value = off; a 0 cap disables that window.
	BudgetEnabled     bool
	BudgetFiveHourUSD float64
	BudgetWeeklyUSD   float64
	BudgetMonthlyUSD  float64
}

// AdmissionService is the boundary wiring: one owner of the verdict cache and
// the compiled policy pointer. Safe for concurrent use.
type AdmissionService struct {
	store  *obsstore.Store
	judge  admission.JudgeClient // may be nil (no judge wired — judged criteria skipped)
	gate   ContentGate           // reuse the existing content posture
	logger *slog.Logger

	hosting      string
	judgeTimeout time.Duration
	cacheTTL     time.Duration
	secretDetect func(string) []string

	budgetEnabled bool
	budget5h      float64
	budgetWeekly  float64
	budgetMonthly float64

	policy atomic.Pointer[admission.PolicySpec]

	// egressPolicy is the compiled Plane-A egress policy (G22), folded into
	// Check the same way the budget gate is (design §3.2). nil ⇒ egress off.
	// Evaluated UNCACHED (it depends on user/cohort/model/budget/size, which
	// the verdict cache key deliberately excludes).
	egressPolicy atomic.Pointer[egress.PolicySpec]

	cacheMu sync.Mutex
	cache   map[string]cachedVerdict

	// egressSessionMu guards egressSessions, the coarse per-session model-pin
	// state for the §3.6 switch cooldown (finding 11): a soft (cost/cohort/
	// size) model switch that would move off the model the session last
	// served is held until the cooldown elapses.
	egressSessionMu sync.Mutex
	egressSessions  map[string]egressSessionState

	// budgetCache is the per-end-user spend cache for the budget gate — kept
	// SEPARATE from the text+policy verdict cache (which keys on message hash
	// + policy hash, ignoring the user): a per-user budget verdict must never
	// poison another user's cached message verdict.
	budgetMu    sync.Mutex
	budgetCache map[string]budgetSpendEntry
}

type cachedVerdict struct {
	res admission.Result
	at  time.Time
}

type budgetSpendEntry struct {
	fiveHour, weekly, monthly float64
	at                        time.Time
}

// egressSessionState is the coarse per-session model-pin state: the model the
// session last effectively served (baseline for the next switch decision) and
// when a switch last fired (for the cooldown).
type egressSessionState struct {
	lastModel string
	switchAt  time.Time
}

// defaultEgressCooldown bounds a same-session budget-band model switch when the
// policy sets no cooldown_seconds. maxEgressSessions bounds the state map.
const (
	defaultEgressCooldown = 5 * time.Minute
	maxEgressSessions     = 4096
)

// budgetSpendTTL bounds staleness of a cached per-end-user spend lookup so the
// admission gate never runs a DB query per request under load. maxBudgetUsers
// bounds the cache map.
const (
	budgetSpendTTL = 30 * time.Second
	maxBudgetUsers = 1024
)

// NewAdmissionService builds a service. judge may be nil. The policy starts
// empty (off) until SetPolicy is called at wiring time.
func NewAdmissionService(store *obsstore.Store, judge admission.JudgeClient, gate ContentGate, logger *slog.Logger, opts AdmissionOptions) *AdmissionService {
	if logger == nil {
		logger = slog.Default()
	}
	jt := opts.JudgeTimeout
	if jt <= 0 {
		jt = 1500 * time.Millisecond
	}
	ct := opts.CacheTTL
	if ct <= 0 {
		ct = 15 * time.Minute
	}
	return &AdmissionService{
		store: store, judge: judge, gate: gate, logger: logger,
		hosting: opts.Hosting, judgeTimeout: jt, cacheTTL: ct,
		secretDetect:   opts.SecretDetect,
		budgetEnabled:  opts.BudgetEnabled,
		budget5h:       opts.BudgetFiveHourUSD,
		budgetWeekly:   opts.BudgetWeeklyUSD,
		budgetMonthly:  opts.BudgetMonthlyUSD,
		cache:          map[string]cachedVerdict{},
		budgetCache:    map[string]budgetSpendEntry{},
		egressSessions: map[string]egressSessionState{},
	}
}

// SetEgressPolicy atomically installs a compiled Plane-A egress policy
// (hot-reload). A nil-equivalent (mode off) leaves egress inert.
func (s *AdmissionService) SetEgressPolicy(p egress.PolicySpec) {
	s.egressPolicy.Store(&p)
}

// EgressPolicy returns the installed egress policy, or false when none is set.
func (s *AdmissionService) EgressPolicy() (egress.PolicySpec, bool) {
	p := s.egressPolicy.Load()
	if p == nil {
		return egress.PolicySpec{}, false
	}
	return *p, true
}

// EgressMode returns the current egress mode token ("off" when unset).
func (s *AdmissionService) EgressMode() string {
	p, ok := s.EgressPolicy()
	if !ok || p.Mode == "" {
		return egress.ModeOff
	}
	return p.Mode
}

// SetPolicy atomically installs a compiled policy (hot-reload) and records its
// content-addressed version. The verdict cache is keyed partly on the policy
// hash, so a policy change auto-invalidates stale verdicts.
func (s *AdmissionService) SetPolicy(ctx context.Context, p admission.PolicySpec) {
	s.policy.Store(&p)
	if s.store != nil && p.Hash != "" {
		_ = s.store.UpsertPolicyVersion(ctx, p.Hash, p.Mode.String(), p.Scope.String(), len(p.Criteria), "")
	}
}

// Policy returns the installed policy, or false when none is set (admission off).
func (s *AdmissionService) Policy() (admission.PolicySpec, bool) {
	p := s.policy.Load()
	if p == nil {
		return admission.PolicySpec{}, false
	}
	return *p, true
}

// Enabled reports whether admission is installed and not off.
func (s *AdmissionService) Enabled() bool {
	p, ok := s.Policy()
	return ok && p.Mode != admission.ModeOff
}

// Mode returns the current mode token ("off" when unset).
func (s *AdmissionService) Mode() string {
	p, ok := s.Policy()
	if !ok {
		return "off"
	}
	return p.Mode.String()
}

// JudgeHosting returns the configured judge-hosting label.
func (s *AdmissionService) JudgeHosting() string {
	if s.hosting == "" {
		return "off"
	}
	return s.hosting
}

// judgeRemote reports whether the judge egresses off the node — any hosting
// that is not loopback-local or off. The §3 layer-3 secret gate only engages
// for a remote judge (a local judge carries no egress risk).
func (s *AdmissionService) judgeRemote() bool {
	return s.hosting != "" && s.hosting != "local" && s.hosting != "off"
}

// AdmissionCheck is the incoming request to gate.
type AdmissionCheck struct {
	Text    string
	Tenant  string
	User    string
	Session string
	TraceID string
	// RequestID soft-joins the verdict to a proxy api_turn for enrichment.
	RequestID string
	// Model is the pre-mutation requested model (design §3.4) — an egress
	// matcher input. Empty ⇒ model-keyed egress matchers do not fire.
	Model string
	// Provider is the incoming wire shape (anthropic|openai) — an egress
	// matcher input.
	Provider string
	// PromptTokensEst is a coarse incoming prompt-size band for egress
	// overload/degrade rules (§3.4 caveat). 0 ⇒ size matchers do not fire.
	PromptTokensEst int
	// Persist=false runs a dry-run (CLI `admission test`) that records nothing.
	Persist bool
}

// EgressOutcome is the plain Plane-A egress directive the boundary computed for
// a request (G22). The zero value / nil = no egress action. It is carried on
// AdmissionResponse so the wiring point can translate it into the proxy's plain
// route contract (design §3.3) — the egress package's types never leak past
// here.
type EgressOutcome struct {
	// Matched is true when a rule fired (recorded to obs_egress_decisions).
	Matched bool
	// Mode is the egress mode this decision was evaluated under (advise|enforce).
	Mode string
	// Action is the resolved verb (none|route_upstream|route_model|set_effort|deny).
	Action string
	// Block is true when a statically-known-invalid target forced an obs-side
	// Block in enforce mode (design §3.6 / finding 1).
	Block         bool
	UpstreamID    string
	TargetURL     string
	TargetShape   string
	Model         string
	Effort        string
	Reason        string
	ReasonCode    string
	RuleName      string
	PolicyHash    string
	OnUnavailable string
	MustUseTarget bool
	SwitchHeld    bool
	// DecisionID is the obs_egress_decisions row id (0 when not persisted). The
	// proxy carries it back on the realized-outcome callback (G22 wave 2) so the
	// row's applied/fail_closed record what the proxy ACTUALLY realized, not
	// just the decision-time intent.
	DecisionID int64
}

// AdmissionResponse is what the SDK/proxy caller sees.
type AdmissionResponse struct {
	Allowed   bool   `json:"allowed"`
	Decision  string `json:"decision"`
	Severity  string `json:"severity"`
	Criterion string `json:"criterion,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Mode      string `json:"mode"`
	JudgeUsed bool   `json:"judge_used"`
	Degraded  string `json:"degraded,omitempty"`
	LatencyMS int    `json:"latency_ms"`
	// EnforceDecision is the decision that WOULD apply under enforce mode, so a
	// dry-run / observe caller can preview enforcement without it taking effect.
	EnforceDecision string `json:"enforce_decision,omitempty"`
	// Egress is the Plane-A egress directive (G22), nil when egress is off or
	// no rule fired. The wiring point translates it into the proxy route
	// contract; in advise mode it is logged/recorded but never applied.
	Egress *EgressOutcome `json:"-"`
}

// Check evaluates a request against the current policy, records a verdict
// (unless Persist=false), and returns the caller response.
//
// Enforcement (admission spec §6): in OBSERVE mode the response always allows
// while the shadow verdict is still recorded (zero user-visible effect); in
// ENFORCE mode the caller sees the real decision — Ask/Deny block, while Flag
// is an admit-but-record verdict that still admits (§4). EnforceDecision always
// previews what enforce would decide, so an observe caller can gauge readiness.
func (s *AdmissionService) Check(ctx context.Context, c AdmissionCheck) AdmissionResponse {
	p, ok := s.Policy()
	if !ok || p.Mode == admission.ModeOff {
		return AdmissionResponse{Allowed: true, Decision: "allow", Mode: "off"}
	}

	start := time.Now()
	text := strings.TrimSpace(c.Text)
	msgHash := hashMessage(text)
	cacheKey := msgHash + ":" + p.Hash

	res, cached := s.cacheGet(cacheKey)
	if !cached {
		ectx := ctx
		if s.judge != nil {
			var cancel context.CancelFunc
			ectx, cancel = context.WithTimeout(ctx, s.judgeTimeout)
			defer cancel()
		}
		res = admission.Evaluate(ectx, admission.Request{
			Text: text, Tenant: c.Tenant, User: c.User, Session: c.Session,
		}, p, s.judge, admission.WithSecretGate(s.secretDetect, s.judgeRemote()))
		s.cachePut(cacheKey, res)
	} else if res.Degraded == "" {
		res.Degraded = "cache"
	}

	// Per-end-user budget gate (org-hosted-app model): evaluated OUTSIDE the
	// text+policy verdict cache above (it depends on the end-user's accumulated
	// spend, not the message) and folded stricter-wins. Its own 30s per-user
	// spend cache keeps it cheap; it fails open — a lookup error never blocks.
	if s.budgetEnabled && c.User != "" && (s.budget5h > 0 || s.budgetWeekly > 0 || s.budgetMonthly > 0) {
		if h, w, m, ok := s.cachedUserSpend(ctx, c.User); ok {
			if bres, fired := admission.BudgetVerdict(h, w, m, s.budget5h, s.budgetWeekly, s.budgetMonthly); fired && bres.Decision > res.Decision {
				res = bres
			}
		}
	}

	latency := int(time.Since(start).Milliseconds())

	// Observe admits everything; enforce blocks Ask/Deny (Flag admits). The
	// recorded row carries the ACTUAL mode so the audit trail distinguishes a
	// shadow verdict from an enforced one.
	modeStr := p.Mode.String()
	allowed := true
	if p.Mode == admission.ModeEnforce {
		allowed = res.Decision < admission.DecisionAsk
	}

	if c.Persist && s.store != nil {
		s.record(ctx, p, c, res, msgHash, latency, modeStr)
	}

	reason := res.Reason

	// Plane-A egress routing (G22) — folded in after the verdict + budget,
	// UNCACHED (design §3.2). In advise mode the directive is evaluated,
	// logged, and recorded but NEVER applied (allowed is untouched). In enforce
	// mode a terminal egress deny or a statically-invalid target converts to a
	// block; the plain directive is carried on the response for the proxy to
	// apply (translated at the wiring boundary).
	egressOut := s.evaluateEgress(ctx, c, res, msgHash)
	if egressOut != nil && egressOut.Mode == egress.ModeEnforce {
		if egressOut.Block || egressOut.Action == string(egress.ActionDeny) {
			allowed = false
			if egressOut.Reason != "" {
				reason = egressOut.Reason
			}
		}
	}

	return AdmissionResponse{
		Allowed:         allowed,
		Decision:        res.Decision.String(),
		Severity:        res.Severity.String(),
		Criterion:       res.Criterion,
		Reason:          reason,
		Mode:            modeStr,
		JudgeUsed:       res.JudgeUsed,
		Degraded:        res.Degraded,
		LatencyMS:       latency,
		EnforceDecision: res.Decision.String(),
		Egress:          egressOut,
	}
}

// BudgetStatus reports the per-end-user budget gate's configured state for the
// status surfaces (the CLI + the dashboard card). The spend totals themselves
// come from the store; this exposes only the wiring-time caps the service holds.
func (s *AdmissionService) BudgetStatus() (enabled bool, fiveHour, weekly, monthly float64) {
	return s.budgetEnabled, s.budget5h, s.budgetWeekly, s.budgetMonthly
}

// cachedUserSpend returns one end-user's rolling-7-day and calendar-month
// spend from obs_spans (via obs_traces.user), cached per user for
// budgetSpendTTL so the budget gate never runs a DB query per request under
// load. ok=false on a nil store, empty user, or lookup error — the gate then
// fails open (a budget query failure never blocks a request).
func (s *AdmissionService) cachedUserSpend(ctx context.Context, user string) (fiveHour, weekly, monthly float64, ok bool) {
	if s.store == nil || user == "" {
		return 0, 0, 0, false
	}
	now := time.Now().UTC()
	s.budgetMu.Lock()
	e, hit := s.budgetCache[user]
	s.budgetMu.Unlock()
	if hit && now.Sub(e.at) >= 0 && now.Sub(e.at) <= budgetSpendTTL {
		return e.fiveHour, e.weekly, e.monthly, true
	}
	fiveHourStart := now.Add(-5 * time.Hour)
	weekStart := now.AddDate(0, 0, -7)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	h, err := s.store.UserSpend(ctx, user, fiveHourStart)
	if err != nil {
		s.logger.Warn("admission budget: 5h spend lookup failed", "err", err)
		return 0, 0, 0, false
	}
	w, err := s.store.UserSpend(ctx, user, weekStart)
	if err != nil {
		s.logger.Warn("admission budget: weekly spend lookup failed", "err", err)
		return 0, 0, 0, false
	}
	m, err := s.store.UserSpend(ctx, user, monthStart)
	if err != nil {
		s.logger.Warn("admission budget: monthly spend lookup failed", "err", err)
		return 0, 0, 0, false
	}
	s.budgetMu.Lock()
	if len(s.budgetCache) >= maxBudgetUsers {
		for k := range s.budgetCache { // bounded map: evict an arbitrary entry
			delete(s.budgetCache, k)
			break
		}
	}
	s.budgetCache[user] = budgetSpendEntry{fiveHour: h, weekly: w, monthly: m, at: now}
	s.budgetMu.Unlock()
	return h, w, m, true
}

// record persists one verdict row, gating the reason excerpt behind the node's
// ContentGate (the raw request is never stored — only its hash). mode is the
// actual posture the verdict was evaluated under (observe | enforce).
func (s *AdmissionService) record(ctx context.Context, p admission.PolicySpec, c AdmissionCheck, res admission.Result, msgHash string, latency int, mode string) {
	reason := ""
	if s.gate != nil && s.gate.AllowsRawContent() {
		reason = res.Reason
	}
	_, err := s.store.InsertAdmissionEvent(ctx, obsstore.AdmissionEventRow{
		TS:            time.Now().UTC(),
		Mode:          mode,
		Decision:      res.Decision.String(),
		Severity:      res.Severity.String(),
		CriterionID:   res.Criterion,
		PolicyHash:    p.Hash,
		JudgeUsed:     res.JudgeUsed,
		JudgeHosting:  s.JudgeHosting(),
		Degraded:      res.Degraded,
		LatencyMS:     latency,
		TraceID:       c.TraceID,
		SessionID:     c.Session,
		Tenant:        c.Tenant,
		User:          c.User,
		RequestID:     c.RequestID,
		MessageHash:   msgHash,
		ReasonExcerpt: reason,
	})
	if err != nil {
		s.logger.Warn("admission: record verdict failed", "err", err)
	}
}

// evaluateEgress runs the Plane-A egress policy (UNCACHED, design §3.2) after
// the verdict + budget. It computes BudgetBurnMax/BudgetKnown from the reused
// per-end-user spend, resolves the cohort, applies the session pin/cooldown,
// converts a statically-invalid enforce target to a Block, persists the
// decision, and returns the plain outcome (nil when egress is off or no rule
// fired). It fails open: nothing here can block a turn that admission allowed
// except a deliberate enforce-mode deny/invalid-target.
func (s *AdmissionService) evaluateEgress(ctx context.Context, c AdmissionCheck, res admission.Result, msgHash string) *EgressOutcome {
	spec, ok := s.EgressPolicy()
	if !ok || spec.Mode == egress.ModeOff || spec.Mode == "" {
		return nil
	}

	burn, known := s.egressBudgetBurn(ctx, c.User)
	sessionModel, cooldownElapsed := s.egressSessionInputs(c.Session, spec)

	d := egress.Evaluate(egress.Input{
		VerdictDecision: res.Decision.String(),
		VerdictSeverity: res.Severity.String(),
		Criterion:       res.Criterion,
		Model:           c.Model,
		Provider:        c.Provider,
		User:            c.User,
		Cohort:          spec.CohortFor(c.User),
		BudgetBurnMax:   burn,
		BudgetKnown:     known,
		PromptTokensEst: c.PromptTokensEst,
		SessionID:       c.Session,
		SessionModel:    sessionModel,
		CooldownElapsed: cooldownElapsed,
	}, spec)
	if !d.Matched {
		return nil
	}

	enforce := spec.Mode == egress.ModeEnforce
	// A statically-KNOWN-invalid target (unknown id slipping past compile) is
	// an obs-side Block in enforce mode (design §3.6 / finding 1) — a route to a
	// resource that does not exist must not silently leak to the default.
	block := enforce && d.Action == egress.ActionRouteUpstream && !d.TargetKnown

	out := &EgressOutcome{
		Matched:       true,
		Mode:          spec.Mode,
		Action:        string(d.Action),
		Block:         block,
		UpstreamID:    d.UpstreamID,
		TargetURL:     d.TargetURL,
		TargetShape:   string(d.TargetShape),
		Model:         d.Model,
		Effort:        string(d.Effort),
		Reason:        d.Reason,
		ReasonCode:    string(d.ReasonCode),
		RuleName:      d.RuleName,
		PolicyHash:    d.PolicyHash,
		OnUnavailable: d.OnUnavailable,
		MustUseTarget: d.MustUseTarget,
		SwitchHeld:    d.SwitchHeld,
	}

	s.updateEgressSession(c.Session, c.Model, d)

	if !enforce {
		// Advise: log the directive so a shadow rollout is visible; never apply.
		s.logger.Info("egress: advise (not applied)",
			"rule", d.RuleName, "action", string(d.Action), "reason_code", string(d.ReasonCode),
			"upstream", d.UpstreamID, "model_to", d.Model, "held", d.SwitchHeld, "user", c.User)
	}

	if c.Persist && s.store != nil {
		out.DecisionID = s.recordEgress(ctx, c, res, msgHash, d, spec.Mode, block)
	}
	return out
}

// RecordEgressRealized records the outcome the proxy actually realized for a
// prior egress decision (G22 wave 2). It routes through the wiring boundary
// (cmd/observer/obs_wire.go's reporter → this method → the store), so
// internal/proxy never imports internal/obs. A zero id / nil store is a no-op.
// applied = the rewritten request actually went to the directed target/model
// and got a non-error response; failClosed = a MustUseTarget locality route was
// refused because its target was unavailable at runtime; outcome is a short
// closed-vocabulary label for the audit view.
func (s *AdmissionService) RecordEgressRealized(ctx context.Context, id int64, applied, failClosed bool, outcome string) {
	if s.store == nil || id == 0 {
		return
	}
	if err := s.store.UpdateEgressRealized(ctx, id, applied, failClosed, outcome); err != nil {
		s.logger.Warn("egress: record realized outcome failed", "id", id, "err", err)
	}
}

// egressBudgetBurn computes the max spend-burn fraction across the configured
// budget windows that have a POSITIVE cap, plus whether spend was actually
// resolved (design §4 normalization). A 0 cap contributes no fraction (never
// divides by zero); missing identity or a lookup failure ⇒ known=false, burn=0
// (spend-unavailable, distinct from a real zero-spend which is known=true).
// Values above 1 are preserved (an over-cap user matches budget_band = 1.0).
func (s *AdmissionService) egressBudgetBurn(ctx context.Context, user string) (float64, bool) {
	if user == "" || (s.budget5h <= 0 && s.budgetWeekly <= 0 && s.budgetMonthly <= 0) {
		return 0, false
	}
	h, w, m, ok := s.cachedUserSpend(ctx, user)
	if !ok {
		return 0, false
	}
	burn := 0.0
	for _, p := range []struct{ spend, cap float64 }{{h, s.budget5h}, {w, s.budgetWeekly}, {m, s.budgetMonthly}} {
		if p.cap > 0 {
			if f := p.spend / p.cap; f > burn {
				burn = f
			}
		}
	}
	return burn, true
}

// egressSessionInputs returns the model the session last effectively served and
// whether the switch cooldown has elapsed (§3.6 pin/cooldown, coarse v1). No
// session id ⇒ no tracking, so a switch is never held.
func (s *AdmissionService) egressSessionInputs(session string, spec egress.PolicySpec) (lastModel string, cooldownElapsed bool) {
	if session == "" {
		return "", true
	}
	cooldown := time.Duration(spec.CooldownSeconds) * time.Second
	if cooldown <= 0 {
		cooldown = defaultEgressCooldown
	}
	s.egressSessionMu.Lock()
	st, ok := s.egressSessions[session]
	s.egressSessionMu.Unlock()
	if !ok {
		return "", true
	}
	return st.lastModel, st.switchAt.IsZero() || time.Since(st.switchAt) >= cooldown
}

// updateEgressSession records the model the session effectively served this
// turn (the switched-to model when a switch applied, else the requested model)
// and stamps the switch time only when a switch actually fired — the cooldown
// baseline for the next turn.
func (s *AdmissionService) updateEgressSession(session, requestedModel string, d egress.Directive) {
	if session == "" {
		return
	}
	s.egressSessionMu.Lock()
	defer s.egressSessionMu.Unlock()
	if len(s.egressSessions) >= maxEgressSessions {
		for k := range s.egressSessions { // bounded map: evict an arbitrary entry
			delete(s.egressSessions, k)
			break
		}
	}
	st := s.egressSessions[session]
	if d.Action == egress.ActionRouteModel && d.Model != "" {
		st.lastModel = d.Model
		st.switchAt = time.Now()
	} else if requestedModel != "" {
		st.lastModel = requestedModel
	}
	s.egressSessions[session] = st
}

// recordEgress persists one egress decision to the node-local
// obs_egress_decisions chain and returns the row id (0 on failure). The
// realized annotations applied/fail_closed start at their decision-time
// BASELINE (false): they track what the proxy ACTUALLY realized on the wire and
// are set later by RecordEgressRealized via the proxy→obs callback (G22 wave 2).
// advise mode never routes, so its row stays applied=false; a blocked/held
// directive never reaches the proxy route path, so it stays false too. `applied`
// therefore means "the routing directive was realized on the wire and
// succeeded" — the refusal path (deny/block) conveys its outcome through
// mode+action instead.
func (s *AdmissionService) recordEgress(ctx context.Context, c AdmissionCheck, res admission.Result, msgHash string, d egress.Directive, mode string, block bool) int64 {
	action := string(d.Action)
	if action == "" {
		action = "none"
	}
	id, err := s.store.InsertEgressDecision(ctx, obsstore.EgressDecisionRow{
		TS:              time.Now().UTC(),
		Mode:            mode,
		RuleName:        d.RuleName,
		PolicyHash:      d.PolicyHash,
		Action:          action,
		UpstreamID:      d.UpstreamID,
		TargetShape:     string(d.TargetShape),
		ModelFrom:       c.Model,
		ModelTo:         d.Model,
		Effort:          string(d.Effort),
		ReasonCode:      string(d.ReasonCode),
		MustUseTarget:   d.MustUseTarget,
		Applied:         false, // realized-outcome baseline; set by the proxy callback
		FailClosed:      false,
		SwitchHeld:      d.SwitchHeld,
		Degraded:        d.Degraded,
		VerdictDecision: res.Decision.String(),
		CriterionID:     res.Criterion,
		MessageHash:     msgHash,
		RequestID:       c.RequestID,
		SessionID:       c.Session,
		Tenant:          c.Tenant,
		User:            c.User,
	})
	if err != nil {
		s.logger.Warn("egress: record decision failed", "err", err)
		return 0
	}
	return id
}

func (s *AdmissionService) cacheGet(key string) (admission.Result, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	cv, ok := s.cache[key]
	if !ok || time.Since(cv.at) > s.cacheTTL {
		return admission.Result{}, false
	}
	return cv.res, true
}

func (s *AdmissionService) cachePut(key string, res admission.Result) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	// Bound the cache defensively; a naive clear on overflow is fine — the
	// cache is a latency optimization, not a correctness dependency.
	if len(s.cache) >= 4096 {
		s.cache = map[string]cachedVerdict{}
	}
	s.cache[key] = cachedVerdict{res: res, at: time.Now()}
}

// hashMessage is the stable message hash used in the verdict row + cache key.
// The raw request text is never stored.
func hashMessage(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
