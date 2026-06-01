package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
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
	Dashboard DashboardConfig `toml:"dashboard"`
}

// DashboardConfig configures the org dashboard's role model and budget engine.
//
// AdminEmails designates org admins: a SAML-authenticated user whose email is
// in this list sees the whole org; everyone else is scoped to the teams they
// lead (org_team_members.role = 'lead'), and a plain member sees nothing. This
// is the bootstrap admin mechanism — group-based admin via the SAML `groups`
// attribute is a future enhancement. BudgetPollSeconds is the budget
// evaluator's cadence (default 60s).
type DashboardConfig struct {
	AdminEmails       []string `toml:"admin_emails"`
	BudgetPollSeconds int      `toml:"budget_poll_seconds"`
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

// Default returns the configuration with all defaults applied. The SAML
// attribute mapping defaults to the canonical Okta-style attribute names.
func Default() Config {
	return Config{
		Server: ServerConfig{
			Listen:            ":8443",
			DBPath:            "/var/lib/observer-org/server.db",
			DataRetentionDays: 730,
			LogLevel:          "info",
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
	return nil
}

// Dump renders cfg back to TOML for the `dump-config` subcommand. Only file
// paths to secrets are shown — never secret material — because the config
// never contains the secrets themselves.
func Dump(cfg Config) (string, error) {
	var sb strings.Builder
	if err := toml.NewEncoder(&sb).Encode(cfg); err != nil {
		return "", fmt.Errorf("orgserver/config.Dump: %w", err)
	}
	return sb.String(), nil
}
