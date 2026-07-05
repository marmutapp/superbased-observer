package hermes

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// Adapter is the file-watcher implementation of the Hermes adapter. It
// complements the hook-driven primary capture path: when the live hook
// plugin is not installed (or for historical sessions that predate
// install, or for `observer backfill --hermes`), the watcher scans
// `<home>/.hermes/state.db` and emits action rows derived from the
// messages.tool_calls JSON column.
//
// Hook + watcher rows share SessionID = sessions.id but carry
// distinct SourceFile values: hook rows "hermes:hook", watcher rows
// the absolute path to state.db. The (SourceFile, SourceEventID)
// UNIQUE index makes each path idempotent against itself but does
// NOT dedup across paths (the H1 audit finding) — that's the
// SessionHookChecker's job: for hook-covered sessions the watcher
// suppresses the classes both paths emit (tool_calls + the token
// aggregate) and keeps the SQLite-only classes (user_prompt,
// assistant text, system_prompt). See buildEvents.
//
// Coverage compared to the hook path:
//   - Activity (assistant tool_calls + tool results): covered.
//   - Session metadata (model, cwd, source, started/ended_at, costs):
//     covered via sessions table.
//   - Token usage: covered via sessions table aggregates (Tier 2,
//     approximate) — the hook path's post_api_request provides
//     per-API-call granularity, but both are approximate until a
//     proxy-route lands.
//   - Real-time: lags by polling interval (default 2s). Hook path
//     is real-time.
type Adapter struct {
	scrubber  *scrub.Scrubber
	roots     []string
	hookCheck SessionHookChecker
}

// WithSessionHookChecker injects the predicate the SQLite path
// consults to learn whether a session already has hook-emitted rows
// (SourceFile="hermes:hook"). Mirrors cursor.Adapter and
// clinecli.Adapter — same H1 cross-path dedup pattern, except hermes
// suppresses per event class rather than per session (the hook path
// carries no message-text callbacks, so user_prompt / assistant text
// / system_prompt must keep flowing from SQLite). Chainable.
func (a *Adapter) WithSessionHookChecker(check SessionHookChecker) *Adapter {
	a.hookCheck = check
	return a
}

// New returns an Adapter with platform-default cross-mount roots and a
// default scrubber.
func New() *Adapter {
	return &Adapter{
		scrubber: scrub.New(),
		roots:    defaultRoots(),
	}
}

// NewWithOptions customises scrubber and roots for tests. Pass nil
// scrubber for the default; pass nil/empty roots for default
// platform discovery.
func NewWithOptions(s *scrub.Scrubber, roots ...string) *Adapter {
	if s == nil {
		s = scrub.New()
	}
	if len(roots) == 0 {
		roots = defaultRoots()
	}
	return &Adapter{scrubber: s, roots: roots}
}

// Name implements adapter.Adapter.
func (*Adapter) Name() string { return models.ToolHermes }

// WatchPaths implements adapter.Adapter.
func (a *Adapter) WatchPaths() []string { return a.roots }

// IsSessionFile implements adapter.Adapter. Matches `state.db` and the
// SQLite WAL/SHM sidecars (`state.db-wal`, `state.db-shm`) — fsnotify
// fires on the WAL when Hermes writes, so the adapter needs to
// recognise those triggers and re-poll the main DB.
//
// Path-component string match, normalises backslashes so fixtures
// with foreign-OS separators still match on Linux CI.
func (a *Adapter) IsSessionFile(path string) bool {
	base := filepath.Base(path)
	switch base {
	case "state.db", "state.db-wal", "state.db-shm":
		// Belt-AND-braces against generic state.db files elsewhere on
		// disk: also require the parent directory to look like a
		// Hermes home (.hermes or AppData/Local/hermes).
		norm := strings.ToLower(strings.ReplaceAll(path, `\`, "/"))
		if !strings.Contains(norm, "/.hermes/") &&
			!strings.Contains(norm, "/appdata/local/hermes/") {
			return false
		}
		return adapter.UnderAnyWatchRoot(path, a.WatchPaths())
	default:
		return false
	}
}

// ParseSessionFile implements adapter.Adapter.
//
// Triggered by the watcher whenever fsnotify sees a change under any of
// `WatchPaths()` whose basename IsSessionFile accepts (state.db,
// state.db-wal, state.db-shm). All three trigger paths route through
// here; the SQLite reader only ever opens the main .db file so WAL
// writes amount to "poll again" semantics.
//
// The fromOffset int64 carries a Hermes-specific watermark — the
// last-seen messages.id from the previous successful scan. The watcher
// persists this verbatim to parse_cursors.offset, so subsequent calls
// resume at the next unprocessed row. messages.id is AUTOINCREMENT
// (monotonically increasing, never reused) which makes this dedup-
// safe even across observer restarts.
//
// Returns a no-op result (NewOffset = fromOffset, zero events) when
// the DB is empty or no messages past the watermark exist. Returns
// the existing fromOffset unchanged when scanStateDB errors — the
// watcher's RetrySuggested gate is not used here because Hermes
// transient locks resolve cleanly under the 2s busy_timeout, and a
// persistent error (corrupt DB, permissions) is best surfaced via
// the warning channel rather than retried indefinitely.
//
// WAL / SHM files are no-op: the basename matcher in IsSessionFile
// dispatches them here so the watcher knows to re-poll the main DB,
// but the parser only looks at the main file.
func (a *Adapter) ParseSessionFile(ctx context.Context, path string, fromOffset int64) (adapter.ParseResult, error) {
	// WAL / SHM events route here as a poll trigger; the SQLite open
	// always targets the main DB file.
	dbPath := path
	if base := filepath.Base(path); base == "state.db-wal" || base == "state.db-shm" {
		dbPath = strings.TrimSuffix(path, "-wal")
		dbPath = strings.TrimSuffix(dbPath, "-shm")
	}

	sessions, messages, maxID, err := scanStateDB(ctx, dbPath, fromOffset)
	if err != nil {
		return adapter.ParseResult{NewOffset: fromOffset}, fmt.Errorf("hermes.ParseSessionFile: %w", err)
	}
	if len(messages) == 0 {
		return adapter.ParseResult{NewOffset: maxID}, nil
	}

	toolEvents, tokenEvents, warnings := buildEvents(ctx, sessions, messages, dbPath, a.scrubber, a.hookCheck)
	return adapter.ParseResult{
		ToolEvents:  toolEvents,
		TokenEvents: tokenEvents,
		NewOffset:   maxID,
		Warnings:    warnings,
	}, nil
}
