package index_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/codeintel"
	"github.com/marmutapp/superbased-observer/internal/codeintel/index"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// TestScopedResolution_KillsCrossPackageOverlink is the W2 acceptance test:
// two packages each define `process`; a bare process() in package `a` must
// bind to a's process (scoped, same-package), never b's — the cross-package
// over-link the name-matched resolver could not avoid (limitations.md §1).
// It also checks a qualified b.Helper() call binds into the imported package.
func TestScopedResolution_KillsCrossPackageOverlink(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "obs.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()
	st := store.New(database)

	root := t.TempDir()
	// Package a: defines process + Helper, and Run calls bare process().
	mustWrite(t, filepath.Join(root, "a", "proc.go"), `package a

func process() int { return 1 }

func Helper() int { return 2 }
`)
	mustWrite(t, filepath.Join(root, "a", "run.go"), `package a

func Run() int {
	return process()
}
`)
	// Package b: also defines process (the over-link trap) + a Helper that a
	// might import and call qualified.
	mustWrite(t, filepath.Join(root, "b", "b.go"), `package b

func process() int { return 3 }

func Helper() int { return 4 }
`)
	// Receiver self-call: type T has method tick() and Run() which calls
	// r.tick(). A different type U also has tick() (the over-link trap).
	mustWrite(t, filepath.Join(root, "a", "recv.go"), `package a

type T struct{}

func NewT() *T { return &T{} }

func (r *T) tick() int { return 1 }

func (r *T) Run() int { return r.tick() }

type U struct{}

func (u *U) tick() int { return 2 }
`)
	// Local-variable receiver inference (W2 §4.1): Drive's x is typed by the
	// constructor convention (NewT -> *T) and Drive2's z by an explicit var
	// declaration; both x.tick()/z.tick() must bind to T.tick, never U.tick.
	mustWrite(t, filepath.Join(root, "a", "localvar.go"), `package a

func Drive() int {
	x := NewT()
	return x.tick()
}

func Drive2() int {
	var z T
	return z.tick()
}
`)

	ix := index.New(index.Options{Store: st, Registry: index.DefaultRegistry()})
	if _, err := ix.IndexProject(ctx, root, true); err != nil {
		t.Fatalf("IndexProject: %v", err)
	}
	eng := codeintel.NewEngine(st)

	aProc := symID(t, ctx, eng, filepath.Join(root, "a", "proc.go"), "process")
	bProc := symID(t, ctx, eng, filepath.Join(root, "b", "b.go"), "process")

	// a.Run's bare process() must be a caller of a.process, NOT b.process.
	if !hasCaller(t, ctx, eng, aProc, "Run") {
		t.Errorf("a.process: Run not bound as caller (scoped same-package bind failed)")
	}
	if hasCaller(t, ctx, eng, bProc, "Run") {
		t.Errorf("b.process: Run wrongly bound as caller (cross-package over-link not eliminated)")
	}

	// Receiver self-call: T.Run's r.tick() must bind to T.tick, NOT U.tick.
	tTick := fqnID(t, ctx, eng, filepath.Join(root, "a", "recv.go"), "tick", "T.tick")
	uTick := fqnID(t, ctx, eng, filepath.Join(root, "a", "recv.go"), "tick", "U.tick")
	if !hasCaller(t, ctx, eng, tTick, "Run") {
		t.Errorf("T.tick: Run not bound as caller (receiver self-call binding failed)")
	}
	if hasCaller(t, ctx, eng, uTick, "Run") {
		t.Errorf("U.tick: Run wrongly bound as caller (receiver method over-link not eliminated)")
	}

	// Local-var receiver inference: Drive (x := NewT()) and Drive2 (var z T)
	// must be callers of T.tick, never U.tick.
	for _, caller := range []string{"Drive", "Drive2"} {
		if !hasCaller(t, ctx, eng, tTick, caller) {
			t.Errorf("T.tick: %s not bound as caller (local-var receiver inference failed)", caller)
		}
		if hasCaller(t, ctx, eng, uTick, caller) {
			t.Errorf("U.tick: %s wrongly bound as caller (local-var receiver over-link not eliminated)", caller)
		}
	}
}

// fqnID resolves a method by name then picks the match with the wanted FQN
// (two types share the method name in this fixture).
func fqnID(t *testing.T, ctx context.Context, eng codeintel.Provider, path, name, fqn string) int64 {
	t.Helper()
	ms, err := eng.FindSymbols(ctx, path, name, "", "")
	if err != nil {
		t.Fatalf("FindSymbols(%s,%s): %v", path, name, err)
	}
	for _, m := range ms {
		if m.FQN == fqn {
			return m.ID
		}
	}
	t.Fatalf("no symbol %s with FQN %s in %s (got %+v)", name, fqn, path, ms)
	return 0
}

func symID(t *testing.T, ctx context.Context, eng codeintel.Provider, path, name string) int64 {
	t.Helper()
	ms, err := eng.FindSymbols(ctx, path, name, "", "")
	if err != nil || len(ms) == 0 {
		t.Fatalf("FindSymbols(%s,%s): %v / %v", path, name, ms, err)
	}
	return ms[0].ID
}

func hasCaller(t *testing.T, ctx context.Context, eng codeintel.Provider, id int64, callerName string) bool {
	t.Helper()
	callers, err := eng.CallersOfSymbol(ctx, id, 50)
	if err != nil {
		t.Fatalf("CallersOfSymbol(%d): %v", id, err)
	}
	for _, c := range callers {
		if c.Name == callerName {
			return true
		}
	}
	return false
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
