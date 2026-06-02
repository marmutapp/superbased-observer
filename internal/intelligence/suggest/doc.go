// Package suggest composes derived patterns (hot files, common commands,
// edit→test, onboarding sequences) and learn rules into a single
// instruction-file body that `observer suggest` writes to CLAUDE.md /
// AGENTS.md / .cursorrules.
//
// The managed-block marker differs from learn's — `suggest` and `learn`
// maintain independent blocks and can coexist in the same file.
package suggest
