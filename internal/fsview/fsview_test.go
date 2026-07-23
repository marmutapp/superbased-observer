package fsview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeFile is a t.Helper that writes data to root/rel, creating parents.
func writeFile(t *testing.T, root, rel string, data []byte) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolve(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a/b.txt", []byte("hi"))

	// A symlink inside the tree pointing OUTSIDE it.
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	// A symlink inside the tree pointing to a sibling inside it (allowed).
	if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "in")); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		rel     string
		wantErr error
	}{
		{"root itself", "", nil},
		{"dot", ".", nil},
		{"nested file", "a/b.txt", nil},
		{"in-tree symlink", "in", nil},
		{"parent escape", "../etc", ErrOutsideRoot},
		{"deep parent escape", "a/../../etc", ErrOutsideRoot},
		{"symlink escape", "escape", ErrOutsideRoot},
		{"absolute path", string(filepath.Separator) + "etc" + string(filepath.Separator) + "passwd", ErrAbsolutePath},
		{"missing", "nope.txt", ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(context.Background(), root, tt.rel)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if !strings.HasPrefix(got, root) && got != root {
				// got is symlink-resolved; on macOS t.TempDir may itself be a
				// symlink, so compare against the resolved root instead.
				rr, _ := filepath.EvalSymlinks(root)
				if !strings.HasPrefix(got, rr) {
					t.Fatalf("resolved %q not under root %q", got, rr)
				}
			}
		})
	}
}

func TestListSortAndTypes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "zeta.txt", []byte("z"))
	writeFile(t, root, "alpha.txt", []byte("a"))
	if err := os.MkdirAll(filepath.Join(root, "mdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOK := true
	if err := os.Symlink(filepath.Join(root, "adir"), filepath.Join(root, "link")); err != nil {
		symlinkOK = false
	}

	entries, truncated, err := List(context.Background(), root, "")
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("small dir should not be truncated")
	}
	// Dirs first (adir, mdir), then files/symlink by name.
	var names []string
	byName := map[string]EntryType{}
	for _, e := range entries {
		names = append(names, e.Name)
		byName[e.Name] = e.Type
	}
	if byName["adir"] != EntryDir || byName["mdir"] != EntryDir {
		t.Fatalf("dirs misclassified: %v", byName)
	}
	if byName["alpha.txt"] != EntryFile {
		t.Fatalf("file misclassified: %v", byName)
	}
	// adir and mdir must sort before any non-dir.
	firstFileIdx := -1
	for i, e := range entries {
		if e.Type != EntryDir {
			firstFileIdx = i
			break
		}
	}
	for i := 0; i < firstFileIdx; i++ {
		if entries[i].Type != EntryDir {
			t.Fatalf("non-dir before dirs at %d: %v", i, names)
		}
	}
	if symlinkOK && byName["link"] != EntrySymlink {
		t.Fatalf("symlink misclassified: %v (a symlinked dir must not read as dir)", byName)
	}

	// A symlinked directory is typed EntrySymlink in a parent listing and is not
	// itself listable (finding 2b): List on the link's own path is rejected.
	if symlinkOK {
		if _, _, err := List(context.Background(), root, "link"); !errors.Is(err, ErrNotDir) {
			t.Fatalf("List(symlinked dir) err = %v, want ErrNotDir", err)
		}
	}
}

func TestListEntryCap(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < maxEntries+50; i++ {
		writeFile(t, root, filepath.Join("f", pad(i)+".txt"), []byte("x"))
	}
	entries, truncated, err := List(context.Background(), root, "f")
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("want truncated=true past the entry cap")
	}
	if len(entries) != maxEntries {
		t.Fatalf("len(entries) = %d, want %d", len(entries), maxEntries)
	}
}

func TestListErrors(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "file.txt", []byte("x"))
	if _, _, err := List(context.Background(), root, "file.txt"); !errors.Is(err, ErrNotDir) {
		t.Fatalf("List(file) err = %v, want ErrNotDir", err)
	}
	if _, _, err := List(context.Background(), root, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("List(missing) err = %v, want ErrNotFound", err)
	}
	if _, _, err := List(context.Background(), root, "../x"); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("List(escape) err = %v, want ErrOutsideRoot", err)
	}
}

func TestRead(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "text.txt", []byte("hello\nworld\n"))
	writeFile(t, root, "bin.dat", []byte{'a', 'b', 0x00, 'c'})
	big := strings.Repeat("x", 300*1024)
	writeFile(t, root, "big.txt", []byte(big))

	t.Run("text", func(t *testing.T) {
		c, err := Read(context.Background(), root, "text.txt", 0)
		if err != nil {
			t.Fatal(err)
		}
		if c.Binary || c.Truncated {
			t.Fatalf("unexpected flags: %+v", c)
		}
		if c.Data != "hello\nworld\n" {
			t.Fatalf("data = %q", c.Data)
		}
		if c.Size != 12 {
			t.Fatalf("size = %d", c.Size)
		}
	})

	t.Run("binary sniff", func(t *testing.T) {
		c, err := Read(context.Background(), root, "bin.dat", 0)
		if err != nil {
			t.Fatal(err)
		}
		if !c.Binary {
			t.Fatal("want Binary=true (NUL in first 8KB)")
		}
		if c.Data != "" {
			t.Fatalf("binary Data must be empty, got %q", c.Data)
		}
	})

	t.Run("truncation", func(t *testing.T) {
		c, err := Read(context.Background(), root, "big.txt", 256*1024)
		if err != nil {
			t.Fatal(err)
		}
		if !c.Truncated {
			t.Fatal("want Truncated=true")
		}
		if len(c.Data) != 256*1024 {
			t.Fatalf("len(Data) = %d, want %d", len(c.Data), 256*1024)
		}
		if c.Size != int64(len(big)) {
			t.Fatalf("Size = %d, want %d", c.Size, len(big))
		}
	})

	t.Run("dir", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Join(root, "d"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(context.Background(), root, "d", 0); !errors.Is(err, ErrIsDir) {
			t.Fatalf("Read(dir) err = %v, want ErrIsDir", err)
		}
	})

	t.Run("escape", func(t *testing.T) {
		if _, err := Read(context.Background(), root, "../../etc/passwd", 0); !errors.Is(err, ErrOutsideRoot) {
			t.Fatalf("Read(escape) err = %v, want ErrOutsideRoot", err)
		}
	})

	t.Run("missing", func(t *testing.T) {
		if _, err := Read(context.Background(), root, "nope.txt", 0); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Read(missing) err = %v, want ErrNotFound", err)
		}
	})

	t.Run("symlink escape read", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink semantics differ on windows")
		}
		outside := t.TempDir()
		secret := filepath.Join(outside, "secret.txt")
		if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(secret, filepath.Join(root, "leak")); err != nil {
			t.Skipf("symlinks unsupported: %v", err)
		}
		if _, err := Read(context.Background(), root, "leak", 0); !errors.Is(err, ErrOutsideRoot) {
			t.Fatalf("Read(symlink escape) err = %v, want ErrOutsideRoot", err)
		}
	})
}

// pad formats i as a zero-padded 5-digit string for stable name sorting.
func pad(i int) string {
	s := make([]byte, 5)
	for j := 4; j >= 0; j-- {
		s[j] = byte('0' + i%10)
		i /= 10
	}
	return string(s)
}
