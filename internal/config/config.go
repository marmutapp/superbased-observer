package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/marmutapp/superbased-observer/internal/notify/digest"
	"github.com/marmutapp/superbased-observer/internal/notify/email"
)

// Config is the root configuration for the observer. Field defaults are set
// by Default(). Partial TOML files (including missing sections) are supported
// — unspecified fields retain their defaults.
type Config struct {
	Observer     ObserverConfig     `toml:"observer"`
	Proxy        ProxyConfig        `toml:"proxy"`
	Dashboard    DashboardConfig    `toml:"dashboard"`
	Compression  CompressionConfig  `toml:"compression"`
	Intelligence IntelligenceConfig `toml:"intelligence"`
	OrgClient    OrgClientConfig    `toml:"org_client"`
	Exporter     ExporterConfig     `toml:"exporter"`
	Ingest       IngestConfig       `toml:"ingest"`
	CacheTrack   CacheTrackConfig   `toml:"cachetrack"`
	CacheWarm    CacheWarmConfig    `toml:"cachewarm"`
	Predict      PredictConfig      `toml:"predict"`
	Browser      BrowserConfig      `toml:"browser"`
	Handoff      HandoffConfig      `toml:"handoff"`
	Terminal     TerminalConfig     `toml:"terminal"`
	CodeIntel    CodeIntelConfig    `toml:"codeintel"`
	Advisor      AdvisorConfig      `toml:"advisor"`
	Routing      RoutingConfig      `toml:"routing"`
	Guard        GuardConfig        `toml:"guard"`
	Profiles     ProfilesConfig     `toml:"profiles"`
	// Benchmark is the [benchmark] surface — the Benchmarks Harness
	// (docs/plans/benchmarks-harness-plan-2026-07-11.md). LOCAL-ONLY. Holds
	// only the retention horizon for the node-local benchmark_* tables today;
	// per-run spend caps live in the run spec's [budget] block, not here.
	Benchmark BenchmarkConfig `toml:"benchmark"`
	// Email is the [email] surface — the shared SMTP notification channel
	// (gap-register G9). LOCAL-ONLY, never distributed. OPT-IN: default off
	// (a fired alert makes an outbound SMTP call). Reused by the node-side
	// obs-alert loop; the org server has its own [email] block. See
	// internal/notify/email.
	Email email.Config `toml:"email"`
	// Digest is the [digest] surface — the scheduled personal cost digest
	// (gap-register G13): a weekly/monthly per-tool/per-model spend rollup
	// emailed through the shared [email] channel. LOCAL-ONLY. OPT-IN: default
	// off, and additionally requires [email].enabled. See internal/notify/digest.
	Digest digest.Config `toml:"digest"`
	// Observability is the [observability] surface — the generalized
	// observability subsystem (internal/obs). OPT-IN: default false.
	Observability ObservabilityConfig `toml:"observability"`
	// AggregateShare is the [aggregate_share] surface — the opt-in aggregate
	// rail (docs/plans/g25-optin-aggregate-rail-design-2026-07-11.md). OPT-IN,
	// default OFF in every path (zero value AND the loader's partial-merge
	// default): it is the product's first first-party network call outside the
	// Teams org-push, so the rail is inert unless the operator explicitly
	// consents via `observer aggregate enable`. Deliberately TOP-LEVEL, not
	// under [org_client], because it is org-independent (works for solo nodes).
	AggregateShare AggregateShareConfig `toml:"aggregate_share"`
	// Remote is the [remote] surface — remote dashboard access
	// (docs/plans/remote-dashboard-access-plan-2026-07-12.md). LOCAL-ONLY,
	// never distributed via [org_client.share] and never a server-forced
	// toggle (the node operator owns exposure entirely — mirrors the org-push
	// posture). OPT-IN, default OFF in every path: exposure is the first
	// network-facing node surface, so it is inert unless the operator
	// configures it. Holds the security substrate settings + the Phase-0
	// [remote.notify] outbound-notification sub-block.
	Remote RemoteConfig `toml:"remote"`
	// Experiments is the [[experiments]] list — productized profile
	// A/B runs (usability arc P6.4). See experiments.go.
	Experiments []ExperimentConfig `toml:"experiments"`
}

// RemoteConfig is the [remote] surface — the remote-dashboard-access security
// substrate + notification rail (remote-dashboard-access plan §5). LOCAL-ONLY:
// never distributed via [org_client.share], never a server-forced toggle. The
// zero value (no [remote] section) is fully off — loopback-only, today's
// behaviour — and the partial-merge default keeps it that way.
type RemoteConfig struct {
	// Enabled is the master switch. false ⇒ loopback-only (today's
	// behaviour); a non-loopback bind fails closed (plan §4.6). Even when
	// true, no listener binds non-loopback until Phase 2 wires the exposure
	// mode — Phase 1 assembles the substrate without exposing anything.
	Enabled bool `toml:"enabled"`
	// Mode selects the exposure transport: off | tailscale | lan. Never a
	// bare "0.0.0.0" (plan §5). Phase 1 accepts only "off".
	Mode string `toml:"mode"`
	// BindAddr is the EXPLICIT IP:port to bind in lan mode (plan §5 — never
	// 0.0.0.0, never an interface name). Empty in off/tailscale mode.
	BindAddr string `toml:"bind_addr"`
	// TailscaleBackendAddr is the dedicated LOOPBACK IP:port the tailnet-serve
	// backend binds in tailscale mode (plan §4.4 Phase 2). It MUST be a
	// loopback address distinct from the owner-trusted direct dashboard
	// listener: `tailscale serve` terminates TLS remotely and forwards
	// plaintext here, so this listener requires auth for EVERY request (no
	// RemoteAddr bypass) and is the ONLY place forwarded-identity headers are
	// consumed. Empty in off/lan mode; `observer remote enable --tailscale`
	// pins a randomized free loopback port here.
	TailscaleBackendAddr string `toml:"tailscale_backend_addr"`
	// TrustedHosts are extra Host-header allow-list entries for the
	// browserGuard Host check on a remote-exposed bind (plan §4.5). Never
	// "allow any Host".
	TrustedHosts []string `toml:"trusted_hosts"`
	// RequireTLS mandates TLS for ALL remote access (plan §5 — no plain-HTTP
	// data path). Default true; not waivable for a data-bearing view.
	RequireTLS bool `toml:"require_tls"`
	// AllowTerminal gates the execute-tier remote terminal surface (Phase 4).
	// Default false — off even for an execute-capable user until enabled.
	AllowTerminal bool `toml:"allow_terminal"`
	// AllowTerminalView is the independent READ opt-in for remote VIEWING of
	// attach/resume terminals (session-attach design §3.2, Phase 4). It is
	// STRICTLY WEAKER than AllowTerminal (write): an attach/resume PTY binds a
	// daemon-owned terminal to a REAL external transcript whose TUI can echo
	// secrets/customer data, so a remote paired device is denied both the
	// snapshot row AND the websocket subscription by default. Turning this on
	// lets a remote caller SEE (subscribe read-only to) such sessions; driving
	// them still requires AllowTerminal AND the full execute-tier writer-acquire
	// conjunction. Default false (deny). Never distributed; never server-forced.
	AllowTerminalView bool `toml:"allow_terminal_view"`
	// AllowStandingTerminalControl is the OPT-IN master switch for the standing
	// terminal-control secret (standing-terminal-access §B): a single durable,
	// hashed-at-rest secret that lets a paired device re-acquire writer control
	// across websocket refreshes WITHOUT a fresh per-terminal owner approval.
	// Default false. It is a STRICT SUPERSET of risk over the single-use flow —
	// anyone holding the secret + a paired session controls EVERY terminal — so
	// it lives beside allow_terminal (which it also requires) and is off until
	// the operator consciously mints a secret from the local dashboard. The raw
	// secret is never stored: its argon2id hash lives in a 0600 sibling file of
	// the pairing secret (remotecfg.StandingTerminalSecretPath).
	AllowStandingTerminalControl bool `toml:"allow_standing_terminal_control"`
	// RevokeStandingOnTakeover is an OPT-IN hardening of standing access.
	// When true, a LOCAL (desktop) writer takeover of a remote writer that
	// held control through the standing credential also revokes that
	// credential itself (the same teardown as the dashboard explicit revoke
	// — verifier hot-disabled, remote writers dropped, credential file
	// removed). Default false — the seamless posture: takeover revokes only
	// the live lease and the paired device may re-acquire later. Local-only;
	// never distributed.
	RevokeStandingOnTakeover bool `toml:"revoke_standing_on_takeover"`
	// WriterLeaseIdleMinutes is the idle lifetime of a remote writer lease
	// (Phase 4 §4.α.2c), refreshed on each Write/Resize. Default 5.
	WriterLeaseIdleMinutes int `toml:"writer_lease_idle_minutes"`
	// WriterLeaseMaxMinutes is the hard cap on a remote writer lease's lifetime
	// (§4.α.2c) after which the holder must re-acquire. Default 30.
	WriterLeaseMaxMinutes int `toml:"writer_lease_max_minutes"`
	// RateLimitPerMin caps auth attempts (code-server-class). Default 6.
	// 0 disables limiting for the PAIRING endpoint only; the standing
	// terminal-control verifier clamps <=0 back to 6/min (each standing attempt
	// costs a 19 MiB argon2 compute, so it is never unlimited).
	RateLimitPerMin int `toml:"rate_limit_per_min"`
	// SessionTTLMinutes is the device-session lifetime (plan §4.3). Default 720
	// (12h); a session older than this is rejected.
	SessionTTLMinutes int `toml:"session_ttl_minutes"`
	// SessionIdleMinutes is the device-session idle timeout. Default 60.
	SessionIdleMinutes int `toml:"session_idle_minutes"`
	// MaxSessions caps concurrent device sessions (plan §4.3). Default 5.
	MaxSessions int `toml:"max_sessions"`
	// Notify is the Phase-0 [remote.notify] outbound-notification sub-block.
	Notify RemoteNotifyConfig `toml:"notify"`
}

// RemoteNotifyConfig is the [remote.notify] sub-block — the Phase-0
// outbound-notification rail (plan §7 Phase 0). Opt-in, default off; the only
// outbound call this plan adds pre-relay, invoked from the dashboard/lifecycle
// layer, never the capture path (pinned by
// tests/invariant/remotenotify_boundary_test.go).
type RemoteNotifyConfig struct {
	// Enabled gates the rail. Default false ⇒ no outbound call.
	Enabled bool `toml:"enabled"`
	// Kind is the transport: webhook | ntfy. Default "webhook".
	Kind string `toml:"kind"`
	// URL is the delivery endpoint (a webhook receiver or an ntfy topic URL).
	URL string `toml:"url"`
	// Events subscribes to lifecycle events. Empty ⇒ all known events.
	// Default ["session_blocked", "session_finished"].
	Events []string `toml:"events"`
}

// ObservabilityConfig is the [observability] surface — the generalized
// observability subsystem (internal/obs; plan
// docs/plans/generalized-observability-custom-app-plan-2026-06-27.md). It is
// OPT-IN (default false): unlike CacheTrack/Guard/Advisor, the zero value is
// the intended default, so no partial-merge entry is required — an install
// with no [observability] section keeps the subsystem off and creates no
// obs_* tables. The subsystem is additionally compiled out by the no_obs
// build tag for minimal distributions (decision D2).
type ObservabilityConfig struct {
	// Enabled gates the whole subsystem: the OTLP /v1/traces receiver,
	// the obs_* schema (applied only when true), ingestion, the
	// trajectory API, and the eval plane. Default false.
	Enabled bool `toml:"enabled"`

	// Eval configures the minimal eval plane (plan §8). Zero value = the
	// `observer eval` CLI works on demand with code scorers; no online
	// sampling, no judge.
	Eval ObservabilityEvalConfig `toml:"eval"`

	// Judge is the SHARED judge binding (admission spec §14 Q4 decision,
	// 2026-07-05): a single [observability.judge] block that both the eval
	// plane and admission fall back to when they don't set their own judge
	// fields. Zero value = no shared judge; the pre-existing
	// [observability.eval] judge_* keys still win where set (no break).
	Judge ObservabilityJudgeConfig `toml:"judge"`

	// Admission configures the input-admission gate (admission spec) — an
	// LLM-as-judge that evaluates incoming user requests to a co-resident
	// agentic app against an admin policy. Zero value = admission off (no
	// schema cost beyond the shared obs migration). Engages only in the
	// co-resident-app posture; meaningless at the coding-agent node.
	Admission ObservabilityAdmissionConfig `toml:"admission"`

	// Alerts configures NODE-SIDE alert evaluation (general-observability
	// gap-audit item #9): threshold rules over this node's OWN local obs_*
	// data, so a node with org sharing off still gets error-rate / cost / p95
	// alerting (org-side alerting in internal/orgserver/obsalert only fires on
	// pushed summaries). Zero value = off. Default false because a fired alert
	// makes an outbound webhook call — explicit opt-in keeps the node
	// egress-free by default.
	Alerts ObservabilityAlertsConfig `toml:"alerts"`

	// Egress configures Plane-A policy egress routing (G22): after admission
	// judges a request, a policy layer may route it to an alternate declared
	// upstream, swap to a cheaper same-shape model, degrade effort, or deny —
	// composed on top of the admission verdict. LOCAL-ONLY, DEFAULT OFF (zero
	// value, same partial-merge rule as the other observability blocks). The
	// obs_egress_decisions audit table is node-local, never pushed.
	Egress ObservabilityEgressConfig `toml:"egress"`
}

// ObservabilityEgressConfig is the [observability.egress] surface (G22, design
// §5). LOCAL-ONLY, never distributed; gated under [observability] enabled.
// DEFAULT OFF (zero value Enabled=false). Translated into the pure
// internal/obs/egress engine's PolicyInput AT THE BOUNDARY (obs_wire), so the
// pure package never imports internal/config.
type ObservabilityEgressConfig struct {
	// Enabled gates the whole egress layer. Default false.
	Enabled bool `toml:"enabled"`
	// Mode is off | advise | enforce. advise evaluates + logs the directive but
	// never applies it; enforce applies it on the proxy path. Empty ⇒ off.
	Mode string `toml:"mode"`
	// CooldownSeconds is the hold window for a same-session budget-band model
	// switch (design §3.6 / finding 11) — a cost switch is held within this
	// window; verdict/locality switches never hold. 0 = the boundary default.
	CooldownSeconds int `toml:"cooldown_seconds"`
	// Rules is the first-match-wins rule table.
	Rules []EgressRuleConfig `toml:"rules"`
	// Targets is the typed upstream target table (id + url + shape). A declared
	// shape is REQUIRED for any enforce-mode route_to_upstream (design finding
	// 6) — [proxy.upstreams] entries carry only URLs.
	Targets []EgressTargetConfig `toml:"targets"`
	// Cohorts is an optional end-user → cohort map (LOCAL-ONLY).
	Cohorts map[string]string `toml:"cohorts"`
}

// EgressRuleConfig is one [[observability.egress.rules]] entry.
type EgressRuleConfig struct {
	Name          string             `toml:"name"`
	When          EgressWhenConfig   `toml:"when"`
	Action        EgressActionConfig `toml:"action"`
	OnUnavailable string             `toml:"on_unavailable"` // fail_open | deny
	Reason        string             `toml:"reason"`
	ReasonCode    string             `toml:"reason_code"`
}

// EgressWhenConfig is a rule's matcher set. BudgetBandAtLeast is a pointer so a
// rule can distinguish "0.0 (zero-spend)" from "unset".
type EgressWhenConfig struct {
	VerdictAtLeast    string   `toml:"verdict_at_least"`
	Criterion         string   `toml:"criterion"`
	SeverityAtLeast   string   `toml:"severity_at_least"`
	ContentClass      string   `toml:"content_class"`
	ModelGlob         string   `toml:"model_glob"`
	Provider          string   `toml:"provider"`
	User              string   `toml:"user"`
	UserCohort        string   `toml:"user_cohort"`
	BudgetBandAtLeast *float64 `toml:"budget_band_at_least"`
	MinPromptTokens   int      `toml:"min_prompt_tokens"`
}

// EgressActionConfig is a rule's action — exactly one primary must be set
// (lint/compile-enforced).
type EgressActionConfig struct {
	RouteToUpstream string `toml:"route_to_upstream"`
	RouteToModel    string `toml:"route_to_model"`
	SetEffort       string `toml:"set_effort"`
	Deny            bool   `toml:"deny"`
	NoRoute         bool   `toml:"no_route"`
}

// EgressTargetConfig is one [[observability.egress.targets]] entry.
type EgressTargetConfig struct {
	ID    string `toml:"id"`
	URL   string `toml:"url"`
	Shape string `toml:"shape"` // anthropic | openai
}

// ObservabilityAlertsConfig is the [observability.alerts] surface. LOCAL-ONLY,
// never distributed; gated under [observability] enabled. It reuses the org
// alert-rule dialect (internal/orgserver/obsalert): metric ∈ error_rate |
// cost_usd | latency_p95_ms, a gt/gte comparator, a threshold, and a window —
// but node windows are in MINUTES (finer than the org's day windows). The
// evaluator loop + webhook client live in cmd/observer; the pure evaluation is
// internal/obs/alert; fired events persist to the node-local obs_alert_events
// table. No node-dashboard surface (obs is Plane-A/web2-only) — the CLI
// (`observer obs alerts`) + the webhook are the node surfaces.
type ObservabilityAlertsConfig struct {
	// Enabled gates the evaluator loop. Default false — a fired alert POSTs to
	// WebhookURL (an outbound network call), so alerting is explicit opt-in.
	Enabled bool `toml:"enabled"`
	// WebhookURL receives the fired-alert JSON payload (one POST per crossing).
	// Empty = record fired events (visible via `observer obs alerts`) but send
	// no webhook — the node stays egress-free.
	WebhookURL string `toml:"webhook_url"`
	// Email, when true, ALSO delivers each fired alert as an email through the
	// shared [email] channel (which must itself be enabled). This is the
	// per-consumer opt-in mirroring WebhookURL — an ADDITIONAL delivery target,
	// not a replacement. Default false.
	Email bool `toml:"email"`
	// EmailTo overrides the [email].to default recipients for this node's alert
	// emails. Empty falls back to [email].to.
	EmailTo []string `toml:"email_to"`
	// EvalIntervalMinutes is how often the evaluator ticks. 0 defaults to 5.
	EvalIntervalMinutes int `toml:"eval_interval_minutes"`
	// Rules is the locally-authored rule table.
	Rules []ObservabilityAlertRuleConfig `toml:"rules"`
}

// ObservabilityAlertRuleConfig is one node alert rule. Mirrors the org
// obs_alert_rules row shape (name / metric / comparator / threshold / window /
// cooldown), with the window/cooldown expressed in minutes.
type ObservabilityAlertRuleConfig struct {
	// Name identifies the rule in fired events + the CLI.
	Name string `toml:"name"`
	// Metric ∈ error_rate | cost_usd | latency_p95_ms.
	Metric string `toml:"metric"`
	// Comparator ∈ gt | gte. Empty defaults to gt.
	Comparator string `toml:"comparator"`
	// Threshold is the value the metric is tested against.
	Threshold float64 `toml:"threshold"`
	// WindowMinutes is the lookback the metric is computed over. 0 defaults to
	// 60 at evaluation.
	WindowMinutes int `toml:"window_minutes"`
	// CooldownMinutes is the minimum gap between fires of this rule (dedup). 0
	// defaults to WindowMinutes (a full window must elapse before re-firing).
	CooldownMinutes int `toml:"cooldown_minutes"`
}

// ObservabilityJudgeConfig is one OpenAI-compatible /chat/completions judge
// binding (the eval/admission shared shape, admission spec §5). LOCAL-ONLY.
// The key is NEVER written to disk: APIKeyEnv names the env var holding it
// (the pi/hermes launcher posture). Zero value = judge unbound (llm_judge
// scorer / admission judge criteria are simply unavailable; code / pre-filter
// layers still run offline).
type ObservabilityJudgeConfig struct {
	// Model names the judge model. Empty = judge disabled.
	Model string `toml:"model"`
	// BaseURL is the OpenAI-compatible chat-completions base URL. Empty
	// defaults to OpenRouter (https://openrouter.ai/api/v1) at the binding.
	BaseURL string `toml:"base_url"`
	// APIKeyEnv names the ENV VAR holding the judge API key — never the key
	// itself. Empty defaults to OPENROUTER_API_KEY. Loopback/local judges
	// need no key (the binding treats a loopback base_url as no-egress and
	// allows an empty/unset key).
	APIKeyEnv string `toml:"api_key_env"`
	// TimeoutMS bounds a single judge call. 0 = the binding's default.
	TimeoutMS int `toml:"timeout_ms"`
	// MaxTokens caps the judge REPLY length (the reply is only a short JSON
	// verdict). 0 = the binding's default. A tight cap bounds latency + cost
	// on a hosted judge and stops a chatty local model from streaming forever.
	MaxTokens int `toml:"max_tokens"`
	// NumCtx is an Ollama-style context-window hint passed through ONLY to a
	// loopback/local judge (a hosted OpenAI-compatible API rejects unknown
	// fields, so it is never sent remotely). 0 = omit. Its effect depends on
	// the local host honoring the field on its /v1/chat/completions endpoint.
	NumCtx int `toml:"num_ctx"`
}

// ObservabilityAdmissionConfig is the [observability.admission] surface
// (admission spec §4). LOCAL-ONLY, never distributed; gated under
// [observability] enabled. Observe-first: Mode defaults to "observe" only
// once Enabled is set — the zero value (Enabled=false) is fully off.
type ObservabilityAdmissionConfig struct {
	// Enabled gates admission WITHIN the obs subsystem. Default false.
	Enabled bool `toml:"enabled"`
	// Mode is off | observe | enforce. P1 ships observe only; enforce is P2.
	// Empty is treated as "observe" when Enabled.
	Mode string `toml:"mode"`
	// Strict = fail-closed on judge error/timeout (Deny). Default false =
	// fail-open (Allow + recorded admission_error).
	Strict bool `toml:"strict"`
	// Scope is last_user | conversation. P1 = last_user; conversation is P3.
	Scope string `toml:"scope"`
	// RetentionDays bounds the obs_admission_events audit table. 0 = keep.
	RetentionDays int `toml:"retention_days"`
	// Criterion is the admin's policy table (§4).
	Criterion []AdmissionCriterionConfig `toml:"criterion"`
	// Judge overrides the shared [observability.judge] for admission only.
	// Zero value = fall back to [observability.judge].
	Judge ObservabilityJudgeConfig `toml:"judge"`
	// Prefilter holds the deterministic allow/deny lists + size ceiling.
	Prefilter AdmissionPrefilterConfig `toml:"prefilter"`
	// SecretRemoteJudge is the local decision (allow|flag|ask|deny) applied
	// when the request carries a pattern-certain secret AND the judge is
	// REMOTE — the request is NOT egressed to the hosted judge; the configured
	// decision is returned locally instead (spec §3 layer 3). Empty/"allow" =
	// off (the request still goes to the — already secret-scrubbed, §item 8 —
	// remote judge). Inert for a local/off judge.
	SecretRemoteJudge string `toml:"secret_remote_judge"`
	// CacheTTLS is the verdict-cache TTL in seconds. 0 = the svc default.
	CacheTTLS int `toml:"cache_ttl_s"`
	// JudgeChunkBytes bounds a single judge call's content size: a request
	// longer than this is split into overlapping windows, each judged, and the
	// verdicts reduced strictest-wins (map-reduce, spec §4 — mirrors the demo's
	// app-layer chunking, now in-core). 0 = the engine default (3500).
	JudgeChunkBytes int `toml:"judge_chunk_bytes"`
	// JudgeChunkOverlapBytes is the overlap between adjacent judge windows so a
	// concern straddling a boundary is still seen whole by one chunk. 0 = the
	// engine default (200); clamped below JudgeChunkBytes.
	JudgeChunkOverlapBytes int `toml:"judge_chunk_overlap_bytes"`
	// IncludeReasoning asks the judge to explain (OpenAI's ~40% latency
	// finding — default false).
	IncludeReasoning bool `toml:"include_reasoning"`
	// Budget is the per-end-user spend guardrail evaluated at the admission
	// chokepoint (the org-hosted-app model: the budgeted subject is an
	// end-user of the hosted app, identified by the app-shared enduser.id /
	// the admission `user` field → obs_traces.user; spend is summed from
	// obs_spans.cost_usd). Off by default.
	Budget AdmissionBudgetConfig `toml:"budget"`
}

// AdmissionBudgetConfig is the per-end-user spend guardrail evaluated at the
// admission chokepoint (docs/guardrails.md; org-budget plan §1). A window
// breach yields a Deny verdict: in observe mode it is recorded as a shadow
// "would-deny"; enforce blocks (P2). It requires the app to share the end-user
// identity (enduser.id / the admission `user` field); an anonymous request is
// inert. A 0 cap disables that window; Enabled=false disables the gate.
//
// PLANE A budget (docs/deployment-models.md): caps a HOSTED-APP END-USER's
// spend at the admission chokepoint. Distinct from the two Plane-B budgets:
// [guard.budget] (GuardBudgetConfig — the DEVELOPER's own provider spend) and
// [routing.budget] (RoutingBudgetConfig — the routing spend-band downshift).
type AdmissionBudgetConfig struct {
	Enabled           bool    `toml:"enabled"`
	PerUser5hUSD      float64 `toml:"per_user_5h_usd"`
	PerUserWeeklyUSD  float64 `toml:"per_user_weekly_usd"`
	PerUserMonthlyUSD float64 `toml:"per_user_monthly_usd"`
	// UserHeader names the request header the proxy pre-forward backstop reads
	// the end-user identity from — the app shares it as an integration
	// requirement (the SDK path passes `user` directly and ignores this).
	// Empty → DefaultAdmissionUserHeader. Absent header ⇒ the per-end-user gate
	// is inert for that request (the app-wide policy still applies).
	UserHeader string `toml:"user_header"`
}

// DefaultAdmissionUserHeader is the request header the proxy admission backstop
// reads the end-user identity from when [observability.admission.budget]
// user_header is unset.
const DefaultAdmissionUserHeader = "X-Superbased-User"

// AdmissionCriterionConfig is one policy criterion (§4). type ∈
// valid_use_case | denied_topics | jailbreak | custom (the pure package
// resolves the vocabulary). Topics apply to denied_topics; Definition to the
// judged types.
type AdmissionCriterionConfig struct {
	ID         string   `toml:"id"`
	Type       string   `toml:"type"`
	Name       string   `toml:"name"`
	Definition string   `toml:"definition"`
	Topics     []string `toml:"topics"`
	Decision   string   `toml:"decision"`
	Severity   string   `toml:"severity"`
}

// AdmissionPrefilterConfig is the deterministic pre-filter layer (§3 layers
// 1-2). Allow/Deny are regex-or-prefix patterns; MaxMessageBytes caps length
// (0 = off).
type AdmissionPrefilterConfig struct {
	Allow           []string `toml:"allow"`
	Deny            []string `toml:"deny"`
	MaxMessageBytes int      `toml:"max_message_bytes"`
}

// ObservabilityEvalConfig configures the obs eval plane (plan §8). LOCAL-ONLY,
// never distributed. Online sampling is off by default (OnlineSampleRate 0).
type ObservabilityEvalConfig struct {
	// OnlineSampleRate, in (0,1], runs OnlineScorers over that fraction of
	// live LLM spans as they ingest (the Langfuse/Arize online-eval model).
	// 0 (default) disables online sampling entirely.
	OnlineSampleRate float64 `toml:"online_sample_rate"`
	// OnlineScorers are the scorer specs run during online sampling, each a
	// string "name" or "name:key=val,key2=val2". Only facts-based code
	// scorers make sense online (status_ok / latency_under / cost_under) —
	// content scorers need a stored body. Empty disables online sampling.
	OnlineScorers []string `toml:"online_scorers"`
	// JudgeModel names the model an llm_judge scorer uses by default (when a
	// scorer spec omits its own model=). Empty disables the LLM judge: the
	// host JudgeClient stays unbound, llm_judge errors clearly, and code
	// scorers run fully offline. The judge call is the ONLY outbound network
	// in the subsystem and runs ONLY for an explicitly-invoked `observer eval
	// run` (never the daemon/online-sampling path).
	JudgeModel string `toml:"judge_model"`
	// JudgeBaseURL is the OpenAI-compatible chat-completions base URL the
	// judge posts to. Empty defaults to OpenRouter (https://openrouter.ai/api/v1).
	JudgeBaseURL string `toml:"judge_base_url"`
	// JudgeAPIKeyEnv names the ENVIRONMENT VARIABLE holding the judge API key
	// — the key is never written to config or disk (the pi/hermes launcher
	// posture). Empty defaults to OPENROUTER_API_KEY.
	JudgeAPIKeyEnv string `toml:"judge_api_key_env"`
}

// GuardConfig is the full [guard] surface (guard spec §16). Same
// partial-merge invariant as CacheTrackConfig: an install with no
// [guard] section gets Enabled=true + Mode="observe" from Default()
// — never a zero-valued false/"" (operator decision D2: fresh
// installs observe and alert; nothing blocks until the operator
// flips enforce).
//
// Sub-sections gate later G-commits (proxy G9, mcp G10, budget G12,
// dialects G11, cloud G15); they are declared now so the config
// vocabulary is stable and a forward-written config file round-trips.
type GuardConfig struct {
	// Enabled gates all guard wiring (ingest seam, hook seam, CLI
	// surfaces). When false, no policy engine is constructed and no
	// guard_events are written.
	Enabled bool `toml:"enabled"`
	// Mode is the global posture: "off" | "observe" | "enforce"
	// (default "observe", D2). In observe, deny/ask-class verdicts
	// are recorded + alerted but the action proceeds.
	Mode string `toml:"mode"`
	// Strict inverts the Q2 fail-open default: a guard internal
	// error then blocks instead of approving. Enterprise posture;
	// default false.
	Strict bool `toml:"strict"`
	// RetentionDays is the guard_events / expired-approvals prune
	// horizon (spec §10.3). Default 365 — audit data wants ≥1y for
	// compliance buyers. ≤0 disables the guard prune.
	RetentionDays int `toml:"retention_days"`

	Rules    GuardRulesConfig    `toml:"rules"`
	Boundary GuardBoundaryConfig `toml:"boundary"`
	Taint    GuardTaintConfig    `toml:"taint"`
	Proxy    GuardProxyConfig    `toml:"proxy"`
	MCP      GuardMCPConfig      `toml:"mcp"`
	Budget   GuardBudgetConfig   `toml:"budget"`
	Alerts   GuardAlertsConfig   `toml:"alerts"`
	Export   GuardExportConfig   `toml:"export"`
	Dialects GuardDialectsConfig `toml:"dialects"`
	Cloud    GuardCloudConfig    `toml:"cloud"`
}

// GuardRulesConfig is [guard.rules] (spec §16): rule disabling and
// the user/project policy-file locations.
type GuardRulesConfig struct {
	// Disable lists rule IDs turned off entirely.
	Disable []string `toml:"disable"`
	// UserPolicy is the user policy file location. Default
	// "~/.observer/guard-policy.toml".
	UserPolicy string `toml:"user_policy"`
	// ProjectPolicy is the project policy file location relative to
	// each project root. Default ".observer/guard-policy.toml".
	ProjectPolicy string `toml:"project_policy"`
	// OrgBundle is the local cache location of the verified org
	// policy bundle envelope (guard spec §14.2). Default
	// "~/.observer/org-policy-bundle.json". Written ONLY by the org
	// client after full signature + key-pin verification; read by
	// every guard construction (daemon and hook processes alike) as
	// the org policy layer, with the signature re-checked at load.
	// The file is absent on non-enrolled installs — absence simply
	// means no org layer.
	OrgBundle string `toml:"org_bundle"`
	// CEL is the Q1 v2 gate for CEL-expression user rules. Parsed
	// but rejected by the loader until the v2 arc lands (decided:
	// matchers v1, CEL deferred).
	CEL bool `toml:"cel"`
}

// GuardBoundaryConfig is [guard.boundary] (spec §16). Nil slices mean
// "use the policy-engine defaults" (policy.DefaultAllowPaths /
// DefaultProtectedBranches); explicitly empty lists mean "none".
type GuardBoundaryConfig struct {
	AllowPaths        []string `toml:"allow_paths"`
	ProtectedBranches []string `toml:"protected_branches"`
}

// GuardTaintConfig is [guard.taint] (spec §4.5, §16).
type GuardTaintConfig struct {
	// Enabled gates taint tracking + the T-5xx rules' input (with
	// tracking off the snapshot is always empty, so taint rules
	// never fire).
	Enabled bool `toml:"enabled"`
	// DecayTurns is the mark lifetime in session turns. Default 10.
	DecayTurns int `toml:"decay_turns"`
}

// GuardProxyConfig is [guard.proxy] (spec §8, lands G9).
type GuardProxyConfig struct {
	// EgressScan gates the §8.2 typed secret scan over the final
	// outbound request body.
	EgressScan bool `toml:"egress_scan"`
	// EgressAction is the enforce-class action when the R-172
	// api_request verdict blocks: "flag" (record only), "mask"
	// (rewrite detector-certain values to [REDACTED:type] and
	// forward; entropy hits never mask), "deny" (synthetic 403,
	// §8.5). Default "mask" per §8.2 ("mask mode default-on in
	// enforce for detector-certain types") — inert in observe mode,
	// where every egress verdict is a flag (D2).
	EgressAction string `toml:"egress_action"` // flag | mask | deny
	// EgressAllow are regex patterns over the MATCHED VALUE: a
	// finding whose value matches is ignored entirely (test
	// fixtures, known-fake keys). Compiled by the guard layer;
	// invalid patterns degrade to load issues. Global in G9 —
	// per-project egress_allow joins when the project policy
	// vocabulary grows a proxy section (deferred, documented).
	EgressAllow []string `toml:"egress_allow"`
	// ResponseScan gates the §8.3 response-side tool_use inspection
	// (flag/alert only in v1).
	ResponseScan bool `toml:"response_scan"`
	// InjectionHeuristics gates the §8.4 prompt-injection heuristics
	// on inbound tool-result/web content (flag + taint, never deny).
	InjectionHeuristics bool `toml:"injection_heuristics"`
}

// GuardMCPConfig is [guard.mcp] (spec §9, lands G10).
type GuardMCPConfig struct {
	Pinning             bool `toml:"pinning"`
	PoisoningHeuristics bool `toml:"poisoning_heuristics"`
}

// GuardBudgetConfig is [guard.budget] (spec §12.1, lands G12). 0
// means off.
//
// PLANE B budget (docs/deployment-models.md): caps the DEVELOPER's OWN
// provider spend on their coding-agent turns. Distinct from [routing.budget]
// (RoutingBudgetConfig — the routing spend-band downshift, also Plane B) and
// from the Plane-A [observability.admission.budget] (AdmissionBudgetConfig —
// a hosted-app END-USER's spend at the admission chokepoint).
type GuardBudgetConfig struct {
	SessionUSD float64 `toml:"session_usd"`
	DailyUSD   float64 `toml:"daily_usd"`
	// WeeklyUSD / MonthlyUSD are $ ceilings over a rolling 7-day
	// window and the calendar month (rules B-604 / B-603), enforced
	// with the same deny-on-proxy discipline as SessionUSD/DailyUSD.
	// 0 disables the window.
	WeeklyUSD  float64 `toml:"weekly_usd"`
	MonthlyUSD float64 `toml:"monthly_usd"`
	Hard       bool    `toml:"hard"`
	// Window gates on the provider's own 5h / weekly usage windows
	// (utilization 0..1, read from limit_snapshots) — distinct from
	// the $ caps above; the "limit" guardrails B-610..B-613.
	Window GuardBudgetWindowConfig `toml:"window"`
}

// GuardBudgetWindowConfig is [guard.budget.window] (spec §12.1) —
// utilization thresholds (0..1) over the provider's unified 5h and
// weekly usage windows. warn flags, deny blocks (proxy). 0 disables
// the threshold. deny must be >= warn when both are set.
type GuardBudgetWindowConfig struct {
	Util5hWarn     float64 `toml:"util_5h_warn"`
	Util5hDeny     float64 `toml:"util_5h_deny"`
	UtilWeeklyWarn float64 `toml:"util_weekly_warn"`
	UtilWeeklyDeny float64 `toml:"util_weekly_deny"`
}

// GuardAlertsConfig is [guard.alerts] (spec §16, lands G5).
type GuardAlertsConfig struct {
	// Desktop enables exec-based desktop notifications (Q3).
	Desktop bool `toml:"desktop"`
	// MinSeverity is the alert threshold: "info" | "warn" | "high" |
	// "critical". Default "high".
	MinSeverity string `toml:"min_severity"`
}

// GuardExportConfig is [guard.export] (spec §11.4, lands G16).
type GuardExportConfig struct {
	OTel bool `toml:"otel"`
}

// GuardDialectsConfig is [guard.dialects] (spec §13.2, lands G11).
type GuardDialectsConfig struct {
	Compile bool     `toml:"compile"`
	Targets []string `toml:"targets"`
}

// GuardCloudConfig is [guard.cloud] (spec §15, lands G15). EVERYTHING
// here requires explicit opt-in (operator decision D1); the master
// Enabled plus per-feature switches all default false.
type GuardCloudConfig struct {
	Enabled         bool                  `toml:"enabled"`
	LLMJudge        GuardLLMJudgeConfig   `toml:"llm_judge"`
	Reputation      GuardReputationConfig `toml:"reputation"`
	Webhooks        []GuardWebhookConfig  `toml:"webhooks"`
	PayloadMaxBytes int                   `toml:"payload_max_bytes"`
}

// GuardLLMJudgeConfig configures the §15.2 LLM-judge reviewer.
// Endpoint is an OpenAI-chat-completions-compatible URL (bring-your-
// own: a local gateway, litellm, or the user's own proxied provider
// session). APIKeyEnv names an ENVIRONMENT VARIABLE holding the
// bearer key — never the key itself (the no-secrets-in-config rule);
// empty means the endpoint authenticates locally or not at all.
type GuardLLMJudgeConfig struct {
	Enabled   bool   `toml:"enabled"`
	Endpoint  string `toml:"endpoint"`
	Model     string `toml:"model"`
	APIKeyEnv string `toml:"api_key_env"`
}

// GuardReputationConfig configures the §15.3 reputation lookups.
type GuardReputationConfig struct {
	Enabled bool `toml:"enabled"`
}

// GuardWebhookConfig is one [[guard.cloud.webhooks]] entry (§15.4).
// RoutingKey is PagerDuty's Events-API v2 routing key (kind =
// "pagerduty" only; URL then stays the standard events endpoint).
type GuardWebhookConfig struct {
	URL         string `toml:"url"`
	Kind        string `toml:"kind"` // generic | slack | discord | pagerduty
	MinSeverity string `toml:"min_severity"`
	RoutingKey  string `toml:"routing_key"`
}

// AdvisorConfig gates the suggestions engine (spec §15.7; plan
// docs/plans/suggestions-engine-implementation-plan-2026-06-10.md).
// Default-ON: read-layer only, local, zero LLM cost. Same partial-merge
// invariant as CacheTrackConfig — an install with no [advisor] section
// must get Enabled=true from Default(), never a zero-valued false.
type AdvisorConfig struct {
	// Enabled gates the /api/suggestions endpoint, the dashboard tab's
	// data, and `observer advise`.
	Enabled bool `toml:"enabled"`
	// WindowDays is the default evidence window. Default 14.
	WindowDays int `toml:"window_days"`
	// MinConfidence hides suggestions below this floor. Default 0.5.
	MinConfidence float64 `toml:"min_confidence"`
	// MinSavingsUSD hides cost suggestions claiming less than this
	// (calibration T7). Default 1.0.
	MinSavingsUSD float64 `toml:"min_savings_usd"`
	// SessionDigest, when true, lets the Claude Code session-start hook
	// inject a ≤400-token advisory digest (top suggestions) as
	// additionalContext. Default OFF until proven quiet (plan Phase 3).
	// The hook only point-reads the advisor_digest snapshot — it never
	// computes (P1).
	SessionDigest bool `toml:"session_digest"`
	// DigestRefreshMinutes is the daemon's advisor_digest refresh
	// cadence. Default 30.
	DigestRefreshMinutes int `toml:"digest_refresh_minutes"`
}

// DefaultAggregateEndpoint is the published default collector endpoint for
// the opt-in aggregate rail (design §9.2). It is seeded by Default() so a
// partial-merge install inherits it; a change to a non-approved host requires
// [aggregate_share].allow_custom_endpoint AND invalidates the consent receipt.
const DefaultAggregateEndpoint = "https://aggregate.superbased.app/v1/submit"

// approvedAggregateHosts is the closed set of hosts the aggregate rail may
// submit to without the self-host/testing escape (design §9.2, finding #21).
var approvedAggregateHosts = map[string]bool{
	"aggregate.superbased.app": true,
}

// AggregateShareConfig is the [aggregate_share] surface — the opt-in aggregate
// rail (docs/plans/g25-optin-aggregate-rail-design-2026-07-11.md §9.2). It is
// OPT-IN and default OFF in EVERY path: the zero value has Enabled=false, and
// the loader's partial-merge default (Default()) also leaves Enabled=false —
// only Endpoint is seeded to the published constant. Unlike CacheTrack/Predict
// (default-ON), the whole rail stays inert until the operator consents via
// `observer aggregate enable`. Deliberately TOP-LEVEL (not under [org_client])
// because it is org-independent — it works for solo nodes and never touches
// the Teams wire.
type AggregateShareConfig struct {
	// Enabled gates the whole rail. Default FALSE (zero value AND partial-
	// merge). Even when true, the daemon submits only while a valid consent
	// receipt exists (design §9.1/§9.3) — enabling in config alone never
	// bypasses consent.
	Enabled bool `toml:"enabled"`
	// Endpoint is the collector URL the coarsened monthly aggregate would be
	// POSTed to. Validated at load: HTTPS-only, no credentials/query, host on
	// the approved list unless AllowCustomEndpoint. A change invalidates the
	// consent receipt (re-consent required). Default DefaultAggregateEndpoint.
	Endpoint string `toml:"endpoint"`
	// AllowCustomEndpoint is the explicit self-host / testing escape (design
	// §9.2): only with it true may Endpoint point at a non-approved host.
	// Default false.
	AllowCustomEndpoint bool `toml:"allow_custom_endpoint"`
}

// PredictConfig is the [predict] surface — the Next-Message Cost &
// Limit Predictor (docs/cost-predictor.md). Default-ON, same partial-
// merge invariant as CacheTrackConfig: an install with no [predict]
// section gets Enabled=true (NOT a zero-valued false) because Load()
// starts from Default() and unmarshals TOML on top. LOCAL-ONLY — never
// distributed to an org server.
type PredictConfig struct {
	// Enabled gates the predictor. The cost estimate is pure read-side
	// math (no cost when unused); when false the proxy also skips
	// limit-snapshot capture and the dashboard/CLI surfaces stay empty.
	Enabled bool `toml:"enabled"`
	// YoungSessionMessages is the §0 T-ladder threshold: at/above this
	// many observed user messages the session's own turns-per-message
	// distribution is trusted; below it the cross-session prior wins.
	// Default 3.
	YoungSessionMessages int `toml:"young_session_messages"`
	// DefaultTurnsPerMessage is the tier-3 fallback fan-out when neither
	// the session nor a prior yields turns-per-message. Default 12.
	DefaultTurnsPerMessage int `toml:"default_turns_per_message"`
	// PriorWindowDays bounds the recency of sessions feeding the
	// cross-session T prior. Default 30. 0 = no bound.
	PriorWindowDays int `toml:"prior_window_days"`
}

// BrowserConfig is the [browser] surface — the opt-in browser-chatbot
// capture rail (docs/plans/browser-extension-and-m365-copilot-proposal-
// 2026-07-10.md §5). LOCAL-ONLY, never distributed via the org policy
// registry (same posture as [predict]/[routing]/[cachewarm]). Same
// partial-merge invariant as CacheTrackConfig: an install with no [browser]
// section inherits the Default() seed (receiver AVAILABLE but the loopback
// LISTENER OFF — the native-messaging bridge is the default binding; the
// HTTP listener is the opt-in alternate). The daemon is the CEILING
// authority on what is stored (§5.1): GranularityCeiling clamps whatever
// the extension sends.
type BrowserConfig struct {
	// Enabled gates the whole browser rail on the daemon side. When false
	// the loopback listener never starts (the native-messaging hook path is
	// unaffected — it opens the DB directly like every CLI hook). Default
	// true so the rail is ready the moment the operator installs the
	// extension; the extension itself is opt-in regardless.
	Enabled bool `toml:"enabled"`
	// Listener configures the opt-in loopback HTTP receiver — the alternate
	// to the default native-messaging bridge.
	Listener BrowserListenerConfig `toml:"listener"`
	// Sites holds per-site enable toggles keyed by the *-web tool name
	// (chatgpt-web/claude-web/perplexity-web/gemini-web/copilot-web). A
	// missing key means "enabled" (fail-open — the extension's own per-site
	// toggle is the primary control; this is the daemon backstop). Set a key
	// false to make the daemon DROP that site's turns regardless of what the
	// extension sends.
	Sites map[string]bool `toml:"sites"`
	// GranularityCeiling is the daemon's maximum stored granularity
	// (§5.1: "usage_only" | "redacted" | "full"). The daemon is the final
	// authority on what is STORED, so the effective granularity of every
	// turn is min(what the extension sent, this ceiling). Default "full":
	// Observer is a local-first observability tool and browser capture
	// follows the same posture as every coding-agent adapter — full
	// prompt/response content stored NODE-LOCALLY (the scrub.Scrubber
	// secrets pass still applies; nothing leaves the node without the
	// [org_client.share] opt-in). The extension discovers this ceiling via
	// the native-messaging `config` event and sends at exactly this level;
	// an operator who wants less sets "redacted" or "usage_only" here (one
	// lever, dashboard-editable). An empty/unknown value is treated as
	// usage_only by the normalizer (fail-closed).
	GranularityCeiling string `toml:"granularity_ceiling"`
	// RetentionDays bounds how long browser rows are kept (rides the
	// existing retention machinery — 0 = keep forever / inherit the global
	// retention). Default 0.
	RetentionDays int `toml:"retention_days"`
	// IngestTimeoutMS bounds the browser-capture DB ingest path
	// (ingestBrowserTurn → store.Ingest). It is DELIBERATELY DECOUPLED from
	// the blocking-hook timeout ([observer.hooks].timeout_ms, ~500ms): by the
	// time this deadline applies the browser hook has ALREADY ACKed the
	// extension on stdout and runs as a detached child, so keeping it short
	// protects nothing — it only sheds captured turns whenever the daemon
	// briefly holds the SQLite write lock (a WAL checkpoint on a large DB, a
	// batch insert) even though the 30s busy_timeout would ride the
	// contention out. Default 35000 (35s); a zero / negative value resolves
	// to the default via IngestTimeout(). Same partial-merge rule as the
	// other [browser] keys — an install with no [browser] section inherits
	// the default.
	IngestTimeoutMS int `toml:"ingest_timeout_ms"`
}

// defaultBrowserIngestTimeoutMS is the fallback browser-capture ingest
// deadline used when [browser].ingest_timeout_ms is unset or non-positive.
// 35s: it must EXCEED the 30s SQLite busy_timeout (db.Open pragma) so a
// maximum-length write-lock wait is ridden out rather than cut off mid-wait
// (a 15s deadline still dropped turns whenever the daemon held the lock
// 15–30s). The pile-up trade-off — every captured turn is its own detached
// hook child, so a long deadline means more concurrent waiters — is accepted
// for this lane: browser-turn arrival is human-paced (single-digit per
// minute), and drops stay legible in browser-health.json telemetry.
const defaultBrowserIngestTimeoutMS = 35000

// maxBrowserIngestTimeoutMS caps [browser].ingest_timeout_ms. It exists so the
// daemon's end-to-end browser-ingest work is GUARANTEED to finish before the
// native-messaging host gives up on it: host.js
// (internal/browserhost/hostfiles/host.js) replies to Chrome after its
// HOST_INGEST_CAP_MS reply cap (40000ms) and then Chrome tears down the
// Windows→WSL bridge, killing a still-running ingest child. Because
// cmd/observer/browser.go now bounds db.Open + store.Ingest under IngestTimeout()
// (started BEFORE db.Open, whose SQLite busy_timeout can wait 30s), the real
// child runtime is <= IngestTimeout(); clamping that to 35000ms holds it ~5s
// below the 40000ms host cap. Keep the two constants in sync — host.js carries
// the matching cross-reference comment on HOST_INGEST_CAP_MS.
const maxBrowserIngestTimeoutMS = 35000

// IngestTimeout returns the browser-capture ingest deadline as a
// time.Duration. A zero or negative IngestTimeoutMS (unset, or an operator
// clamp) resolves to the generous default so the detached, post-ACK DB write
// is never starved by the short blocking-hook timeout; a value above
// maxBrowserIngestTimeoutMS is clamped DOWN so the end-to-end bound can never
// exceed the native host's reply cap even if Validate is bypassed (e.g. a
// programmatically-built config in a test).
func (b BrowserConfig) IngestTimeout() time.Duration {
	ms := b.IngestTimeoutMS
	if ms <= 0 {
		ms = defaultBrowserIngestTimeoutMS
	}
	if ms > maxBrowserIngestTimeoutMS {
		ms = maxBrowserIngestTimeoutMS
	}
	return time.Duration(ms) * time.Millisecond
}

// BrowserListenerConfig is the [browser.listener] sub-surface — the opt-in
// loopback HTTP receiver (internal/ingest/browser). The native-messaging
// bridge is the default binding; this is the alternate for deployments that
// prefer HTTP. Its own dedicated port, never :8820 / the dashboard mux.
type BrowserListenerConfig struct {
	// Enabled gates the loopback listener. Default FALSE — the
	// native-messaging bridge is the default receiver; turn this on only
	// when a deployment routes captured turns over HTTP instead.
	Enabled bool `toml:"enabled"`
	// ListenAddr is the host:port bind. Default 127.0.0.1:8821. A
	// non-loopback host is refused unless AllowNonLoopback is set.
	ListenAddr string `toml:"listen_addr"`
	// AllowNonLoopback permits a non-loopback bind (default false — the
	// network-posture guard).
	AllowNonLoopback bool `toml:"allow_non_loopback"`
	// Token is the shared secret the loopback receiver requires in the
	// X-SBO-Browser-Token header (defense-in-depth for the clientless,
	// default-off ingress — A4). Empty means the daemon auto-generates one at
	// start and persists it 0600 next to the observer DB
	// (browser-ingest-token), so an operator never has to set it by hand; set
	// it explicitly only to pin a known value.
	Token string `toml:"token"`
}

// HandoffConfig is the [handoff] surface — session handoff /
// continue-anywhere (docs/plans/session-handoff-plan-2026-07-03.md §12).
// LOCAL-ONLY, never distributed via the org policy registry. Read-side
// feature: default-ON with the same partial-merge rule as CacheTrack.
type HandoffConfig struct {
	// Enabled gates the handoff surfaces (CLI today; dashboard/MCP in P2).
	Enabled bool `toml:"enabled"`
	// TailMessages is the verbatim-tail length for the distilled_tail
	// carry mode. Default 6.
	TailMessages int `toml:"tail_messages"`
	// MaxDocTokens budgets the rendered HandoverDoc (file/MCP lanes);
	// sections degrade tail-first past it. Default 12000 (≈48KB).
	MaxDocTokens int `toml:"max_doc_tokens"`
	// DefaultCarry is the carry mode when the caller names none. Default
	// "distilled_tail".
	DefaultCarry string `toml:"default_carry"`
	// FileName templates the inject_file artifact; {shortid} expands.
	// Default "HANDOFF-{shortid}.md".
	FileName string `toml:"file_name"`
	// HookMaxBytes budgets the P3 hook lane (Phase 0 D-P0.2: SessionStart
	// additionalContext delivers intact only to ~8KB). Default 8192.
	HookMaxBytes int `toml:"hook_max_bytes"`
	// HookTTLMinutes bounds how long an armed inject_hook handoff waits for
	// the next target session before it expires (plan §10/§12). It is also
	// one-shot: the first delivery marks the row delivered. Default 240
	// (4h) — a stale armed handoff must never fire days later.
	HookTTLMinutes int `toml:"hook_ttl_minutes"`
	// RetentionDays is the horizon for the handoffs-table prune sweep
	// (plan §15 P4), swept by runRetention through
	// store.PruneHandoffRows. handoffs rows are tiny content-free
	// metadata (hashes/enums/paths), so the default is generous —
	// 180 days keeps ~6 months of handoff history for the target-session
	// linker while still bounding growth. 0 = keep forever.
	RetentionDays int `toml:"retention_days"`
	// AllowDashboardLaunch gates the dashboard's "Launch <tool> here"
	// embedded web terminal (docs/session-handoff.md launch section): the
	// daemon spawns `observer <tool> --continue-from <id>` in a PTY and
	// streams its TUI into the browser over a websocket. Default TRUE — it
	// lives entirely within the dashboard's loopback + browserGuard trust
	// boundary (opaque token minted only by an Origin-checked POST; the ws
	// upgrade rejects cross-origin by default). Set false to disable the
	// launch surface entirely (the endpoints 503 and the button hides).
	// LOCAL-ONLY, like the rest of [handoff].
	AllowDashboardLaunch bool `toml:"allow_dashboard_launch"`
	// ContextWarnTokens is the conservative context-window floor the handoff
	// estimate warns against: when the FULL carry exceeds it, the modal/CLI
	// flag that the target tool's (often unknown) default model may be too
	// small to rehydrate the whole context in one shot — the "350K in Opus
	// into a 200K free model" mismatch. Default 200000; 0 disables the
	// warning. LOCAL-ONLY.
	ContextWarnTokens int `toml:"context_warn_tokens"`
	// MaxCacheBytes is the safety cap on the `full_cache` carry doc — the
	// only mode that inlines the actual UN-EXCERPTED read bodies (the "full
	// incl cache" mode; docs/session-handoff.md). full_cache deliberately
	// bypasses the ~100KB inline-prompt bound so the warm context travels
	// from the first prompt, so this cap is the guard against a pathological
	// multi-hundred-MB session writing an unusable doc: past it the doc is
	// truncated with an honest note. Default 8388608 (8MB); 0 = uncapped.
	// LOCAL-ONLY.
	MaxCacheBytes int `toml:"max_cache_bytes"`
}

// TerminalConfig is the [terminal] surface — the embedded web-terminal
// cockpit (docs/plans/terminal-product-exploitation-plan-2026-07-12.md §8).
// LOCAL-ONLY: it never appears in [org_client.share] and is never distributed
// to an org. Terminal-wide knobs live at the top; fresh-agent launch is a
// SEPARATE, default-OFF opt-in under [terminal.launch] — it EXPANDS execution
// authority, so (unlike the code_graph→codeintel rename) it is never migrated
// on. [terminal].enabled gates the terminal-wide surface; it does NOT grant
// fresh-launch (that needs [terminal.launch].allow_fresh_agent too).
//
// Same partial-merge invariant as CacheTrackConfig: an install with no
// [terminal] section gets the Default() seed (Enabled=true, Status.Enabled=true,
// but Launch.AllowFreshAgent=false); a partial section keeps unset fields at
// their Default() values.
type TerminalConfig struct {
	// Enabled gates the terminal-wide surface (status API, sessions list).
	// Default TRUE. The existing handoff-continue launcher stays gated by
	// [handoff].allow_dashboard_launch — this does NOT absorb it.
	Enabled bool `toml:"enabled"`
	// MaxConcurrent caps live terminal sessions (default 9 — sized for the
	// Terminal Workspace grid; the termsession zero-value fallback stays 4).
	// 0 falls back to the termsession default.
	MaxConcurrent int `toml:"max_concurrent"`
	// IdleTimeout reaps a terminal whose PTY has seen no I/O for this long
	// (Go duration string, e.g. "30m"). Empty uses the termsession default.
	IdleTimeout string `toml:"idle_timeout"`
	// RingBytes bounds each session's raw replay ring (default 262144). 0 uses
	// the termsession default.
	RingBytes int `toml:"ring_bytes"`
	// MaxSubscribers caps concurrent read-only viewers PER session (Phase 4
	// output fan-out, §4.α.1). Default 8. 0 uses the termsession default.
	MaxSubscribers int `toml:"max_subscribers"`
	// Launch is the fresh-agent launch opt-in block (F1). ALL default-off.
	Launch TerminalLaunchConfig `toml:"launch"`
	// Status is the agent-status detection block (F4). Default-on.
	Status TerminalStatusConfig `toml:"status"`
	// Attach is the session-attach block (session-attach design Phase 1).
	// Default-on: the attach socket is AF_UNIX owner-only 0600.
	Attach TerminalAttachConfig `toml:"attach"`
}

// TerminalAttachConfig is the [terminal.attach] block — session attach
// (session-attach design 2026-07-19, Phase 1). Unlike [terminal.launch], this
// block is default-ON: the attach control channel is an AF_UNIX socket at mode
// 0600 (owner-only, never network-reachable), so serving it grants no authority
// beyond what the operator already has at their own shell — per-session attach
// stays opt-in via the `observer <tool> --attach` flag.
type TerminalAttachConfig struct {
	// Enabled gates the daemon's attach socket. Default TRUE. With it false the
	// daemon does not serve the socket and `--attach` has nothing to connect to.
	Enabled bool `toml:"enabled"`
	// RouteProxy makes attach-launched children inherit the launcher's
	// proxy-routing env (so an attach session captures tokens through :8820).
	// Default TRUE (design §6 decision #7: proxy-routed by default with an
	// escape hatch — the CLI `--no-proxy` flag and a future Settings toggle both
	// express opting out).
	RouteProxy bool `toml:"route_proxy"`
	// DefaultOn makes the `observer <tool>` launchers attach by default
	// (resilient-attach arc): a launch resumes/attaches to a live session
	// automatically unless the operator opts out per-launch with `--no-attach`.
	// Default TRUE. Read per-launch by the CLI, so a change takes effect on the
	// next launch with no daemon restart. The partial-merge default keeps this
	// TRUE for an existing [terminal.attach] block that predates the key: the
	// field is seeded true in Default() and BurntSushi's field-level decode
	// leaves an absent key untouched (same mechanism as RouteProxy above).
	DefaultOn bool `toml:"default_on"`
	// ReclaimOnInput lets a native `observer <tool> --attach` terminal RE-TAKE
	// the writer after a dashboard Jump-in stole it: when the wrapper's lease is
	// revoked and the operator types a real key (anything but a bare ESC), the
	// daemon re-acquires the local writer through the normal funnel, delivers the
	// keystroke, and tells the wrapper control is back. ESC-initiated chunks —
	// including a TUI's machine-generated cursor-position / Device-Attributes
	// replies — never reclaim, so the dashboard's control is never stolen with
	// nobody typing. Default TRUE. Off restores the fence-and-notify behavior
	// (one revoked notice, keystrokes dropped). The partial-merge default keeps
	// this TRUE for an existing [terminal.attach] block that predates the key
	// (seeded true in Default(); BurntSushi leaves an absent key untouched, same
	// mechanism as RouteProxy / DefaultOn above).
	ReclaimOnInput bool `toml:"reclaim_on_input"`
}

// TerminalLaunchConfig is the [terminal.launch] block — the fresh-agent
// launch opt-in (plan §8/F1). Every field defaults to the ZERO value (off /
// empty): a fresh (non-handoff) agent launch from the dashboard is refused
// until the operator consciously grants it. This block EXPANDS execution
// authority, so it is never seeded on by Default().
//
// `allow_shell` is intentionally ABSENT — a general browser shell is a
// separate, separately-reviewed feature, not part of F1.
type TerminalLaunchConfig struct {
	// AllowFreshAgent is the master opt-in for non-handoff launches. Default
	// FALSE. With it false, POST /api/terminal/launch refuses every request.
	AllowFreshAgent bool `toml:"allow_fresh_agent"`
	// AllowedTools is the allow-list of launchable tool names a fresh launch
	// may start (e.g. ["claude-code","codex"]). Empty = none (deny-all). A
	// tool must ALSO be launchable in the capability registry.
	AllowedTools []string `toml:"allowed_tools"`
	// AllowedProjectRoots is the operator-configured allow-list of directories
	// a fresh launch may set as the child cwd. Each entry is canonicalized
	// (real filesystem identity, symlinks resolved) at validation time; a
	// requested project_root must canonicalize to (or under) an entry. Empty
	// = no project_root is accepted (the launcher's own default cwd is used).
	// This is NOT "a project Observer has seen" — observed roots are learned
	// from tool data and may be stale or attacker-influenced.
	AllowedProjectRoots []string `toml:"allowed_project_roots"`
}

// TerminalStatusConfig is the [terminal.status] block — agent-status
// detection (plan §8/F4). Default-on. Per-tool prompt patterns are shipped
// data, not config.
type TerminalStatusConfig struct {
	// Enabled gates the status classifier + the status API/WS frame. Default
	// TRUE (seeded by Default()).
	Enabled bool `toml:"enabled"`
}

// BenchmarkConfig is the [benchmark] surface — the Benchmarks Harness
// (docs/plans/benchmarks-harness-plan-2026-07-11.md). LOCAL-ONLY, never
// distributed to an org (same posture as [routing]/[predict]). The harness
// itself is CLI-driven and operator-invoked; the only persistent config today
// is the retention horizon for the node-local benchmark_* tables.
type BenchmarkConfig struct {
	// RetentionDays is the horizon for the benchmark_* prune sweep (plan
	// §3.12), swept by runRetention through store.PruneBenchmarkRows. Benchmark
	// rows hold repo paths, prompts, final-answer excerpts, and judge
	// rationales, so the default is bounded — 180 days keeps ~6 months of run
	// history for baseline diffs. 0 = keep forever.
	RetentionDays int `toml:"retention_days"`
}

// CodeIntelConfig is the [codeintel] surface — the in-process code-
// intelligence module (docs/codeintel/). LOCAL-ONLY, never distributed
// to an org (same posture as [routing]/[predict]/[cachewarm]). Same
// partial-merge invariant as CacheTrackConfig: an install with no
// [codeintel] section gets Enabled=true (Default() seeds it); a partial
// section keeps unset fields at their Default() values.
//
// Defaults are conservative: indexing is read-only on the repo; the
// only model-visible change (compression.code_aggressive) is OFF and
// opt-in per project AND language.
type CodeIntelConfig struct {
	// Enabled gates the whole module. When false the indexer doesn't
	// run, the native provider reports unavailable, and the CLI/MCP
	// surfaces stay empty.
	Enabled bool `toml:"enabled"`
	// Languages, when non-empty, restricts indexing to this subset of
	// the embedded language set. Empty = all supported languages.
	Languages []string `toml:"languages"`
	// AutoIndexLimit consent-gates a NEW project whose file count
	// exceeds it (a huge monorepo waits for `observer index <path>`).
	// Default 25000.
	AutoIndexLimit int `toml:"auto_index_limit"`
	// MaxFileBytes skips files larger than this. Default 2_000_000.
	MaxFileBytes int64 `toml:"max_file_bytes"`
	// IgnorePaths lists project-root path prefixes the auto-indexer must
	// never index. A project at or under any of these is skipped on
	// `observer start` (an explicit `observer index <path>` still works,
	// with a warning). Use absolute paths. Local-only; never distributed
	// to an org server.
	IgnorePaths []string `toml:"ignore_paths"`
	// RetentionDays is the codeintel_* prune horizon: a project whose
	// most recent index pass (MAX(codeintel_files.indexed_at)) is older
	// than this is deleted wholesale — files, nodes, edges, sites,
	// minhash, embeddings, FTS — via the same per-project delete path
	// `observer index delete -r` uses. The index is rebuildable from
	// the repo, so age-pruning is safe; a project still being indexed
	// always has a fresh indexed_at and is never touched, and a project
	// with NO successful index pass yet (MAX(indexed_at)=0, e.g.
	// freshly registered pending files) is also never touched. Swept by
	// the retention pass (startup + the [observer.retention] periodic
	// tick + `observer prune`). Default 90 — conservative, so an
	// actively-used index is never nuked. ≤ 0 disables the codeintel
	// prune entirely.
	RetentionDays int `toml:"retention_days"`

	Index       CodeIntelIndexConfig       `toml:"index"`
	Compression CodeIntelCompressionConfig `toml:"compression"`
	Semantic    CodeIntelSemanticConfig    `toml:"semantic"`
}

// CodeIntelIndexConfig is the [codeintel.index] block — resource +
// scheduling controls for the offline indexer.
type CodeIntelIndexConfig struct {
	// OnStart indexes known projects when `observer start` boots.
	// Default true.
	OnStart bool `toml:"on_start"`
	// Watch incrementally re-indexes on save (reuses fsnotify
	// mechanics, distinct from the session-file watcher). Default true.
	Watch bool `toml:"watch"`
	// Mode is "auto" (index/watch automatically) or "manual" (only
	// `observer index`). Default "auto".
	Mode string `toml:"mode"`
	// Workers caps index parallelism (0 = auto). Default 0.
	Workers int `toml:"workers"`
	// IdleOnly pauses indexing while the machine is busy. Default false.
	IdleOnly bool `toml:"idle_only"`
	// DiskBudgetMB caps the index size; cold projects LRU-evict past
	// this. Default 500. 0 = no cap.
	DiskBudgetMB int `toml:"disk_budget_mb"`
}

// CodeIntelCompressionConfig is the [codeintel.compression] block — the
// ONLY model-visible behaviour, all OFF by default.
type CodeIntelCompressionConfig struct {
	// CodeAggressive enables opt-in body-collapse. DEFAULT OFF.
	CodeAggressive bool `toml:"code_aggressive"`
	// AggressiveLanguages opts in body-collapse per language; empty =
	// none even when CodeAggressive is true.
	AggressiveLanguages []string `toml:"aggressive_languages"`
	// PreviewOnly logs what WOULD collapse and changes nothing.
	PreviewOnly bool `toml:"preview_only"`
}

// CodeIntelSemanticConfig is the [codeintel.semantic] block.
type CodeIntelSemanticConfig struct {
	// Embedder selects the embedding backend: "tfidf" (default) or
	// (future) "neural".
	Embedder string `toml:"embedder"`
	// SimilarTo enables MinHash/LSH near-clone edges. Default true.
	SimilarTo bool `toml:"similar_to"`
}

// CacheWarmConfig is the [cachewarm] surface — the cache-expiry warning
// system + the opt-in smart keep-warm
// (docs/plans/cache-expiry-warning-and-keepwarm-plan-2026-06-25.md).
// LOCAL-ONLY, never distributed (same posture as [predict]/[routing]).
//
// The WARNING half (Part A) is default-ON: it is a pure read over the
// cache_entries the engine already writes, with zero LLM cost and no
// outward network call. The KEEP-WARM half (Part B, the nested Keepwarm
// block) is OFF by default — it is an outward-facing, money-spending
// action and is treated like routing enforce: explicit operator opt-in.
//
// Same partial-merge invariant as CacheTrackConfig: an install with no
// [cachewarm] section MUST get Enabled=true (Default() seeds it); a
// partial section keeps the unset fields at their Default() values.
type CacheWarmConfig struct {
	// Enabled gates the warning system. When false the dashboard/CLI/MCP
	// cache-expiry surfaces stay empty and no keep-warm runs.
	Enabled bool `toml:"enabled"`
	// WarnAtSeconds is the time-to-expiry threshold for the 'soon'
	// severity. Default 90.
	WarnAtSeconds int `toml:"warn_at_seconds"`
	// CriticalAtSeconds is the time-to-expiry threshold for the
	// 'critical' severity. Default 30. Clamped to ≤ WarnAtSeconds.
	CriticalAtSeconds int `toml:"critical_at_seconds"`
	// MinValueUSD suppresses warnings for caches whose value-at-risk is
	// below this floor (not worth keeping warm). Default 0.05.
	MinValueUSD float64 `toml:"min_value_usd"`
	// Implicit (OpenAI/Codex) caches expose NO fixed TTL — survival is a
	// best-effort retention policy, not a lease (see
	// docs/general_info/openai_cache_expiry.md). We model it as a GRADED
	// idle-risk progression keyed on time since last activity, surfaced as
	// an ESTIMATE (the card hedges with "~"):
	//   idle < ImplicitWarnSeconds          → ok    (high-confidence reuse)
	//   ImplicitWarnSeconds ≤ idle < Crit    → soon  ("at risk of expiry")
	//   ImplicitCriticalSeconds ≤ idle < Max → critical ("significantly
	//                                           increased risk of expiry")
	//   idle ≥ ImplicitMaxSeconds            → expired (hard max)
	// Defaults follow the extended-cache reality for gpt-5.5/gpt-5/gpt-4.1:
	// 1h / 2h / 24h. Anthropic explicit caches ignore these — they carry a
	// real TTL and use WarnAtSeconds/CriticalAtSeconds.
	ImplicitWarnSeconds int `toml:"implicit_warn_seconds"`
	// ImplicitCriticalSeconds — idle age at which an implicit cache is at
	// significantly increased risk of eviction. Default 7200 (2h).
	ImplicitCriticalSeconds int `toml:"implicit_critical_seconds"`
	// ImplicitMaxSeconds — idle age at which an implicit cache is treated as
	// expired (OpenAI's hard 24h retention ceiling). Default 86400 (24h).
	ImplicitMaxSeconds int `toml:"implicit_max_seconds"`
	// Keepwarm is the Part B keep-warm sub-surface (advise/enforce).
	Keepwarm KeepWarmConfig `toml:"keepwarm"`
}

// KeepWarmConfig is the [cachewarm.keepwarm] sub-surface — the Part B
// economics + action mode. Default OFF: it is the only outward-facing,
// money-spending action in this feature.
type KeepWarmConfig struct {
	// Mode is "off" | "advise" | "enforce". Default "off". advise
	// surfaces a recommendation (e.g. switch to the 1h tier) but sends
	// nothing; enforce additionally permits the proxy in-memory replay
	// path (Anthropic proxied sessions only) — built in a later phase.
	Mode string `toml:"mode"`
	// MinValueUSD is the value-at-risk floor below which keep-warm is
	// never recommended. Default 0.20 (higher than the warning floor —
	// it is not worth acting on small caches).
	MinValueUSD float64 `toml:"min_value_usd"`
	// MinResumeConfidence gates a keep-warm bet on the likelihood the
	// session actually resumes (a warm cache nobody returns to is wasted
	// spend). 0..1. Default 0.5.
	MinResumeConfidence float64 `toml:"min_resume_confidence"`
}

// CacheTrackConfig gates the proxy-side cache observation engine
// (docs/plans/cache-tracking-implementation-spec-2026-06-08.md §11).
// Default-ON per spec §11: the feature is local, passive, network-
// free, and writes only hashes/counts/enums (no content). An
// install with no [cachetrack] section MUST get Enabled=true via
// the loader's partial-merge against Default() (NOT zero-valued
// false) — the live-daemon-captures-nothing bug traced to this.
type CacheTrackConfig struct {
	// Enabled gates the engine. When false, the proxy still
	// parses request bodies for cache_control markers (cheap;
	// already in the requestShape single pass) but the engine
	// is not constructed and no cache_* rows are written.
	Enabled bool `toml:"enabled"`
	// MaxTrackedSessions is the LRU bound on the engine's
	// per-session CacheModel map. Default 64. ≤ 0 disables the
	// cap (unbounded; not recommended).
	MaxTrackedSessions int `toml:"max_tracked_sessions"`
	// CalibrateLogPath enables the per-block diagnostic sidecar
	// when non-empty: every block fed to Engine.ObserveTurn is
	// written as one JSON line carrying (api_turn_id, seq, level,
	// kind, len_raw, sha_raw, len_canon, sha_canon) — plus a
	// bounded canonical-bytes prefix for tools+system levels
	// (message-level stays hash-only per CLAUDE.md "no content"
	// rule).
	//
	// Off by default (""). When set, auto-stops after ~200 blocks
	// so the file stays small and the daemon's hot path is
	// untouched for the rest of the soak. Used to localize chain-
	// hash drift to the lowest-seq differing block (the 39326aa9
	// soak post Fix B left seq=29 tools boundary still drifting
	// — this sidecar is the next-step diagnostic).
	CalibrateLogPath string `toml:"calibrate_log_path"`
	// RetentionDays is the per-table horizon for the cache_* row
	// sweep (spec §9). cache_segments + cache_events past this
	// horizon are deleted; cache_entries in terminal states
	// (expired / invalidated / unverified) past a tighter 14-day
	// horizon are also deleted (live entries are never pruned
	// regardless of age — they may still be in the provider).
	// Default 90. ≤ 0 disables the cache prune entirely.
	//
	// The sweep runs from the retention pass (startup + the
	// [observer.retention].interval_hours periodic tick) — the
	// same pass that handles actions / observer_log /
	// file_state. See cmd/observer/prune.go::runRetention. Each
	// call is idempotent: a second run within the same horizon
	// is a no-op (TestPruneCacheRows_SecondRunNoop).
	//
	// NOT pushed to the org server: cache_* tables are NODE-LOCAL
	// per `tests/invariant/privacy_test.go::TestSelectUnpushedSinceExcludesCacheTables`.
	// The sweep stays node-local; no org-push coupling.
	RetentionDays int `toml:"retention_days"`
	// ---
	// DELIBERATE OMISSION — R7 cache_scope salt.
	//
	// Spec §11 + §24.4 R7 names `sha256(upstream_host + ":" +
	// auth_identity + scope_salt)[:16]` as the eventual cache_scope
	// derivation. Today both seam call sites
	// (`internal/proxy/proxy.go::966` Tier-1 +
	// `internal/store/store.go::1376` Tier-2) use the literal
	// "default" — workspace-blind, single-scope. The R7 derivation
	// landing means wiring an auth_identity source (header parse?
	// operator email? machine ID?) at BOTH seams, then composing
	// with this salt. That's a cross-cutting change touching
	// proxy + store + per-adapter Tier-2 emit. Out of scope for
	// the v1 cachetrack closure; tracked as backlog item 9 in
	// `docs/plans/cachetrack-p3-backlog-2026-06-09.md` (R7
	// cache_scope derivation). DO NOT add ScopeSalt as an empty
	// field — the absence of the field IS the documentation that
	// this is deferred. If/when R7 lands, the field shape is
	// likely `string` plus a non-empty default per install.
}

// ExporterConfig groups second-rail telemetry exporters (Teams & Org
// Visibility, spec §2.4.3). Currently only the OTel exporter. Like the org
// client it is OFF by default: a solo-local install has no [exporter] section
// (or Enabled=false on each sub-exporter), so the daemon makes zero exporter
// network calls and the behaviour is byte-identical to a non-org build.
type ExporterConfig struct {
	OTel OTelExporterConfig `toml:"otel"`
}

// IngestConfig groups the agent-side native-telemetry receivers
// (native-console integration). Today only the OTLP logs receiver exists.
type IngestConfig struct {
	OTel IngestOTelConfig `toml:"otel"`
}

// IngestOTelConfig configures the embedded OTLP logs receiver that ingests a
// coding assistant's native telemetry (e.g. Claude Code with
// CLAUDE_CODE_ENABLE_TELEMETRY=1) directly — no OpenTelemetry Collector needed.
// Disabled by default: a solo-local install opens no listener and the
// solo-local UX stays byte-identical.
type IngestOTelConfig struct {
	// Enabled gates the receiver. When false (default), start.go opens no
	// listener and the process accepts no OTLP traffic.
	Enabled bool `toml:"enabled"`
	// GRPCAddr / HTTPAddr are the loopback binds for OTLP/gRPC and OTLP/HTTP.
	// Empty disables that transport. Defaults 127.0.0.1:4317 / 127.0.0.1:4318.
	GRPCAddr string `toml:"grpc_addr"`
	HTTPAddr string `toml:"http_addr"`
	// AllowNonLoopback permits binding a non-loopback address. Default false —
	// opening the receiver to a network is an explicit operator decision with
	// a documented threat model (native-console template §2.2 / L3).
	AllowNonLoopback bool `toml:"allow_non_loopback"`
	// ContentCapture sets how much of the OTel stream's CONTENT the receiver
	// stores: "full" (prompts + tool I/O bodies, when the admin enabled the
	// OTEL_LOG_* flags at Claude Code), "metadata" (turns/tokens only — content
	// events skipped), or "none" (alias of metadata). Default "full" — in an
	// admin-driven deployment the admin already chose to emit this content at
	// the source. Stored content is scrubbed for secrets and obeys the same
	// node-side push gate as locally-captured content.
	ContentCapture string `toml:"content_capture"`
}

// OTelExporterConfig configures the agent-side OpenTelemetry exporter that
// emits one gen_ai.client span per api_turns row to any OTLP/HTTP endpoint
// (spec §2.4.3). It requires only M0 (it works identically on a solo-local
// install and never couples to the org server). All fields default to the
// safe, privacy-preserving option; OTEL_* environment variables override the
// file values at construction time per the OTel configuration spec.
type OTelExporterConfig struct {
	// Enabled gates the exporter. When false (the default), start.go starts
	// no exporter goroutine and the process makes zero OTLP network calls.
	Enabled bool `toml:"enabled"`
	// Endpoint is the OTLP/HTTP collector endpoint as host:port (no scheme,
	// no path — the SDK appends /v1/traces). Default "localhost:4318".
	// Overridden by OTEL_EXPORTER_OTLP_ENDPOINT / _TRACES_ENDPOINT.
	Endpoint string `toml:"endpoint"`
	// Insecure sends over plain HTTP instead of HTTPS. Default false.
	Insecure bool `toml:"insecure"`
	// PollIntervalSeconds is the row-tail poll cadence against api_turns.id.
	// Default 1.
	PollIntervalSeconds int `toml:"poll_interval_seconds"`
	// EmitPromptContent attaches prompt/completion bodies as the
	// gen_ai.client.inference.operation.details event. Default false — the
	// data-volume and privacy implications are documented in the exporter's
	// doc.go; the operator who turns it on has read that doc.
	EmitPromptContent bool `toml:"emit_prompt_content"`
	// EmitUserEmail attaches sbo.user.email when the agent is enrolled.
	// Default false — opt-in for the customer who wants per-developer slicing
	// in their own backend.
	EmitUserEmail bool `toml:"emit_user_email"`
	// SemconvStability is the value the exporter advertises for
	// OTEL_SEMCONV_STABILITY_OPT_IN. Default "gen_ai_latest_experimental" so
	// the exporter emits the v1.41.0 gen_ai.* attribute names. Overridden by
	// the OTEL_SEMCONV_STABILITY_OPT_IN environment variable.
	SemconvStability string `toml:"semconv_stability"`
}

// OrgClientConfig configures the Teams & Org Visibility agent-side push
// client (spec §2.6). It is OFF by default: a solo-local install has no
// [org_client] section (or Enabled=false), and that absence is the trigger
// for the no-op path — the daemon starts no push loop and writes no org
// data. Only an enrolled agent with Enabled=true pushes anything.
type OrgClientConfig struct {
	// Enabled gates the entire push loop. When false (the default), the
	// agent behaves byte-identically to a non-org build.
	Enabled bool `toml:"enabled"`
	// OrgServerURL is the base URL of the customer's org server, e.g.
	// "https://observer-org.acme.example". Required when Enabled.
	OrgServerURL string `toml:"org_server_url"`
	// PushIntervalSeconds is the cadence of the push loop. Default 900 (15m).
	PushIntervalSeconds int `toml:"push_interval_seconds"`
	// PolicyPollIntervalSeconds is the cadence of the org policy-bundle
	// poll (guard spec §14.2). Default 3600 (1h). The poll also fires
	// once at `observer start`. Only meaningful on an enrolled agent
	// with the guard enabled — the daemon starts no poll loop
	// otherwise.
	PolicyPollIntervalSeconds int `toml:"policy_poll_interval_seconds"`
	// MaxPushBytes caps the uncompressed JSON size of a single batch.
	// Default 1 MiB; the client clamps to MaxPushBytesCeiling (16 MiB).
	MaxPushBytes int64 `toml:"max_push_bytes"`
	// KeychainID is the OS-keychain service/account handle under which the
	// bearer (and the agent's Ed25519 signing key) are stored.
	KeychainID string `toml:"keychain_id"`
	// Share gates the v1.8.0 content-bearing columns on the push payload.
	// Default zero value = metadata-only (hashes + counts only; raw paths
	// and commands withheld). A node operator can opt into full-content
	// sharing by setting [org_client.share].full_content = true. The org
	// admin cannot force this remotely — it lives solely in the node's
	// local config file.
	Share OrgClientShareConfig `toml:"share"`
	// Scope restricts which projects (by root path) push at all. Default
	// zero value = all projects. Combine with Share to narrow what
	// crosses the wire from both axes.
	Scope OrgClientScopeConfig `toml:"scope"`
}

// OrgClientScopeConfig restricts which projects (by root path) feed
// into the push payload. Both lists are exact-string match against
// `projects.root_path`. When ProjectRootAllowlist is non-empty, ONLY
// rows whose project_root is in the list are eligible. When
// ProjectRootDenylist is non-empty, rows whose project_root is in the
// list are skipped. Allowlist + denylist can be combined; denylist is
// applied to the allowlist result.
//
// Both lists are per-node config (TOML); the org admin cannot set them
// remotely. They sit alongside OrgClientShareConfig as the operator's
// other major scope-narrowing knob (alongside the share-mode opt-in).
type OrgClientScopeConfig struct {
	ProjectRootAllowlist []string `toml:"project_root_allowlist"`
	ProjectRootDenylist  []string `toml:"project_root_denylist"`
}

// OrgClientShareConfig is the per-node opt-in for full-content org
// sharing (v1.8.0 privacy posture, addressing Issues 1 + 2 of the
// 2026-06-02 teams test findings).
//
// FullContent, when true, causes the push seam to ship raw command
// bodies (actions.target for run_command), raw assistant prose
// (actions.target for task_complete), raw filesystem paths
// (actions.source_file, sessions.project_root, sessions.git_remote,
// api_turns.project_root, token_usage.project_root +
// token_usage.source_file) in addition to the always-present sha256
// hashes. When false (the default), only the hashes ship; raw
// content/path columns are stripped at the SQL seam.
//
// TargetActionAllowlist is a per-action opt-in for the raw target
// column specifically, useful when the operator wants the org dashboard
// to display human-readable file paths for safe action types
// (read_file, edit_file, write_file) but withhold commands
// (run_command) and prose (task_complete). Values must be exact
// action_type strings from models.ActionXxx. Empty list means: no
// per-action exception — when FullContent is false, NO action ships a
// raw target.
//
// These knobs are intentionally redundant with FullContent (a node that
// turns FullContent on doesn't need the allowlist) so a cautious
// operator can ship a *targeted* subset without buying into the full
// content posture. The contract is the same in either case: this lives
// on the node, the server cannot flip it remotely.
//
// PLANE BOUNDARY (docs/deployment-models.md; audit finding M1). This
// one struct carries flags for BOTH deployment planes. Grouped below:
//   - Plane B (this node's OWN coding-agent usage → org admin):
//     FullContent, TargetActionAllowlist, AdminManaged, RoutingSummary.
//   - Plane A (general observability of an admin/org-hosted LLM app whose
//     END-USERS route through Observer): ObsSummary, ObsTraces, ObsContent,
//     ObsEvalSummary.
//
// The wire paths are already separate (obs tiers compose via the
// store.ObsOrgProviders func seam; the privacy sentinel forbids obs_*
// table names in orgpush.go). This grouping is legibility only — every
// flag is node-side opt-in, default false, never server-forced.
type OrgClientShareConfig struct {
	// --- Plane B: this node's own coding-agent usage ---
	FullContent           bool     `toml:"full_content"`
	TargetActionAllowlist []string `toml:"target_action_allowlist"`
	// AdminManaged flips the content-sharing default for an admin-driven
	// (native-console) deployment: when true, all content-bearing columns ship
	// raw by default. The premise differs from FullContent's node-opt-in — here
	// the org admin provisions the node via managed-settings/MDM and configured
	// the telemetry collection at the source, so sharing-by-default is the
	// intended posture. It remains a NODE-SIDE config the admin authors through
	// provisioning; there is no server-side force override (the no-remote-force
	// invariant holds). Default false — the zero value stays metadata-only.
	AdminManaged bool `toml:"admin_managed"`
	// RoutingSummary opts the §R19.4 routing aggregate (counts +
	// dollars by tier/reason ONLY — never decision rows) onto the
	// push. Its own consent toggle, default false, node-side only —
	// the org admin cannot force it (model-routing spec §R26.4 +
	// the share-mode posture).
	RoutingSummary bool `toml:"routing_summary"`
	// --- Plane A: general observability of an admin/org-hosted LLM app ---
	// Org-tier observability opt-ins now live under the nested
	// [org_client.share.obs] sub-table (Obs below) so the config namespace
	// reflects the plane split (plane-separation audit M1). The four flat
	// keys after it (obs_summary/obs_traces/obs_content/obs_eval_summary)
	// are DEPRECATED but still parsed for one release: migrateLegacyOrgShareObs
	// maps them onto Obs.* at load (deprecation-warning once per key), and
	// `observer config migrate` (step 2) physically rewrites them.
	Obs OrgClientShareObsConfig `toml:"obs"`

	// Deprecated flat obs share keys — kept parseable for one release for
	// config-compat. Prefer [org_client.share.obs] (Obs above). Consumers
	// read Obs.*; the load-time shim copies these onto it when Obs.* is unset.
	ObsSummary     bool `toml:"obs_summary"`
	ObsTraces      bool `toml:"obs_traces"`
	ObsContent     bool `toml:"obs_content"`
	ObsEvalSummary bool `toml:"obs_eval_summary"`
}

// OrgClientShareObsConfig is [org_client.share.obs] — the Plane-A org-tier
// observability opt-ins (obs-org-tier plan §1, the T1–T4 ladder). Each
// default false, each independent, each node-side only (no server force).
// Summary = T1 aggregate rollup (content-free); Traces = T2 trace/span
// structure (hashes only); Content = T3 raw span bodies (additionally
// requires full_content/admin_managed for the raw body — the content_hash
// ships regardless); EvalSummary = T4 eval-run health. The underlying obs_*
// tables stay node-local; only these aggregates/structure cross the wire,
// via the obs provider seam.
type OrgClientShareObsConfig struct {
	Summary     bool `toml:"summary"`
	Traces      bool `toml:"traces"`
	Content     bool `toml:"content"`
	EvalSummary bool `toml:"eval_summary"`
	// Admission = T6 input-admission verdicts + policy snapshots (Plane-A
	// admission org tier, gap-audit 2026-07-10 §2.1 / #1a). Default false,
	// node-side opt-in only — there is NO remote toggle, the org admin cannot
	// force it (same posture as the other obs_* opt-ins). Verdict metadata is
	// content-free; the PII/prose columns additionally require
	// full_content/admin_managed, while the admin-authored policy body ships
	// regardless. The underlying obs_admission_* tables stay node-local; only
	// this tier's rows cross the wire, via the obs provider seam.
	Admission bool `toml:"admission"`
	// EvalItems = T7 per-item eval scores (Plane-A eval-run detail org tier,
	// gap-audit 2026-07-10 §1 / §2.2 / §6). Default false, node-side opt-in
	// only — there is NO remote toggle (same posture as the other obs_*
	// opt-ins). Distinct from EvalSummary (T4, run/scorer aggregates): this
	// ships the per-item scores that let the org Evals page drill into one run
	// and diff two runs. The score metadata + content_hash are content-free;
	// the item content excerpts additionally require full_content/admin_managed.
	// The underlying obs_eval_* tables stay node-local; only this tier's rows
	// cross the wire, via the obs provider seam.
	EvalItems bool `toml:"eval_items"`
}

// Org-client push-size bounds (spec §2.4.2).
const (
	// DefaultMaxPushBytes is the default uncompressed batch ceiling (1 MiB).
	DefaultMaxPushBytes int64 = 1 << 20
	// MaxPushBytesCeiling is the hard upper bound the client clamps to (16 MiB).
	MaxPushBytesCeiling int64 = 16 << 20
	// DefaultPushIntervalSeconds is the default push cadence (15 minutes).
	DefaultPushIntervalSeconds = 900
	// DefaultPolicyPollIntervalSeconds is the default org policy-bundle
	// poll cadence (1 hour — guard spec §14.2).
	DefaultPolicyPollIntervalSeconds = 3600
	// DefaultKeychainID is the default keychain service handle.
	DefaultKeychainID = "sbo-org-bearer-v1"
)

// OTel exporter defaults (spec §2.4.3 / §2.7).
const (
	// DefaultOTelEndpoint is the default OTLP/HTTP collector endpoint
	// (host:port). The SDK appends the /v1/traces path.
	DefaultOTelEndpoint = "localhost:4318"
	// DefaultOTelPollIntervalSeconds is the default api_turns row-tail
	// poll cadence.
	DefaultOTelPollIntervalSeconds = 1
	// DefaultOTelSemconvStability emits the v1.41.0 GenAI attribute names
	// per the OTel semconv transition plan.
	DefaultOTelSemconvStability = "gen_ai_latest_experimental"
	// DefaultIngestOTelGRPCAddr / DefaultIngestOTelHTTPAddr are the loopback
	// binds for the embedded OTLP logs receiver — the standard OTLP ports on
	// 127.0.0.1 so managed-settings can point Claude Code straight at them.
	DefaultIngestOTelGRPCAddr = "127.0.0.1:4317"
	DefaultIngestOTelHTTPAddr = "127.0.0.1:4318"

	// Content-capture levels for [ingest.otel].content_capture.
	ContentCaptureFull     = "full"     // store prompts + tool I/O content
	ContentCaptureMetadata = "metadata" // turns/tokens only; skip content events
	ContentCaptureNone     = "none"     // alias of metadata
)

// CapturesContent reports whether the configured content-capture level stores
// OTel content events. Unknown/empty values are treated as "full" (the default).
func (c IngestOTelConfig) CapturesContent() bool {
	switch c.ContentCapture {
	case ContentCaptureMetadata, ContentCaptureNone:
		return false
	default: // ContentCaptureFull and any unrecognized value
		return true
	}
}

// ObserverConfig groups settings for the capture side of the system.
type ObserverConfig struct {
	DBPath      string            `toml:"db_path"`
	LogLevel    string            `toml:"log_level"`
	Watch       WatchConfig       `toml:"watch"`
	Freshness   FreshnessConfig   `toml:"freshness"`
	Secrets     SecretsConfig     `toml:"secrets"`
	Retention   RetentionConfig   `toml:"retention"`
	Hooks       HooksConfig       `toml:"hooks"`
	Antigravity AntigravityConfig `toml:"antigravity"`
	Process     ProcessConfig     `toml:"process"`

	// ConfigVersion is the schema-migration stamp written by the config
	// auto-migration rail (internal/config/migrate). It records the
	// highest migration step applied to this file so the migrator can
	// skip an already-current config cheaply. Absent/0 on legacy files.
	// The migration DECISION reads the version from the raw file text;
	// this field only keeps the key out of the decoder's Undecoded set
	// and available to any in-process consumer. See MigrateFile.
	ConfigVersion int `toml:"config_version"`
}

// AntigravityConfig controls the Antigravity adapter's behavior.
//
// NetworkRecovery selects the fallback strategy when local .pb file
// decryption fails (which is currently the case on Windows hosts —
// the documented AES-128-CTR scheme doesn't match the Windows-side
// cipher). Values:
//
//   - "" / "off" (default): no fallback. Decrypt failure → warning,
//     skip the file. Pure-local, no network calls, no process tree
//     introspection.
//   - "local": try the running language_server's gRPC API on
//     localhost, falling back through the article-described
//     ConvertTrajectoryToMarkdown path. Requires Antigravity to be
//     running. The language_server has the in-memory key and decrypts
//     locally; we just consume the Markdown response and parse it
//     into events. Lossy compared to direct decryption (no per-tool
//     args, no token counts) but recovers the conversation flow.
//
// The "local" mode is opt-in because:
//   - It introspects running processes (visible cmdline args, CSRF
//     tokens) — strictly local, but a behavior change worth surfacing.
//   - On WSL2-with-Windows-Antigravity, the call requires a Windows-
//     side bridge (PowerShell shell-out) which adds per-call latency.
//   - The Markdown parser produces approximate events, not the
//     full-fidelity ToolEvent stream a real .pb decrypt would yield.
type AntigravityConfig struct {
	NetworkRecovery string `toml:"network_recovery"`

	// DumpShapeMismatchesDir, when non-empty, enables an opt-in
	// debug mode that writes the raw GetCascadeTrajectory gRPC
	// response bytes to disk whenever the adapter's
	// ParseStructuredTrajectory yields zero tokens AND empty model
	// on a non-trivial payload (≥ 10 KiB). Each dump goes to
	// <dir>/<conversation_id>.bin so the operator can compare wire
	// shapes between known-working and known-broken sessions when
	// investigating the v1.6.10 audit residual: some sessions
	// return 100s of KB of structured data but the proto-path
	// mapping at structured.go:122-185 doesn't extract anything
	// (e.g. maintainer session e371fdb1-… returned 430,202 bytes,
	// 0 tokens, model="").
	//
	// Default empty = no dumping. Set to an absolute path like
	// "/tmp/antigravity-shape-dumps/" to enable. The directory is
	// created on first dump (mkdir -p semantics). Files are owned
	// by the observer process; rotate or delete manually. (Issue
	// #5 follow-up — first pass only added the tracef warning.)
	DumpShapeMismatchesDir string `toml:"dump_shape_mismatches_dir"`
}

// ProcessConfig is the [observer.process] surface — the optional
// OS-level process-capture layer (docs/process-observability.md §11).
//
// UNLIKE CacheTrack/Guard/Advisor, this feature is OPT-IN: Enabled
// defaults to FALSE (D1). It may require elevated privileges and captures
// sensitive process metadata, so an install with no [observer.process]
// section makes zero process-backend calls and behaves byte-identically to
// a build without the feature (MVP acceptance criterion 1).
//
// The non-zero defaults below (Backend, RetentionDays, QueueSize,
// BatchSize, and the sub-section fields) only take effect once the operator
// flips Enabled. They are set in Default() so a partial [observer.process]
// section (e.g. just `enabled = true`) inherits sane values rather than
// zero — the same partial-merge discipline CacheTrackConfig documents.
type ProcessConfig struct {
	// Enabled gates the whole backend lifecycle. False = no backend is
	// constructed, no process_runs/process_events rows are written.
	// Restart-gated: flipping this in a running daemon takes effect on the
	// next start (the dashboard setup flow says so — §11).
	Enabled bool `toml:"enabled"`
	// Backend selects the capture source: "auto" (pick the best available
	// for the host OS), "linux_ebpf", "etw", "endpointsecurity", "poll"
	// (low-fidelity dev/test fallback), or "off". Default "auto".
	Backend string `toml:"backend"`
	// CaptureUnattributed stores process rows that could not be joined to a
	// session (D5/§9.2.7). Default false: unattributed processes are counted
	// for health only, never persisted, to bound the privacy blast radius.
	CaptureUnattributed bool `toml:"capture_unattributed"`
	// RetentionDays is the process_runs/process_events prune horizon, swept
	// by the retention pass — startup + the [observer.retention].
	// interval_hours periodic tick (cmd/observer/prune.go::runRetention).
	// Default 30. ≤ 0 disables the process prune.
	RetentionDays int `toml:"retention_days"`
	// QueueSize bounds the userspace enrichment queue between the backend
	// and the store batch writer. Overflow drops newest low-value events
	// after a health counter (§15). Default 10000.
	QueueSize int `toml:"queue_size"`
	// BatchSize is the store insert batch size (§15 targets 100–500 rows
	// per transaction). Default 250.
	BatchSize int `toml:"batch_size"`

	// WindowsBinaryPath is the Windows observer.exe the cross-OS bridge
	// (§5.5) execs over WSL interop — a /mnt/<drive>/... path (a C:\… path is
	// accepted and translated). Empty = auto-resolve. Used only by the
	// "bridge" backend (and "auto" on WSL). LOCAL-ONLY, never distributed.
	WindowsBinaryPath string `toml:"windows_binary_path"`
	// PollIntervalMS is the unified process-table snapshot cadence (the
	// "process poll rate" the dashboard Settings page exposes). It drives BOTH
	// the Linux /proc poll backend AND the Windows cross-OS bridge capturer, so
	// a single knob controls how often every process source is sampled. Default
	// 2000 (2s). Lower = fresher capture + more CPU; higher = cheaper but more
	// likely to miss short-lived commands (the poll backend can't see a process
	// that starts and exits inside one interval). LOCAL-ONLY, never distributed.
	PollIntervalMS int `toml:"poll_interval_ms"`
	// BridgePollIntervalMS optionally overrides PollIntervalMS for the bridge
	// capturer only (back-compat / asymmetric-cadence escape hatch). Zero =
	// inherit PollIntervalMS. Default 0. LOCAL-ONLY, never distributed.
	BridgePollIntervalMS int `toml:"bridge_poll_interval_ms"`
	// CorrelateIntervalMS is the cadence of the daemon's periodic background
	// cross-OS correlation sweep (docs/process-observability.md §9.2.5). The
	// deferred CorrelateCrossOS pass — the join that makes an unattributed
	// process row VISIBLE to a session — otherwise runs only lazily (a human
	// opening the dashboard Processes drawer, or `observer process tree`), so
	// captured rows stay invisible until someone looks. The sweep re-runs that
	// same idempotent, confidence-guarded store pass over the recent-active
	// session set every interval, so attribution converges without a viewer.
	// Default 90000 (90s); <= 0 inherits that default. RESTART-BOUND: the sweep
	// goroutine reads this once at daemon start (like Enabled / the poll loop),
	// so a change takes effect on the next start. LOCAL-ONLY, never distributed.
	CorrelateIntervalMS int `toml:"correlate_interval_ms"`

	Argv       ProcessArgvConfig       `toml:"argv"`
	Executable ProcessExecutableConfig `toml:"executable"`
	Env        ProcessEnvConfig        `toml:"env"`
	Network    ProcessNetworkConfig    `toml:"network"`
	Filesystem ProcessFilesystemConfig `toml:"filesystem"`
}

// ProcessArgvConfig controls argv capture (§8 Command group, §12.1).
type ProcessArgvConfig struct {
	// Mode: "preview" (scrubbed + capped preview plus full-argv hash),
	// "hash_only" (hash only, no preview — the enterprise posture), or
	// "off". Default "preview" (§19 Q1: on by default, capped, scrubbed).
	Mode string `toml:"mode"`
	// MaxPreviewBytes caps the stored argv preview. Default 512.
	MaxPreviewBytes int `toml:"max_preview_bytes"`
	// StoreArgCount keeps the integer argc even when the preview is off.
	// Default true.
	StoreArgCount bool `toml:"store_arg_count"`
}

// ProcessExecutableConfig controls executable hashing (§8 Executable group).
type ProcessExecutableConfig struct {
	// HashEnabled turns on content hashing of the resolved exe. Default
	// false (§19 Q6: useful but adds I/O cost).
	HashEnabled bool `toml:"hash_enabled"`
	// MaxHashFileSizeMB caps the file size eligible for hashing. Default 25.
	MaxHashFileSizeMB int `toml:"max_hash_file_size_mb"`
}

// ProcessEnvConfig controls environment posture capture (§8.1). Values
// always flow through the scrubber and a max-byte cap; this is posture
// (presence/hash), never full env.
type ProcessEnvConfig struct {
	// Enabled captures env posture (proxy-var presence, PATH hash,
	// virtualenv/node/CI hints). Default true.
	Enabled bool `toml:"enabled"`
	// Allowlist names additional env keys whose (scrubbed, capped) values
	// may be stored. Default empty.
	Allowlist []string `toml:"allowlist"`
	// StorePathHash stores a hash of PATH rather than its value. Default
	// true.
	StorePathHash bool `toml:"store_path_hash"`
}

// ProcessNetworkConfig controls network_connect capture (§7.2). Off until
// the privacy UX lands (§19 Q3).
type ProcessNetworkConfig struct {
	// Enabled turns on outbound-connect capture for attributed processes.
	// Default false.
	Enabled bool `toml:"enabled"`
	// CaptureRemoteHost stores the resolved remote host (scrubbed). Default
	// true (only meaningful when Enabled).
	CaptureRemoteHost bool `toml:"capture_remote_host"`
	// RedactPrivateIPs drops RFC1918/loopback destinations. Default false.
	RedactPrivateIPs bool `toml:"redact_private_ips"`
	// CaptureBodies stores capped/scrubbed request + response excerpts when a
	// capture source can see plaintext. Supported values:
	// "off" (metadata only), "proxied" (Observer proxy flows only), "available"
	// (any future plaintext/instrumented backend). Default "off".
	CaptureBodies string `toml:"capture_bodies"`
	// MaxRequestBytes caps the stored request-body excerpt. Default 65536.
	MaxRequestBytes int `toml:"max_request_bytes"`
	// MaxResponseBytes caps the stored response-body excerpt. Default 65536.
	MaxResponseBytes int `toml:"max_response_bytes"`
	// CaptureHeaders stores a scrubbed, compact allowlist of request/response
	// headers with body captures. Default true.
	CaptureHeaders bool `toml:"capture_headers"`
	// ScrubBodies applies the secret scrubber before body excerpts are stored.
	// Default true.
	ScrubBodies bool `toml:"scrub_bodies"`
	// StoreBinary allows non-text/binary response bodies to be stored as text
	// excerpts after best-effort UTF-8 conversion. Default false: binary bodies
	// get hashes/byte counts only.
	StoreBinary bool `toml:"store_binary"`
}

// ProcessFilesystemConfig controls file_write/file_open_sensitive capture
// (§7.2). Off outside high-signal paths.
type ProcessFilesystemConfig struct {
	// Enabled turns on filesystem side-effect capture. Default false.
	Enabled bool `toml:"enabled"`
	// Mode: "sensitive" (credential/config/SSH/token-like paths only),
	// "writes" (writes outside the project root), or
	// "all_attributed_writes". Default "sensitive".
	Mode string `toml:"mode"`
}

// WatchConfig controls the file watcher daemon.
type WatchConfig struct {
	PollIntervalSeconds int      `toml:"poll_interval_seconds"`
	MaxFileSizeMB       int      `toml:"max_file_size_mb"`
	EnabledAdapters     []string `toml:"enabled_adapters"`
}

// FreshnessConfig controls content hashing and classification.
type FreshnessConfig struct {
	EnableContentHashing bool     `toml:"enable_content_hashing"`
	MaxHashFileSizeMB    int      `toml:"max_hash_file_size_mb"`
	FastPathStatOnly     bool     `toml:"fast_path_stat_only"`
	IgnorePatterns       []string `toml:"ignore_patterns"`
}

// SecretsConfig controls the scrubbing pipeline.
type SecretsConfig struct {
	EnableScrubbing bool     `toml:"enable_scrubbing"`
	ExtraPatterns   []string `toml:"extra_patterns"`
}

// RetentionConfig controls DB pruning.
type RetentionConfig struct {
	MaxAgeDays            int  `toml:"max_age_days"`
	MaxDBSizeMB           int  `toml:"max_db_size_mb"`
	PruneOnStartup        bool `toml:"prune_on_startup"`
	ObserverLogMaxAgeDays int  `toml:"observer_log_max_age_days"`
	// IntervalHours is the cadence of the daemon's periodic maintenance
	// tick: `observer start` re-runs the full retention pass (the same
	// cmd/observer/prune.go::runRetention that prune_on_startup and
	// `observer prune` invoke) every IntervalHours while the daemon is
	// up, so a daemon that stays up for weeks still prunes. Default 24
	// (daily). ≤ 0 disables the tick (startup + manual prune only —
	// the pre-v1.20 behavior).
	IntervalHours int `toml:"interval_hours"`
}

// HooksConfig controls hook runtime.
type HooksConfig struct {
	TimeoutMS int `toml:"timeout_ms"`
	// AutoRegister, when true, has `observer start` install hooks for
	// every detected tool on every launch (idempotent). New tools
	// installed after the daemon was first started are picked up on
	// the next restart without a manual `observer init`. Safe by
	// default: never overwrites user-authored hook entries — conflicts
	// log a warning and skip. Default: true.
	AutoRegister bool `toml:"auto_register"`
}

// ProxyConfig controls the API reverse proxy.
type ProxyConfig struct {
	Enabled           bool   `toml:"enabled"`
	Port              int    `toml:"port"`
	AnthropicUpstream string `toml:"anthropic_upstream"`
	OpenAIUpstream    string `toml:"openai_upstream"`
	ChatGPTUpstream   string `toml:"chatgpt_upstream"`
	GeminiUpstream    string `toml:"gemini_upstream"`
	ForceChatGPTHTTP  bool   `toml:"force_chatgpt_http"`
	// PrewarmTargets are URLs the proxy fires HEAD against at startup
	// to populate the http.Transport connection pool with warm TLS
	// sessions (V6-3 mitigation). The first real proxy request reuses
	// a pooled connection, saving the TLS handshake cost that pushed
	// codex's inner-pipe TTFB past its ~15s timeout. Defaults set in
	// Default(); empty slice disables pre-warm entirely.
	PrewarmTargets []string `toml:"prewarm_targets"`
	// Upstreams maps a routing id to an upstream base URL, selected
	// per-request via a `/up/<id>/` path prefix (Phase C per-provider
	// upstream selection). A routed tool whose traffic must reach a
	// non-default host — e.g. hermes → OpenRouter — points its base URL at
	// http://127.0.0.1:<port>/up/<id>/v1 and the proxy strips the prefix
	// and forwards to the mapped host. The value is the host root WITHOUT
	// the version suffix (the client's base URL keeps `/v1`), exactly like
	// the fixed upstreams: e.g. openrouter = "https://openrouter.ai/api".
	// Empty/unset → only the fixed three upstreams exist (current
	// behavior; fail-open). LOCAL-ONLY — never distributed to org nodes.
	Upstreams map[string]string `toml:"upstreams"`
}

// DashboardConfig controls the local analytics dashboard listener. LOCAL-ONLY
// (never distributed via [org_client.share]). The zero value (no [dashboard]
// section) preserves today's behaviour: `observer start` binds the dashboard on
// the built-in 127.0.0.1:8081 default. Setting Addr makes that address durable
// across daemon/service restarts without the per-run --dashboard-addr flag —
// the config layer sits BELOW the flag and the OBSERVER_DASHBOARD_ADDR env var
// in the precedence ladder (flag > env > config > default), resolved in
// cmd/observer.
type DashboardConfig struct {
	// Addr is the durable dashboard listen address as host:port
	// (e.g. "127.0.0.1:8082"). Empty ⇒ the built-in default is used. An
	// EMPTY host (":8082", i.e. bind-all-interfaces / 0.0.0.0) and any
	// non-loopback host are ACCEPTED at config-load time — the port is the
	// only thing validated for shape (numeric, 1–65535) — because they do
	// NOT bypass the remote-exposure guard: the resolved address still passes
	// through dashboard.CheckRemoteBind, which fails closed unless the
	// [remote] security substrate is armed (mirrors the --dashboard-addr flag
	// path). Validated as host:port with a numeric port at load; the
	// remote-bind policy is enforced separately at bind time.
	Addr string `toml:"addr"`
}

// CompressionConfig groups all four compression layers' toggles.
type CompressionConfig struct {
	CodeGraph    CodeGraphConfig    `toml:"code_graph"`
	Shell        ShellConfig        `toml:"shell"`
	Indexing     IndexingConfig     `toml:"indexing"`
	Conversation ConversationConfig `toml:"conversation"`
}

// CodeGraphConfig is the DEPRECATED [compression.code_graph] block.
//
// Deprecated: the external code-graph companion was
// decommissioned in Phase 4 in favour of the in-process [codeintel]
// module. The block is parsed for one release window only so existing
// configs don't break: on load, migrateLegacyCodeGraph maps `enabled`
// onto [codeintel].enabled and `auto_index` onto
// [codeintel.index].on_start, emits a per-key deprecation warning, then
// the values are otherwise unused. `auto_install` and `path` have no
// in-process analog (no binary is downloaded, no graph.db is read) and
// are reported as removed. See docs/codeintel/migration-from-codegraph.md.
type CodeGraphConfig struct {
	Enabled     bool   `toml:"enabled"`
	AutoInstall bool   `toml:"auto_install"`
	AutoIndex   bool   `toml:"auto_index"`
	Path        string `toml:"path"`
}

// ShellConfig controls shell output filtering.
type ShellConfig struct {
	Enabled         bool     `toml:"enabled"`
	ExcludeCommands []string `toml:"exclude_commands"`
}

// IndexingConfig controls FTS5 tool output indexing.
type IndexingConfig struct {
	Enabled         bool `toml:"enabled"`
	MaxExcerptBytes int  `toml:"max_excerpt_bytes"`
	Embeddings      bool `toml:"embeddings"`
}

// ConversationConfig controls conversation-level compression.
//
// Mode selects the strategy:
//   - "token": legacy default. Per-type compression then drop the
//     lowest-scored non-preserved messages until target_ratio is met.
//   - "cache": restricts drops to the tail half of the conversation
//     and injects a cache_control marker at the prefix boundary.
//   - "cache_aware": designed for Anthropic Pro/Max where the SDK
//     already places cache_control markers. Skips drops entirely
//     (drop ranking is budget-relative and shifts across turns,
//     invalidating Anthropic's prefix cache), narrows per-type
//     compression eligibility to RoleTool only, and skips cache_control
//     injection. The cross-turn determinism this preserves is what
//     makes cache_creation tokens fall on subsequent turns. No-ops
//     gracefully (effectively ModeToken without drops) when no SDK
//     marker is present.
type ConversationConfig struct {
	Enabled       bool     `toml:"enabled"`
	Mode          string   `toml:"mode"`
	TargetRatio   float64  `toml:"target_ratio"`
	PreserveLastN int      `toml:"preserve_last_n"`
	CompressTypes []string `toml:"compress_types"`
	// DisableDrops turns off the budget enforcer's lossy Pass-2
	// eviction (dropping the lowest-importance messages and replacing
	// runs with a `[N messages compressed — use search_past_outputs]`
	// marker). When true, the budget target becomes best-effort: only
	// the content-preserving per-type compressors run, and the body
	// ships with whatever compression they achieved — even 0% — never
	// truncated, never dropped.
	//
	// Default false = drops allowed = the historical behaviour for every
	// existing config and profile. Set true on a profile (see the
	// codex-safe recipe) whose per-type compressors frequently no-op, so
	// the enforcer can't degenerate into a drop-only pass. This is a
	// PROVIDER-NEUTRAL capability, distinct from the Anthropic-shaped
	// `mode = "cache_aware"` (which ALSO skips drops but additionally
	// narrows per-type eligibility to tool_result messages for prefix-
	// cache determinism). Added 2026-07-16.
	DisableDrops bool             `toml:"disable_drops"`
	Logs         LogsConfig       `toml:"logs"`
	Stash        StashConfig      `toml:"stash"`
	Compaction   CompactionConfig `toml:"compaction"`
	Rolling      RollingConfig    `toml:"rolling"`
}

// LogsConfig tunes LogsCompressor's final head+tail truncation pass
// (step 8 in the LogsCompressor pipeline; see
// internal/compression/conversation/logs.go). The earlier
// content-preserving steps (ANSI strip, CR collapse, dedup, blank-run
// cap) are not configurable — they have no destructive failure mode.
//
// The truncation pass is the only step that can elide content the
// agent may want to re-read. For codex-variant models (gpt-5.3-codex
// family) the post-truncation elision marker is treated as missing
// data and triggers re-derivation — see V7-11 in
// docs/v4-codex-compression-recipe-and-issues.md. Operators tuning
// for codex-variant workloads can disable truncation entirely by
// setting `max_lines = 0`, or raise the head/tail budgets so typical
// source-file reads (200-500 lines) survive verbatim.
type LogsConfig struct {
	// MaxLines is the ceiling on the post-dedup line count; zero
	// disables the truncation pass entirely. Default 200.
	MaxLines int `toml:"max_lines"`
	// Head is the line count preserved at the head of an over-budget
	// body; zero falls back to MaxLines/2. Default 100.
	Head int `toml:"head"`
	// Tail is the line count preserved at the tail of an over-budget
	// body; zero falls back to MaxLines/2. Default 100.
	Tail int `toml:"tail"`
}

// RollingConfig controls the v1.4.43+ / Tier 2 / D20 rolling-
// summarisation feature: when a session's conversation crosses
// ThresholdTokens, the proxy calls Anthropic with the user's captured
// auth to get a one-paragraph summary of older messages, then
// replaces them inline with a `[<N> earlier messages summarized: ...]`
// marker. Cross-turn invariance is preserved via a per-session
// sticky boundary — see internal/compression/conversation/rolling.go.
//
// Disabled by default. Opt-in via
// `compression.conversation.rolling.enabled = true`. Once dogfood on
// long sessions shows the cost/benefit (Haiku call cost vs. the
// avoided context blow-up + cache_creation premium) lands net-
// positive, the default may flip.
type RollingConfig struct {
	Enabled         bool `toml:"enabled"`
	ThresholdTokens int  `toml:"threshold_tokens"`
	// SummaryModel is the Anthropic-side rolling-summary model.
	// Default: "claude-haiku-4-5".
	SummaryModel string `toml:"summary_model"`
	// OpenAISummaryModel is the OpenAI-side rolling-summary model used
	// when the proxy is forwarding codex (or any other OpenAI-flavoured)
	// traffic. Default: "gpt-5-nano" (free per OpenAI's 2026-04-29
	// catalog). The two summary models are independent — Anthropic and
	// OpenAI traffic each pick their own.
	OpenAISummaryModel string `toml:"openai_summary_model"`
	AuthCacheSize      int    `toml:"auth_cache_size"`
}

// CompactionConfig controls the v1.4.43+ / Tier 3 / D23 compaction-
// survival feature: when enabled, the proxy detects Anthropic requests
// whose session_id has a recent compaction event in the observer DB
// and prepends a synthetic system block carrying recovery context
// (last reads, last edits, recent failures, learned rules) so the
// model can re-orient without re-Reading every file.
//
// Disabled by default. Opt-in via
// `compression.conversation.compaction.inject_post_compact = true`.
// Once dogfood shows recovery-context utility (model uses the data
// rather than ignoring it) and cross-turn invariance holds in
// practice, this may flip default-on.
type CompactionConfig struct {
	InjectPostCompact bool `toml:"inject_post_compact"`
}

// StashConfig controls the v1.4.41 / Tier 1 / G31 (CCR — Compressed
// Content Retrieval) feature: tool_result bodies whose post-per-type-
// compression size still exceeds ThresholdBytes are written to a
// content-addressed on-disk stash and replaced inline with a marker
// referencing the SHA. The model retrieves originals via the
// `retrieve_stashed` MCP tool.
//
// Disabled by default for the first release; opt-in via
// `compression.conversation.stash.enabled = true`. Once dogfood data
// shows the retrieve-rate is healthy and threshold tuning lands, this
// will flip to default-on.
type StashConfig struct {
	Enabled        bool   `toml:"enabled"`
	Dir            string `toml:"dir"`             // default: ~/.observer/stash
	ThresholdBytes int    `toml:"threshold_bytes"` // default: 8192
	MaxTotalMB     int    `toml:"max_total_mb"`    // default: 1024
}

// IntelligenceConfig groups intelligence-layer settings.
//
// MonthlyBudgetUSD is the user's self-set spend cap for the calendar
// month — surfaced on the Analysis dashboard as a progress tile. Zero
// disables budget tracking. Stored in `intelligence.monthly_budget_usd`.
// The Settings page (PR 2 of the dashboard refresh) writes this from the
// UI; until then users can edit `config.toml` directly.
//
// ProjectBudgetsUSD maps a project root path (exactly as the projects
// table records it) to a monthly advisory budget for that project.
// Absent / zero = no per-project budget. ADVISORY ONLY — budget state
// renders banners and tiles; nothing ever gates proxy traffic (P1).
// Stored in `[intelligence.project_budgets_usd]`; edited from the Cost
// page's Budget card or `observer config set
// intelligence.project_budgets_usd.<root> <usd>`.
type IntelligenceConfig struct {
	CodeGraph         IntelligenceCodeGraphConfig `toml:"code_graph"`
	Pricing           PricingConfig               `toml:"pricing"`
	APIKeyEnv         string                      `toml:"api_key_env"`
	SummaryModel      string                      `toml:"summary_model"`
	MonthlyBudgetUSD  float64                     `toml:"monthly_budget_usd"`
	ProjectBudgetsUSD map[string]float64          `toml:"project_budgets_usd"`
	MCP               IntelligenceMCPConfig       `toml:"mcp"`
}

// IntelligenceMCPConfig groups settings for the V7-12 retrieval-surface
// MCP tools and their shared audit log. Top-level switch is per-tool
// (each subtype carries its own Enabled flag) — there is no global
// `[intelligence.mcp].enabled` knob because operators should be able
// to disable individual tools without losing audit visibility.
//
// Features is the V7-16 allow-list for the four V7-12 retrieval tools
// (`get_file`, `get_symbols`, `get_relations`, `retrieve_stashed`).
// Scope is deliberately V7-12-only — the 13 built-in observability
// tools (check_*, get_action_details, get_cost_summary,
// get_failure_context, get_file_history, get_last_test_result,
// get_project_patterns, get_redundancy_report,
// get_session_recovery_context, get_session_summary,
// list_actions_around, search_past_outputs) are NOT filtered. See
// D-3 in docs/plans/v1.7.11-stash-retrieval-correctness-plan-2026-05-31.md
// for the trade-off discussion.
//
// Precedence: per-tool `enabled = false` ALWAYS wins. Features filter
// cannot re-enable a per-tool-disabled tool. Empty Features (the
// default) = no filter, per-tool flags decide alone.
type IntelligenceMCPConfig struct {
	Features        []string                             `toml:"features"`
	GetFile         IntelligenceMCPGetFileConfig         `toml:"get_file"`
	GetSymbols      IntelligenceMCPGetSymbolsConfig      `toml:"get_symbols"`
	GetRelations    IntelligenceMCPGetRelationsConfig    `toml:"get_relations"`
	RetrieveStashed IntelligenceMCPRetrieveStashedConfig `toml:"retrieve_stashed"`
	Audit           IntelligenceMCPAuditConfig           `toml:"audit"`
}

// IntelligenceMCPGetFileConfig tunes the v1.7.8 get_file MCP tool.
//
// AllowExtensions empty disables the allow-list (operator escape
// hatch for binary-mostly workloads); see V7-13 Gap 4 in
// docs/v4-codex-compression-recipe-and-issues.md.
//
// DenyPaths supports a small glob syntax: `*`, `?`, `<dir>/**`.
// Patterns using unsupported syntax (`[abc]`, `{a,b}`, escapes) are
// silently dead; [Load] emits a warning per unsupported pattern at
// startup so operators notice.
//
// MaxResponseKB caps individual response bytes. Truncated responses
// carry `truncated: true` so the agent knows to retry tighter.
type IntelligenceMCPGetFileConfig struct {
	Enabled         bool     `toml:"enabled"`
	AllowExtensions []string `toml:"allow_extensions"`
	DenyPaths       []string `toml:"deny_paths"`
	MaxResponseKB   int      `toml:"max_response_kb"`
}

// IntelligenceMCPGetSymbolsConfig tunes the v1.7.9 get_symbols MCP
// tool (V7-12 retrieval surface, second of four).
//
// Path-safety knobs (allow/deny lists, max response size) live on
// [IntelligenceMCPGetFileConfig] and are SHARED — one place to keep
// in sync; both tools resolve files the same way and operators
// shouldn't have to author two extension allow-lists. If a future
// release surfaces a divergence (e.g. get_symbols wants binary file
// extensions get_file shouldn't allow), this struct grows its own
// AllowExtensions/DenyPaths fields and the resolver prefers them
// over GetFile's.
//
// MaxCallers and MaxCallees cap the per-symbol callers/callees list
// returned by `include_relations: true`. The accompanying
// `callers_count` / `callees_count` fields report the unlimited
// totals so the agent sees `callers_count: 47, callers: [...top 20...]`
// without being misled.
type IntelligenceMCPGetSymbolsConfig struct {
	Enabled    bool `toml:"enabled"`
	MaxCallers int  `toml:"max_callers"`
	MaxCallees int  `toml:"max_callees"`
}

// IntelligenceMCPGetRelationsConfig tunes the v1.7.10 get_relations
// MCP tool (V7-12 retrieval surface, third of four).
//
// Path-safety knobs (allow/deny lists, max response KB) live on
// [IntelligenceMCPGetFileConfig] and are SHARED across all three
// retrieval tools — one place for operators to keep in sync.
//
// MaxDepth caps BFS recursion depth; MaxResults caps the per-call
// reachable-node count. Lower these on very large codebases where
// a worst-case BFS could otherwise visit thousands of nodes per call.
// Larger values are bounded only by SQLite recursive-CTE cost; the
// MCP layer truncates at MaxResults regardless.
type IntelligenceMCPGetRelationsConfig struct {
	Enabled    bool `toml:"enabled"`
	MaxDepth   int  `toml:"max_depth"`
	MaxResults int  `toml:"max_results"`
}

// IntelligenceMCPRetrieveStashedConfig tunes the v1.7.11
// retrieve_stashed MCP tool extension (V7-12 retrieval surface,
// fourth of four).
//
// Enabled gates registration. When the stash itself is enabled
// (`[compression.conversation.stash].enabled = true`) AND this flag is
// true, the tool is registered. Setting this to false lets operators
// keep proxy-side stash compression active while denying the agent
// the retrieval surface (e.g., security/audit scenarios where the
// proxy and the MCP server have asymmetric trust).
//
// MaxShasPerCall caps the array-form sha input per request. Defaults
// to 25 (matches get_symbols's per-call batch cap). Set higher only
// if your agent reliably emits larger batches AND your audit budget
// can absorb the per-sha row volume.
type IntelligenceMCPRetrieveStashedConfig struct {
	Enabled        bool `toml:"enabled"`
	MaxShasPerCall int  `toml:"max_shas_per_call"`
}

// IntelligenceMCPAuditConfig controls the V7-14 audit log. When
// Enabled, every V7-12 MCP call (success or denial) writes one row
// into mcp_audit. Operators can `SELECT ... FROM mcp_audit` for
// "what was denied?" / "what is the agent reading?" forensics until
// the operator CLI lands (deferred to v1.8.x).
//
// Default true: the table is local-only, the volume budget is small,
// and the forensic value is high. Privacy-conscious operators opt
// out with one TOML line.
type IntelligenceMCPAuditConfig struct {
	Enabled bool `toml:"enabled"`
}

// UnsupportedDenyPatterns returns the subset of [DenyPaths] that use
// glob syntax outside the supported `*`, `?`, `<dir>/**` subset
// (e.g. character classes `[a-z]`, brace alternation `{a,b}`, or
// escape sequences `\x`). The patterns are NOT removed from the
// config — they're inert at match time. Callers (notably
// `observer serve`) emit one warning line per returned pattern at
// startup so operators notice silently-dead deny rules.
func (g IntelligenceMCPGetFileConfig) UnsupportedDenyPatterns() []string {
	var bad []string
	for _, p := range g.DenyPaths {
		if hasUnsupportedDenyGlob(p) {
			bad = append(bad, p)
		}
	}
	return bad
}

// hasUnsupportedDenyGlob mirrors internal/mcp/pathsafety.go's
// hasUnsupportedGlobSyntax. Duplicated here (5 lines) so config
// doesn't import mcp — config sits beneath mcp in the dependency
// graph. Keep both in sync; the test
// [TestIntelligenceMCPGetFileConfig_UnsupportedDenyPatterns] pins
// the contract.
func hasUnsupportedDenyGlob(pattern string) bool {
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '[', ']', '{', '}', '\\':
			return true
		}
	}
	return false
}

// IntelligenceCodeGraphConfig is the DEPRECATED [intelligence.code_graph]
// block.
//
// Deprecated: superseded by the in-process [codeintel] module (Phase 4).
// Parsed for one release window; on load `enabled` maps onto
// [codeintel].enabled with a deprecation warning. See
// docs/codeintel/migration-from-codegraph.md.
type IntelligenceCodeGraphConfig struct {
	Enabled bool `toml:"enabled"`
}

// PricingConfig carries per-model input/output/cache pricing.
type PricingConfig struct {
	Models map[string]ModelPricing `toml:"models"`
}

// ModelPricing is per-million-token pricing for a single model. CacheCreation
// is optional — when zero, the cost engine defaults it to 1.25 × Input
// (Anthropic's published cache-write premium). CacheCreation1h is the
// 1-hour ephemeral tier rate; defaults to 2 × CacheCreation when zero.
//
// LongContextThreshold + LongContext* model providers that reprice an
// entire request when the prompt exceeds a token threshold (Anthropic
// Sonnet 4 / 4.5 at 200K, OpenAI gpt-5.4 / gpt-5.5 at 272K, Gemini
// 2.5 Pro / 3.1 Pro at 200K). Threshold zero disables the tier; each
// LongContext* rate falls back to its standard counterpart when zero.
type ModelPricing struct {
	Input           float64 `toml:"input"`
	Output          float64 `toml:"output"`
	CacheRead       float64 `toml:"cache_read"`
	CacheCreation   float64 `toml:"cache_creation"`
	CacheCreation1h float64 `toml:"cache_creation_1h"`

	LongContextThreshold       int64   `toml:"long_context_threshold"`
	LongContextInput           float64 `toml:"long_context_input"`
	LongContextOutput          float64 `toml:"long_context_output"`
	LongContextCacheRead       float64 `toml:"long_context_cache_read"`
	LongContextCacheCreation   float64 `toml:"long_context_cache_creation"`
	LongContextCacheCreation1h float64 `toml:"long_context_cache_creation_1h"`

	// FastMultiplier scales every per-token rate when the turn was
	// served in the provider's low-latency "fast" tier (Anthropic Opus
	// 4.8 speed:"fast" → 2× across all dimensions). Zero means no fast
	// tier. Operators can pin per-model overrides for future SKUs that
	// adopt fast mode.
	FastMultiplier float64 `toml:"fast_multiplier"`
}

// Default returns the baked-in defaults (spec §16.1).
func Default() Config {
	return Config{
		Observer: ObserverConfig{
			DBPath:   "~/.observer/observer.db",
			LogLevel: "info",
			Watch: WatchConfig{
				PollIntervalSeconds: 2,
				MaxFileSizeMB:       50,
				EnabledAdapters: []string{
					"claude-code", "codex", "cline", "cline-cli", "roo-code", "cursor", "copilot", "copilot-cli", "cowork", "opencode", "openclaw", "pi", "gemini-cli", "antigravity", "antigravity-cli", "hermes", "kilo-code", "kilo-code-cli", "qwen-code", "kiro-cli", "crush", "kimi-code", "grok", "devin", "qoder", "aider", "goose", "chatgpt-web", "claude-web", "perplexity-web", "gemini-web", "copilot-web",
				},
			},
			Freshness: FreshnessConfig{
				EnableContentHashing: true,
				MaxHashFileSizeMB:    10,
				FastPathStatOnly:     true,
				IgnorePatterns: []string{
					"node_modules/", ".git/", "vendor/", "dist/", "build/",
					"target/", "__pycache__/",
					"*.exe", "*.bin", "*.wasm",
				},
			},
			Secrets: SecretsConfig{
				EnableScrubbing: true,
			},
			Retention: RetentionConfig{
				MaxAgeDays:            180,
				MaxDBSizeMB:           2048,
				PruneOnStartup:        true,
				ObserverLogMaxAgeDays: 30,
				IntervalHours:         24,
			},
			Hooks: HooksConfig{
				TimeoutMS:    500,
				AutoRegister: true,
			},
			// Process observability (docs/process-observability.md §11) is
			// OPT-IN: Enabled defaults false (D1). The non-zero defaults
			// below only matter once the operator flips it; they're set
			// here so a partial [observer.process] section inherits sane
			// values rather than zeros (the CacheTrack partial-merge rule).
			Process: ProcessConfig{
				Enabled:              false,
				Backend:              "auto",
				CaptureUnattributed:  false,
				RetentionDays:        30,
				QueueSize:            10000,
				BatchSize:            250,
				PollIntervalMS:       2000,
				BridgePollIntervalMS: 0,     // 0 = inherit PollIntervalMS (see resolveProcessPollIntervals)
				CorrelateIntervalMS:  90000, // background cross-OS correlation sweep cadence (90s); 0 = inherit
				Argv: ProcessArgvConfig{
					Mode:            "preview",
					MaxPreviewBytes: 512,
					StoreArgCount:   true,
				},
				Executable: ProcessExecutableConfig{
					HashEnabled:       false,
					MaxHashFileSizeMB: 25,
				},
				Env: ProcessEnvConfig{
					Enabled:       true,
					StorePathHash: true,
				},
				Network: ProcessNetworkConfig{
					Enabled:           false,
					CaptureRemoteHost: true,
					RedactPrivateIPs:  false,
					CaptureBodies:     "off",
					MaxRequestBytes:   64 * 1024,
					MaxResponseBytes:  64 * 1024,
					CaptureHeaders:    true,
					ScrubBodies:       true,
					StoreBinary:       false,
				},
				Filesystem: ProcessFilesystemConfig{
					Enabled: false,
					Mode:    "sensitive",
				},
			},
		},
		// Org client is OFF by default (solo-local invariant). The defaults
		// below only take effect once a user sets [org_client] enabled = true.
		OrgClient: OrgClientConfig{
			Enabled:                   false,
			PushIntervalSeconds:       DefaultPushIntervalSeconds,
			PolicyPollIntervalSeconds: DefaultPolicyPollIntervalSeconds,
			MaxPushBytes:              DefaultMaxPushBytes,
			KeychainID:                DefaultKeychainID,
		},
		// OTel exporter is OFF by default (solo-local invariant). The
		// defaults below only take effect once a user sets
		// [exporter.otel] enabled = true.
		Exporter: ExporterConfig{
			OTel: OTelExporterConfig{
				Enabled:             false,
				Endpoint:            DefaultOTelEndpoint,
				Insecure:            false,
				PollIntervalSeconds: DefaultOTelPollIntervalSeconds,
				EmitPromptContent:   false,
				EmitUserEmail:       false,
				SemconvStability:    DefaultOTelSemconvStability,
			},
		},
		Ingest: IngestConfig{
			OTel: IngestOTelConfig{
				Enabled:          false,
				GRPCAddr:         DefaultIngestOTelGRPCAddr,
				HTTPAddr:         DefaultIngestOTelHTTPAddr,
				AllowNonLoopback: false,
				ContentCapture:   ContentCaptureFull,
			},
		},
		// CacheTrack is default-ON per spec §11: local, passive,
		// network-free, hashes/counts/enums only. An install with
		// no [cachetrack] section gets Enabled=true here; partial-
		// merge against an empty section leaves it true so the
		// loader doesn't silently downgrade to false.
		CacheTrack: CacheTrackConfig{
			Enabled:            true,
			MaxTrackedSessions: 64,
			RetentionDays:      90,
		},
		// CacheWarm (cache-expiry warning + smart keep-warm) — the
		// WARNING half is default-ON (pure read over cache_entries, zero
		// LLM cost); the KEEP-WARM half (Keepwarm.Mode) is OFF by default
		// (outward-facing spend, opt-in like routing enforce). Same
		// partial-merge rule as CacheTrack — an install with no
		// [cachewarm] section keeps Enabled=true and Keepwarm.Mode="off".
		CacheWarm: CacheWarmConfig{
			Enabled:                 true,
			WarnAtSeconds:           90,
			CriticalAtSeconds:       30,
			MinValueUSD:             0.05,
			ImplicitWarnSeconds:     3600,
			ImplicitCriticalSeconds: 7200,
			ImplicitMaxSeconds:      86400,
			Keepwarm: KeepWarmConfig{
				Mode:                "off",
				MinValueUSD:         0.20,
				MinResumeConfidence: 0.5,
			},
		},
		// Predict (Next-Message Cost & Limit Predictor) is default-ON:
		// the cost estimate is pure read-side math, the limit capture is
		// a cheap header parse on the proxy path. Same partial-merge rule
		// as CacheTrack — an install with no [predict] section keeps
		// Enabled=true.
		Predict: PredictConfig{
			Enabled:                true,
			YoungSessionMessages:   3,
			DefaultTurnsPerMessage: 12,
			PriorWindowDays:        30,
		},
		// AggregateShare (opt-in aggregate rail) is OFF by default in every
		// path. Unlike CacheTrack/Predict, the partial-merge default keeps
		// Enabled=false — only Endpoint is seeded so a consenting operator
		// inherits the published collector without hand-typing it. The zero
		// value (no [aggregate_share] section) is also OFF. LOCAL-ONLY,
		// org-independent, never on the Teams wire.
		AggregateShare: AggregateShareConfig{
			Enabled:             false,
			Endpoint:            DefaultAggregateEndpoint,
			AllowCustomEndpoint: false,
		},
		// Remote (remote dashboard access) is OFF by default in every path:
		// exposure is the first network-facing node surface, so the zero value
		// (no [remote] section) AND the partial-merge default keep it inert —
		// loopback-only, today's behaviour. The non-zero seeds are the SECURE
		// defaults a consenting operator inherits (TLS required, terminal off,
		// argon2-class rate limit, bounded device sessions). LOCAL-ONLY,
		// org-independent, never on the Teams wire.
		Remote: RemoteConfig{
			Enabled:                      false,
			Mode:                         "off",
			RequireTLS:                   true,
			AllowTerminal:                false,
			AllowStandingTerminalControl: false,
			WriterLeaseIdleMinutes:       5,
			WriterLeaseMaxMinutes:        30,
			RateLimitPerMin:              6,
			SessionTTLMinutes:            720,
			SessionIdleMinutes:           60,
			MaxSessions:                  5,
			Notify: RemoteNotifyConfig{
				Enabled: false,
				Kind:    "webhook",
				Events:  []string{"session_blocked", "session_finished"},
			},
		},
		// Browser (opt-in browser-chatbot capture rail) — the RAIL is
		// default-ON (Enabled=true) so it is ready the moment the operator
		// installs the extension, but the HTTP LISTENER is default-OFF
		// (the native-messaging bridge is the default receiver) and the
		// GranularityCeiling defaults to "full" — local-first posture,
		// same as coding-agent capture (see the BrowserConfig field
		// comment for the rationale + the fail-closed extension side).
		// Same partial-merge rule as CacheTrack — an install with no
		// [browser] section keeps these values. LOCAL-ONLY.
		Browser: BrowserConfig{
			Enabled: true,
			Listener: BrowserListenerConfig{
				Enabled:    false,
				ListenAddr: "127.0.0.1:8821",
			},
			GranularityCeiling: "full",
			RetentionDays:      0,
			IngestTimeoutMS:    defaultBrowserIngestTimeoutMS,
		},
		// Handoff (session handoff / continue-anywhere) is default-ON:
		// pure read-side until the operator runs `observer handoff`.
		// Same partial-merge rule as CacheTrack. LOCAL-ONLY.
		Handoff: HandoffConfig{
			Enabled:              true,
			TailMessages:         6,
			MaxDocTokens:         12000,
			DefaultCarry:         "distilled_tail",
			FileName:             "HANDOFF-{shortid}.md",
			HookMaxBytes:         8192,
			HookTTLMinutes:       240,
			RetentionDays:        180,
			AllowDashboardLaunch: true,
			ContextWarnTokens:    200_000,
			MaxCacheBytes:        8 * 1024 * 1024,
		},
		// Terminal (the embedded web-terminal cockpit) is default-ON for the
		// terminal-wide surface + status detection, but fresh-agent launch is
		// a SEPARATE default-OFF opt-in ([terminal.launch].allow_fresh_agent).
		// Same partial-merge rule as CacheTrack — an install with no [terminal]
		// section inherits these values. LOCAL-ONLY.
		Terminal: TerminalConfig{
			Enabled:        true,
			MaxConcurrent:  9,
			IdleTimeout:    "30m",
			RingBytes:      262144,
			MaxSubscribers: 8,
			Status:         TerminalStatusConfig{Enabled: true},
			// Attach is default-ON (owner-only AF_UNIX 0600 socket), children
			// proxy-routed by default; DefaultOn makes the launchers attach by
			// default (opt-out per-launch with --no-attach).
			Attach: TerminalAttachConfig{Enabled: true, RouteProxy: true, DefaultOn: true, ReclaimOnInput: true},
			// Launch stays zero-valued: fresh launch is opt-in.
		},
		// Benchmark (the Benchmarks Harness) is CLI-driven; the only default
		// is the retention horizon for the node-local benchmark_* tables.
		Benchmark: BenchmarkConfig{
			RetentionDays: 180,
		},
		// CodeIntel (the in-process code-intelligence module) is
		// default-ON for indexing (read-only on the repo, zero LLM cost),
		// but the only model-visible change (compression.code_aggressive)
		// stays OFF. Same partial-merge rule as CacheTrack.
		CodeIntel: CodeIntelConfig{
			Enabled:        true,
			AutoIndexLimit: 25000,
			MaxFileBytes:   2_000_000,
			RetentionDays:  90,
			Index: CodeIntelIndexConfig{
				OnStart:      true,
				Watch:        true,
				Mode:         "auto",
				DiskBudgetMB: 500,
			},
			Compression: CodeIntelCompressionConfig{},
			Semantic: CodeIntelSemanticConfig{
				Embedder:  "tfidf",
				SimilarTo: true,
			},
		},
		// Advisor (the suggestions engine, spec §15.7) is default-ON:
		// read-layer only, local, zero LLM cost. Same partial-merge
		// rule as CacheTrack — an install with no [advisor] section
		// gets Enabled=true. Detector thresholds default in the
		// advisor package (Phase-0 calibrated); config overrides land
		// in a later phase.
		Advisor: AdvisorConfig{
			Enabled:              true,
			WindowDays:           14,
			MinConfidence:        0.5,
			MinSavingsUSD:        1.0,
			SessionDigest:        false,
			DigestRefreshMinutes: 30,
		},
		// Routing (docs/model-routing-spec.md §R21) is OFF by default —
		// an opt-in feature, unlike cachetrack. The sub-defaults matter
		// for the partial-merge invariant: `[routing] enabled = true`
		// alone must yield advise mode on the value template, never
		// zero-valued strings.
		Routing: defaultRouting(),
		// Guard (security & control layer, guard spec §16) is
		// default-ON in OBSERVE mode (D2): local, deterministic,
		// flags + alerts but never blocks until the operator flips
		// enforce. Same partial-merge rule as CacheTrack — an
		// install with no [guard] section gets Enabled=true +
		// Mode="observe", never zero values. Boundary slices stay
		// nil so the policy-engine defaults apply. Cloud features
		// are all-off (D1: explicit opt-in only).
		Guard: GuardConfig{
			Enabled:       true,
			Mode:          "observe",
			Strict:        false,
			RetentionDays: 365,
			Rules: GuardRulesConfig{
				UserPolicy:    "~/.observer/guard-policy.toml",
				ProjectPolicy: ".observer/guard-policy.toml",
				OrgBundle:     "~/.observer/org-policy-bundle.json",
			},
			Taint: GuardTaintConfig{
				Enabled:    true,
				DecayTurns: 10,
			},
			Proxy: GuardProxyConfig{
				EgressScan: true,
				// "mask" per §8.2: default-on in enforce for
				// detector-certain types; inert in observe mode (D2 —
				// observe never mutates, every verdict is a flag).
				EgressAction:        "mask",
				ResponseScan:        true,
				InjectionHeuristics: true,
			},
			MCP: GuardMCPConfig{
				Pinning:             true,
				PoisoningHeuristics: true,
			},
			Alerts: GuardAlertsConfig{
				Desktop:     true,
				MinSeverity: "high",
			},
			Dialects: GuardDialectsConfig{
				Compile: true,
			},
			Cloud: GuardCloudConfig{
				PayloadMaxBytes: 4096,
			},
		},
		Profiles: defaultProfiles(),
		Proxy: ProxyConfig{
			Enabled:           true,
			Port:              8820,
			AnthropicUpstream: "https://api.anthropic.com",
			OpenAIUpstream:    "https://api.openai.com",
			ChatGPTUpstream:   "https://chatgpt.com",
			GeminiUpstream:    "https://generativelanguage.googleapis.com",
			// V6-3 pre-warm: HEAD against these URLs at proxy
			// startup so the first real codex request reuses a
			// warm TLS connection. Empty slice (set explicitly in
			// config.toml) disables pre-warm.
			PrewarmTargets: []string{
				"https://chatgpt.com/",
				"https://api.openai.com/",
			},
		},
		Compression: CompressionConfig{
			CodeGraph: CodeGraphConfig{
				Enabled:     true,
				AutoInstall: true,
				AutoIndex:   true,
			},
			Shell: ShellConfig{
				Enabled:         true,
				ExcludeCommands: []string{"curl", "playwright"},
			},
			Indexing: IndexingConfig{
				Enabled:         true,
				MaxExcerptBytes: 2048,
			},
			Conversation: ConversationConfig{
				Enabled:       false,
				Mode:          "cache_aware",
				TargetRatio:   0.85,
				PreserveLastN: 5,
				// Default excludes "text": its head-tail truncation
				// strategy elides mid-content from tool_result bodies the
				// agent may re-reference, forcing re-reads (one fewer
				// answer, two more turns).
				//
				// "code" entered the allow-list in v1.4.40 once
				// CodeCompressor was rewritten content-preserving (no
				// body elision, no signature-only skeleton). JSON schema
				// replacement, logs dedup, and code skeleton are all
				// content-preserving; text head-tail is not. Users who
				// want text can opt in explicitly.
				//
				// V7-22 (v1.7.22, 2026-06-01): the default was temporarily
				// flipped to []{} based on an n=4 measurement that showed
				// +60% cost regression on the V7-21 binary. V7-24 (v1.7.23,
				// 2026-06-01) re-measured on the V7-22 binary at n=8 and
				// found the cascade had stopped — V7-22's preceding fixes
				// (V7-19 nil-trap + V7-21 tools-defs gate) closed enough of
				// the re-marshal pathway that per-type compression no longer
				// breaks Anthropic's prefix cache.
				//
				// Restored to ["json","logs","code"] as the empirical
				// winner: -6.9% cost vs no-proxy (n=8), CV 7.6% (tighter
				// than OFF's 7.5%), zero tail outliers. Operators MUST
				// also set ENABLE_TOOL_SEARCH=true in the launching shell
				// to recover Claude Code's deferred MCP loading (otherwise
				// the SDK eager-inlines all MCP schemas under
				// ANTHROPIC_BASE_URL, costing ~+21K tokens per turn).
				//
				// "text" is omitted by choice — TextCompressor head-tail
				// eliding middle content is the v1.4.38 regression class.
				// "tools" is opt-in (V7-21). Stash is opt-in but cache-
				// breaking on Anthropic — see StashConfig.
				//
				// See docs/v1.7.23-compression-savings-empirical-2026-06-01.md.
				CompressTypes: []string{"json", "logs", "code"},
				Logs: LogsConfig{
					MaxLines: 200,
					Head:     100,
					Tail:     100,
				},
				Stash: StashConfig{
					Enabled:        false,
					Dir:            "~/.observer/stash",
					ThresholdBytes: 8192,
					MaxTotalMB:     1024,
				},
				Compaction: CompactionConfig{
					InjectPostCompact: false,
				},
				Rolling: RollingConfig{
					Enabled:            false,
					ThresholdTokens:    80000,
					SummaryModel:       "claude-haiku-4-5",
					OpenAISummaryModel: "gpt-5-nano",
					AuthCacheSize:      1024,
				},
			},
		},
		Intelligence: IntelligenceConfig{
			CodeGraph: IntelligenceCodeGraphConfig{Enabled: true},
			Pricing:   PricingConfig{Models: map[string]ModelPricing{}},
			MCP: IntelligenceMCPConfig{
				GetFile: IntelligenceMCPGetFileConfig{
					Enabled: true,
					AllowExtensions: []string{
						"ts", "tsx", "js", "jsx", "mjs", "cjs",
						"py", "rs", "go", "java", "kt", "rb", "php", "swift",
						"c", "cc", "cpp", "h", "hpp", "cs",
						"md", "txt", "json", "toml", "yaml", "yml",
						"html", "css", "scss", "sass",
						"sh", "bash", "ps1", "sql",
					},
					DenyPaths: []string{
						".env*", "*.key", "*.pem", "*.pfx", "*.p12",
						".git/**", ".hg/**", ".svn/**",
						"node_modules/**", "vendor/**",
						".ssh/**", ".aws/**", ".gnupg/**",
						".npmrc", ".pypirc", ".netrc",
					},
					MaxResponseKB: 100,
				},
				GetSymbols: IntelligenceMCPGetSymbolsConfig{
					Enabled:    true,
					MaxCallers: 20,
					MaxCallees: 20,
				},
				GetRelations: IntelligenceMCPGetRelationsConfig{
					Enabled:    true,
					MaxDepth:   5,
					MaxResults: 100,
				},
				RetrieveStashed: IntelligenceMCPRetrieveStashedConfig{
					Enabled:        true,
					MaxShasPerCall: 25,
				},
				// Features empty = no filter applied (per-tool flags
				// alone decide registration). See doc-comment on
				// IntelligenceMCPConfig.Features for V7-16 precedence.
				Features: []string{},
				Audit:    IntelligenceMCPAuditConfig{Enabled: true},
			},
		},
	}
}

// LoadOptions parameterizes Load.
type LoadOptions struct {
	// GlobalPath overrides the location of the user-global config. Defaults to
	// ~/.observer/config.toml.
	GlobalPath string
	// ProjectPath, when set, is a per-project .observer/config.toml that
	// overrides the global file.
	ProjectPath string
	// Env is the environment lookup function. Defaults to os.Getenv.
	Env func(string) string
}

// Load merges defaults ← global TOML ← project TOML ← environment overrides.
// Missing TOML files are not errors (defaults apply). Env variable form:
// OBSERVER_<SECTION>_<KEY> (uppercased, underscores). Nested sections are
// joined with additional underscores, e.g. OBSERVER_COMPRESSION_CONVERSATION_ENABLED.
// ResolveGlobalPath returns the global config file path Load would
// use given the same override. Lets callers (notably the Settings
// page's PUT /api/config/pricing handler) locate the file for
// save-back operations without reimplementing the resolution rule.
func ResolveGlobalPath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".observer", "config.toml"), nil
}

func Load(opts LoadOptions) (Config, error) {
	if opts.Env == nil {
		opts.Env = os.Getenv
	}
	cfg := Default()

	// NOTE (Track R, P2.2): the v1.7.6 recipe overlay that used to sit
	// here is gone. Recipes are now compression PROFILES resolved per
	// traffic class at the proxy boundary (profiles.go) — never merged
	// into the global Config, so a dashboard save can no longer bake
	// recipe values into config.toml. `observer start --recipe` survives
	// as a deprecated alias mapped onto [profiles] for the run, in
	// cmd/observer.

	globalPath := opts.GlobalPath
	if globalPath == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			globalPath = filepath.Join(home, ".observer", "config.toml")
		}
	}
	globalMeta, err := mergeTOMLFile(&cfg, globalPath)
	if err != nil {
		return Config{}, err
	}
	metas := []toml.MetaData{globalMeta}
	if opts.ProjectPath != "" {
		projMeta, err := mergeTOMLFile(&cfg, opts.ProjectPath)
		if err != nil {
			return Config{}, err
		}
		metas = append(metas, projMeta)
	}
	// Capture the config-file dashboard addr BEFORE env overrides so a
	// MALFORMED OBSERVER_DASHBOARD_ADDR can be dropped without failing Load —
	// matching the silent-drop precedent of the int/float/bool env overrides
	// in setEnvValue (a bad env value is ignored, never fatal). Because
	// resolveDashboardAddr (cmd/observer) reads the same env var directly and
	// ranks a valid --dashboard-addr flag ABOVE it, a hard Load failure here
	// would let a garbage env value shadow a perfectly good flag. Demote to a
	// one-time warning and restore the file value so validation proceeds.
	dashAddrFromFile := cfg.Dashboard.Addr
	applyEnvOverrides(&cfg, opts.Env)
	if envAddr := strings.TrimSpace(opts.Env("OBSERVER_DASHBOARD_ADDR")); envAddr != "" {
		if err := ValidateDashboardAddr(cfg.Dashboard.Addr); err != nil {
			emitConfigWarnOnce(fmt.Sprintf("OBSERVER_DASHBOARD_ADDR=%q ignored (%v); falling back to --dashboard-addr flag / [dashboard].addr / default", envAddr, err))
			cfg.Dashboard.Addr = dashAddrFromFile
		}
	}

	// Phase 4 decommission: map the deprecated [compression.code_graph]
	// and [intelligence.code_graph] blocks onto [codeintel] and warn once
	// per legacy key. Honored for one release window, then removed.
	for _, w := range migrateLegacyCodeGraph(&cfg, metas) {
		emitDeprecationOnce(w)
	}

	// M1 plane-separation: map the deprecated flat [org_client.share]
	// obs_* keys onto the nested [org_client.share.obs] sub-table and warn
	// once per legacy key. Honored for one release window, then removed.
	for _, w := range migrateLegacyOrgShareObs(&cfg, metas) {
		emitDeprecationOnce(w)
	}

	cfg.Observer.DBPath = expandHome(cfg.Observer.DBPath)
	cfg.Compression.Conversation.Stash.Dir = expandHome(cfg.Compression.Conversation.Stash.Dir)

	if err := Validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// deprecationEmit guards one-per-process printing of config deprecation
// lines. config.Load runs in ~20 short- and long-lived call sites during a
// single `observer start` (proxy, watcher, dashboard, every feature
// goroutine, hooks auto-register) — each re-parses the same file and,
// pre-dedup, re-printed the identical block of deprecation lines, drowning
// the `dashboard → http://…` readiness banner. The dedup suppresses only
// the repeated PRINTING; the key MAPPING in migrateLegacyCodeGraph /
// migrateLegacyOrgShareObs still runs on every Load, and the FIRST
// occurrence of each distinct message is always shown.
var deprecationEmit struct {
	mu       sync.Mutex
	seen     map[string]struct{}
	hintDone bool
}

// emitDeprecationOnce prints "config: deprecation: <msg>" to stderr at most
// once per process, keyed on msg, and prints a single follow-up remediation
// hint the first time any deprecation is emitted. Concurrency-safe: several
// components may Load config in parallel goroutines.
func emitDeprecationOnce(msg string) {
	deprecationEmit.mu.Lock()
	defer deprecationEmit.mu.Unlock()
	if deprecationEmit.seen == nil {
		deprecationEmit.seen = make(map[string]struct{})
	}
	if _, ok := deprecationEmit.seen[msg]; ok {
		return
	}
	deprecationEmit.seen[msg] = struct{}{}
	fmt.Fprintln(os.Stderr, "config: deprecation: "+msg)
	if !deprecationEmit.hintDone {
		deprecationEmit.hintDone = true
		fmt.Fprintln(os.Stderr, "config: deprecation: the keys above are deprecated aliases still honored this run — "+
			"edit ~/.observer/config.toml to adopt the new keys (or run `observer config migrate`); this notice prints once per process.")
	}
}

// emitConfigWarnOnce prints "config: warning: <msg>" to stderr at most once
// per process, keyed on msg. Shares the emit-once dedup state with
// emitDeprecationOnce because config.Load runs in ~20 call sites per process
// and each would otherwise re-print the identical line. Unlike a deprecation
// this carries no remediation-hint follow-up.
func emitConfigWarnOnce(msg string) {
	deprecationEmit.mu.Lock()
	defer deprecationEmit.mu.Unlock()
	if deprecationEmit.seen == nil {
		deprecationEmit.seen = make(map[string]struct{})
	}
	if _, ok := deprecationEmit.seen[msg]; ok {
		return
	}
	deprecationEmit.seen[msg] = struct{}{}
	fmt.Fprintln(os.Stderr, "config: warning: "+msg)
}

// resetDeprecationEmitForTest clears the one-per-process dedup state so a
// test can assert the emit-once behavior across multiple Load calls.
func resetDeprecationEmitForTest() {
	deprecationEmit.mu.Lock()
	defer deprecationEmit.mu.Unlock()
	deprecationEmit.seen = nil
	deprecationEmit.hintDone = false
}

func mergeTOMLFile(cfg *Config, path string) (toml.MetaData, error) {
	if path == "" {
		return toml.MetaData{}, nil
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return toml.MetaData{}, nil
	}
	if err != nil {
		return toml.MetaData{}, fmt.Errorf("config.Load: read %s: %w", path, err)
	}
	meta, err := toml.Decode(string(body), cfg)
	if err != nil {
		return toml.MetaData{}, fmt.Errorf("config.Load: parse %s: %w", path, err)
	}
	return meta, nil
}

// migrateLegacyCodeGraph maps the deprecated codegraph config blocks onto
// the [codeintel] family and returns one deprecation message per legacy
// key actually present across the loaded files (Phase 4 decommission, plan
// §11.5). The mapping is honored only when the corresponding [codeintel]
// key was NOT explicitly set, so a config carrying both keeps [codeintel]
// authoritative. Keys with no in-process analog (the external binary
// download + graph.db path) are reported as removed, not remapped — the
// concerns disappear with the third-party binary.
//
// metas carries the BurntSushi decode metadata for each loaded file so we
// can distinguish "key present" from "default value"; toml.MetaData's zero
// value answers IsDefined==false, so an absent file contributes nothing.
func migrateLegacyCodeGraph(cfg *Config, metas []toml.MetaData) []string {
	defined := func(keys ...string) bool {
		for _, m := range metas {
			if m.IsDefined(keys...) {
				return true
			}
		}
		return false
	}
	codeintelEnabledSet := defined("codeintel", "enabled")
	codeintelOnStartSet := defined("codeintel", "index", "on_start")
	compressionEnabledSet := defined("compression", "code_graph", "enabled")

	var warnings []string
	// [compression.code_graph]
	if compressionEnabledSet {
		warnings = append(warnings,
			"compression.code_graph.enabled is deprecated; use codeintel.enabled (see docs/codeintel/configuration.md)")
		if !codeintelEnabledSet {
			cfg.CodeIntel.Enabled = cfg.Compression.CodeGraph.Enabled
		}
	}
	if defined("compression", "code_graph", "auto_index") {
		warnings = append(warnings,
			"compression.code_graph.auto_index is deprecated; use codeintel.index.on_start")
		if !codeintelOnStartSet {
			cfg.CodeIntel.Index.OnStart = cfg.Compression.CodeGraph.AutoIndex
		}
	}
	if defined("compression", "code_graph", "auto_install") {
		warnings = append(warnings,
			"compression.code_graph.auto_install is removed; the code index is in-process (no third-party binary is downloaded)")
	}
	if defined("compression", "code_graph", "path") {
		warnings = append(warnings,
			"compression.code_graph.path is removed; the in-process code index has no external graph.db")
	}
	// [intelligence.code_graph]
	if defined("intelligence", "code_graph", "enabled") {
		warnings = append(warnings,
			"intelligence.code_graph.enabled is deprecated; use codeintel.enabled (see docs/codeintel/configuration.md)")
		if !codeintelEnabledSet && !compressionEnabledSet {
			cfg.CodeIntel.Enabled = cfg.Intelligence.CodeGraph.Enabled
		}
	}
	return warnings
}

// migrateLegacyOrgShareObs maps the deprecated flat [org_client.share] obs_*
// keys onto the nested [org_client.share.obs] sub-table and returns one
// deprecation message per legacy key actually present across the loaded
// files (plane-separation audit M1). The mapping is honored only when the
// corresponding nested key was NOT explicitly set, so a config carrying both
// keeps the nested [org_client.share.obs] value authoritative — mirroring
// migrateLegacyCodeGraph. metas distinguishes "key present" from "default
// value" (see that function's note on toml.MetaData).
func migrateLegacyOrgShareObs(cfg *Config, metas []toml.MetaData) []string {
	defined := func(keys ...string) bool {
		for _, m := range metas {
			if m.IsDefined(keys...) {
				return true
			}
		}
		return false
	}
	sh := &cfg.OrgClient.Share
	var warnings []string
	type mapping struct {
		flatKey   string
		flatVal   bool
		nestedKey string
		nestedSet bool
		dst       *bool
	}
	mappings := []mapping{
		{"obs_summary", sh.ObsSummary, "summary", defined("org_client", "share", "obs", "summary"), &sh.Obs.Summary},
		{"obs_traces", sh.ObsTraces, "traces", defined("org_client", "share", "obs", "traces"), &sh.Obs.Traces},
		{"obs_content", sh.ObsContent, "content", defined("org_client", "share", "obs", "content"), &sh.Obs.Content},
		{"obs_eval_summary", sh.ObsEvalSummary, "eval_summary", defined("org_client", "share", "obs", "eval_summary"), &sh.Obs.EvalSummary},
	}
	for _, mp := range mappings {
		if !defined("org_client", "share", mp.flatKey) {
			continue
		}
		warnings = append(warnings,
			"org_client.share."+mp.flatKey+" is deprecated; use org_client.share.obs."+mp.nestedKey)
		if !mp.nestedSet {
			*mp.dst = mp.flatVal
		}
	}
	return warnings
}

// Validate checks semantic constraints on cfg.
func Validate(cfg Config) error {
	if cfg.Observer.DBPath == "" {
		return errors.New("config: observer.db_path is required")
	}
	switch cfg.Observer.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: observer.log_level %q not in {debug, info, warn, error}", cfg.Observer.LogLevel)
	}
	if cfg.Observer.Watch.PollIntervalSeconds < 0 {
		return errors.New("config: observer.watch.poll_interval_seconds must be >= 0")
	}
	if cfg.Observer.Hooks.TimeoutMS <= 0 {
		return errors.New("config: observer.hooks.timeout_ms must be > 0")
	}
	if cfg.Proxy.Enabled && (cfg.Proxy.Port <= 0 || cfg.Proxy.Port > 65535) {
		return fmt.Errorf("config: proxy.port %d out of range", cfg.Proxy.Port)
	}
	if err := validateDashboard(cfg.Dashboard); err != nil {
		return err
	}
	if err := validateCompression(cfg.Compression); err != nil {
		return err
	}
	if err := validateRouting(cfg.Routing); err != nil {
		return err
	}
	if err := validateRemote(cfg.Remote); err != nil {
		return err
	}
	if cfg.CacheWarm.Enabled {
		switch cfg.CacheWarm.Keepwarm.Mode {
		case "", "off", "advise", "enforce":
		default:
			return fmt.Errorf("config: cachewarm.keepwarm.mode %q not in {off, advise, enforce}", cfg.CacheWarm.Keepwarm.Mode)
		}
	}
	switch cfg.Browser.GranularityCeiling {
	case "", "usage_only", "redacted", "full":
	default:
		return fmt.Errorf("config: browser.granularity_ceiling %q not in {usage_only, redacted, full}", cfg.Browser.GranularityCeiling)
	}
	if cfg.Browser.IngestTimeoutMS > maxBrowserIngestTimeoutMS {
		// The end-to-end browser ingest (db.Open + store.Ingest, bounded by
		// BrowserConfig.IngestTimeout()) must finish before the native host's
		// 40s reply cap tears down the WSL bridge and kills the child
		// mid-write. Reject a value that would break that guarantee rather
		// than silently clamp, so the operator learns their setting is unsafe.
		return fmt.Errorf("config: browser.ingest_timeout_ms %d exceeds the maximum %d (it must stay below the native-messaging host's 40000ms reply cap so a slow ingest is never killed mid-write)", cfg.Browser.IngestTimeoutMS, maxBrowserIngestTimeoutMS)
	}
	if err := validateGuard(cfg.Guard); err != nil {
		return err
	}
	if b := cfg.Observability.Admission.Budget; b.PerUser5hUSD < 0 || b.PerUserWeeklyUSD < 0 || b.PerUserMonthlyUSD < 0 {
		return errors.New("config: observability.admission.budget.per_user_*_usd must be >= 0")
	}
	for _, jc := range []ObservabilityJudgeConfig{cfg.Observability.Judge, cfg.Observability.Admission.Judge} {
		if jc.TimeoutMS < 0 || jc.MaxTokens < 0 || jc.NumCtx < 0 {
			return errors.New("config: observability judge timeout_ms/max_tokens/num_ctx must be >= 0")
		}
	}
	if err := validateObservabilityAlerts(cfg.Observability.Alerts); err != nil {
		return err
	}
	if err := cfg.Email.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := cfg.Digest.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := validateProcessObs(cfg.Observer.Process); err != nil {
		return err
	}
	if err := validateAggregateShare(cfg.AggregateShare); err != nil {
		return err
	}
	if err := validateTerminal(cfg.Terminal); err != nil {
		return err
	}
	return nil
}

// validateTerminal checks the [terminal] block: bounds must be non-negative
// and IdleTimeout (when set) must parse as a Go duration. The launch
// allow-lists are validated at spawn time (canonicalization needs the real
// filesystem), not here.
func validateTerminal(c TerminalConfig) error {
	if c.MaxConcurrent < 0 {
		return errors.New("config: terminal.max_concurrent must be >= 0")
	}
	if c.RingBytes < 0 {
		return errors.New("config: terminal.ring_bytes must be >= 0")
	}
	if strings.TrimSpace(c.IdleTimeout) != "" {
		if _, err := time.ParseDuration(c.IdleTimeout); err != nil {
			return fmt.Errorf("config: terminal.idle_timeout %q is not a valid duration: %w", c.IdleTimeout, err)
		}
	}
	return nil
}

// validateAggregateShare enforces the endpoint binding (design §9.2, finding
// #21): the collector endpoint must be HTTPS, carry no credentials or query
// string, and resolve to an approved host unless the explicit self-host /
// testing escape (AllowCustomEndpoint) is set. Validation runs only when the
// rail is enabled — a default-off install with an empty/custom endpoint never
// fails to load. A change of endpoint invalidates the consent receipt
// (enforced at the CheckConsent seam, not here).
func validateAggregateShare(c AggregateShareConfig) error {
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(c.Endpoint) == "" {
		return errors.New("config: aggregate_share.endpoint is required when aggregate_share.enabled is true")
	}
	u, err := url.Parse(strings.TrimSpace(c.Endpoint))
	if err != nil {
		return fmt.Errorf("config: aggregate_share.endpoint %q is not a valid URL: %w", c.Endpoint, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("config: aggregate_share.endpoint must be https (got %q)", u.Scheme)
	}
	if u.User != nil {
		return errors.New("config: aggregate_share.endpoint must not carry credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("config: aggregate_share.endpoint must not carry a query string or fragment")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return fmt.Errorf("config: aggregate_share.endpoint %q has no host", c.Endpoint)
	}
	if !approvedAggregateHosts[host] && !c.AllowCustomEndpoint {
		return fmt.Errorf("config: aggregate_share.endpoint host %q is not approved; set aggregate_share.allow_custom_endpoint=true only for a self-host/testing collector", host)
	}
	return nil
}

// validateRemote checks the [remote] block (remote-dashboard-access plan §5).
// Validation runs only when the rail is enabled — a default-off install never
// fails to load. Mode is a closed enum; Phase 1 accepts only "off" (tailscale
// and lan land in Phases 2/3). The notify sub-block is validated whenever it is
// enabled, independent of the master switch, since the outbound rail can run
// with the exposure listener off.
func validateRemote(c RemoteConfig) error {
	if c.Enabled {
		switch strings.ToLower(strings.TrimSpace(c.Mode)) {
		case "", "off":
		case "tailscale":
			// Phase 2 (plan §4.4): the tailnet-serve backend binds a dedicated
			// LOOPBACK address distinct from the owner-trusted direct listener.
			// A non-loopback backend would defeat the whole point (the backend
			// must be reachable ONLY via `tailscale serve`).
			if err := validateTailscaleBackendAddr(c.TailscaleBackendAddr); err != nil {
				return err
			}
		case "lan":
			return fmt.Errorf("config: remote.mode %q is deferred (Phase 3, operator decision 2026-07-12 — tailnet-HTTPS-only for v1); use \"tailscale\"", c.Mode)
		default:
			return fmt.Errorf("config: remote.mode %q not in {off, tailscale, lan}", c.Mode)
		}
		if c.RateLimitPerMin < 0 {
			return fmt.Errorf("config: remote.rate_limit_per_min %d must be >= 0", c.RateLimitPerMin)
		}
		if c.MaxSessions < 0 {
			return fmt.Errorf("config: remote.max_sessions %d must be >= 0", c.MaxSessions)
		}
		if c.Enabled && len(c.TrustedHosts) == 0 && strings.EqualFold(strings.TrimSpace(c.Mode), "tailscale") {
			return errors.New("config: remote.trusted_hosts must name the tailnet host (the Host the browser sends) when remote.mode = \"tailscale\" — `observer remote enable --tailscale` populates it")
		}
	}
	if c.Notify.Enabled {
		switch strings.ToLower(strings.TrimSpace(c.Notify.Kind)) {
		case "", "webhook", "ntfy":
		default:
			return fmt.Errorf("config: remote.notify.kind %q not in {webhook, ntfy}", c.Notify.Kind)
		}
		if strings.TrimSpace(c.Notify.URL) == "" {
			return errors.New("config: remote.notify.url is required when remote.notify.enabled is true")
		}
		u, err := url.Parse(strings.TrimSpace(c.Notify.URL))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("config: remote.notify.url %q must be a valid http(s) URL", c.Notify.URL)
		}
	}
	return nil
}

// validateDashboard enforces that [dashboard].addr, when set, parses as a
// host:port pair with a NUMERIC port in 1–65535. Empty is valid (the built-in
// default applies). Delegates to ValidateDashboardAddr so the same shape check
// backs both the config-load path and cmd/observer's env-value guard.
func validateDashboard(d DashboardConfig) error {
	return ValidateDashboardAddr(d.Addr)
}

// ValidateDashboardAddr validates a dashboard listen address string. It is a
// pure SHAPE check — host:port must parse and the port must be a base-10
// integer in 1–65535. Empty is valid (the built-in default applies). It
// intentionally does NOT judge the host: an EMPTY host (":8082", meaning
// bind-all-interfaces / 0.0.0.0) and any non-loopback host are accepted here
// and gated instead by dashboard.CheckRemoteBind, which fails closed unless
// the [remote] security substrate is armed. Keeping the security policy in one
// owner (CheckRemoteBind) and the shape check here mirrors the proxy split
// (proxy.port is a numeric range check; the bind host is a separate flag).
// Mirrors the proxy.port range check's loud-error style.
func ValidateDashboardAddr(addr string) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("config: dashboard.addr %q must be host:port (e.g. 127.0.0.1:8082): %w", addr, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("config: dashboard.addr %q needs a numeric port (got %q)", addr, port)
	}
	if n < 1 || n > 65535 {
		return fmt.Errorf("config: dashboard.addr %q port %d out of range (1–65535)", addr, n)
	}
	return nil
}

// validateTailscaleBackendAddr enforces plan §4.4: the tailnet-serve backend
// address must be an explicit LOOPBACK host:port (never 0.0.0.0, never a
// non-loopback IP, never an interface name). `tailscale serve` forwards
// plaintext to this address, so it must be reachable ONLY on loopback.
func validateTailscaleBackendAddr(addr string) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return errors.New("config: remote.tailscale_backend_addr is required when remote.mode = \"tailscale\" (a loopback IP:port for `tailscale serve` to forward to) — `observer remote enable --tailscale` pins one")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("config: remote.tailscale_backend_addr %q must be host:port: %w", addr, err)
	}
	if port == "" || port == "0" {
		return fmt.Errorf("config: remote.tailscale_backend_addr %q needs an explicit non-zero port", addr)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("config: remote.tailscale_backend_addr host %q must be an explicit loopback IP (127.0.0.1 / ::1) — the tailnet-serve backend is loopback-only (plan §4.4)", host)
	}
	return nil
}

// validateCompression checks the [compression] block's conversation mode / ratio
// enums and the log head/tail bounds. Extracted from Validate to keep that
// function's cyclomatic complexity in check.
func validateCompression(c CompressionConfig) error {
	if c.Conversation.Enabled {
		switch c.Conversation.Mode {
		case "token", "cache", "cache_aware":
		default:
			return fmt.Errorf("config: compression.conversation.mode %q not in {token, cache, cache_aware}", c.Conversation.Mode)
		}
		if r := c.Conversation.TargetRatio; r <= 0 || r >= 1 {
			return fmt.Errorf("config: compression.conversation.target_ratio %.2f must be in (0, 1)", r)
		}
	}
	logs := c.Conversation.Logs
	if logs.MaxLines < 0 {
		return fmt.Errorf("config: compression.conversation.logs.max_lines %d must be >= 0", logs.MaxLines)
	}
	if logs.Head < 0 || logs.Tail < 0 {
		return fmt.Errorf("config: compression.conversation.logs.head/tail must be >= 0 (head=%d, tail=%d)", logs.Head, logs.Tail)
	}
	return nil
}

// validateProcessObs checks the [observer.process] block's enums + numeric
// bounds, but only when process observability is enabled — a stale disabled
// section never fails the daemon (the feature is opt-in). Extracted from
// Validate to keep that function's cyclomatic complexity in check.
func validateProcessObs(p ProcessConfig) error {
	if !p.Enabled {
		return nil
	}
	switch p.Backend {
	case "auto", "bridge", "both", "linux_ebpf", "etw", "endpointsecurity", "poll", "off":
	default:
		return fmt.Errorf("config: observer.process.backend %q not in {auto, bridge, both, linux_ebpf, etw, endpointsecurity, poll, off}", p.Backend)
	}
	if p.PollIntervalMS < 0 {
		return fmt.Errorf("config: observer.process.poll_interval_ms must be >= 0 (0 inherits the default), got %d", p.PollIntervalMS)
	}
	if p.BridgePollIntervalMS < 0 {
		return fmt.Errorf("config: observer.process.bridge_poll_interval_ms must be >= 0 (0 inherits poll_interval_ms), got %d", p.BridgePollIntervalMS)
	}
	if p.CorrelateIntervalMS < 0 {
		return fmt.Errorf("config: observer.process.correlate_interval_ms must be >= 0 (0 inherits the default), got %d", p.CorrelateIntervalMS)
	}
	switch p.Argv.Mode {
	case "preview", "hash_only", "off":
	default:
		return fmt.Errorf("config: observer.process.argv.mode %q not in {preview, hash_only, off}", p.Argv.Mode)
	}
	switch p.Filesystem.Mode {
	case "sensitive", "writes", "all_attributed_writes":
	default:
		return fmt.Errorf("config: observer.process.filesystem.mode %q not in {sensitive, writes, all_attributed_writes}", p.Filesystem.Mode)
	}
	switch p.Network.CaptureBodies {
	case "", "off", "proxied", "available":
	default:
		return fmt.Errorf("config: observer.process.network.capture_bodies %q not in {off, proxied, available}", p.Network.CaptureBodies)
	}
	if p.Network.MaxRequestBytes < 0 {
		return fmt.Errorf("config: observer.process.network.max_request_bytes must be >= 0, got %d", p.Network.MaxRequestBytes)
	}
	if p.Network.MaxResponseBytes < 0 {
		return fmt.Errorf("config: observer.process.network.max_response_bytes must be >= 0, got %d", p.Network.MaxResponseBytes)
	}
	if p.QueueSize <= 0 {
		return fmt.Errorf("config: observer.process.queue_size %d must be > 0 when enabled", p.QueueSize)
	}
	if p.BatchSize <= 0 {
		return fmt.Errorf("config: observer.process.batch_size %d must be > 0 when enabled", p.BatchSize)
	}
	return nil
}

// validateGuard checks the [guard] block's enums + budget/limit bounds (only
// when the guard is enabled — a stale disabled section never fails the daemon).
// Extracted from Validate to keep that function's cyclomatic complexity in
// check as the guard surface grew.
// validateObservabilityAlerts checks the [observability.alerts] block — the
// metric vocabulary + comparator enums + non-negative numeric bounds — only
// when alerting is enabled, so a stale disabled section never fails the daemon
// (the feature is opt-in). The metric set is kept in lock-step with
// internal/obs/alert.Metrics (config is a lower layer and cannot import obs).
func validateObservabilityAlerts(a ObservabilityAlertsConfig) error {
	if !a.Enabled {
		return nil
	}
	if a.EvalIntervalMinutes < 0 {
		return errors.New("config: observability.alerts.eval_interval_minutes must be >= 0")
	}
	for i, r := range a.Rules {
		switch r.Metric {
		case "error_rate", "cost_usd", "latency_p95_ms":
		default:
			return fmt.Errorf("config: observability.alerts.rules[%d].metric %q not in {error_rate, cost_usd, latency_p95_ms}", i, r.Metric)
		}
		switch r.Comparator {
		case "", "gt", "gte":
		default:
			return fmt.Errorf("config: observability.alerts.rules[%d].comparator %q not in {gt, gte}", i, r.Comparator)
		}
		if r.Threshold < 0 {
			return fmt.Errorf("config: observability.alerts.rules[%d].threshold must be >= 0", i)
		}
		if r.WindowMinutes < 0 || r.CooldownMinutes < 0 {
			return fmt.Errorf("config: observability.alerts.rules[%d] window_minutes/cooldown_minutes must be >= 0", i)
		}
	}
	return nil
}

func validateGuard(g GuardConfig) error {
	if !g.Enabled {
		return nil
	}
	switch g.Mode {
	case "off", "observe", "enforce":
	default:
		return fmt.Errorf("config: guard.mode %q not in {off, observe, enforce}", g.Mode)
	}
	switch g.Alerts.MinSeverity {
	case "", "info", "warn", "high", "critical":
	default:
		return fmt.Errorf("config: guard.alerts.min_severity %q not in {info, warn, high, critical}", g.Alerts.MinSeverity)
	}
	switch g.Proxy.EgressAction {
	case "", "flag", "mask", "deny":
	default:
		return fmt.Errorf("config: guard.proxy.egress_action %q not in {flag, mask, deny}", g.Proxy.EgressAction)
	}
	if g.Rules.CEL {
		return errors.New("config: guard.rules.cel is not yet supported (CEL user rules are deferred — matchers v1)")
	}
	b := g.Budget
	if b.SessionUSD < 0 || b.DailyUSD < 0 || b.WeeklyUSD < 0 || b.MonthlyUSD < 0 {
		return errors.New("config: guard.budget.*_usd must be >= 0")
	}
	w := b.Window
	for _, u := range []float64{w.Util5hWarn, w.Util5hDeny, w.UtilWeeklyWarn, w.UtilWeeklyDeny} {
		if u < 0 || u > 1 {
			return fmt.Errorf("config: guard.budget.window utilization %.2f must be in [0, 1]", u)
		}
	}
	if w.Util5hDeny > 0 && w.Util5hWarn > 0 && w.Util5hDeny < w.Util5hWarn {
		return errors.New("config: guard.budget.window.util_5h_deny must be >= util_5h_warn")
	}
	if w.UtilWeeklyDeny > 0 && w.UtilWeeklyWarn > 0 && w.UtilWeeklyDeny < w.UtilWeeklyWarn {
		return errors.New("config: guard.budget.window.util_weekly_deny must be >= util_weekly_warn")
	}
	return nil
}

// HookTimeout returns the hook timeout as a time.Duration.
func (c HooksConfig) HookTimeout() time.Duration {
	return time.Duration(c.TimeoutMS) * time.Millisecond
}

func expandHome(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[2:])
}

// applyEnvOverrides walks cfg via reflection and applies any matching
// OBSERVER_<...> environment variables. Supports string, int, float64,
// bool, and []string (comma-separated).
func applyEnvOverrides(cfg *Config, env func(string) string) {
	v := reflect.ValueOf(cfg).Elem()
	applyEnvToStruct(v, []string{"OBSERVER"}, env)
}

func applyEnvToStruct(v reflect.Value, prefix []string, env func(string) string) {
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		field := t.Field(i)
		tag := field.Tag.Get("toml")
		if tag == "" {
			tag = field.Name
		}
		// Split embedded options like "name,omitempty".
		tag = strings.SplitN(tag, ",", 2)[0]
		if tag == "-" {
			continue
		}
		envSegment := strings.ToUpper(strings.ReplaceAll(tag, ".", "_"))
		newPrefix := append(append([]string{}, prefix...), envSegment)
		fv := v.Field(i)

		if fv.Kind() == reflect.Struct {
			applyEnvToStruct(fv, newPrefix, env)
			continue
		}
		key := strings.Join(newPrefix, "_")
		raw := env(key)
		if raw == "" {
			continue
		}
		setEnvValue(fv, raw)
	}
}

func setEnvValue(fv reflect.Value, raw string) {
	switch fv.Kind() {
	case reflect.String:
		fv.SetString(raw)
	case reflect.Int, reflect.Int32, reflect.Int64:
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			fv.SetInt(n)
		}
	case reflect.Float32, reflect.Float64:
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			fv.SetFloat(f)
		}
	case reflect.Bool:
		if b, err := strconv.ParseBool(raw); err == nil {
			fv.SetBool(b)
		}
	case reflect.Slice:
		if fv.Type().Elem().Kind() == reflect.String {
			parts := strings.Split(raw, ",")
			for i := range parts {
				parts[i] = strings.TrimSpace(parts[i])
			}
			fv.Set(reflect.ValueOf(parts))
		}
	default:
		// Unsupported types are ignored — add cases as needed.
	}
}
