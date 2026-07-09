package diag

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
)

// mkObserverDB creates <home>/.observer/observer.db and returns its path.
func mkObserverDB(t *testing.T, home string) string {
	t.Helper()
	dir := filepath.Join(home, ".observer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p := filepath.Join(dir, "observer.db")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestDetectCrossEnvSiblingDBs(t *testing.T) {
	nativeHome := t.TempDir()
	foreignHome := t.TempDir()
	bareForeignHome := t.TempDir() // foreign home with no .observer/observer.db

	daemonDB := mkObserverDB(t, nativeHome) // the daemon's own DB
	foreignDB := mkObserverDB(t, foreignHome)
	mkObserverDB(t, nativeHome) // (already created by daemonDB)

	homes := []crossmount.HomeRoot{
		{Path: nativeHome, OS: "linux", Origin: "native"},
		{Path: foreignHome, OS: "windows", Origin: "wsl-mnt:marmu"},
		{Path: bareForeignHome, OS: "windows", Origin: "wsl-mnt:other"},
	}

	got := DetectCrossEnvSiblingDBs(daemonDB, homes)
	if len(got) != 1 {
		t.Fatalf("got %d siblings want 1: %+v", len(got), got)
	}
	if got[0].Path != foreignDB {
		t.Errorf("sibling path = %q want %q", got[0].Path, foreignDB)
	}
	if got[0].Origin != "wsl-mnt:marmu" || got[0].OS != "windows" {
		t.Errorf("sibling meta = %+v", got[0])
	}
}

// TestDetectCrossEnvSiblingDBs_SkipsNativeHome: the native home's
// observer.db (even a non-daemon custom path would leave it) is never
// flagged — only foreign-OS straddles.
func TestDetectCrossEnvSiblingDBs_SkipsNativeHome(t *testing.T) {
	nativeHome := t.TempDir()
	mkObserverDB(t, nativeHome)
	// Daemon uses a CUSTOM path elsewhere, so the native default DB != dbPath.
	customDB := filepath.Join(t.TempDir(), "custom", "observer.db")

	homes := []crossmount.HomeRoot{
		{Path: nativeHome, OS: "linux", Origin: "native"},
	}
	got := DetectCrossEnvSiblingDBs(customDB, homes)
	if len(got) != 0 {
		t.Errorf("native home must never be flagged, got %+v", got)
	}
}

// TestDetectCrossEnvSiblingDBs_SkipsSameFile: a foreign mount alias that
// resolves to the daemon's own dbPath is not a sibling.
func TestDetectCrossEnvSiblingDBs_SkipsSameFile(t *testing.T) {
	home := t.TempDir()
	db := mkObserverDB(t, home)
	homes := []crossmount.HomeRoot{
		{Path: home, OS: "windows", Origin: "wsl-mnt:marmu"},
	}
	got := DetectCrossEnvSiblingDBs(db, homes) // dbPath == the foreign candidate
	if len(got) != 0 {
		t.Errorf("same-file candidate must be skipped, got %+v", got)
	}
}
