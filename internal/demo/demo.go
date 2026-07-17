package demo

import (
	"context"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter/claudecode"
	"github.com/marmutapp/superbased-observer/internal/models"
)

//go:embed fixtures/*.jsonl
var fixturesFS embed.FS

// NewestEventAge is how far before "now" the newest fixture event
// lands after rebasing — close enough that the Live view shows the
// most recent demo session as active on first look.
const NewestEventAge = 5 * time.Minute

// SourceFileScheme prefixes the virtual source_file values stamped on
// demo rows. The fixtures are parsed from a throwaway temp directory;
// recording that path would leak a meaningless local detail into the
// demo rows, so SourceFile is rewritten to a stable
// "demo://<fixture-name>" instead.
const SourceFileScheme = "demo://"

// Events parses the embedded fixture transcripts through the real
// claude-code adapter and rebases every timestamp so the newest event
// lands NewestEventAge before now (relative spacing — the multi-day
// spread — is preserved). The fixtures are written to a temp directory
// only because the adapter API is path-based; it is removed before
// returning.
//
// Any adapter warning is returned as an error: the fixture set is
// static and embedded, so a warning means a malformed fixture shipped
// — fail loudly rather than seed a partial dataset.
func Events(ctx context.Context, now time.Time) ([]models.ToolEvent, []models.TokenEvent, error) {
	dir, err := os.MkdirTemp("", "observer-demo-fixtures-*")
	if err != nil {
		return nil, nil, fmt.Errorf("demo.Events: temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	names, err := fixtureNames()
	if err != nil {
		return nil, nil, err
	}

	a := claudecode.New()
	var events []models.ToolEvent
	var tokens []models.TokenEvent
	for _, name := range names {
		body, err := fixturesFS.ReadFile("fixtures/" + name)
		if err != nil {
			return nil, nil, fmt.Errorf("demo.Events: read embedded %s: %w", name, err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			return nil, nil, fmt.Errorf("demo.Events: stage %s: %w", name, err)
		}
		res, err := a.ParseSessionFile(ctx, path, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("demo.Events: parse %s: %w", name, err)
		}
		if len(res.Warnings) > 0 {
			return nil, nil, fmt.Errorf("demo.Events: fixture %s produced parse warnings (malformed fixture shipped): %v", name, res.Warnings)
		}
		virtual := SourceFileScheme + name
		for i := range res.ToolEvents {
			res.ToolEvents[i].SourceFile = virtual
		}
		for i := range res.TokenEvents {
			res.TokenEvents[i].SourceFile = virtual
		}
		events = append(events, res.ToolEvents...)
		tokens = append(tokens, res.TokenEvents...)
		// CacheObservations are deliberately dropped: the demo store
		// runs without a cachetrack engine, so feeding them would be a
		// silent no-op anyway — the Cache tab keeps its honest empty
		// state in demo mode.
	}
	if len(events) == 0 {
		return nil, nil, fmt.Errorf("demo.Events: embedded fixtures produced zero events")
	}

	rebase(events, tokens, now.UTC().Add(-NewestEventAge))
	return events, tokens, nil
}

// fixtureNames lists the embedded fixture files in stable order.
func fixtureNames() ([]string, error) {
	entries, err := fixturesFS.ReadDir("fixtures")
	if err != nil {
		return nil, fmt.Errorf("demo.fixtureNames: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// rebase shifts every event timestamp by one shared delta so the
// newest event across both slices lands exactly at newestTarget.
func rebase(events []models.ToolEvent, tokens []models.TokenEvent, newestTarget time.Time) {
	var newest time.Time
	for i := range events {
		if events[i].Timestamp.After(newest) {
			newest = events[i].Timestamp
		}
	}
	for i := range tokens {
		if tokens[i].Timestamp.After(newest) {
			newest = tokens[i].Timestamp
		}
	}
	if newest.IsZero() {
		return
	}
	delta := newestTarget.Sub(newest)
	for i := range events {
		events[i].Timestamp = events[i].Timestamp.Add(delta)
	}
	for i := range tokens {
		tokens[i].Timestamp = tokens[i].Timestamp.Add(delta)
	}
}
