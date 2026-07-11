package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter/claudecode"
	"github.com/marmutapp/superbased-observer/internal/compression/indexing"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/demo"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// demoSeeder assembles the dashboard's demo-mode seeder (usability arc
// P6.7): a TEMPORARY database in its own temp directory, migrated to
// the current schema, seeded from internal/demo's embedded fixture
// transcripts through the real store.Ingest path (failures recorded,
// FTS5 excerpts indexed so demo search works). The real observer.db
// is never read or written; cleanup closes the handle and removes the
// directory. Assembly lives here in cmd — the dashboard package never
// imports adapter, db, store, or demo packages (the ToolCatalog seam
// pattern).
func demoSeeder(logger *slog.Logger) func(ctx context.Context) (*sql.DB, func() error, error) {
	return func(ctx context.Context) (*sql.DB, func() error, error) {
		events, tokens, err := demo.Events(ctx, time.Now().UTC())
		if err != nil {
			return nil, nil, err
		}
		dir, err := os.MkdirTemp("", "observer-demo-*")
		if err != nil {
			return nil, nil, err
		}
		database, err := db.Open(ctx, db.Options{Path: filepath.Join(dir, "demo.db")})
		if err != nil {
			_ = os.RemoveAll(dir)
			return nil, nil, err
		}
		cleanup := func() error {
			cerr := database.Close()
			rerr := os.RemoveAll(dir)
			return errors.Join(cerr, rerr)
		}
		st := store.New(database)
		if _, err := st.Ingest(ctx, events, tokens, store.IngestOptions{
			IsNativeTool:   claudecode.IsNativeTool,
			RecordFailures: true,
			Indexer:        indexing.New(database, 0), // 0 → DefaultMaxExcerptBytes
		}); err != nil {
			_ = cleanup()
			return nil, nil, err
		}
		logger.Info("demo database seeded", "dir", dir,
			"events", len(events), "token_rows", len(tokens))
		return database, cleanup, nil
	}
}
