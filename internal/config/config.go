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
}

// ExporterConfig groups second-rail telemetry exporters (Teams & Org
// Visibility, spec §2.4.3). Currently only the OTel exporter. Like the org
// client it is OFF by default: a solo-local install has no [exporter] section
// (or Enabled=false on each sub-exporter), so the daemon makes zero exporter
// network calls and the behaviour is byte-identical to a non-org build.
type ExporterConfig struct {
	OTel OTelExporterConfig `toml:"otel"`
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
}

// Org-client push-size bounds (spec §2.4.2).
const (
	// DefaultMaxPushBytes is the default uncompressed batch ceiling (1 MiB).
	DefaultMaxPushBytes int64 = 1 << 20
	// MaxPushBytesCeiling is the hard upper bound the client clamps to (16 MiB).
	MaxPushBytesCeiling int64 = 16 << 20
	// DefaultPushIntervalSeconds is the default push cadence (15 minutes).
	DefaultPushIntervalSeconds = 900
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
)

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
type IntelligenceConfig struct {
	CodeGraph        IntelligenceCodeGraphConfig `toml:"code_graph"`
	Pricing          PricingConfig               `toml:"pricing"`
	APIKeyEnv        string                      `toml:"api_key_env"`
	SummaryModel     string                      `toml:"summary_model"`
	MonthlyBudgetUSD float64                     `toml:"monthly_budget_usd"`
	MCP              IntelligenceMCPConfig       `toml:"mcp"`
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
				MaxDBSizeMB:           500,
				PruneOnStartup:        true,
				ObserverLogMaxAgeDays: 30,
			},
			Hooks: HooksConfig{
				TimeoutMS:    500,
				AutoRegister: true,
			},
		},
		// Org client is OFF by default (solo-local invariant). The defaults
		// below only take effect once a user sets [org_client] enabled = true.
		OrgClient: OrgClientConfig{
			Enabled:             false,
			PushIntervalSeconds: DefaultPushIntervalSeconds,
			MaxPushBytes:        DefaultMaxPushBytes,
			KeychainID:          DefaultKeychainID,
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
	// RecipeName, when set, names an embedded recipe (see [LoadRecipe])
	// that is applied between [Default] and any TOML file overrides.
	// Empty disables. Unknown name returns an error from [Load].
	RecipeName string
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

	// Recipe overlay (v1.7.6+): a named embedded recipe is applied
	// between Default and the TOML files, so user files / env vars
	// still win. Empty RecipeName falls through to the legacy path.
	if opts.RecipeName != "" {
		recipeCfg, err := LoadRecipe(opts.RecipeName)
		if err != nil {
			return Config{}, err
		}
		cfg = recipeCfg
	}

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
