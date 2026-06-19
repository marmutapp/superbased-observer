package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the root configuration for the observer. Field defaults are set
// by Default(). Partial TOML files (including missing sections) are supported
// — unspecified fields retain their defaults.
type Config struct {
	Observer     ObserverConfig     `toml:"observer"`
	Proxy        ProxyConfig        `toml:"proxy"`
	Compression  CompressionConfig  `toml:"compression"`
	Intelligence IntelligenceConfig `toml:"intelligence"`
	OrgClient    OrgClientConfig    `toml:"org_client"`
	Exporter     ExporterConfig     `toml:"exporter"`
	Ingest       IngestConfig       `toml:"ingest"`
	CacheTrack   CacheTrackConfig   `toml:"cachetrack"`
	Predict      PredictConfig      `toml:"predict"`
	Advisor      AdvisorConfig      `toml:"advisor"`
	Routing      RoutingConfig      `toml:"routing"`
	Guard        GuardConfig        `toml:"guard"`
	Profiles     ProfilesConfig     `toml:"profiles"`
	// Experiments is the [[experiments]] list — productized profile
	// A/B runs (usability arc P6.4). See experiments.go.
	Experiments []ExperimentConfig `toml:"experiments"`
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
type GuardBudgetConfig struct {
	SessionUSD float64 `toml:"session_usd"`
	DailyUSD   float64 `toml:"daily_usd"`
	Hard       bool    `toml:"hard"`
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
	// The sweep runs from the existing maintenance tick — the
	// same retention pass that handles actions / observer_log /
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
type OrgClientShareConfig struct {
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
	// by the existing maintenance tick (cmd/observer/prune.go::runRetention).
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
	ForceChatGPTHTTP  bool   `toml:"force_chatgpt_http"`
	// PrewarmTargets are URLs the proxy fires HEAD against at startup
	// to populate the http.Transport connection pool with warm TLS
	// sessions (V6-3 mitigation). The first real proxy request reuses
	// a pooled connection, saving the TLS handshake cost that pushed
	// codex's inner-pipe TTFB past its ~15s timeout. Defaults set in
	// Default(); empty slice disables pre-warm entirely.
	PrewarmTargets []string `toml:"prewarm_targets"`
}

// CompressionConfig groups all four compression layers' toggles.
type CompressionConfig struct {
	CodeGraph    CodeGraphConfig    `toml:"code_graph"`
	Shell        ShellConfig        `toml:"shell"`
	Indexing     IndexingConfig     `toml:"indexing"`
	Conversation ConversationConfig `toml:"conversation"`
}

// CodeGraphConfig controls the codebase-memory-mcp companion.
//
// `Path`, when set, overrides the auto-discovery used by
// [github.com/marmutapp/superbased-observer/internal/codegraph.FindProjectDB].
// Operators with multiple projects sharing one observer install can
// set this to pin the graph.db consumed by the v1.7.7 marker
// enrichment (V7-11 mitigation (e)). Empty falls back to
// `FindProjectDB(cwd)`, scanning observer's data dir.
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
	Enabled       bool             `toml:"enabled"`
	Mode          string           `toml:"mode"`
	TargetRatio   float64          `toml:"target_ratio"`
	PreserveLastN int              `toml:"preserve_last_n"`
	CompressTypes []string         `toml:"compress_types"`
	Logs          LogsConfig       `toml:"logs"`
	Stash         StashConfig      `toml:"stash"`
	Compaction    CompactionConfig `toml:"compaction"`
	Rolling       RollingConfig    `toml:"rolling"`
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

// IntelligenceCodeGraphConfig is a distinct sub-config (separate from the
// compression layer) because a user may enable intelligence queries against
// the code graph without enabling the companion binary.
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
					"claude-code", "codex", "cline", "cline-cli", "roo-code", "cursor", "copilot", "copilot-cli", "cowork", "opencode", "openclaw", "pi", "gemini-cli", "antigravity", "hermes", "kilo-code", "kilo-code-cli",
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
				BridgePollIntervalMS: 0, // 0 = inherit PollIntervalMS (see resolveProcessPollIntervals)
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
	if err := mergeTOMLFile(&cfg, globalPath); err != nil {
		return Config{}, err
	}
	if opts.ProjectPath != "" {
		if err := mergeTOMLFile(&cfg, opts.ProjectPath); err != nil {
			return Config{}, err
		}
	}
	applyEnvOverrides(&cfg, opts.Env)

	cfg.Observer.DBPath = expandHome(cfg.Observer.DBPath)
	cfg.Compression.Conversation.Stash.Dir = expandHome(cfg.Compression.Conversation.Stash.Dir)

	if err := Validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func mergeTOMLFile(cfg *Config, path string) error {
	if path == "" {
		return nil
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("config.Load: read %s: %w", path, err)
	}
	if err := toml.Unmarshal(body, cfg); err != nil {
		return fmt.Errorf("config.Load: parse %s: %w", path, err)
	}
	return nil
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
	if cfg.Compression.Conversation.Enabled {
		switch cfg.Compression.Conversation.Mode {
		case "token", "cache", "cache_aware":
		default:
			return fmt.Errorf("config: compression.conversation.mode %q not in {token, cache, cache_aware}", cfg.Compression.Conversation.Mode)
		}
		if r := cfg.Compression.Conversation.TargetRatio; r <= 0 || r >= 1 {
			return fmt.Errorf("config: compression.conversation.target_ratio %.2f must be in (0, 1)", r)
		}
	}
	logs := cfg.Compression.Conversation.Logs
	if logs.MaxLines < 0 {
		return fmt.Errorf("config: compression.conversation.logs.max_lines %d must be >= 0", logs.MaxLines)
	}
	if logs.Head < 0 || logs.Tail < 0 {
		return fmt.Errorf("config: compression.conversation.logs.head/tail must be >= 0 (head=%d, tail=%d)", logs.Head, logs.Tail)
	}
	if err := validateRouting(cfg.Routing); err != nil {
		return err
	}
	if cfg.Guard.Enabled {
		switch cfg.Guard.Mode {
		case "off", "observe", "enforce":
		default:
			return fmt.Errorf("config: guard.mode %q not in {off, observe, enforce}", cfg.Guard.Mode)
		}
		switch cfg.Guard.Alerts.MinSeverity {
		case "", "info", "warn", "high", "critical":
		default:
			return fmt.Errorf("config: guard.alerts.min_severity %q not in {info, warn, high, critical}", cfg.Guard.Alerts.MinSeverity)
		}
		switch cfg.Guard.Proxy.EgressAction {
		case "", "flag", "mask", "deny":
		default:
			return fmt.Errorf("config: guard.proxy.egress_action %q not in {flag, mask, deny}", cfg.Guard.Proxy.EgressAction)
		}
		if cfg.Guard.Rules.CEL {
			return errors.New("config: guard.rules.cel is not yet supported (CEL user rules are deferred — matchers v1)")
		}
	}
	// Process observability: validate enums only when enabled, so a stale
	// disabled section never fails the daemon (the feature is opt-in).
	if cfg.Observer.Process.Enabled {
		switch cfg.Observer.Process.Backend {
		case "auto", "bridge", "both", "linux_ebpf", "etw", "endpointsecurity", "poll", "off":
		default:
			return fmt.Errorf("config: observer.process.backend %q not in {auto, bridge, both, linux_ebpf, etw, endpointsecurity, poll, off}", cfg.Observer.Process.Backend)
		}
		if cfg.Observer.Process.PollIntervalMS < 0 {
			return fmt.Errorf("config: observer.process.poll_interval_ms must be >= 0 (0 inherits the default), got %d", cfg.Observer.Process.PollIntervalMS)
		}
		if cfg.Observer.Process.BridgePollIntervalMS < 0 {
			return fmt.Errorf("config: observer.process.bridge_poll_interval_ms must be >= 0 (0 inherits poll_interval_ms), got %d", cfg.Observer.Process.BridgePollIntervalMS)
		}
		switch cfg.Observer.Process.Argv.Mode {
		case "preview", "hash_only", "off":
		default:
			return fmt.Errorf("config: observer.process.argv.mode %q not in {preview, hash_only, off}", cfg.Observer.Process.Argv.Mode)
		}
		switch cfg.Observer.Process.Filesystem.Mode {
		case "sensitive", "writes", "all_attributed_writes":
		default:
			return fmt.Errorf("config: observer.process.filesystem.mode %q not in {sensitive, writes, all_attributed_writes}", cfg.Observer.Process.Filesystem.Mode)
		}
		if cfg.Observer.Process.QueueSize <= 0 {
			return fmt.Errorf("config: observer.process.queue_size %d must be > 0 when enabled", cfg.Observer.Process.QueueSize)
		}
		if cfg.Observer.Process.BatchSize <= 0 {
			return fmt.Errorf("config: observer.process.batch_size %d must be > 0 when enabled", cfg.Observer.Process.BatchSize)
		}
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
