// Package turnmerge is the pure dedup core for native-console integrations
// (docs/native-console-integration-template.md, Phase 1).
//
// When a single API turn is observed by more than one source — the proxy
// (exact), a provider's native telemetry (exact, e.g. Claude Code OTel), and
// the JSONL usage envelope (approximate) — those observations must collapse
// into ONE row keyed by request_id, never duplicate. This package owns the
// precedence decision: given the existing row and an incoming observation, it
// returns the merged row and whether to Insert, Update, or do nothing.
//
// It is deliberately free of any I/O — no database/sql, no models import — so
// the merge contract can be exhaustively unit-tested with synthetic rows and
// reused from the store seam (which maps models.APITurn <-> turnmerge.Turn at
// the boundary). Fidelity is assigned AT THE BOUNDARY from each source's
// capabilities; this package never branches on a tool/source string (CLAUDE.md
// module rule #3). The per-field rule set is a data table walked top-down
// (rule #5), not a conditional ladder.
package turnmerge
