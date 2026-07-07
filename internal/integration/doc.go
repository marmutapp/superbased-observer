// Package integration is the adapter Integration Capability Registry —
// the single, table-driven source of truth for what each AI-tool adapter
// can integrate with: a proxy route, a hook-registration mechanism, an
// MCP config target, vendor native-console rails, and its token/cost
// capture tier.
//
// It exists to kill a recurring anti-pattern. Before it, `observer init`
// (cmd/observer/init.go::wireAIClients), internal/hook/register.go, and
// the MCP-config writer each carried their OWN hardcoded 3-adapter switch
// (claude-code / codex / cursor), so every cross-cutting capability was a
// "two-or-three-tool club" and adding an adapter meant editing five files.
// That violates the project's anti-spaghetti rules #3 (branch on
// capabilities, never on source identity) and #5 (decision logic is
// table-driven). This package is the capability table; the writers at the
// boundary (cmd/observer, internal/hook, internal/diag) iterate it and
// dispatch on capability SHAPE, not on tool name.
//
// Design rules (CLAUDE.md module boundaries):
//   - Pure data + lookups. NO database/sql, net/http, or fsnotify — the
//     consumers own all I/O.
//   - Additive (rule #6): new capability fields must not force changes in
//     unrelated consumers; For returns a zero-value-safe Capability.
//   - One owner: this is THE registry. It subsumes the former
//     internal/diag.routableTools proxy-route table; nothing else should
//     re-declare per-tool integration capabilities.
//
// The registry is filled incrementally across the adapter-coverage-parity
// phases (docs/plans/adapter-coverage-parity-plan-2026-06-26.md): Phase 0
// seeds the proxy-route capability (migrated from routableTools); later
// phases populate Hook, MCP, Native, and TokenTier.
package integration
