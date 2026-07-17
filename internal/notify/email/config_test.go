package email

import (
	"os"
	"strings"
	"testing"
)

func TestConfigValidate(t *testing.T) {
	base := Config{Enabled: true, Host: "smtp.example.com", From: "obs@example.com"}
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr bool
	}{
		{"disabled skips all checks", func(c *Config) { c.Enabled = false; c.Host = ""; c.From = "" }, false},
		{"valid minimal", func(c *Config) {}, false},
		{"missing host", func(c *Config) { c.Host = "" }, true},
		{"blank host", func(c *Config) { c.Host = "   " }, true},
		{"missing from", func(c *Config) { c.From = "" }, true},
		{"bad port high", func(c *Config) { c.Port = 70000 }, true},
		{"bad port negative", func(c *Config) { c.Port = -1 }, true},
		{"port ok", func(c *Config) { c.Port = 587 }, false},
		{"tls starttls", func(c *Config) { c.TLSMode = "starttls" }, false},
		{"tls implicit", func(c *Config) { c.TLSMode = "tls" }, false},
		{"tls none", func(c *Config) { c.TLSMode = "none" }, false},
		{"tls bad", func(c *Config) { c.TLSMode = "ssl" }, true},
		{"auth plain", func(c *Config) { c.Auth = "plain" }, false},
		{"auth login", func(c *Config) { c.Auth = "login" }, false},
		{"auth bad", func(c *Config) { c.Auth = "oauth" }, true},
		{"negative timeout", func(c *Config) { c.TimeoutSeconds = -5 }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base
			tt.mutate(&c)
			err := c.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err=%v wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func TestConfigResolveEnv(t *testing.T) {
	t.Setenv("SBO_TEST_SMTP_PW", "s3cr3t")
	c := Config{CredEnv: "SBO_TEST_SMTP_PW"}
	got := c.Resolve()
	if got.Cred != "s3cr3t" {
		t.Fatalf("Resolve env: got %q", got.Cred)
	}
	// Original is unchanged (value semantics).
	if c.Cred != "" {
		t.Fatalf("Resolve mutated receiver: %q", c.Cred)
	}
}

func TestConfigResolveFile(t *testing.T) {
	dir := t.TempDir()
	f := dir + "/pw"
	if err := os.WriteFile(f, []byte("filepw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Config{CredFile: f}
	if got := c.Resolve(); got.Cred != "filepw" {
		t.Fatalf("Resolve file: got %q", got.Cred)
	}
}

func TestConfigRedactionNeverLeaks(t *testing.T) {
	c := Config{Enabled: true, Host: "h", From: "f@x", Username: "u", Cred: "topsecret", CredEnv: "PW_ENV"}
	if strings.Contains(c.String(), "topsecret") {
		t.Fatalf("String leaked credential: %s", c.String())
	}
	if r := c.Redacted(); r.Cred != "" {
		t.Fatalf("Redacted kept credential: %q", r.Cred)
	}
	// Redacted keeps the reference (not a secret).
	if r := c.Redacted(); r.CredEnv != "PW_ENV" {
		t.Fatalf("Redacted dropped env reference: %q", r.CredEnv)
	}
}

func TestPortDefaults(t *testing.T) {
	if p := (Config{TLSMode: "tls"}).port(); p != 465 {
		t.Fatalf("implicit tls default port = %d, want 465", p)
	}
	if p := (Config{}).port(); p != 587 {
		t.Fatalf("starttls default port = %d, want 587", p)
	}
	if p := (Config{Port: 2525}).port(); p != 2525 {
		t.Fatalf("explicit port = %d, want 2525", p)
	}
}
