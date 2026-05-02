package watcher

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/marmutapp/superbased-observer/internal/adapter"
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
	return &Watcher{
		store:           s,
		registry:        r,
		logger:          opts.Logger,
		nativePredicate: opts.NativePredicate,
		allow:           opts.Allow,
		debounce:        opts.Debounce,
		pollInterval:    pollInterval,
		classifier:      opts.Classifier,
		indexer:         opts.Indexer,
	}
}

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
				if err := w.processFile(ctx, a, path, forceFromZero); err != nil {
					res.Errors++
					w.logger.Warn("watcher.Scan: process failed",
						"adapter", a.Name(), "path", path, "err", err)
					return nil
				}
				res.FilesProcessed++
				return nil
			})
			if walkErr != nil && walkErr != ctx.Err() {
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
func (w *Watcher) Watch(ctx context.Context) error {
	detected := w.registry.Detected(w.allow)
	if len(detected) == 0 {
		w.logger.Info("watcher.Watch: no adapters detected — entering idle mode")
		<-ctx.Done()
		return nil
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("watcher.Watch: fsnotify.NewWatcher: %w", err)
	}
	defer fsw.Close()

	// Map root → adapter, so events can be dispatched.
	byRoot := map[string]adapter.Adapter{}
	for _, a := range detected {
		for _, root := range a.WatchPaths() {
			if err := addRecursive(fsw, root); err != nil {
				w.logger.Warn("watcher.Watch: add path", "root", root, "err", err)
				continue
			}
			byRoot[root] = a
		}
	}

	// Initial scan, so existing files are caught up before watching.
	if _, err := w.Scan(ctx); err != nil {
		return err
	}

	// Polling fallback. fsnotify is documented to drop events on busy or
	// virtualized filesystems (e.g. WSL2 reading from a Windows NTFS
	// mount, network FUSE mounts). When that happens for a Write, the
	// debounced fire never trips and the watcher silently sits behind a
	// growing JSONL until the user clicks Run All. The poller is the
	// safety net: every tick re-checks every known parse_cursors row,
	// and every 15th tick re-scans the watch roots to discover never-
	// seen files (Create-event drops are the same bug class).
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
		pending   = map[debounceKey]*time.Timer{}
		pendingMu sync.Mutex
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
			a := adapterForPath(byRoot, ev.Name)
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
			pendingMu.Unlock()
		}
	}
}

// processFile reads the saved cursor, parses the file, ingests the events,
// and persists the new cursor — all inside a single logical unit. When
// forceFromZero is true the cursor is ignored (Rescan path) and parsing
// starts at offset 0; the post-parse cursor update still uses MAX(),
// so a re-scan can never regress an advanced cursor.
func (w *Watcher) processFile(ctx context.Context, a adapter.Adapter, path string, forceFromZero bool) error {
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
		w.logger.Warn("adapter warning", "adapter", a.Name(), "path", path, "msg", msg)
	}
	native := w.nativePredicate[a.Name()]
	if native == nil {
		native = func(string) bool { return false }
	}
	if _, err := w.store.Ingest(ctx, res.ToolEvents, res.TokenEvents, store.IngestOptions{
		IsNativeTool:   native,
		Classifier:     w.classifier,
		RecordFailures: true,
		Indexer:        w.indexer,
	}); err != nil {
		return err
	}
	if res.NewOffset > off {
		if err := w.store.SetCursor(ctx, path, res.NewOffset); err != nil {
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
			// No current adapter recognises this path (orphan
			// parse_cursors row from a tightened IsSessionFile).
			// Same exclusion rule the health endpoint uses.
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

// adapterFor returns the registered adapter whose IsSessionFile claims
// path, or nil if none does. Used by the poller to dispatch a per-file
// reprocess without re-walking the watch roots.
func (w *Watcher) adapterFor(path string) adapter.Adapter {
	for _, a := range w.registry.Detected(w.allow) {
		if a.IsSessionFile(path) {
			return a
		}
	}
	return nil
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

func hasPathPrefix(p, prefix string) bool {
	absP, _ := filepath.Abs(p)
	absPrefix, _ := filepath.Abs(prefix)
	if absP == absPrefix {
		return true
	}
	// Ensure prefix boundary.
	if len(absP) > len(absPrefix) && absP[:len(absPrefix)] == absPrefix && absP[len(absPrefix)] == filepath.Separator {
		return true
	}
	return false
}

// addRecursive adds root and every subdirectory to the fsnotify watcher.
// Non-existent paths return nil — callers check detection separately.
func addRecursive(fsw *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
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
