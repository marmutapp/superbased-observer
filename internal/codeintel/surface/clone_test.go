package surface_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/codeintel"
	"github.com/marmutapp/superbased-observer/internal/codeintel/index"
	"github.com/marmutapp/superbased-observer/internal/codeintel/surface"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// TestSimilar_BodyCloneAcrossFiles is the W3 acceptance test: two functions
// with DIFFERENT names/signatures but an identical body are detected as
// near-clones — something the old signature-only MinHash (name+fqn+
// signature) could not see. The body-shingle MinHash (computed at index
// time) is what catches it.
func TestSimilar_BodyCloneAcrossFiles(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "obs.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()
	st := store.New(database)

	root := t.TempDir()
	// Identical body, deliberately dissimilar names/params so signature-only
	// similarity would NOT pair them.
	write(t, filepath.Join(root, "a.go"), `package p

func aaa(p int) int {
	acc := 0
	for k := 0; k < p; k++ {
		acc += k
	}
	return acc
}
`)
	write(t, filepath.Join(root, "b.go"), `package p

func zzz(p int) int {
	acc := 0
	for k := 0; k < p; k++ {
		acc += k
	}
	return acc
}
`)

	ix := index.New(index.Options{Store: st, Registry: index.DefaultRegistry()})
	if _, err := ix.IndexProject(ctx, root, true); err != nil {
		t.Fatalf("IndexProject: %v", err)
	}
	svc := surface.New(codeintel.NewEngine(st))
	aaa := find(t, ctx, codeintel.NewEngine(st), filepath.Join(root, "a.go"), "aaa")

	sim, err := svc.Similar(ctx, aaa.ID, 10)
	if err != nil {
		t.Fatalf("Similar: %v", err)
	}
	if !containsName(symNames(sim), "zzz") {
		t.Errorf("Similar(aaa) = %v, want it to include the body-clone zzz", symNames(sim))
	}
}
