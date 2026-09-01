package watcher

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/cachetrack"
	"github.com/marmutapp/superbased-observer/internal/compression/indexing"
	"github.com/marmutapp/superbased-observer/internal/freshness"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// Logger is the subset of slog.Logger used by the watcher. Satisfied by
// *slog.Logger.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// Watcher drives session-file ingestion for every registered adapter, both
// one-shot (Scan) and continuous (Watch).
type Watcher struct {
	store    *store.Store
	registry *adapter.Registry
	logger   Logger
	// nativePredicate maps an adapter name to its native-tool predicate
	// used for setting actions.is_native_tool.
	nativePredicate map[string]func(rawToolName string) bool
	// allow restricts which adapter names are active; empty means all.
	allow []string
	// debounce delays re-parsing of a file after a fsnotify event to coalesce
	// bursts of writes.
	debounce time.Duration
	// pollInterval, when > 0, drives the polling fallback that recovers
	// from fsnotify Write/Create events dropped on busy filesystems
	// (notably WSL2/NTFS). Zero disables polling — fsnotify is the only
	// trigger.
	pollInterval time.Duration
	// classifier, when non-nil, is passed to store.Ingest so file-typed
	// actions get content_hash + freshness computed.
	classifier *freshness.Classifier
	// indexer, when non-nil, stores tool output excerpts in FTS5.
	indexer *indexing.Indexer
	// fileLocks serializes concurrent processFile invocations per
	// source_file. fsnotify-debounced fires and poller-tick fires CAN
	// race on the same file (documented "race is safe via UNIQUE
	// constraints"), but the loser of that race wastes a full
	// BEGIN IMMEDIATE acquisition cycle and — when the holder is slow
	// on lossy filesystems (WSL2 /mnt/c, OneDrive-synced dirs) —
	// can trip SQLITE_BUSY. Per-file locking eliminates the intra-file
	// race; inter-file contention is still handled by SQLite's
	// busy_timeout backoff.
	fileLocks sync.Map // map[string]*sync.Mutex
	// warningDedup suppresses repeated adapter warnings with the same
	// (adapter, path, message) tuple within a TTL window. The
	// antigravity adapter, on Windows, emits the same OSCrypt /
	// unrecoverable warning every ~30 s poll for the lifetime of an
	// untouched .pb file — at ~96% of stderr volume it drowns real
	// diagnostics (V3 batch finding). With a 5-minute window the same
	// signal still surfaces, just not every poll. nil disables dedup
	// (every warning fires).
	warningDedup *warningDeduper
	// maxFileBytes skips parsing files larger than this (DoS guard). Zero
	// disables the cap. See Options.MaxFileBytes.
	maxFileBytes int64
	// skipModifiedBefore gates the historic scan's WalkDir callback.
	// See Options.SkipModifiedBefore.
	skipModifiedBefore time.Time

	// Live fsnotify state, owned by Watch and mutated by RefreshRoots /
	// applyDetectedRoots under liveMu. fsw is non-nil only while Watch is
	// inside its event loop; RefreshRoots is a no-op hot-add when Watch
	// has not started yet (Scan still runs).
	liveMu    sync.Mutex
	fsw       *fsnotify.Watcher
	byRoot    map[string]adapter.Adapter
	refreshCh chan struct{} // buffered 1; nudges Watch to re-apply + Scan
}

// Options configures New.
type Options struct {
	Logger          Logger
	NativePredicate map[string]func(string) bool
	Allow           []string
	Debounce        time.Duration
	// PollInterval, when > 0, runs a polling fallback alongside fsnotify
	// in Watch — every tick, every known parse_cursors row is stat()'d
	// and reprocessed if file_size > byte_offset; every 15th tick a full
	// Scan walks the watch roots to discover never-seen files. Recovers
	// from fsnotify event drops on WSL2/NTFS and other lossy
	// filesystems. Zero (or negative) disables polling entirely.
	PollInterval time.Duration
	// Classifier is optional; when set, file-typed actions gain freshness
	// classification and the file_state table is updated.
	Classifier *freshness.Classifier
	// Indexer is optional; when set, tool output excerpts go into FTS5
	// action_excerpts.
	Indexer *indexing.Indexer
	// AdapterWarningTTL is the dedup window for adapter warnings logged
	// from processFile. Identical (adapter, path, message) tuples are
	// suppressed until the TTL expires. Zero uses
	// defaultAdapterWarningTTL (5 min); a negative value disables dedup.
	// See V3-3 in docs/observer-platform-issues-v3.md.
	AdapterWarningTTL time.Duration
	// MaxFileBytes skips parsing any session file larger than this many
	// bytes — a DoS guard against a malformed or hostile multi-GB file in
	// a watch root driving the daemon into unbounded allocation. Zero (or
	// negative) disables the cap. Wired from [watcher].max_file_bytes.
	MaxFileBytes int64
	// SkipModifiedBefore, when non-zero, makes the historic scan (Scan /
	// Rescan) skip any session file whose mtime is strictly before this
	// instant — a file whose last write predates the window cannot
	// contain any in-window rows, so parsing it is pure waste. This is
	// honored ONLY in scan()'s WalkDir callback, using the fs.DirEntry
	// the walk already handed us (no extra os.Stat on the hit path) for
	// a regular file; a symlink entry costs one extra os.Stat to reach
	// the TARGET's mtime, since processFile follows safe symlinks and
	// parses the target, not the link. The continuous Watch()/fsnotify/
	// poller path is untouched — a live
	// write always bumps mtime to "now", so the filter would never fire
	// there anyway, and Watch's own initial Scan call still benefits.
	// The zero value (time.Time{}) changes nothing: every file is
	// processed exactly as before this field existed.
	SkipModifiedBefore time.Time
}

// New returns a watcher. Reasonable zero defaults apply.
func New(s *store.Store, r *adapter.Registry, opts Options) *Watcher {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Debounce <= 0 {
		opts.Debounce = 300 * time.Millisecond
	}
	if opts.NativePredicate == nil {
		opts.NativePredicate = map[string]func(string) bool{}
	}
	pollInterval := opts.PollInterval
	if pollInterval < 0 {
		pollInterval = 0
	}
	warningTTL := opts.AdapterWarningTTL
	if warningTTL == 0 {
		warningTTL = defaultAdapterWarningTTL
	}
	// A negative TTL disables dedup. newWarningDeduper handles
	// non-positive values as "always allow", so a single deduper
	// instance covers both branches.
	return &Watcher{
		store:              s,
		registry:           r,
		logger:             opts.Logger,
		nativePredicate:    opts.NativePredicate,
		allow:              opts.Allow,
		debounce:           opts.Debounce,
		pollInterval:       pollInterval,
		classifier:         opts.Classifier,
		indexer:            opts.Indexer,
		warningDedup:       newWarningDeduper(warningTTL),
		maxFileBytes:       opts.MaxFileBytes,
		skipModifiedBefore: opts.SkipModifiedBefore,
		refreshCh:          make(chan struct{}, 1),
	}
}

// RefreshRoots re-runs adapter detection, hot-adds any newly-existing
// watch roots into the live fsnotify set, and performs an immediate Scan.
//
// This is the seam behind the New Terminal install→launch capture gap:
// tools installed while the daemon is already running (Muse, Prime Agent,
// and any other adapter whose sessions directory appears only after first
// use) were invisible to Watch because detection + addRecursive ran once
// at start. Callers (dashboard launch/install) invoke this fail-open;
// repeated calls are cheap and idempotent.
//
// When Watch is not yet running, hot-add is skipped and only Scan runs
// (Detected roots that exist are still walked). A pending refresh signal
// is still queued so the next Watch loop iteration applies roots as soon
// as fsnotify is live.
func (w *Watcher) RefreshRoots(ctx context.Context) (ScanResult, error) {
	added := w.applyDetectedRoots()
	if added > 0 {
		w.logger.Info("watcher.RefreshRoots: hot-added watch roots", "count", added)
	}
	select {
	case w.refreshCh <- struct{}{}:
	default:
	}
	return w.Scan(ctx)
}

// applyDetectedRoots adds every currently-Detected adapter root that is
// not already in byRoot to the live fsnotify watcher. Returns the number
// of newly added roots. No-op when Watch is not running (fsw == nil).
func (w *Watcher) applyDetectedRoots() int {
	w.liveMu.Lock()
	defer w.liveMu.Unlock()
	if w.fsw == nil || w.byRoot == nil {
		return 0
	}
	added := 0
	for _, a := range w.registry.Detected(w.allow) {
		for _, root := range a.WatchPaths() {
			if root == "" {
				continue
			}
			if _, ok := w.byRoot[root]; ok {
				continue
			}
			if err := addRecursive(w.fsw, root); err != nil {
				w.logger.Warn("watcher.applyDetectedRoots: add path",
					"adapter", a.Name(), "root", root, "err", err)
				continue
			}
			w.byRoot[root] = a
			added++
			w.logger.Info("watcher: hot-added watch root",
				"adapter", a.Name(), "root", root)
		}
	}
	return added
}

// snapshotByRoot returns a copy of the live root→adapter map for
// lock-free event dispatch.
func (w *Watcher) snapshotByRoot() map[string]adapter.Adapter {
	w.liveMu.Lock()
	defer w.liveMu.Unlock()
	out := make(map[string]adapter.Adapter, len(w.byRoot))
	for k, v := range w.byRoot {
		out[k] = v
	}
	return out
}

// SetCacheEngine wires the per-process cachetrack.Engine through to the
// watcher's store so Tier-2 (transcript) cache observations are recorded.
// The daemon passes the SAME instance the proxy uses (Proxy.CacheEngine)
// so both feed paths advance one shared CacheModel state; cross-tier
// dedup (CacheEventExistsForMessage) keeps a message from being observed
// twice. Until this is called the watcher's store has a nil engine and
// drops every transcript cache observation — which is why non-proxied
// sessions had no cache_entries. Idempotent; nil is a no-op disable.
func (w *Watcher) SetCacheEngine(e *cachetrack.Engine) { w.store.SetCacheEngine(e) }

// Scan walks every detected adapter's watch paths once, parsing every
// session file from its saved offset. Returns the total number of newly
// inserted actions + a count of errors (non-fatal).
func (w *Watcher) Scan(ctx context.Context) (ScanResult, error) {
	return w.scan(ctx, false)
}

// Rescan is Scan with the saved cursor ignored — every JSONL is parsed
// from offset 0 again. The (source_file, source_event_id) UNIQUE index
// makes ingest idempotent, so re-walking is safe; rows that already
// exist are no-ops, rows the watcher dropped silently get inserted.
//
// Surfaced via `observer scan --force` and the dashboard's "Run All"
// button. Recovery path for the well-known watcher-falls-behind
// failure mode: parse_cursors stuck at offset N while the JSONL has
// grown past N, and only some action types (typically user_prompts
// via a separate path) get ingested while assistant turns + tool
// calls go missing.
func (w *Watcher) Rescan(ctx context.Context) (ScanResult, error) {
	return w.scan(ctx, true)
}

func (w *Watcher) scan(ctx context.Context, forceFromZero bool) (ScanResult, error) {
	var res ScanResult
	detected := w.registry.Detected(w.allow)
	if len(detected) == 0 {
		w.logger.Info("watcher.Scan: no adapters detected — nothing to do")
		return res, nil
	}
	for _, a := range detected {
		for _, root := range a.WatchPaths() {
			walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if err != nil {
					// Missing roots are not fatal — skip.
					return nil
				}
				if d.IsDir() {
					return nil
				}
				if !a.IsSessionFile(path) {
					return nil
				}
				if !w.skipModifiedBefore.IsZero() {
					// A file whose last write predates the window
					// cannot contain any in-window rows — skip it
					// without ever opening it. Use the fs.DirEntry
					// info the walk already fetched (no extra
					// os.Stat on the hit path) — EXCEPT for a
					// symlink: WalkDir's DirEntry is built from
					// Lstat, so d.Info() reports the LINK's own
					// mtime, while processFile follows safe symlinks
					// and parses the TARGET. An old symlink pointing
					// at a freshly-updated session file must not be
					// skipped on the link's stale mtime, so for a
					// symlink entry we os.Stat(path) instead, which
					// follows the link to the target's mtime. If
					// either stat errors (e.g. a raced deletion or a
					// broken symlink), fall through and let
					// processFile's own os.Stat/open handle it —
					// fail-open, never fail-skip.
					info, infoErr := d.Info()
					if infoErr == nil && d.Type()&fs.ModeSymlink != 0 {
						info, infoErr = os.Stat(path)
					}
					if infoErr == nil && info.ModTime().Before(w.skipModifiedBefore) {
						return nil
					}
				}
				if err := w.processFile(ctx, a, path, forceFromZero); err != nil {
					res.Errors++
					w.logger.Warn("watcher.Scan: process failed",
						"adapter", a.Name(), "path", path, "err", err)
					return nil
				}
				res.FilesProcessed++
				return nil
			})
			if walkErr != nil && !errors.Is(walkErr, ctx.Err()) {
				w.logger.Warn("watcher.Scan: walk failed",
					"adapter", a.Name(), "root", root, "err", walkErr)
			}
		}
	}
	return res, nil
}

// ScanResult is the summary of a scan invocation.
type ScanResult struct {
	FilesProcessed int
	Errors         int
}

// Watch starts an fsnotify watch on every detected adapter's roots (plus an
// initial Scan) and keeps ingesting until ctx is cancelled.
//
// Unlike the pre-RefreshRoots idle path, an empty Detected set at start no
// longer parks forever on ctx.Done: the event loop + poller always run so
// RefreshRoots (and the poller's periodic applyDetectedRoots) can hot-add
// roots when a tool is installed after the daemon starts. An empty
// enabled_adapters allow-list still yields nothing to watch — Detected
// stays empty — so ephemeral/benchmark configs that pin `[]` remain safe.
func (w *Watcher) Watch(ctx context.Context) error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("watcher.Watch: fsnotify.NewWatcher: %w", err)
	}
	defer func() {
		w.liveMu.Lock()
		w.fsw = nil
		w.byRoot = nil
		w.liveMu.Unlock()
		_ = fsw.Close()
	}()

	w.liveMu.Lock()
	w.fsw = fsw
	w.byRoot = map[string]adapter.Adapter{}
	w.liveMu.Unlock()

	if n := w.applyDetectedRoots(); n == 0 {
		w.logger.Info("watcher.Watch: no adapters detected yet — waiting for RefreshRoots / poller re-detect")
	}

	// Initial scan, so existing files are caught up before watching.
	if _, err := w.Scan(ctx); err != nil {
		return err
	}

	// Drain any RefreshRoots signal that arrived before fsw was live so
	// we don't leave a stale nudge that would only re-Scan once.
	select {
	case <-w.refreshCh:
		_ = w.applyDetectedRoots()
		if _, err := w.Scan(ctx); err != nil {
			return err
		}
	default:
	}

	// Polling fallback. fsnotify is documented to drop events on busy or
	// virtualized filesystems (e.g. WSL2 reading from a Windows NTFS
	// mount, network FUSE mounts). When that happens for a Write, the
	// debounced fire never trips and the watcher silently sits behind a
	// growing JSONL until the user clicks Run All. The poller is the
	// safety net: every tick re-checks every known parse_cursors row,
	// and every 15th tick re-scans the watch roots to discover never-
	// seen files (Create-event drops are the same bug class) AND
	// applyDetectedRoots so a tool installed mid-daemon is eventually
	// fsnotify-wired even without a dashboard kick.
	//
	// processFile is idempotent (cursor + the (source_file,
	// source_event_id) UNIQUE index), so racing fsnotify and the poller
	// is safe.
	if w.pollInterval > 0 {
		go w.runPoller(ctx)
	}

	type debounceKey struct {
		path string
	}
	var (
		pending       = map[debounceKey]*time.Timer{}
		pendingSettle = map[debounceKey]*time.Timer{}
		pendingMu     sync.Mutex
	)
	fire := func(a adapter.Adapter, path string) {
		if err := w.processFile(ctx, a, path, false); err != nil {
			w.logger.Warn("watcher.Watch: process failed",
				"adapter", a.Name(), "path", path, "err", err)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-w.refreshCh:
			_ = w.applyDetectedRoots()
			if _, err := w.Scan(ctx); err != nil && ctx.Err() == nil {
				w.logger.Warn("watcher.Watch: refresh scan failed", "err", err)
			}
		case err, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			w.logger.Warn("watcher.Watch: fsnotify error", "err", err)
		case ev, ok := <-fsw.Events:
			if !ok {
				return nil
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create) == 0 {
				continue
			}
			a := adapterForPath(w.snapshotByRoot(), ev.Name)
			if a == nil || !a.IsSessionFile(ev.Name) {
				// New directory created under a watched root? Try to watch it.
				if ev.Op&fsnotify.Create != 0 {
					_ = addIfDir(fsw, ev.Name)
				}
				continue
			}
			k := debounceKey{path: ev.Name}
			pendingMu.Lock()
			if t, ok := pending[k]; ok {
				t.Stop()
			}
			aLocal, pathLocal := a, ev.Name
			pending[k] = time.AfterFunc(w.debounce, func() {
				pendingMu.Lock()
				delete(pending, k)
				pendingMu.Unlock()
				fire(aLocal, pathLocal)
			})
			// Some tools create the session file early, then append the
			// interesting tail (token_count, task_complete, tool output)
			// a moment later. On Windows/NTFS those follow-up Write
			// events can go missing in practice, leaving the cursor stuck
			// at a partial file until the next full rescan. A second,
			// longer debounce gives each touched session file one more
			// parse pass after writes have settled, even if the OS never
			// delivers another event.
			const settleDelay = 2 * time.Second
			if t, ok := pendingSettle[k]; ok {
				t.Stop()
			}
			pendingSettle[k] = time.AfterFunc(settleDelay, func() {
				pendingMu.Lock()
				delete(pendingSettle, k)
				pendingMu.Unlock()
				fire(aLocal, pathLocal)
			})
			pendingMu.Unlock()
		}
	}
}

// processFile reads the saved cursor, parses the file, ingests the events,
// and persists the new cursor — all inside a single logical unit. When
// forceFromZero is true the cursor is ignored (Rescan path) and parsing
// starts at offset 0; the post-parse cursor update still uses MAX(),
// so a re-scan can never regress an advanced cursor.
// symlinkLeafEscapes reports whether path is a symlink whose resolved target
// lies outside every one of roots. Each root is resolved too, so a legitimately
// symlinked watch root still matches its own files. Regular files and symlinks
// that resolve back inside a watch root return false (the fast path); a
// dangling or looping symlink returns true (refuse). The returned string is the
// resolved target, for the skip log.
func symlinkLeafEscapes(path string, roots []string) (bool, string) {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return false, "" // regular file (or vanished) — unchanged behaviour.
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return true, "" // dangling / looping symlink — refuse.
	}
	for _, r := range roots {
		if r == "" {
			continue
		}
		resolvedRoot := r
		if rr, err := filepath.EvalSymlinks(r); err == nil {
			resolvedRoot = rr
		}
		if adapter.HasPathPrefix(real, resolvedRoot) {
			return false, real // target stays inside a watch root — allow.
		}
	}
	return true, real
}

func (w *Watcher) processFile(ctx context.Context, a adapter.Adapter, path string, forceFromZero bool) error {
	// Refuse a symlink whose target escapes the watch root before opening it.
	// fsnotify + WalkDir only gate on the path PREFIX (HasPathPrefix does not
	// resolve symlinks, by design — a watch root may itself be a symlink), so a
	// symlink planted under a watched dir (~/.claude/projects/x.jsonl ->
	// /etc/passwd) would otherwise be opened and its contents excerpted into
	// the DB. Regular files and in-tree symlinks take the fast path unchanged.
	if escapes, real := symlinkLeafEscapes(path, a.WatchPaths()); escapes {
		w.logger.Warn("watcher.processFile: skipping symlink whose target escapes the watch root",
			"path", path, "resolved", real)
		return nil
	}

	// DoS guard: skip a file larger than the configured cap before parsing.
	// A malformed or hostile multi-GB session file in a watch root would
	// otherwise drive the adapter's whole-file read into unbounded allocation.
	if w.maxFileBytes > 0 {
		if fi, err := os.Stat(path); err == nil && fi.Size() > w.maxFileBytes {
			w.logger.Warn("watcher.processFile: skipping oversize file",
				"path", path, "size", fi.Size(), "max", w.maxFileBytes)
			return nil
		}
	}

	// Recover from a panic in adapter parsing. The daemon runs the proxy,
	// dashboard, and watcher in one process, so a single adapter panic on a
	// malformed file would otherwise crash all three. A recovered parse is
	// logged and the file skipped; the watcher keeps running.
	defer func() {
		if r := recover(); r != nil {
			w.logger.Warn("watcher.processFile: recovered from adapter panic",
				"path", path, "adapter", a.Name(), "panic", r)
		}
	}()

	// Serialize concurrent processFile invocations for the same file.
	// fsnotify-debounced fires and poller-tick fires can race on the
	// same source_file; without a per-file lock the loser holds a
	// pending BEGIN IMMEDIATE acquisition while the winner ingests,
	// which on slow filesystems (WSL2 /mnt/c, OneDrive-synced dirs)
	// can exceed the 30s busy_timeout and trip SQLITE_BUSY. The lock
	// is cheap (~150ns per acquire) and outlives the call only as
	// long as it's contended; sync.Map auto-cleans nothing but the
	// per-path mutex value is ~24 bytes — leak is bounded by the
	// number of distinct session files the daemon ever observed.
	muRaw, _ := w.fileLocks.LoadOrStore(path, &sync.Mutex{})
	mu := muRaw.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	var off int64
	if !forceFromZero {
		var err error
		off, err = w.store.GetCursor(ctx, path)
		if err != nil {
			return err
		}
	}
	res, err := a.ParseSessionFile(ctx, path, off)
	if err != nil {
		return err
	}
	for _, msg := range res.Warnings {
		// Dedup identical (adapter, path, message) within the TTL —
		// otherwise antigravity's OSCrypt-retrieval warnings (and any
		// other adapter that emits the same warning every poll) flood
		// stderr and drown the signal. The first occurrence per TTL
		// window still fires at WARN; suppressed repeats are dropped
		// silently. See V3-3.
		//
		// M4.5 of the 2026-06-02 teams test follow-ups: antigravity
		// decrypt failures all share the same root cause (the documented
		// AES-128-CTR scheme doesn't match the Windows-side cipher) and
		// the initial-scan burst can produce hundreds of lines per
		// `observer start`. Collapse them to a single dedup entry by
		// dropping the per-path component of the key for that family.
		// The first decrypt-failure warning per TTL window fires;
		// subsequent decrypt-failure warnings (any file, any path) are
		// suppressed until the window expires.
		key := a.Name() + "|" + path + "|" + msg
		if isAntigravityDecryptFailure(a.Name(), msg) {
			key = a.Name() + "|<decrypt-failure-batch>|" + extractDecryptFailureFamily(msg)
		}
		if !w.warningDedup.Allow(key) {
			continue
		}
		w.logger.Warn("adapter warning", "adapter", a.Name(), "path", path, "msg", msg)
	}
	native := w.nativePredicate[a.Name()]
	if native == nil {
		native = func(string) bool { return false }
	}
	if _, err := w.store.Ingest(ctx, res.ToolEvents, res.TokenEvents, store.IngestOptions{
		IsNativeTool:        native,
		Classifier:          w.classifier,
		RecordFailures:      true,
		Indexer:             w.indexer,
		CacheObservations:   res.CacheObservations,
		SessionProcessSeeds: res.SessionProcessSeeds,
		SessionLineages:     res.SessionLineages,
		OutcomeUpdates:      res.OutcomeUpdates,
	}); err != nil {
		return err
	}
	// Persist the cursor when:
	//   - the adapter advanced past the prior offset (normal progress), OR
	//   - the adapter explicitly asked to be re-polled (RetrySuggested)
	//     — without writing a cursor row, fresh files with no prior
	//     entry would never be picked up by pollCursors, so the retry
	//     hint would be silently dropped.
	// MAX(off, NewOffset) guards against accidental cursor regression
	// when RetrySuggested is set with NewOffset < off (e.g. an adapter
	// returning fromOffset on a transient miss).
	if res.NewOffset > off || res.RetrySuggested {
		target := res.NewOffset
		if off > target {
			target = off
		}
		if err := w.store.SetCursor(ctx, path, target); err != nil {
			return err
		}
	}
	return nil
}

// pollFullScanEvery is how many poll ticks pass between full-tree scans.
// At the default 2s tick that's one root walk every 30s — frequent
// enough to catch fsnotify Create drops within a minute, infrequent
// enough to keep the cost on deep adapter trees (codex
// sessions/YYYY/MM/DD/) negligible.
const pollFullScanEvery = 15

// runPoller drives the polling fallback inside Watch. Owns its own
// ticker so it never stalls the fsnotify select loop. Exits with ctx.
func (w *Watcher) runPoller(ctx context.Context) {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	tickCount := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tickCount++
			if tickCount%pollFullScanEvery == 0 {
				// Hot-add any roots that appeared since Watch started
				// (dashboard install, CLI install of a new tool, …)
				// before walking — otherwise Scan finds files but
				// live fsnotify still misses Create/Write on the new tree.
				_ = w.applyDetectedRoots()
				if _, err := w.Scan(ctx); err != nil && ctx.Err() == nil {
					w.logger.Warn("watcher.poll: full scan failed", "err", err)
				}
				continue
			}
			if err := w.pollCursors(ctx); err != nil && ctx.Err() == nil {
				w.logger.Warn("watcher.poll: cursor pass failed", "err", err)
			}
		}
	}
}

// pollCursors stats every known session file and re-runs processFile when
// the file has grown past the saved cursor. Cheap: one query +
// N stats. Logs at Info level only when a poll actually advanced a
// cursor, so steady-state polling produces no noise.
func (w *Watcher) pollCursors(ctx context.Context) error {
	cursors, err := w.store.ListCursors(ctx)
	if err != nil {
		return err
	}
	for _, c := range cursors {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fi, statErr := osStat(c.SourceFile)
		if statErr != nil {
			// File gone or unreadable. Not a poll concern — orphan
			// surfacing lives in the dashboard's health endpoint.
			continue
		}
		if fi.Size() <= c.ByteOffset {
			continue
		}
		a := w.adapterFor(c.SourceFile)
		if a == nil {
			// No current adapter owns this path (orphan
			// parse_cursors row from a tightened IsSessionFile,
			// or a stale row from a removed adapter). Same
			// exclusion rule the health endpoint uses.
			continue
		}
		// Defensive: root-based dispatch picked an adapter, but the
		// adapter's IsSessionFile must also accept the file (this
		// catches stray files placed inside a watch root that
		// shouldn't be ingested — e.g. README.md inside
		// ~/.codex/sessions). Skip + log so the operator sees the
		// mismatch instead of misrouting silently.
		if !a.IsSessionFile(c.SourceFile) {
			w.logger.Warn("watcher.poll: root-matched adapter rejected file shape",
				"adapter", a.Name(), "path", c.SourceFile)
			continue
		}
		behind := fi.Size() - c.ByteOffset
		if err := w.processFile(ctx, a, c.SourceFile, false); err != nil {
			w.logger.Warn("watcher.poll: process failed",
				"adapter", a.Name(), "path", c.SourceFile, "err", err)
			continue
		}
		w.logger.Info("watcher.poll: caught up dropped writes",
			"adapter", a.Name(), "path", c.SourceFile, "behind_bytes", behind)
	}
	return nil
}

// adapterFor returns the adapter whose WatchPaths contain path, or
// nil if none does. Used by the poller to dispatch a per-file
// reprocess without re-walking the watch roots.
//
// Pre-v1.4.51 this iterated registry.Detected(allow) and returned the
// first adapter whose IsSessionFile claimed path — pure shape-based
// dispatch. Because the registry sorts adapters by Name() and
// claude-code's IsSessionFile was a bare `.jsonl` extension match,
// any JSONL file (including Codex rollout-*.jsonl) ended up
// dispatched to claude-code first, silently misrouting + stranding
// token rows whenever fsnotify dropped a write event on WSL2/NTFS.
//
// Post-fix: dispatch uses longest-watched-root prefix — same rule
// the fsnotify event-handler path has always used (adapterForPath).
// The registry is still queried per call so dynamically-added
// adapters appear without restarting Watch.
func (w *Watcher) adapterFor(path string) adapter.Adapter {
	if byRoot := w.snapshotByRoot(); len(byRoot) > 0 {
		return adapterForPath(byRoot, path)
	}
	byRoot := map[string]adapter.Adapter{}
	for _, a := range w.registry.Detected(w.allow) {
		for _, root := range a.WatchPaths() {
			byRoot[root] = a
		}
	}
	return adapterForPath(byRoot, path)
}

// adapterForPath returns the adapter whose watched root is a prefix of path,
// or nil if none match.
func adapterForPath(byRoot map[string]adapter.Adapter, path string) adapter.Adapter {
	// Prefer the longest matching root.
	var (
		bestRoot string
		best     adapter.Adapter
	)
	for root, a := range byRoot {
		if hasPathPrefix(path, root) && len(root) > len(bestRoot) {
			bestRoot = root
			best = a
		}
	}
	return best
}

// hasPathPrefix delegates to adapter.HasPathPrefix — single source of
// truth for path-prefix semantics shared by the watcher and every
// adapter's IsSessionFile.
func hasPathPrefix(p, prefix string) bool {
	return adapter.HasPathPrefix(p, prefix)
}

// addRecursive adds root and every subdirectory to the fsnotify watcher.
// A root that is itself a FILE (aider's per-repo transcript paths) is
// watched directly. Non-existent paths return nil — callers check
// detection separately.
func addRecursive(fsw *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			// Only the root itself; non-root files are covered by their
			// parent directory's watch as usual.
			if path == root {
				return fsw.Add(path)
			}
			return nil
		}
		return fsw.Add(path)
	})
}

func addIfDir(fsw *fsnotify.Watcher, path string) error {
	fi, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	info, err := dirStat(fi)
	if err != nil || !info.IsDir() {
		return err
	}
	return fsw.Add(fi)
}

// dirStat is a wrapper over os.Stat used only so that the call site reads as
// a directory check — kept here to avoid importing os in the main body.
func dirStat(path string) (fs.FileInfo, error) {
	return osStat(path)
}
