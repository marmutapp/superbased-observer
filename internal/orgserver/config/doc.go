// Package config loads and validates the org server's configuration from
// its TOML file (default /etc/observer-org/config.toml), per the Teams &
// Org Visibility spec §2.6.
//
// The server config is entirely separate from the agent config
// (internal/config): the org server is a distinct binary (cmd/observer-org)
// with its own deployment, so it does not share the agent's
// ~/.observer/config.toml shape or loader. The two never run in the same
// process.
//
// Load applies Default() then merges the on-disk TOML over it (partial
// files are supported — unset fields keep their defaults). Validate is a
// separate step the `serve` and `doctor` subcommands call; `dump-config`
// deliberately skips it so an operator can inspect a partial config while
// filling it in.
//
// Secrets are never embedded in this config — only filesystem paths to
// them (SP cert/key, bearer signing key, SCIM token, session HMAC key).
// The files are read at boot and never re-read; doctor verifies they exist
// and (for the SCIM token) carry 0600 mode.
package config
