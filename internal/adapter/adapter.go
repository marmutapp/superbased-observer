package adapter

import (
	"context"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// Adapter turns one AI coding tool's session data into normalized ToolEvents
// and TokenEvents. Implementations live in internal/adapter/<name>.
//
// Implementations MUST:
//   - Scrub raw tool inputs before returning — no secrets escape the adapter.
//   - Emit deterministic SourceEventID values so re-parsing is idempotent.
//   - Advance the returned offset past the last fully-parsed event, so that
//     the caller can persist it to parse_cursors and skip on next call.
type Adapter interface {
	// Name is a stable identifier stored in the `tool` column. It must be
	// one of the models.Tool* constants.
	Name() string

	// WatchPaths returns absolute directories the watcher should monitor
	// for new or changed session files. Paths that do not exist are skipped
	// at registry time — adapters should return their canonical path
	// regardless of installed state.
	WatchPaths() []string

	// IsSessionFile filters fsnotify events. Returns true if path is a
	// session file this adapter should parse.
	IsSessionFile(path string) bool

	// ParseSessionFile parses path from fromOffset to EOF, returning the
	// produced events and the new byte offset to persist. Malformed lines
	// are skipped, not fatal — adapters advance past them so repeated calls
	// make progress.
	ParseSessionFile(ctx context.Context, path string, fromOffset int64) (ParseResult, error)
}

// ParseResult is the value returned by Adapter.ParseSessionFile.
type ParseResult struct {
	ToolEvents  []models.ToolEvent
	TokenEvents []models.TokenEvent
	// CacheObservations carries per-assistant-turn cache-relevant
	// views (Tier-2, transcript-derived) emitted by adapters that
	// can see content blocks + usage envelopes. Populated by the
	// claudecode adapter in C7 of the cachetrack arc (docs/plans/
	// cache-tracking-implementation-spec-2026-06-08.md §9). Tier-1
	// rollout adds codex / opencode / kilo-cli / cline-cli per
	// §14.3. Additive — adapters that don't populate it leave it
	// nil; the watcher / store pass-through silently no-ops on an
	// empty slice.
	CacheObservations []models.CacheTurnObservation
	// NewOffset is the byte offset to persist in parse_cursors. Subsequent
	// calls with this value as fromOffset will resume from the next
	// unparsed byte.
	NewOffset int64
	// Warnings carries non-fatal messages (malformed lines, unknown tool
	// names, etc.) for logging. They do not prevent progress.
	Warnings []string
	// RetrySuggested asks the watcher to keep the file on the poll
	// loop even when NewOffset == fromOffset (no advance). Adapters
	// set this when a transient miss may resolve on a later attempt —
	// e.g. a fresh Antigravity-CLI .pb file whose decrypt secret
	// hasn't been bootstrapped or whose embedded gRPC server is still
	// booting. Adapters are responsible for the freshness gate: set
	// only when retry has a plausible chance of succeeding. The
	// watcher writes the cursor (with MAX semantics) so the row stays
	// visible to pollCursors, which then refires processFile on the
	// next tick.
	RetrySuggested bool
}
