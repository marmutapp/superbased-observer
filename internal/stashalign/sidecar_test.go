package stashalign

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestWriteReadSidecar_RoundTrip pins the basic contract: WriteSidecar
// → ReadSidecar yields the exact path back, trailing newline stripped.
func TestWriteReadSidecar_RoundTrip(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	want := filepath.Join(t.TempDir(), "stash")

	if err := WriteSidecar(home, want); err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}
	got, err := ReadSidecar(home)
	if err != nil {
		t.Fatalf("ReadSidecar: %v", err)
	}
	if got != want {
		t.Errorf("round-trip: got %q, want %q", got, want)
	}
}

// TestReadSidecar_MissingReturnsEmpty pins the no-proxy-yet posture:
// a missing sidecar reads as ("", nil), not an error.
func TestReadSidecar_MissingReturnsEmpty(t *testing.T) {
	t.Parallel()
	got, err := ReadSidecar(t.TempDir())
	if err != nil {
		t.Errorf("missing sidecar should not error, got %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestWriteSidecar_CreatesHomeDir pins the auto-create behavior.
// observer's general posture is "create the user-state directory on
// first use" — WriteSidecar follows the same convention so a fresh
// install doesn't fail.
func TestWriteSidecar_CreatesHomeDir(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	home := filepath.Join(parent, "observer-home-that-does-not-exist")

	if err := WriteSidecar(home, "/tmp/stash"); err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}
	if _, err := os.Stat(home); err != nil {
		t.Errorf("home dir not created: %v", err)
	}
	got, err := ReadSidecar(home)
	if err != nil || got != "/tmp/stash" {
		t.Errorf("round-trip after auto-mkdir: got (%q, %v)", got, err)
	}
}

// TestWriteSidecar_AtomicReplace: writing twice keeps the last writer's
// value. Last-writer-wins is the documented semantic.
func TestWriteSidecar_AtomicReplace(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	for _, path := range []string{"/tmp/first", "/tmp/second", "/tmp/third"} {
		if err := WriteSidecar(home, path); err != nil {
			t.Fatalf("WriteSidecar %q: %v", path, err)
		}
	}
	got, err := ReadSidecar(home)
	if err != nil || got != "/tmp/third" {
		t.Errorf("got (%q, %v), want (/tmp/third, nil)", got, err)
	}
}

// TestWriteSidecar_RequiresInputs pins the input-validation guard.
// Both homeDir and stashDir are required.
func TestWriteSidecar_RequiresInputs(t *testing.T) {
	t.Parallel()
	if err := WriteSidecar("", "/tmp/x"); err == nil {
		t.Errorf("empty homeDir should error")
	}
	if err := WriteSidecar(t.TempDir(), ""); err == nil {
		t.Errorf("empty stashDir should error")
	}
}

// TestReadSidecar_RequiresHomeDir pins the input-validation guard on
// the reader.
func TestReadSidecar_RequiresHomeDir(t *testing.T) {
	t.Parallel()
	if _, err := ReadSidecar(""); err == nil {
		t.Errorf("empty homeDir should error")
	}
}

// TestReadSidecar_PropagatesOtherIOErrors: errors other than
// not-exist propagate so operators can distinguish "no proxy ran yet"
// from "permissions are broken."
func TestReadSidecar_PropagatesOtherIOErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based unreadable-file does not restrict the owner on Windows")
	}
	t.Parallel()
	home := t.TempDir()
	// Make the sidecar file unreadable by writing to it then chmod 000.
	if err := WriteSidecar(home, "/tmp/x"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(filepath.Join(home, SidecarFilename), 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(home, SidecarFilename), 0o600) })

	_, err := ReadSidecar(home)
	// On some platforms root can read even mode 000 files. Skip the
	// negative assertion when running as root.
	if os.Geteuid() == 0 {
		t.Skip("running as root — mode 000 is unenforced")
	}
	if err == nil || errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected permission-style I/O error, got %v", err)
	}
}

// TestCompareDirs_ExactMatch: literal equal paths match without
// touching the filesystem.
func TestCompareDirs_ExactMatch(t *testing.T) {
	t.Parallel()
	ok, hint := CompareDirs("/home/user/.observer/stash", "/home/user/.observer/stash")
	if !ok || hint != "" {
		t.Errorf("equal paths: ok=%v hint=%q", ok, hint)
	}
}

// TestCompareDirs_TrailingSlashMatch: paths differing only in trailing
// slash are equivalent (filepath.Clean normalises).
func TestCompareDirs_TrailingSlashMatch(t *testing.T) {
	t.Parallel()
	ok, hint := CompareDirs("/home/user/.observer/stash/", "/home/user/.observer/stash")
	if !ok || hint != "" {
		t.Errorf("trailing-slash: ok=%v hint=%q", ok, hint)
	}
}

// TestCompareDirs_SymlinkResolves: a symlink and its target match
// after EvalSymlinks. Pins the load-bearing case where operators
// symlink the stash dir to a shared volume.
func TestCompareDirs_SymlinkResolves(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "real-stash")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(dir, "link-stash")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported on this filesystem: %v", err)
	}
	ok, hint := CompareDirs(link, target)
	if !ok || hint != "" {
		t.Errorf("symlink ↔ target: ok=%v hint=%q", ok, hint)
	}
}

// TestCompareDirs_Mismatch: divergent paths produce a structured
// recovery hint naming both paths so operators see the discrepancy.
func TestCompareDirs_Mismatch(t *testing.T) {
	t.Parallel()
	mcp := "/home/user/.observer/stash"
	proxy := "/home/user/.observer-codex/stash"
	ok, hint := CompareDirs(mcp, proxy)
	if ok {
		t.Errorf("divergent paths matched")
	}
	if !strings.Contains(hint, mcp) || !strings.Contains(hint, proxy) {
		t.Errorf("hint missing one of the paths: %q", hint)
	}
}

// TestCompareDirs_EmptySide: an empty stash dir on either side is a
// distinct failure mode — the hint names it specifically rather than
// claiming a path-vs-path mismatch.
func TestCompareDirs_EmptySide(t *testing.T) {
	t.Parallel()
	ok, hint := CompareDirs("", "/tmp/proxy")
	if ok {
		t.Errorf("empty mcpDir should not match")
	}
	if !strings.Contains(hint, "no stash dir configured") {
		t.Errorf("expected 'no stash dir configured' hint, got: %q", hint)
	}
}

// TestSidecarPath: trivial helper, but document the contract that
// the path is composable with filepath.
func TestSidecarPath(t *testing.T) {
	t.Parallel()
	got := SidecarPath("/home/user/.observer")
	want := "/home/user/.observer/" + SidecarFilename
	if filepath.ToSlash(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
