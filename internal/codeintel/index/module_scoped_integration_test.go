package index_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/codeintel"
	"github.com/marmutapp/superbased-observer/internal/codeintel/index"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// TestModuleScopedResolution_TS is the W2 §4.2 acceptance test: two TS
// files each define `helper`; main.ts imports it from ./a and calls it.
// The module-import scoped resolver must bind the call to a.ts's helper,
// never b.ts's — the cross-file over-link the name-matched resolver picks
// arbitrarily. It also checks a namespace import (`import * as u`).
func TestModuleScopedResolution_TS(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "obs.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()
	st := store.New(database)

	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.ts"), `export function helper(): number { return 1 }
export function render(): string { return "a" }
`)
	mustWrite(t, filepath.Join(root, "b.ts"), `export function helper(): number { return 2 }
`)
	mustWrite(t, filepath.Join(root, "main.ts"), `import { helper } from './a'
import * as ui from './a'

export function run(): number {
	ui.render()
	return helper()
}
`)

	ix := index.New(index.Options{Store: st, Registry: index.DefaultRegistry()})
	if _, err := ix.IndexProject(ctx, root, true); err != nil {
		t.Fatalf("IndexProject: %v", err)
	}
	eng := codeintel.NewEngine(st)

	aHelper := symID(t, ctx, eng, filepath.Join(root, "a.ts"), "helper")
	bHelper := symID(t, ctx, eng, filepath.Join(root, "b.ts"), "helper")
	aRender := symID(t, ctx, eng, filepath.Join(root, "a.ts"), "render")

	// helper() in main.ts is imported from ./a -> binds a.ts's helper.
	if !hasCaller(t, ctx, eng, aHelper, "run") {
		t.Errorf("a.ts helper: run not bound as caller (import-bound scoped resolution failed)")
	}
	if hasCaller(t, ctx, eng, bHelper, "run") {
		t.Errorf("b.ts helper: run wrongly bound as caller (cross-file over-link not eliminated)")
	}
	// ui.render() via the namespace import binds a.ts's render.
	if !hasCaller(t, ctx, eng, aRender, "run") {
		t.Errorf("a.ts render: run not bound as caller (namespace import binding failed)")
	}
}
