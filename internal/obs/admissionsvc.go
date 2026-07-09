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

	cacheMu sync.Mutex
	cache   map[string]cachedVerdict

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
		secretDetect:  opts.SecretDetect,
		budgetEnabled: opts.BudgetEnabled,
		budget5h:      opts.BudgetFiveHourUSD,
		budgetWeekly:  opts.BudgetWeeklyUSD,
		budgetMonthly: opts.BudgetMonthlyUSD,
		cache:         map[string]cachedVerdict{},
		budgetCache:   map[string]budgetSpendEntry{},
	}
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
	// Persist=false runs a dry-run (CLI `admission test`) that records nothing.
	Persist bool
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

	return AdmissionResponse{
		Allowed:         allowed,
		Decision:        res.Decision.String(),
		Severity:        res.Severity.String(),
		Criterion:       res.Criterion,
		Reason:          res.Reason,
		Mode:            modeStr,
		JudgeUsed:       res.JudgeUsed,
		Degraded:        res.Degraded,
		LatencyMS:       latency,
		EnforceDecision: res.Decision.String(),
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
