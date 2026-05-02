// Package shell provides a per-command shell-output filter engine used by
// `observer run <command>` and the PreToolUse Bash hook rewrite. Filters
// are declared in TOML, chained per command, and streamed line-by-line.
// See spec §10 Layer 1.
package shell
