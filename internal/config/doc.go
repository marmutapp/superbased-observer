// Package config loads and validates the SuperBased Observer configuration.
//
// Config is sourced from (in order): defaults → ~/.observer/config.toml →
// per-project .observer/config.toml → OBSERVER_<SECTION>_<KEY> env vars.
// See spec §16.
package config
