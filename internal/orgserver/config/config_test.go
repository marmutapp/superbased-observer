package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatalf("Load missing file: %v", err)
	}
	def := Default()
	if cfg.Server.Listen != def.Server.Listen {
		t.Errorf("Listen = %q, want default %q", cfg.Server.Listen, def.Server.Listen)
	}
	if cfg.Bearer.DefaultLifetimeDays != 90 {
		t.Errorf("DefaultLifetimeDays = %d, want 90", cfg.Bearer.DefaultLifetimeDays)
	}
	if cfg.SAML.AttributeMapping["email"] != "Email" {
		t.Errorf("default attribute mapping email = %q, want Email", cfg.SAML.AttributeMapping["email"])
	}
}

func TestLoadMergesOverDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
[server]
listen = ":9000"
external_url = "https://org.example"
db_path = "/tmp/server.db"
session_key_path = "/etc/observer-org/session.key"

[bearer]
signing_key_path = "/etc/observer-org/bearer/signing.key"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Listen != ":9000" {
		t.Errorf("Listen = %q, want :9000", cfg.Server.Listen)
	}
	// Unset fields keep their defaults.
	if cfg.Server.DataRetentionDays != 730 {
		t.Errorf("DataRetentionDays = %d, want default 730", cfg.Server.DataRetentionDays)
	}
	if cfg.Bearer.DefaultLifetimeDays != 90 {
		t.Errorf("DefaultLifetimeDays = %d, want default 90", cfg.Bearer.DefaultLifetimeDays)
	}
	// AttributeMapping not declared → defaults retained.
	if cfg.SAML.AttributeMapping["groups"] != "Groups" {
		t.Errorf("groups mapping = %q, want default Groups", cfg.SAML.AttributeMapping["groups"])
	}
}

func TestLoadExplicitMappingReplacesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
[saml]
attribute_mapping = { email = "urn:email", display_name = "urn:cn" }
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SAML.AttributeMapping["email"] != "urn:email" {
		t.Errorf("email mapping = %q, want urn:email", cfg.SAML.AttributeMapping["email"])
	}
	// The default "groups" key must NOT survive an explicit mapping.
	if _, ok := cfg.SAML.AttributeMapping["groups"]; ok {
		t.Errorf("explicit mapping should replace defaults wholesale; stale 'groups' key present")
	}
}

func TestValidate(t *testing.T) {
	valid := func() Config {
		c := Default()
		c.Server.ExternalURL = "https://org.example"
		c.Server.SessionKeyPath = "/k/session.key"
		c.SAML.SPEntityID = "https://org.example/saml/metadata"
		c.SAML.SPCertPath = "/k/sp.crt"
		c.SAML.SPKeyPath = "/k/sp.key"
		c.SAML.IDPMetadataURL = "https://idp.example/metadata"
		c.SCIM.AuthTokenPath = "/k/scim.token"
		c.Bearer.SigningKeyPath = "/k/bearer.key"
		return c
	}

	if err := Validate(valid()); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"empty external_url", func(c *Config) { c.Server.ExternalURL = "" }, "external_url"},
		{"relative external_url", func(c *Config) { c.Server.ExternalURL = "org.example" }, "external_url"},
		{"empty session_key_path", func(c *Config) { c.Server.SessionKeyPath = "" }, "session_key_path"},
		{"bad log level", func(c *Config) { c.Server.LogLevel = "trace" }, "log_level"},
		{"negative retention", func(c *Config) { c.Server.DataRetentionDays = -1 }, "data_retention_days"},
		{"missing sp entity", func(c *Config) { c.SAML.SPEntityID = "" }, "sp_entity_id"},
		{"missing sp cert", func(c *Config) { c.SAML.SPCertPath = "" }, "sp_cert_path"},
		{"missing idp metadata", func(c *Config) { c.SAML.IDPMetadataURL = "" }, "idp_metadata_url"},
		{"missing scim token", func(c *Config) { c.SCIM.AuthTokenPath = "" }, "scim.auth_token_path"},
		{"missing bearer key", func(c *Config) { c.Bearer.SigningKeyPath = "" }, "bearer.signing_key_path"},
		{"zero bearer lifetime", func(c *Config) { c.Bearer.DefaultLifetimeDays = 0 }, "default_lifetime_days"},
		{"zero token lifetime", func(c *Config) { c.Enrolment.DefaultTokenLifetimeDays = 0 }, "default_token_lifetime_days"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := valid()
			tt.mutate(&c)
			err := Validate(c)
			if err == nil {
				t.Fatalf("expected error mentioning %q, got nil", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.want)
			}
		})
	}
}

// TestLoadDevComposeConfig is a section-header guard: the dev
// `deploy/observer-org/config.toml` MUST yield a non-empty
// `cfg.Dashboard.AdminEmails` after Load(). The v1.8.2 first cut
// placed the key under [org], which TOML silently dropped — every
// dashboard endpoint then 403'd or returned zeros. This test would
// fail loudly on that misplacement (N6 in
// docs/teams-test-regression-v1.8.2-2026-06-04.md).
func TestLoadDevComposeConfig(t *testing.T) {
	path := filepath.Join("..", "..", "..", "deploy", "observer-org", "config.toml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("dev config %s not present: %v", path, err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load %s: %v", path, err)
	}
	if len(cfg.Dashboard.AdminEmails) == 0 {
		t.Fatalf("cfg.Dashboard.AdminEmails empty after Load(%s) — section header probably wrong (should be [dashboard])", path)
	}
	// The dev config seeds both kristophjunge's IdP default user and the
	// quickstart-provisioned admin; check the kristophjunge default is
	// present so a future operator can't accidentally drop user1@.
	found := false
	for _, e := range cfg.Dashboard.AdminEmails {
		if e == "user1@example.com" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("dev admin_emails missing user1@example.com (the IdP default), got %v", cfg.Dashboard.AdminEmails)
	}
}

func TestDumpRoundTrips(t *testing.T) {
	cfg := Default()
	cfg.Server.ExternalURL = "https://org.example"
	out, err := Dump(cfg)
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if !strings.Contains(out, "external_url") || !strings.Contains(out, "https://org.example") {
		t.Errorf("dump missing external_url:\n%s", out)
	}
	if strings.Contains(out, "BEGIN") {
		t.Errorf("dump unexpectedly contains key material")
	}
}
