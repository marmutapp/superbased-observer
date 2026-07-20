package email

import (
	"fmt"
	"os"
	"strings"
)

// TLS mode names for Config.TLSMode.
const (
	// TLSStartTLS upgrades a plaintext connection with STARTTLS after the
	// greeting (the submission-port 587 convention). This is the default when
	// TLSMode is empty.
	TLSStartTLS = "starttls"
	// TLSImplicit dials straight into TLS (the SMTPS port 465 convention).
	TLSImplicit = "tls"
	// TLSNone sends over an unencrypted connection. Only sane for a loopback
	// relay; AUTH credentials are refused over an unencrypted non-loopback
	// connection regardless (see the auth builders).
	TLSNone = "none"
)

// Auth method names for Config.Auth.
const (
	// AuthAuto (empty) picks PLAIN when the server advertises it, else LOGIN.
	AuthAuto = ""
	// AuthPlain forces AUTH PLAIN.
	AuthPlain = "plain"
	// AuthLogin forces AUTH LOGIN (the Office 365 / some-relay dialect).
	AuthLogin = "login"
)

// Config is the [email] block: the SMTP settings + default recipients shared by
// every consumer (the node alert loop and the org-server evaluators). It is the
// SINGLE owner of the email-config shape — both config packages
// (internal/config and internal/orgserver/config) embed this type as their
// [email] surface, so the shape and its validation live in one place.
//
// LOCAL-ONLY: like every other credential-bearing block it is never distributed
// over the org wire. Zero value = disabled (Enabled=false).
//
// The credential field (TOML key "password") is named Cred in Go; it is
// redacted from String/Redacted and must never be logged. Prefer CredEnv or
// CredFile so no secret lives on disk.
type Config struct {
	// Enabled gates the whole channel. Default false — a fired alert makes an
	// outbound SMTP call, so email is explicit opt-in and a default install
	// stays egress-free.
	Enabled bool `toml:"enabled"`
	// Host is the SMTP server hostname (also the TLS ServerName and the
	// AUTH/loopback check host). Required when Enabled.
	Host string `toml:"host"`
	// Port is the SMTP port. 0 defaults to 465 for implicit TLS, else 587.
	Port int `toml:"port"`
	// Username is the SMTP AUTH user. Empty = no authentication (a local relay
	// that accepts unauthenticated submission).
	Username string `toml:"username"`
	// Cred is the SMTP AUTH password (TOML key "password"). DISCOURAGED in a
	// config file — prefer CredEnv or CredFile so no secret lives on disk. It
	// is redacted from String/Redacted and must never be logged.
	Cred string `toml:"password"`
	// CredEnv names the ENV VAR holding the password (preferred, the
	// api_key_env posture). Resolve reads it into Cred; it wins over a direct
	// Cred value.
	CredEnv string `toml:"password_env"`
	// CredFile is a path to a file whose contents are the password (the org
	// api_key_file posture). Resolve reads it when CredEnv is unset.
	CredFile string `toml:"password_file"`
	// From is the envelope + header From address. Required when Enabled.
	From string `toml:"from"`
	// To is the default recipient list used when a consumer supplies none.
	To []string `toml:"to"`
	// TLSMode ∈ starttls | tls | none. Empty defaults to starttls.
	TLSMode string `toml:"tls_mode"`
	// Auth ∈ "" (auto) | plain | login. Empty auto-selects.
	Auth string `toml:"auth"`
	// TimeoutSeconds bounds one full send (dial + handshake + conversation).
	// 0 defaults to 15.
	TimeoutSeconds int `toml:"timeout_seconds"`
}

// Validate checks the block only when Enabled (a stale disabled section never
// fails the daemon — email is opt-in). It never touches the network or the
// filesystem; it catches structurally-wrong config so a consumer fails fast.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(c.Host) == "" {
		return fmt.Errorf("email: host is required when enabled")
	}
	if strings.TrimSpace(c.From) == "" {
		return fmt.Errorf("email: from is required when enabled")
	}
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("email: port %d out of range", c.Port)
	}
	switch c.TLSMode {
	case "", TLSStartTLS, TLSImplicit, TLSNone:
	default:
		return fmt.Errorf("email: tls_mode %q not in {starttls, tls, none}", c.TLSMode)
	}
	switch c.Auth {
	case AuthAuto, AuthPlain, AuthLogin:
	default:
		return fmt.Errorf("email: auth %q not in {plain, login}", c.Auth)
	}
	if c.TimeoutSeconds < 0 {
		return fmt.Errorf("email: timeout_seconds must be >= 0")
	}
	return nil
}

// Resolve returns a copy with the credential materialized from CredEnv (if set)
// or CredFile (if set), leaving a direct Cred otherwise. The resolved copy is
// what the sender uses; it is never logged or dumped.
func (c Config) Resolve() Config {
	out := c
	switch {
	case c.CredEnv != "":
		out.Cred = os.Getenv(c.CredEnv)
	case c.CredFile != "":
		if b, err := os.ReadFile(c.CredFile); err == nil {
			out.Cred = strings.TrimRight(string(b), "\r\n")
		}
	}
	return out
}

// port returns the effective port, defaulting by TLS mode.
func (c Config) port() int {
	if c.Port != 0 {
		return c.Port
	}
	if c.tlsMode() == TLSImplicit {
		return 465
	}
	return 587
}

// tlsMode returns the effective TLS mode, defaulting to STARTTLS.
func (c Config) tlsMode() string {
	if c.TLSMode == "" {
		return TLSStartTLS
	}
	return c.TLSMode
}

// timeoutSeconds returns the effective per-send timeout in seconds, default 15.
func (c Config) timeoutSeconds() int {
	if c.TimeoutSeconds <= 0 {
		return 15
	}
	return c.TimeoutSeconds
}

// Redacted returns a copy with every credential source blanked — safe to encode
// (e.g. an org `dump-config`) or attach to a diagnostic. CredEnv and CredFile
// are references (an env-var name / a path), not secrets, so they are kept.
func (c Config) Redacted() Config {
	out := c
	out.Cred = ""
	return out
}

// String renders the config with the credential redacted, so a whole-Config log
// line can never leak it.
func (c Config) String() string {
	pw := "unset"
	switch {
	case c.CredEnv != "":
		pw = "env:" + c.CredEnv
	case c.CredFile != "":
		pw = "file:" + c.CredFile
	case c.Cred != "":
		pw = "***redacted***"
	}
	return fmt.Sprintf("email.Config{enabled=%t host=%q port=%d username=%q cred=%s from=%q to=%d tls=%q auth=%q}",
		c.Enabled, c.Host, c.port(), c.Username, pw, c.From, len(c.To), c.tlsMode(), c.Auth)
}
