// Package mcpsec implements the MCP security config layer (guard spec
// §9): inventory of every client's configured MCP servers, pin-on-
// first-sight with rug-pull/binary-swap detection, and static
// poisoning heuristics over observed tool descriptions.
//
// The package is PURE LOGIC (CLAUDE.md module rule 1): it parses
// bytes it is handed, hashes, diffs against pin records it is handed,
// and returns findings — no SQL, no HTTP, no fsnotify, no os.ReadFile
// on its own initiative (Inventory takes a read function). Config
// locations come from internal/mcp/locate (the one owner of MCP
// config paths, shared with `observer init`'s registrar); findings
// are policy.MCPFinding values the guard layer evaluates through the
// real engine so mode/disable/override semantics apply uniformly.
// I/O sequencing (read configs → load pins → diff → persist) lives in
// the cmd composition layer, the guardScannerAdapter precedent.
//
// # Pin format (documented deviation from the §9.2 one-hash sketch)
//
// Spec §9.2 sketches the pin as one SHA-256 over (command/URL +
// sorted tool names + tool description texts). This package stores a
// COMPOSITE pin instead — "v1 cfg:<sha256> tools:<sha256|->" — two
// component hashes in one guard_pins.pin_hash column:
//
//   - the cfg half covers transport + command/URL + args + env KEY
//     names (values excluded: env values carry rotating secrets, and
//     a token rotation must not read as a binary swap);
//   - the tools half covers the sorted (name, description, param
//     docs) set, "-" until tools have been observed.
//
// Two halves because attribution needs them: a cfg-half change is
// R-305 (binary/path swap), a tools-half change is R-302 (rug-pull),
// and a tools half going empty→set is ENRICHMENT (first observation
// completing the pin), not drift. A single hash cannot distinguish
// these three, and mislabeling enrichment as a rug-pull would make
// R-302 fire on every server's first proxied session.
//
// # Where tool descriptions come from
//
// Client config files carry command/URL only — tool names and
// descriptions exist at MCP-handshake time. This package NEVER
// executes a server to ask (spawning every configured stdio server
// from a security scanner is the vulnerability, not the feature).
// Instead the proxy seam observes the tools array the client itself
// sends in API request bodies (guard spec §9.2 "observed MCP
// tool-call metadata") and hands the MCP-prefixed declarations here.
// Consequence, documented honestly: tools-half pinning, R-302 and
// R-303 engage only for proxy-routed clients; config-only installs
// get cfg-half pinning (R-301/R-305).
//
// # Baseline-quiet first scan
//
// The very first scan of a client (no pin rows for it) pins every
// server silently — an R-301 storm on install would teach operators
// to ignore the rule. R-301 fires for servers appearing AFTER a
// client's baseline exists.
package mcpsec
