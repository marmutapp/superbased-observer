package egress

// Action is the primary egress verb a rule resolves to (exactly one per rule,
// lint-enforced). The zero value ActionNone is "no egress action".
type Action string

const (
	// ActionNone is the zero value — no egress directive.
	ActionNone Action = ""
	// ActionRouteUpstream selects an alternate declared upstream (the
	// operator's "another resource").
	ActionRouteUpstream Action = "route_upstream"
	// ActionRouteModel swaps to a same-shape model (an explicit cheaper model
	// in v1).
	ActionRouteModel Action = "route_model"
	// ActionSetEffort degrades effort — best-effort, provider-compatible only.
	ActionSetEffort Action = "set_effort"
	// ActionDeny is a terminal refusal (fail-closed data-locality).
	ActionDeny Action = "deny"
)

// ProviderShape is the incoming wire shape. v1 is anthropic|openai only —
// Gemini is excluded (design finding 17): admission's extractLastUserText sends
// non-OpenAI providers through the Anthropic parser, so a Gemini body yields no
// text and skips the gate.
type ProviderShape string

const (
	ShapeAnthropic ProviderShape = "anthropic"
	ShapeOpenAI    ProviderShape = "openai"
)

// validShape reports whether s is a v1-supported provider shape.
func validShape(s string) bool {
	return s == string(ShapeAnthropic) || s == string(ShapeOpenAI)
}

// Effort is the Plane-A effort-degrade vocabulary, mirroring the routing effort
// names as a local enum (no internal/routing import).
type Effort string

const (
	EffortMinimal Effort = "minimal"
	EffortLow     Effort = "low"
	EffortMedium  Effort = "medium"
	EffortHigh    Effort = "high"
)

// validEffort reports whether s is a known effort level.
func validEffort(s string) bool {
	switch Effort(s) {
	case EffortMinimal, EffortLow, EffortMedium, EffortHigh:
		return true
	default:
		return false
	}
}

// OnUnavailable is the per-rule fail posture when a routed target is
// unavailable at runtime. The proxy owns the runtime enforcement (design §3.6 /
// finding 1); obs only expresses the intent.
const (
	OnUnavailableFailOpen = "fail_open"
	OnUnavailableDeny     = "deny"
)

// ReasonCode is the closed Plane-A reason vocabulary recorded on every egress
// decision row — never free text (design §4).
type ReasonCode string

const (
	ReasonFlaggedLocal       ReasonCode = "egress_flagged_local"
	ReasonBudgetBand         ReasonCode = "egress_budget_band"
	ReasonCohortUpstream     ReasonCode = "egress_cohort_upstream"
	ReasonOverloadDegrade    ReasonCode = "egress_overload_degrade"
	ReasonSensitiveLocalOnly ReasonCode = "egress_sensitive_local_only"
	ReasonDenyUnavailable    ReasonCode = "egress_deny_unavailable"
	ReasonFailOpen           ReasonCode = "egress_fail_open"
	ReasonNoRoute            ReasonCode = "egress_no_route"
	ReasonSwitchHeld         ReasonCode = "egress_switch_held"
	ReasonEffortNoop         ReasonCode = "egress_effort_noop"
)

// validReasonCode reports whether s is a member of the closed reason enum.
func validReasonCode(s string) bool {
	switch ReasonCode(s) {
	case ReasonFlaggedLocal, ReasonBudgetBand, ReasonCohortUpstream,
		ReasonOverloadDegrade, ReasonSensitiveLocalOnly, ReasonDenyUnavailable,
		ReasonFailOpen, ReasonNoRoute, ReasonSwitchHeld, ReasonEffortNoop:
		return true
	default:
		return false
	}
}

// Target is a typed upstream target: an id, its URL, and its declared wire
// shape. A shape is REQUIRED for any enforce-mode route_to_upstream (design
// finding 6) — [proxy.upstreams] entries are bare URLs with no shape, which
// cannot safely authorize a body/model rewrite.
type Target struct {
	ID    string
	URL   string
	Shape ProviderShape
}

// Input carries only match inputs — never raw request text (design §3.1).
type Input struct {
	// VerdictDecision is the admission decision (allow|flag|ask|deny).
	VerdictDecision string
	// VerdictSeverity is the admission severity (info|warn|high|critical).
	VerdictSeverity string
	// Criterion is the fired criterion id ("" = none).
	Criterion string
	// Model is the requested (pre-mutation) top-level model (§3.4).
	Model string
	// Provider is the incoming wire shape — v1: anthropic or openai only.
	Provider string
	// User is the end-user identity ("" = anonymous).
	User string
	// Cohort is the boundary-resolved cohort ("" = none).
	Cohort string
	// BudgetBurnMax is the max burn fraction across the user's windows (§4).
	BudgetBurnMax float64
	// BudgetKnown is true only when spend was actually resolved — distinguishes
	// zero-spend from spend-unavailable (§4).
	BudgetKnown bool
	// PromptTokensEst is a coarse size band for overload/degrade rules,
	// measured on the pre-mutation envelope (§3.4 caveat).
	PromptTokensEst int
	// SessionID is the session for pin/cooldown (§3.6).
	SessionID string
	// SessionModel is the model already served earlier in this session, if
	// known ("" = none) — the pinning input.
	SessionModel string
	// CooldownElapsed reports whether the per-policy cooldown window has elapsed
	// since this session's last model switch. A soft (cost/cohort/size) switch
	// is held when this is false; hard (verdict/criterion/severity) switches
	// never hold (§3.6 / finding 11).
	CooldownElapsed bool
}

// Directive is the plain result; the zero value = no egress action.
type Directive struct {
	// Matched is true when a rule fired (even a no_route exemption or a held
	// switch), so the boundary records a decision row. false = no rule matched.
	Matched bool
	// Action is the resolved primary verb (ActionNone when a matched rule is a
	// no_route exemption or a held switch).
	Action Action
	// UpstreamID is the target id for a route_upstream directive.
	UpstreamID string
	// TargetURL is the resolved target URL — set only for a KNOWN-valid id.
	TargetURL string
	// TargetShape is the declared shape of the resolved target, carried so the
	// proxy can enforce same-shape without re-deriving it.
	TargetShape ProviderShape
	// TargetKnown is false when Action==route_upstream but the id has no
	// compiled target (statically-invalid); the boundary converts this to a
	// Block in enforce mode (design §3.6 / finding 1).
	TargetKnown bool
	// Model is the same-shape model for a route_model directive.
	Model string
	// Effort is the target effort level for a set_effort directive.
	Effort Effort
	// Reason is the user-facing message on a deny.
	Reason string
	// ReasonCode is the closed-enum reason for this directive.
	ReasonCode ReasonCode
	// RuleName is the rule that fired.
	RuleName string
	// PolicyHash is the compiled egress policy hash (attribution across a hot
	// reload; recorded on every decision row).
	PolicyHash string
	// OnUnavailable is the runtime fail posture (fail_open|deny) the proxy
	// honors for a route_upstream.
	OnUnavailable string
	// MustUseTarget is set when the rule is a locality/sensitive rule the proxy
	// must honor through retries and fallback (§3.6). Accompanies
	// OnUnavailable=deny.
	MustUseTarget bool
	// SwitchHeld is true when a same-session model switch was held by the
	// cooldown (finding 11); Action is then ActionNone.
	SwitchHeld bool
	// Degraded is an auditable degrade note ("" = none).
	Degraded string
}
