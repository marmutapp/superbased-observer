package proxy

import (
	"net/http"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// toolSignature is one row of the header → tool identification table:
// when the named request header carries the given case-insensitive
// prefix, the connection is attributed to the normalized tool
// identity. Evidence is client-controlled (a local process talking to
// its own local proxy), so a row only ever selects which compression
// profile applies — never any other proxy behavior.
type toolSignature struct {
	header string // canonical header name, looked up via http.Header.Get
	prefix string // case-insensitive prefix the header value must carry
	tool   string // normalized models.Tool* identity
}

// toolSignatures is the L1 tool-resolution fallback table for the
// compression profile router's per-tool tier (R2). The pidbridge has
// exactly one feeder (claude-code's SessionStart hook), so every
// hookless client used to resolve tool="" and silently skip
// [profiles.by_tool] / tool: experiment classes (deviation D20,
// docs/plans/usability-review-2026-06-10.md §10.2). Rows below are the
// signals each client was VERIFIED to send (installed-bundle string
// extraction, 2026-06-12; codex corroborated by the live 0.133 wire
// capture in docs/observer-platform-issues-v4.md V4-4):
//
//   - claude-code ≥2.1: UA "claude-cli/<ver> …" (plus x-app: cli). The
//     bridge normally wins for claude-code; this row is the safety net
//     for installs without registered hooks. Known mimic: pi spoofs
//     the claude-cli UA for subscription auth — inert, since the
//     anthropic provider tier resolves the claude-code profile anyway.
//   - codex (codex-rs): UA "codex_cli_rs/<ver> (…)" and an
//     "Originator: codex_cli_rs" header on both auth paths.
//   - kilo CLI (@kilocode/cli): gateway-provider requests carry
//     "X-Title: Kilo Code" (no custom UA — Bun's default); the
//     anthropic-direct path sets UA "Kilo-Code/<ver>". Kilo is an
//     opencode fork that still ships opencode-UA code paths, so kilo
//     rows MUST sort before opencode rows (first hit wins).
//   - opencode: provider headers "X-Title: opencode" and UA
//     "opencode/<ver>" on its own fetches.
//
// Deliberately absent: cline-cli sends only stock SDK UAs
// (anthropic-sdk-typescript/axios — nothing distinctive), so it keeps
// degrading to the per-provider tier until a cline hook feeder ships;
// copilot-cli, cursor, and hermes talk to their own backends and never
// reach the proxy's upstreams.
var toolSignatures = []toolSignature{
	{header: "User-Agent", prefix: "claude-cli/", tool: models.ToolClaudeCode},
	{header: "User-Agent", prefix: "codex_cli_rs/", tool: models.ToolCodex},
	{header: "Originator", prefix: "codex", tool: models.ToolCodex},
	{header: "User-Agent", prefix: "Kilo-Code/", tool: models.ToolKiloCodeCLI},
	{header: "X-Title", prefix: "Kilo Code", tool: models.ToolKiloCodeCLI},
	{header: "User-Agent", prefix: "opencode/", tool: models.ToolOpenCode},
	{header: "X-Title", prefix: "opencode", tool: models.ToolOpenCode},
}

// toolFromHeaders resolves a request's owning AI tool from the
// signature table. It is consulted by requestClass ONLY after the
// pidbridge resolver missed — hook-fed bridge identity always wins —
// and a miss here ("", false) leaves the class degrading to the
// per-provider assignment tier, exactly like a bridge miss today.
func toolFromHeaders(h http.Header) (string, bool) {
	for _, sig := range toolSignatures {
		v := h.Get(sig.header)
		if len(v) < len(sig.prefix) {
			continue
		}
		if strings.EqualFold(v[:len(sig.prefix)], sig.prefix) {
			return sig.tool, true
		}
	}
	return "", false
}
