package index

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/marmutapp/superbased-observer/internal/codeintel"
	"github.com/marmutapp/superbased-observer/internal/codeintel/parse"
	"github.com/marmutapp/superbased-observer/internal/codeintel/semantic"
)

// Options configures an Indexer. The CLI/daemon maps [codeintel] config
// onto this at the boundary so the index package never imports
// internal/config (keeps the orchestrator config-shape-agnostic).
type Options struct {
	Store    codeintel.IndexStore
	Registry *parse.Registry
	// Languages, when non-empty, gates which languages are indexed
	// (subset of the registry). Empty = every registered language.
	Languages []codeintel.Language
	// MaxFileBytes skips files larger than this (0 = a 2 MiB default).
	MaxFileBytes int64
	// AutoIndexLimit consent-gates a NEW project whose candidate file
	// count exceeds it (0 = a 25,000 default). Explicit indexing
	// (force=true) bypasses the gate — the developer has consented.
	AutoIndexLimit int
	// ExtraIgnoreDirs are directory basenames to skip in addition to
	// the built-in set.
	ExtraIgnoreDirs []string
	Logger          *slog.Logger
}

const (
	defaultMaxFileBytes   = 2 << 20 // 2 MiB
	defaultAutoIndexLimit = 25000
)

// defaultIgnoreDirs are directory basenames never walked. Source-tree
// indexing has different ignore needs than session-file ingestion, so
// this list is owned here (plan D9) rather than shared.
var defaultIgnoreDirs = map[string]struct{}{
	".git": {}, "node_modules": {}, "vendor": {}, ".venv": {}, "venv": {},
	"__pycache__": {}, "dist": {}, "build": {}, "target": {}, ".next": {},
	".idea": {}, ".vscode": {}, ".observer": {}, "bin": {}, "obj": {},
	".gradle": {}, ".cache": {}, ".gomodcache": {}, "testdata": {},
}

// Indexer walks a project's source tree and persists symbols through the
// store seam. It is codeintel's single writer (CLAUDE.md "one owner per
// table") and runs only at index time — never on the proxy hot path
// (ADR-0002).
type Indexer struct {
	opts   Options
	logger *slog.Logger
}

// Report summarizes one IndexProject pass.
type Report struct {
	Project      string
	Scanned      int  // candidate source files seen
	Indexed      int  // files (re)parsed and saved
	Unchanged    int  // skipped — content hash matched an indexed row
	Skipped      int  // skipped — too large / unreadable
	Failed       int  // parse/save errors
	NeedsConsent bool // candidate count exceeded AutoIndexLimit and force=false
}

// New builds an Indexer, applying defaults.
func New(opts Options) *Indexer {
	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = defaultMaxFileBytes
	}
	if opts.AutoIndexLimit <= 0 {
		opts.AutoIndexLimit = defaultAutoIndexLimit
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	return &Indexer{opts: opts, logger: opts.Logger}
}

// IndexProject indexes (or incrementally re-indexes) the source tree
// rooted at root. The project key is the absolute root path. When
// force is false and the project is new and larger than AutoIndexLimit,
// it returns Report{NeedsConsent:true} WITHOUT indexing (the §5b.4 DX
// guardrail); explicit `observer index <path>` passes force=true.
func (ix *Indexer) IndexProject(ctx context.Context, root string, force bool) (Report, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return Report{}, fmt.Errorf("index.IndexProject: abs %q: %w", root, err)
	}
	rep := Report{Project: absRoot}

	candidates, err := ix.collectCandidates(absRoot)
	if err != nil {
		return rep, err
	}
	rep.Scanned = len(candidates)

	if !force && len(candidates) > ix.opts.AutoIndexLimit {
		// Consent gate: register the files as needs_consent so
		// `index status` shows the project, but index nothing.
		for _, c := range candidates {
			_ = ix.opts.Store.CodeIntelRegisterFile(ctx, absRoot, c.path, string(c.lang))
			_ = ix.opts.Store.CodeIntelSetFileStatus(ctx, absRoot, c.path, "needs_consent")
		}
		rep.NeedsConsent = true
		ix.logger.Info("codeintel: project exceeds auto_index_limit; awaiting consent",
			"project", absRoot, "files", len(candidates), "limit", ix.opts.AutoIndexLimit)
		return rep, nil
	}

	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		switch outcome := ix.indexFile(ctx, absRoot, c); outcome {
		case outcomeIndexed:
			rep.Indexed++
		case outcomeUnchanged:
			rep.Unchanged++
		case outcomeSkipped:
			rep.Skipped++
		case outcomeFailed:
			rep.Failed++
		}
	}
	// Project-level name-matched CALLS resolution sweep — fills in the
	// cross-file forward references left unresolved at file-save time.
	// Only worth running when something was (re)indexed this pass.
	if rep.Indexed > 0 {
		if resolved, err := ix.opts.Store.CodeIntelResolveCalls(ctx, absRoot); err != nil {
			ix.logger.Warn("codeintel: call resolution failed", "project", absRoot, "err", err)
		} else {
			ix.logger.Debug("codeintel: resolved calls", "project", absRoot, "edges", resolved)
		}
		// Scoped upgrade pass (W2): bind bare/qualified calls within their
		// package/imports for languages with a scope rule set (Go today),
		// fixing cross-package over-link. Best-effort — a failure leaves the
		// name-matched edges intact.
		if upgraded, err := ix.opts.Store.CodeIntelResolveScoped(ctx, absRoot); err != nil {
			ix.logger.Warn("codeintel: scoped resolution failed", "project", absRoot, "err", err)
		} else {
			ix.logger.Debug("codeintel: scoped-upgraded calls", "project", absRoot, "edges", upgraded)
		}
		// Tier C (Phase 6): rebuild the project's FTS / embedding /
		// MinHash rows from the now-current nodes. Best-effort — a
		// failure leaves search/semantic degraded, never aborts indexing.
		if err := ix.opts.Store.CodeIntelBuildDerived(ctx, absRoot); err != nil {
			ix.logger.Warn("codeintel: build derived (search/semantic) failed", "project", absRoot, "err", err)
		}
	}
	ix.logger.Info("codeintel: indexed project",
		"project", absRoot, "indexed", rep.Indexed, "unchanged", rep.Unchanged,
		"skipped", rep.Skipped, "failed", rep.Failed)
	return rep, nil
}

type candidate struct {
	path string
	lang codeintel.Language
}

// collectCandidates walks the tree and returns every file with a
// supported, allow-listed extension, skipping ignored dirs and symlinks.
func (ix *Indexer) collectCandidates(absRoot string) ([]candidate, error) {
	var out []candidate
	walkErr := filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable dir/file — skip it, don't abort the whole walk.
			return nil //nolint:nilerr // best-effort indexing
		}
		if d.IsDir() {
			if ix.ignoredDir(d.Name()) && path != absRoot {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil // never follow symlinks (loop/escape safety)
		}
		lang, ok := parse.LanguageForPath(path)
		if !ok || !ix.langAllowed(lang) {
			return nil
		}
		out = append(out, candidate{path: path, lang: lang})
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("index.collectCandidates: walk %q: %w", absRoot, walkErr)
	}
	return out, nil
}

type indexOutcome int

const (
	outcomeIndexed indexOutcome = iota
	outcomeUnchanged
	outcomeSkipped
	outcomeFailed
)

func (ix *Indexer) indexFile(ctx context.Context, project string, c candidate) indexOutcome {
	fi, err := os.Stat(c.path)
	if err != nil || fi.Size() > ix.opts.MaxFileBytes {
		return outcomeSkipped
	}
	src, err := os.ReadFile(c.path)
	if err != nil {
		return outcomeSkipped
	}
	hash := hashBytes(src)

	// Incremental: an indexed row with the same content hash is fresh.
	prevHash, status, found, err := ix.opts.Store.CodeIntelFileState(ctx, project, c.path)
	if err == nil && found && status == "indexed" && prevHash == hash {
		return outcomeUnchanged
	}

	parser, ok := ix.opts.Registry.For(c.lang)
	if !ok {
		return outcomeSkipped
	}
	_ = ix.opts.Store.CodeIntelSetFileStatus(ctx, project, c.path, "indexing")

	res, perr := parser.Parse(ctx, src, c.lang, c.path)
	if perr != nil && len(res.Nodes) == 0 {
		_ = ix.opts.Store.CodeIntelSetFileStatus(ctx, project, c.path, "failed")
		ix.logger.Debug("codeintel: parse failed", "file", c.path, "err", perr)
		return outcomeFailed
	}

	saveErr := ix.opts.Store.CodeIntelSaveFile(ctx, codeintel.FileResult{
		Project:     project,
		Path:        c.path,
		Lang:        c.lang,
		Parser:      res.Parser,
		ContentHash: hash,
		MTime:       fi.ModTime().Unix(),
		Nodes:       res.Nodes,
		Imports:     res.Imports,
		Calls:       res.Calls,
		BodyBuckets: bodyBuckets(src, res.Nodes),
	})
	if saveErr != nil {
		_ = ix.opts.Store.CodeIntelSetFileStatus(ctx, project, c.path, "failed")
		ix.logger.Warn("codeintel: save failed", "file", c.path, "err", saveErr)
		return outcomeFailed
	}
	return outcomeIndexed
}

func (ix *Indexer) ignoredDir(name string) bool {
	if _, ok := defaultIgnoreDirs[name]; ok {
		return true
	}
	return slices.Contains(ix.opts.ExtraIgnoreDirs, name)
}

func (ix *Indexer) langAllowed(lang codeintel.Language) bool {
	if len(ix.opts.Languages) == 0 {
		return true
	}
	return slices.Contains(ix.opts.Languages, lang)
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// bodyBuckets computes per-node body-shingle MinHash LSH buckets (W3
// near-clone) from the file content + each node's byte span. The body bytes
// stay HERE (only the band hashes are persisted, privacy). Returns a slice
// aligned with nodes; an entry is nil when the node has no usable byte span
// (so it contributes no clone signature).
func bodyBuckets(src []byte, nodes []codeintel.Node) [][]uint64 {
	if len(nodes) == 0 {
		return nil
	}
	out := make([][]uint64, len(nodes))
	for i, n := range nodes {
		if n.StartByte < 0 || n.EndByte <= n.StartByte || n.EndByte > len(src) {
			continue
		}
		out[i] = semantic.BodyShingleBuckets(string(src[n.StartByte:n.EndByte]))
	}
	return out
}
