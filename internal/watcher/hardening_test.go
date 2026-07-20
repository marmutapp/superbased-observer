package watcher

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// stubFlagAdapter records whether ParseSessionFile was called and can be told
// to panic, so the size-cap and panic-recovery guards can be exercised.
type stubFlagAdapter struct {
	name        string
	root        string
	called      bool
	shouldPanic bool
}

func (s *stubFlagAdapter) Name() string                { return s.name }
func (s *stubFlagAdapter) WatchPaths() []string        { return []string{s.root} }
func (s *stubFlagAdapter) IsSessionFile(p string) bool { return strings.HasSuffix(p, ".stub") }

func (s *stubFlagAdapter) ParseSessionFile(_ context.Context, _ string, _ int64) (adapter.ParseResult, error) {
	s.called = true
	if s.shouldPanic {
		panic("adapter blew up on a malformed file")
	}
	return adapter.ParseResult{}, nil
}

func quietWatcher(t *testing.T, opts Options) (*Watcher, *store.Store) {
	t.Helper()
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(t.TempDir(), "w.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := store.New(database)
	return New(s, adapter.NewRegistry(), opts), s
}

func TestProcessFileSkipsOversizeFile(t *testing.T) {
	root := t.TempDir()
	stub := &stubFlagAdapter{name: "stub", root: root}
	w, _ := quietWatcher(t, Options{MaxFileBytes: 8})

	path := filepath.Join(root, "big.stub")
	if err := os.WriteFile(path, []byte("this file is well over eight bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.processFile(context.Background(), stub, path, false); err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if stub.called {
		t.Error("adapter was called on an oversize file; the size cap did not skip it")
	}
}

func TestProcessFileParsesWhenUnderCap(t *testing.T) {
	root := t.TempDir()
	stub := &stubFlagAdapter{name: "stub", root: root}
	w, _ := quietWatcher(t, Options{MaxFileBytes: 1024})

	path := filepath.Join(root, "small.stub")
	if err := os.WriteFile(path, []byte("tiny"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.processFile(context.Background(), stub, path, false); err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if !stub.called {
		t.Error("adapter was not called on an under-cap file")
	}
}

func TestProcessFileRecoversFromAdapterPanic(t *testing.T) {
	root := t.TempDir()
	stub := &stubFlagAdapter{name: "stub", root: root, shouldPanic: true}
	w, _ := quietWatcher(t, Options{})

	path := filepath.Join(root, "boom.stub")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Must not propagate the panic; a recovered parse returns nil so the
	// watcher keeps running.
	if err := w.processFile(context.Background(), stub, path, false); err != nil {
		t.Fatalf("processFile returned an error instead of recovering: %v", err)
	}
}
