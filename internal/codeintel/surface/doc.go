// Package surface is the surface registry: each surface is a named,
// read-only capability over the store (e.g. symbols_in_file, callers_of,
// architecture, dead_code, semantic_query), declared as one struct with
// {name, inputSchema, handler}. Adding a surface — and exposing it via
// CLI/MCP/dashboard — is a single registry entry, not a change to the
// engine or its consumers (the second extensibility axis). See
// docs/codeintel/surfaces.md and docs/codeintel/adding-a-surface.md.
package surface
