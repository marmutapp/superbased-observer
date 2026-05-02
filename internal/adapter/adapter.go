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
	// NewOffset is the byte offset to persist in parse_cursors. Subsequent
	// calls with this value as fromOffset will resume from the next
	// unparsed byte.
	NewOffset int64
	// Warnings carries non-fatal messages (malformed lines, unknown tool
	// names, etc.) for logging. They do not prevent progress.
	Warnings []string
}
