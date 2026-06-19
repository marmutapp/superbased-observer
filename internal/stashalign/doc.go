// Package stashalign provides cross-process discovery of the active
// conversation-stash directory (V7-8).
//
// The compression pipeline's stash compressor writes blobs into
// [compression.conversation.stash].Dir. The retrieve_stashed MCP tool
// reads blobs from the same path. Proxy and MCP server are
// independent processes — when they read different config files
// (e.g. `observer proxy --config A.toml` and codex spawning
// `observer serve` with default config), they can point at different
// stash dirs and the agent's retrieve_stashed calls silently miss.
//
// This package decouples the discovery: the proxy that initialises
// its stash writes its active dir into a sidecar file under the
// user's observer home; `observer serve` reads the sidecar on
// startup and warns if its own configured dir doesn't match. Last-
// writer-wins on the sidecar is acceptable — a stale write at worst
// produces a one-off false positive warning.
//
// See docs/v4-codex-compression-recipe-and-issues.md V7-8 and
// docs/v1.7.13-stash-dir-alignment-plan-2026-05-31.md for the design
// rationale.
package stashalign
