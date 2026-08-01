package config

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/marmutapp/superbased-observer/internal/notify/digest"
	"github.com/marmutapp/superbased-observer/internal/notify/email"
)

// DefaultPath is the on-disk location the server config is read from when
// no override is supplied.
const DefaultPath = "/etc/observer-org/config.toml"

// Config is the root org-server configuration. Section defaults are set by
// Default(); a partial TOML file (including missing sections) is supported,
// with unspecified fields retaining their defaults.
type Config struct {
	Server    ServerConfig    `toml:"server"`
	SAML      SAMLConfig      `toml:"saml"`
	SCIM      SCIMConfig      `toml:"scim"`
	Bearer    BearerConfig    `toml:"bearer"`
	Enrolment EnrolmentConfig `toml:"enrolment"`
	Policy    PolicyConfig    `toml:"policy"`
	Dashboard DashboardConfig `toml:"dashboard"`
	// CCAnalytics is the OPTIONAL server-side poller for a coding assistant's
	// own org-analytics API (native-console Workstream C). Default disabled;
	// admin-keyed; server-side only — the agent never touches it. See
	// internal/orgserver/ccanalytics.
	CCAnalytics CCAnalyticsConfig `toml:"cc_analytics"`
	// CodexAnalytics is the OPTIONAL server-side poller for OpenAI Codex's org
	// analytics (native-console instance #2, Rail C). Same posture as
	// CCAnalytics: default disabled, admin-keyed, server-side only. See
	// internal/orgserver/codexanalytics.
	CodexAnalytics CodexAnalyticsConfig `toml:"codex_analytics"`
	// CopilotAnalytics is the OPTIONAL server-side poller for GitHub Copilot's
	// org analytics (native-console instance #3, Rail C). Same posture: default
	// disabled, admin-keyed (a GitHub token), server-side only. Unlike CC/Codex
	// it polls UP TO THREE surfaces (one scheduler each). See
	// internal/orgserver/copilotanalytics.
	CopilotAnalytics CopilotAnalyticsConfig `toml:"copilot_analytics"`
	// M365Copilot is the OPTIONAL server-side poller for Microsoft 365 Copilot's
	// org-tier telemetry (native-console instance #4 / World 1). MICROSOFT's M365
	// Copilot, NOT GitHub Copilot (that is CopilotAnalytics). Same posture:
	// default disabled, admin-keyed (an Entra app client secret), server-side
	// only. See internal/orgserver/m365copilotanalytics.
	M365Copilot M365CopilotConfig `toml:"m365_copilot"`
	// Email is the OPTIONAL shared SMTP notification channel (gap-register G9),
	// an ADDITIONAL delivery target for the budget + obs-alert evaluators
	// alongside their per-alert webhooks. Default disabled; the per-evaluator
	// opt-ins live in [dashboard] (budget_alert_email / obs_alert_email). The
	// credential is supplied via password_env / password_file, never inline —
	// Dump redacts any direct password. See internal/notify/email.
	Email email.Config `toml:"email"`
	// Digest is the OPTIONAL scheduled report digest (gap-register G13): a
	// weekly/monthly org spend rollup emailed through the shared [email]
	// channel. Default disabled; requires [email].enabled too. Recipients
	// default to [email].to. See internal/orgserver/digest.
	Digest digest.Config `toml:"digest"`
}

// CCAnalyticsConfig configures the Claude Code Analytics API poller
// (native-console Phase 5). The admin supplies the API key via the
// CC_ANALYTICS_API_KEY env var or a secret file (never inline in TOML, never
// agent-side). ApiKind selects the auth path: "enterprise" (Enterprise
// Analytics API key) or "admin" (Console/Team Admin API key, sk-ant-admin…).
type CCAnalyticsConfig struct {
	Enabled           bool   `toml:"enabled"`
	ApiKind           string `toml:"api_kind"`     // "enterprise" | "admin"
	APIKeyFile        string `toml:"api_key_file"` // path to a secret file; or use CC_ANALYTICS_API_KEY
	BaseURL           string `toml:"base_url"`     // override for testing; default is Anthropic's
	PollIntervalHours int    `toml:"poll_interval_hours"`
	LagToleranceHours int    `toml:"lag_tolerance_hours"`
}

// CodexAnalyticsConfig configures the OpenAI Codex org-analytics poller
// (native-console instance #2, Rail C). Surface selects which API:
// "chatgpt_enterprise" (api.chatgpt.com, credits, workspace-scoped — requires
// workspace_id) or "openai_org" (api.openai.com, dollars, org-scoped). The admin
// supplies the Bearer key via CODEX_ANALYTICS_API_KEY or a secret file (never
// inline, never agent-side). CreditToUSD is the rate-card factor the Phase-3
// spendCTE merge uses to normalize ChatGPT-surface credits to USD (0 = keep
// native credits / no USD merge until a rate is set).
type CodexAnalyticsConfig struct {
	Enabled           bool    `toml:"enabled"`
	Surface           string  `toml:"surface"`      // "chatgpt_enterprise" | "openai_org"
	APIKeyFile        string  `toml:"api_key_file"` // secret file; or CODEX_ANALYTICS_API_KEY
	BaseURL           string  `toml:"base_url"`     // override for testing
	WorkspaceID       string  `toml:"workspace_id"` // required for chatgpt_enterprise
	PollIntervalHours int     `toml:"poll_interval_hours"`
	LagToleranceHours int     `toml:"lag_tolerance_hours"`
	CreditToUSD       float64 `toml:"credit_to_usd"`
}

// CopilotAnalyticsConfig configures the GitHub Copilot org-analytics poller
// (native-console instance #3, Rail C). Surfaces selects which APIs to poll
// (default all three: "engagement" | "seats" | "billing"); one scheduler runs per
// surface. Owner is the GitHub org login or enterprise slug; OwnerType picks the
// scope ("org" | "enterprise" — the enhanced-billing surface is org-only). The
// admin supplies a GitHub token via COPILOT_ANALYTICS_TOKEN or a secret file
// (never inline, never agent-side). PerSeatPriceUSD is the plan's per-seat monthly
// price the SIBLING cost read multiplies seat counts by ($19 Business / $39
// Enterprise) — the seats API returns counts, not dollars.
type CopilotAnalyticsConfig struct {
	Enabled           bool     `toml:"enabled"`
	Surfaces          []string `toml:"surfaces"`     // engagement | seats | billing
	Owner             string   `toml:"owner"`        // org login or enterprise slug
	OwnerType         string   `toml:"owner_type"`   // "org" | "enterprise"
	Report            string   `toml:"report"`       // engagement report (default users-1-day)
	APIKeyFile        string   `toml:"api_key_file"` // secret file; or COPILOT_ANALYTICS_TOKEN
	BaseURL           string   `toml:"base_url"`     // override for testing
	PollIntervalHours int      `toml:"poll_interval_hours"`
	LagToleranceHours int      `toml:"lag_tolerance_hours"`
	PerSeatPriceUSD   float64  `toml:"per_seat_price_usd"`
}

// M365CopilotConfig configures the Microsoft 365 Copilot org-tier poller
// (native-console instance #4 / World 1). Surfaces selects which rails to poll
// (default both: "graph" Rail A content + "purview" Rail B metadata); one
// scheduler runs per surface. The admin supplies the Entra app credentials:
// TenantID + ClientID inline (not secret) and the client secret via the
// M365_COPILOT_CLIENT_SECRET env var or ClientSecretFile (never inline, never
// agent-side). App-only OAuth2 (AiEnterpriseInteraction.Read.All; delegated NOT
// supported). GLOBAL COMMERCIAL CLOUD ONLY (GCC-High / DoD / 21Vianet NOT
// supported). StoreContent controls whether Rail A stores the actual body
// (default true — the point of Rail A) or runs metadata-only (hash only). The
// stored content is disclosed on the dashboard only behind the admin-tier view
// gate. aiInteractionHistory is NOT metered today (re-verify per release; never
// "free").
type M365CopilotConfig struct {
	Enabled           bool     `toml:"enabled"`
	Surfaces          []string `toml:"surfaces"`           // graph | purview
	TenantID          string   `toml:"tenant_id"`          // Entra tenant id
	ClientID          string   `toml:"client_id"`          // Entra app (client) id
	ClientSecretFile  string   `toml:"client_secret_file"` // secret file; or M365_COPILOT_CLIENT_SECRET
	GraphBaseURL      string   `toml:"graph_base_url"`     // override for testing
	LoginBaseURL      string   `toml:"login_base_url"`     // override for testing
	StoreContent      bool     `toml:"store_content"`      // default true; false = metadata-only
	PollIntervalHours int      `toml:"poll_interval_hours"`
	LagToleranceHours int      `toml:"lag_tolerance_hours"`
}

// DashboardConfig configures the org dashboard's role model and budget engine.
//
// AdminEmails designates org admins: a SAML-authenticated user whose email is
// in this list sees the whole org; everyone else is scoped to the teams they
// lead (org_team_members.role = 'lead'), and a plain member sees nothing. This
// is the bootstrap admin mechanism — group-based admin via the SAML `groups`
// attribute is a future enhancement. BudgetPollSeconds is the budget
// evaluator's cadence (default 60s).
//
// PolicyAdminEmails and SecurityViewerEmails are the guard-layer RBAC roles
// (guard spec §14.5, G14), expressed the same way as the bootstrap admin
// list — config email allow-lists, case-insensitive, layered ON TOP of the
// existing model rather than replacing it:
//
//   - policy_admin: may author and publish org policy bundles from the
//     dashboard (the same api.PublishPolicyBundle gate the CLI uses) and
//     reads the org-wide guard rollups (you cannot author a floor without
//     seeing the rule-hit data it lands on).
//   - security_viewer: reads the org-wide guard rollups (/api/org/guard/*)
//     but cannot publish policy. The role widens GUARD visibility only —
//     cost/team/project endpoints keep their existing admin/lead scoping.
//   - everyone else: a team lead sees their teams' guard rollups (the same
//     lead scope as cost data); a plain member sees nothing.
//
// An admin (AdminEmails) implicitly holds both guard roles. Node-local guard
// stays single-operator (spec §14.5 / D2) — these roles exist only on the
// org server.
type DashboardConfig struct {
	AdminEmails          []string               `toml:"admin_emails"`
	PolicyAdminEmails    []string               `toml:"policy_admin_emails"`
	SecurityViewerEmails []string               `toml:"security_viewer_emails"`
	BudgetPollSeconds    int                    `toml:"budget_poll_seconds"`
	ContentRetention     ContentRetentionConfig `toml:"content_retention"`
	// BudgetAlertEmail, when true (and [email] is enabled), ALSO emails every
	// fired budget-threshold alert to the [email].to recipients, alongside the
	// per-budget webhook. Default false (per-consumer opt-in, gap-register G9).
	BudgetAlertEmail bool `toml:"budget_alert_email"`
	// ObsAlertEmail is the same opt-in for the obs-alert-rule evaluator: email
	// each fired trajectory alert to [email].to alongside the per-rule webhook.
	// Default false.
	ObsAlertEmail bool `toml:"obs_alert_email"`
}

// ContentRetentionConfig is the admin-facing prune horizon for stored message
// content (the otel_content bodies surfaced by the Phase 7 viewer). It mirrors
// the individual node's retention model (config-driven horizon, ≤0 disables).
//
// DEFAULT is OFF (keep forever): large bodies are accepted by design. When
// OTelContentDays > 0 a daily server-side sweep NULLs the body of any
// otel_content row older than the horizon while KEEPING its content_hash and
// the row itself (so audit / dedup / "a body existed here" survive, and re-push
// stays idempotent). This is the org server's only retention sweep — there is
// no node-equivalent server prune for any other table.
type ContentRetentionConfig struct {
	OTelContentDays int `toml:"otel_content_days"` // 0 = keep forever (default); >0 = prune bodies older than N days
}

// ServerConfig groups the core HTTP-server settings.
//
// SessionKeyPath is an addition over the spec's illustrative §2.6 block:
// the SAML session cookie is HMAC-signed over a server-side secret, and the
// project rule is that no long-lived secret lives in code or env — only in
// a configured file path. So the HMAC key is read from this path at boot
// (raw bytes, ≥32 recommended). It is distinct from the bearer signing key
// on purpose: mixing key material across purposes is poor hygiene.
type ServerConfig struct {
	Listen            string `toml:"listen"`
	ExternalURL       string `toml:"external_url"`
	DBPath            string `toml:"db_path"`
	DataRetentionDays int    `toml:"data_retention_days"`
	SessionKeyPath    string `toml:"session_key_path"`
	LogLevel          string `toml:"log_level"`
	// DevAuth, when true, exposes POST /auth/dev/login — a password-free
	// session-issuing endpoint that lets an admin bypass SAML for local
	// evaluation (Issue 5 of the 2026-06-02 teams test findings). Hard
	// gated for compose-only use: the server logs a startup WARN when
	// enabled, /healthz reports `dev_auth: on`, and the endpoint refuses
	// anything other than POST. Setting this in production is a
	// configuration mistake; it should never be enabled on a server
	// reachable from the public internet.
	DevAuth bool `toml:"dev_auth"`
	// MemberInvites, when true, lets a NON-admin SAML member mint an
	// enrolment token for an already-provisioned teammate (the Teams
	// bottom-up invite loop, docs/plans/tier3-local-contract-and-teams-
	// invite-plan-2026-07-31.md Arc 2). DEFAULT FALSE — delegation is an
	// admin opt-in, never a silent change to the trust model.
	//
	// What it does NOT do: it is not an account-creation path. The target
	// must already be an ACTIVE org member (SCIM-provisioned, or JIT-created
	// by a prior SAML login); minting for an unknown email is a 404. v1
	// deliberately has no email self-signup — that is an identity-model
	// change needing its own review.
	//
	// Trust note (read before enabling): an enrolment token is a bearer
	// handoff for the TARGET user's identity, so a member who mints for a
	// teammate could also redeem that token themselves and push activity
	// attributed to the teammate. Admin-only minting bounds that to trusted
	// operators; enabling this widens it to every member. The mitigations
	// shipped with the flag are: default-off, the per-member monthly cap
	// below, an `invite_minted` audit_log row naming the inviter, and
	// enrolment_tokens.minted_by so an admin can see who minted what and
	// whether it was redeemed.
	MemberInvites bool `toml:"member_invites"`
	// MemberInviteMonthlyCap bounds how many enrolment tokens ONE actor may
	// mint per UTC calendar month. It applies to every delegated mint —
	// both the SAML member path and the bearer-authenticated agent path —
	// so a leaked node bearer cannot become an unbounded token faucet.
	// Admin mints are not capped (an admin can already mint from the CLI).
	// Default 10; must be >= 1.
	MemberInviteMonthlyCap int `toml:"member_invite_monthly_cap"`
}

// SAMLConfig configures the SAML 2.0 service-provider side. Cert/key are
// PEM file paths; the IdP metadata is fetched from idp_metadata_url on
// boot. AttributeMapping maps the canonical user fields to the SAML
// assertion attribute names the customer's IdP emits.
type SAMLConfig struct {
	SPEntityID       string            `toml:"sp_entity_id"`
	SPCertPath       string            `toml:"sp_cert_path"`
	SPKeyPath        string            `toml:"sp_key_path"`
	IDPMetadataURL   string            `toml:"idp_metadata_url"`
	AttributeMapping map[string]string `toml:"attribute_mapping"`
}

// SCIMConfig configures SCIM 2.0 provisioning. The static bearer token used
// by the IdP's SCIM client is read from AuthTokenPath (0600-mode file).
type SCIMConfig struct {
	AuthTokenPath string `toml:"auth_token_path"`
}

// BearerConfig configures agent-bearer minting. SigningKeyPath is an
// Ed25519 private key (PEM PKCS#8); DefaultLifetimeDays is the bearer TTL.
type BearerConfig struct {
	SigningKeyPath      string `toml:"signing_key_path"`
	DefaultLifetimeDays int    `toml:"default_lifetime_days"`
}

// EnrolmentConfig configures one-time enrolment-token issuance.
type EnrolmentConfig struct {
	DefaultTokenLifetimeDays int `toml:"default_token_lifetime_days"`
}

// PolicyConfig configures the org guard-policy bundle channel (guard
// spec §14.2, G13). SigningKeyPath is the Ed25519 POLICY signing key
// (PEM PKCS#8) — deliberately distinct from the bearer signing key
// (different rotation and exposure profiles; a leaked bearer key must
// not let an attacker push policy, and vice versa). OPTIONAL: when
// empty, the policy channel is simply off — enrolment omits the
// policy public key, `observer-org policy publish` refuses to run,
// and agents see 404 on the bundle endpoint (exactly the pre-G13
// behaviour). Generate one with `observer-org policy keygen`.
//
// Key-exposure posture (G14 design call, documented deviation from
// G13's CLI-only signing): with dashboard bundle authoring enabled, a
// policy_admin's publish makes the SERVER PROCESS read this key and
// sign — under G13 the long-running process only derived the public
// half at boot and signing happened solely in the operator-run CLI.
// The dashboard handler loads the key from this path PER PUBLISH and
// drops it immediately (no private-key material retained between
// requests; on-disk rotation takes effect without a restart). The
// trade is deliberate: the key file was already readable by the
// server's UID, the publish surface is gated on the §14.5
// policy_admin role and audit-logged, and the alternative (keeping
// publish CLI-only) was judged worse than making the floor authorable
// where the rule-hit data lives. Operators who want the stricter G13
// posture simply leave PolicyAdminEmails empty — the dashboard then
// refuses every publish and the CLI remains the only signer.
type PolicyConfig struct {
	SigningKeyPath string `toml:"signing_key_path"`
}

// Default returns the configuration with all defaults applied. The SAML
// attribute mapping defaults to the canonical Okta-style attribute names.
func Default() Config {
	return Config{
		Server: ServerConfig{
			Listen:            ":8443",
			DBPath:            "/var/lib/observer-org/server.db",
			DataRetentionDays: 730,
			LogLevel:          "info",
			// Delegated invites are admin opt-in; the cap only bites once
			// they are enabled, but it carries a default so an operator who
			// flips member_invites alone still gets a bounded surface.
			MemberInvites:          false,
			MemberInviteMonthlyCap: 10,
		},
		SAML: SAMLConfig{
			AttributeMapping: map[string]string{
				"email":        "Email",
				"display_name": "DisplayName",
				"groups":       "Groups",
			},
		},
		Bearer: BearerConfig{
			DefaultLifetimeDays: 90,
		},
		Enrolment: EnrolmentConfig{
			DefaultTokenLifetimeDays: 7,
		},
		Dashboard: DashboardConfig{
			BudgetPollSeconds: 60,
		},
		CCAnalytics: CCAnalyticsConfig{
			Enabled:           false,
			ApiKind:           "admin",
			PollIntervalHours: 24,
			// Freshness lag is ~1h (research findings B4 — "only data older
			// than 1 hour is included"); 2h gives margin so the scheduler
			// doesn't re-poll a day that's still settling.
			LagToleranceHours: 2,
		},
		CodexAnalytics: CodexAnalyticsConfig{
			Enabled:           false,
			Surface:           "chatgpt_enterprise",
			PollIntervalHours: 24,
			// Codex-Enterprise analytics lags up to ~12h (findings Q-C5); 13h
			// margin so the trailing-window end never outruns settled data.
			LagToleranceHours: 13,
		},
		CopilotAnalytics: CopilotAnalyticsConfig{
			Enabled:           false,
			Surfaces:          []string{"engagement", "seats", "billing"},
			OwnerType:         "org",
			PollIntervalHours: 24,
			// Usage-metrics reports land "within two full days" (findings §3);
			// 48h margin keeps the trailing window on settled data.
			LagToleranceHours: 48,
			// Copilot Business per-seat monthly price; Enterprise tenants set 39.
			PerSeatPriceUSD: 19,
		},
		M365Copilot: M365CopilotConfig{
			Enabled:  false,
			Surfaces: []string{"graph"},
			// Rail A is the content rail — store the body by default; flip to
			// false for a metadata-only (hash) deployment.
			StoreContent:      true,
			PollIntervalHours: 24,
			// Interaction/audit data settles within a day; 24h margin keeps the
			// trailing window on settled data.
			LagToleranceHours: 24,
		},
	}
}

// Load applies Default() then merges the TOML file at path over it. A
// missing file is not an error (the caller gets pure defaults), so
// `dump-config` can show the effective baseline. Semantic checks live in
// Validate, which the caller runs separately.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		path = DefaultPath
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("orgserver/config.Load: read %s: %w", path, err)
	}
	// Default() seeds AttributeMapping; clear it so an explicit mapping in
	// the file replaces the defaults wholesale rather than merging key-by-
	// key (BurntSushi merges maps additively, which would leave stale
	// default keys an operator meant to drop).
	if mappingPresent(body) {
		cfg.SAML.AttributeMapping = nil
	}
	if err := toml.Unmarshal(body, &cfg); err != nil {
		return Config{}, fmt.Errorf("orgserver/config.Load: parse %s: %w", path, err)
	}
	return cfg, nil
}

// mappingPresent reports whether the TOML body declares an explicit
// [saml].attribute_mapping. A cheap structural decode avoids a second full
// parse; we only need to know if the key exists.
func mappingPresent(body []byte) bool {
	var probe struct {
		SAML struct {
			AttributeMapping map[string]string `toml:"attribute_mapping"`
		} `toml:"saml"`
	}
	if err := toml.Unmarshal(body, &probe); err != nil {
		return false
	}
	return probe.SAML.AttributeMapping != nil
}

// Validate checks semantic constraints required for `serve`. It does not
// touch the filesystem (doctor does the deep file checks); it only catches
// structurally invalid config so the server fails fast with a clear error
// rather than at first request.
func Validate(cfg Config) error {
	if cfg.Server.Listen == "" {
		return errors.New("orgserver/config: server.listen is required")
	}
	if cfg.Server.ExternalURL == "" {
		return errors.New("orgserver/config: server.external_url is required")
	}
	if u, err := url.Parse(cfg.Server.ExternalURL); err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("orgserver/config: server.external_url %q must be an absolute URL", cfg.Server.ExternalURL)
	}
	if cfg.Server.DBPath == "" {
		return errors.New("orgserver/config: server.db_path is required")
	}
	if cfg.Server.SessionKeyPath == "" {
		return errors.New("orgserver/config: server.session_key_path is required")
	}
	switch cfg.Server.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("orgserver/config: server.log_level %q not in {debug, info, warn, error}", cfg.Server.LogLevel)
	}
	if cfg.Server.DataRetentionDays < 0 {
		return errors.New("orgserver/config: server.data_retention_days must be >= 0")
	}
	// The cap is validated unconditionally (not only when member_invites is
	// on) so a typo is caught at boot rather than the first delegated mint.
	// A zero/negative cap would silently mean "no member may ever invite",
	// which is what member_invites = false already says — refuse the
	// ambiguous spelling.
	if cfg.Server.MemberInviteMonthlyCap < 1 {
		return errors.New("orgserver/config: server.member_invite_monthly_cap must be >= 1 (set server.member_invites = false to disable delegated invites)")
	}
	if cfg.SAML.SPEntityID == "" {
		return errors.New("orgserver/config: saml.sp_entity_id is required")
	}
	if cfg.SAML.SPCertPath == "" || cfg.SAML.SPKeyPath == "" {
		return errors.New("orgserver/config: saml.sp_cert_path and saml.sp_key_path are required")
	}
	if cfg.SAML.IDPMetadataURL == "" {
		return errors.New("orgserver/config: saml.idp_metadata_url is required")
	}
	if cfg.SCIM.AuthTokenPath == "" {
		return errors.New("orgserver/config: scim.auth_token_path is required")
	}
	if cfg.Bearer.SigningKeyPath == "" {
		return errors.New("orgserver/config: bearer.signing_key_path is required")
	}
	if cfg.Bearer.DefaultLifetimeDays <= 0 {
		return errors.New("orgserver/config: bearer.default_lifetime_days must be > 0")
	}
	if cfg.Enrolment.DefaultTokenLifetimeDays <= 0 {
		return errors.New("orgserver/config: enrolment.default_token_lifetime_days must be > 0")
	}
	if err := cfg.Email.Validate(); err != nil {
		return fmt.Errorf("orgserver/config: %w", err)
	}
	if err := cfg.Digest.Validate(); err != nil {
		return fmt.Errorf("orgserver/config: %w", err)
	}
	// A negative plan price would not produce a negative bill — the telemetry
	// rollup refuses to price seats at all below zero — so the visible effect
	// is that seat cost silently vanishes from the dashboard. Fail fast with
	// the reason instead. Zero is legal and means "price not supplied".
	//
	// NaN and Inf must be rejected explicitly: TOML accepts the literals `nan`
	// and `inf` without error (verified against the decoder), and `< 0` catches
	// neither — NaN compares false to everything, and +Inf is not negative. The
	// consequences differ and both are bad: `nan` prices nothing (the rollup's
	// `> 0` gate is false) so the subscription silently disappears, while `inf`
	// prices seats at +Inf, which encoding/json refuses — and writeJSON has
	// already sent 200 by then and discards the encode error, so the endpoint
	// answers 200 with no valid body.
	if price := cfg.CopilotAnalytics.PerSeatPriceUSD; price < 0 || math.IsNaN(price) || math.IsInf(price, 0) {
		return fmt.Errorf("orgserver/config: copilot_analytics.per_seat_price_usd %v must be a finite value >= 0 (0 means unpriced; seat cost is then omitted)",
			price)
	}
	return nil
}

// Dump renders cfg back to TOML for the `dump-config` subcommand. Only file
// paths to secrets are shown — never secret material — because the config
// never contains the secrets themselves.
func Dump(cfg Config) (string, error) {
	// Blank any direct [email] password before encoding — the dump promise is
	// "only paths to secrets, never secret material". password_env /
	// password_file references are kept (they are not secrets).
	cfg.Email = cfg.Email.Redacted()
	var sb strings.Builder
	if err := toml.NewEncoder(&sb).Encode(cfg); err != nil {
		return "", fmt.Errorf("orgserver/config.Dump: %w", err)
	}
	return sb.String(), nil
}
